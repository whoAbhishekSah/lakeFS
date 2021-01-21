package onboard

//go:generate mockgen -source=rocks_catalog_actions.go -destination=mock/rocks_catalog_actions.go -package=mock

import (
	"context"
	"errors"
	"fmt"

	"github.com/treeverse/lakefs/catalog"
	"github.com/treeverse/lakefs/catalog/rocks"
	"github.com/treeverse/lakefs/cmdutils"
	"github.com/treeverse/lakefs/graveler"
	"github.com/treeverse/lakefs/logging"
)

// RocksCatalogRepoActions is in-charge of importing data to lakeFS with Rocks implementation
type RocksCatalogRepoActions struct {
	repoID    graveler.RepositoryID
	committer string
	logger    logging.Logger

	entryCataloger     entryCataloger
	createdMetaRangeID *graveler.MetaRangeID
	previousCommitID   graveler.CommitID

	progress *cmdutils.Progress
	commit   *cmdutils.Progress
	prefixes []string
	ref      graveler.Ref
}

func (c *RocksCatalogRepoActions) Progress() []*cmdutils.Progress {
	return []*cmdutils.Progress{c.commit, c.progress}
}

// entryCataloger is a facet for EntryCatalog for rocks import commands
type entryCataloger interface {
	WriteMetaRange(ctx context.Context, repositoryID graveler.RepositoryID, it rocks.EntryIterator) (*graveler.MetaRangeID, error)
	CommitExistingMetaRange(ctx context.Context, repositoryID graveler.RepositoryID, parentCommitID graveler.CommitID, metaRangeID graveler.MetaRangeID, committer string, message string, metadata graveler.Metadata) (graveler.CommitID, error)
	ListEntries(ctx context.Context, repositoryID graveler.RepositoryID, ref graveler.Ref, prefix, delimiter rocks.Path) (rocks.EntryListingIterator, error)
	UpdateBranch(ctx context.Context, repositoryID graveler.RepositoryID, branchID graveler.BranchID, ref graveler.Ref) (*graveler.Branch, error)
	GetBranch(ctx context.Context, repositoryID graveler.RepositoryID, branchID graveler.BranchID) (*graveler.Branch, error)
	CreateBranch(ctx context.Context, repositoryID graveler.RepositoryID, branchID graveler.BranchID, ref graveler.Ref) (*graveler.Branch, error)
}

func NewRocksCatalogRepoActions(metaRanger entryCataloger, repository graveler.RepositoryID, committer string, logger logging.Logger, prefixes []string) *RocksCatalogRepoActions {
	return &RocksCatalogRepoActions{
		entryCataloger: metaRanger,
		repoID:         repository,
		committer:      committer,
		logger:         logger,
		prefixes:       prefixes,
		progress:       cmdutils.NewActiveProgress("Objects imported", cmdutils.Spinner),
		commit:         cmdutils.NewActiveProgress("Commit progress", cmdutils.Spinner),
		ref:            catalog.DefaultImportBranchName, // TODO(itai): change this to any chosen commit by the user to support plumbing
	}
}

var ErrWrongIterator = errors.New("rocksCatalogRepoActions can only accept InventoryIterator")

func (c *RocksCatalogRepoActions) ApplyImport(ctx context.Context, it Iterator, _ bool) (*Stats, error) {
	c.logger.Trace("start apply import")

	c.progress.Activate()

	invIt, ok := it.(*InventoryIterator)
	if !ok {
		return nil, ErrWrongIterator
	}

	listIt, err := c.entryCataloger.ListEntries(ctx, c.repoID, c.ref, "", "")
	if err != nil {
		return nil, fmt.Errorf("listing commit: %w", err)
	}

	c.createdMetaRangeID, err = c.entryCataloger.WriteMetaRange(ctx, c.repoID, newPrefixMergeIterator(NewValueToEntryIterator(invIt, c.progress), listIt, c.prefixes))
	if err != nil {
		return nil, fmt.Errorf("write meta range: %w", err)
	}

	c.progress.SetCompleted(true)
	return &Stats{
		AddedOrChanged: int(c.progress.Current()),
	}, nil
}

func (c *RocksCatalogRepoActions) GetPreviousCommit(ctx context.Context) (commit *catalog.CommitLog, err error) {
	branch, err := c.entryCataloger.GetBranch(ctx, c.repoID, catalog.DefaultImportBranchName)
	if errors.Is(err, graveler.ErrBranchNotFound) {
		// first import, let's create the branch with an empty commit
		branch, err = c.entryCataloger.CreateBranch(ctx, c.repoID, catalog.DefaultImportBranchName, "master")
		if err != nil {
			return nil, fmt.Errorf("creating default branch %s: %w", catalog.DefaultImportBranchName, err)
		}
	}

	c.previousCommitID = branch.CommitID

	// returning nil since Rocks doesn't use the diff iterator
	return nil, nil
}

var ErrNoMetaRange = errors.New("nothing to commit - meta-range wasn't created")

func (c *RocksCatalogRepoActions) Commit(ctx context.Context, commitMsg string, metadata catalog.Metadata) (string, error) {
	c.commit.Activate()
	defer c.commit.SetCompleted(true)

	if c.createdMetaRangeID == nil {
		return "", ErrNoMetaRange
	}
	commitID, err := c.entryCataloger.CommitExistingMetaRange(ctx, c.repoID, c.previousCommitID, *c.createdMetaRangeID, c.committer, commitMsg, graveler.Metadata(metadata))
	if err != nil {
		return "", fmt.Errorf("creating commit from existing metarange %s: %w", *c.createdMetaRangeID, err)
	}

	_, err = c.entryCataloger.UpdateBranch(ctx, c.repoID, catalog.DefaultImportBranchName, graveler.Ref(commitID))
	if err != nil {
		return "", fmt.Errorf("updating branch: %w", err)
	}

	return string(commitID), nil
}
