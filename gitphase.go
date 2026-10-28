package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/google/go-github/v74/github"
)

const (
	fetchChunkSize  = 200 // commit SHAs per fetch from GitLab
	pushChunkSize   = 100 // refspecs per push of temporary branches
	deleteChunkSize = 200 // refspecs per push of temporary branch deletions
)

// chunk splits items into consecutive slices of at most size elements. A size
// of zero or less returns all items in one chunk.
func chunk[T any](items []T, size int) [][]T {
	if len(items) == 0 {
		return nil
	}
	if size <= 0 {
		return [][]T{items}
	}

	chunks := make([][]T, 0, (len(items)+size-1)/size)
	for size < len(items) {
		chunks = append(chunks, items[:size:size])
		items = items[size:]
	}
	return append(chunks, items)
}

// pushTemporaryBranches fetches the commits for all merge requests that need
// temporary branches, verifies that they are present, and pushes the temporary
// source and target branches to GitHub in batches. A merge request whose
// commits are still missing after the fetch is marked to be skipped.
func (p *project) pushTemporaryBranches(ctx context.Context, work []*mrWork) error {
	var shas []string
	for _, w := range work {
		if w.needsBranch && w.skip == "" {
			shas = append(shas, w.sourceHash.String(), w.targetHash.String())
		}
	}
	if len(shas) == 0 {
		return nil
	}

	// Closed or merged merge requests often have their source branch deleted, so
	// the commits are not reachable from any fetched branch. GitLab allows
	// fetching them directly by SHA (AllowReachableSHA1InWant).
	for _, shaChunk := range chunk(shas, fetchChunkSize) {
		if err := ctx.Err(); err != nil {
			return err
		}
		logger.Debug("fetching merge request commits by SHA", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "count", len(shaChunk))
		if err := p.fetchCommitsBySHA(ctx, shaChunk); err != nil {
			return fmt.Errorf("fetching merge request commits by SHA: %v", err)
		}
	}

	var refs []plumbing.ReferenceName
	defer func() {
		// Drop the local temporary refs so they don't accumulate in the set of haves for later fetches and pushes
		for _, ref := range refs {
			if err := p.repo.Storer.RemoveReference(ref); err != nil {
				sendErr(fmt.Errorf("removing local temporary branch %s: %v", ref.Short(), err))
			}
		}
	}()

	var refSpecs []config.RefSpec
	for _, w := range work {
		if !w.needsBranch || w.skip != "" {
			continue
		}

		// Skip rather than fail at push time if an object is missing
		sourceCommit, err := object.GetCommit(p.repo.Storer, w.sourceHash)
		if err != nil {
			w.skip = fmt.Sprintf("commit %s not found after fetch", w.sourceHash)
			continue
		}
		targetCommit, err := object.GetCommit(p.repo.Storer, w.targetHash)
		if err != nil {
			w.skip = fmt.Sprintf("commit %s not found after fetch", w.targetHash)
			continue
		}

		// GitHub rejects a pull request without commits between base and head, so skip it here
		// instead of pushing branches for a pull request that cannot be created.
		noCommits := sourceCommit.Hash == targetCommit.Hash
		if !noCommits {
			if noCommits, err = sourceCommit.IsAncestor(targetCommit); err != nil {
				// Not decisive; push the branches and let GitHub decide.
				logger.Warn("could not check whether source commit is an ancestor of target commit", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "merge_request_id", w.mr.IID, "error", err)
				noCommits = false
			}
		}
		if noCommits {
			w.skip = "source branch has no commits beyond the target branch"
			continue
		}

		for _, branch := range []struct {
			name string
			hash plumbing.Hash
		}{{w.targetRef, w.targetHash}, {w.sourceRef, w.sourceHash}} {
			ref := plumbing.NewBranchReferenceName(branch.name)
			logger.Trace("creating temporary branch for merged/closed merge request", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", w.mr.IID, "branch", branch.name, "sha", branch.hash)
			if err := p.repo.Storer.SetReference(plumbing.NewHashReference(ref, branch.hash)); err != nil {
				return fmt.Errorf("creating temporary branch %s: %v", branch.name, err)
			}
			refs = append(refs, ref)
			refSpecs = append(refSpecs, config.RefSpec(fmt.Sprintf("%[1]s:%[1]s", ref)))
		}
	}

	for _, refSpecChunk := range chunk(refSpecs, pushChunkSize) {
		if err := ctx.Err(); err != nil {
			return err
		}
		logger.Debug("pushing temporary branches", "owner", p.githubPath[0], "repo", p.githubPath[1], "count", len(refSpecChunk))
		if err := p.repo.PushContext(ctx, &git.PushOptions{
			RemoteName: "github",
			RefSpecs:   refSpecChunk,
			Force:      true,
		}); err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
			return fmt.Errorf("pushing temporary branches to github: %v", err)
		}
	}

	return nil
}

// deleteTemporaryBranches deletes all migration-source-* and migration-target-*
// branches on GitHub, including leftovers from earlier interrupted runs. The
// branches are listed from GitHub directly, not from the branch cache, which
// does not know about the temporary branches.
func (p *project) deleteTemporaryBranches(ctx context.Context) error {
	branches, _, err := gh.Repositories.ListBranches(ctx, p.githubPath[0], p.githubPath[1], &github.BranchListOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	})
	if err != nil {
		return fmt.Errorf("listing branches: %v", err)
	}

	var refSpecs []config.RefSpec
	for _, branch := range branches {
		name := branch.GetName()
		if strings.HasPrefix(name, "migration-source-") || strings.HasPrefix(name, "migration-target-") {
			refSpecs = append(refSpecs, config.RefSpec(fmt.Sprintf(":refs/heads/%s", name)))
		}
	}

	if len(refSpecs) == 0 {
		logger.Trace("no temporary branches to delete", "owner", p.githubPath[0], "repo", p.githubPath[1])
		return nil
	}

	for _, refSpecChunk := range chunk(refSpecs, deleteChunkSize) {
		logger.Debug("deleting temporary branches", "owner", p.githubPath[0], "repo", p.githubPath[1], "count", len(refSpecChunk))
		if err := p.repo.PushContext(ctx, &git.PushOptions{
			RemoteName: "github",
			RefSpecs:   refSpecChunk,
			Force:      true,
		}); err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
			return fmt.Errorf("pushing branch deletions to github: %v", err)
		}
	}

	return nil
}
