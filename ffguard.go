package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

// divergedBranch is a branch whose tip on GitHub holds commits that the same
// branch on GitLab does not have, for example pull requests merged on GitHub
// after the first migration.
type divergedBranch struct {
	Name      string
	GithubTip plumbing.Hash
	GitlabTip plumbing.Hash
}

// isFastForward reports whether moving a branch from githubTip to gitlabTip
// only adds commits. A githubTip that is not among the objects fetched from
// GitLab is a commit GitLab does not have, so the move would drop it.
func isFastForward(repo *git.Repository, githubTip, gitlabTip plumbing.Hash) (bool, error) {
	if githubTip == gitlabTip {
		return true, nil
	}

	githubCommit, err := repo.CommitObject(githubTip)
	if errors.Is(err, plumbing.ErrObjectNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading commit %s: %v", githubTip, err)
	}

	gitlabCommit, err := repo.CommitObject(gitlabTip)
	if err != nil {
		return false, fmt.Errorf("reading commit %s: %v", gitlabTip, err)
	}

	return githubCommit.IsAncestor(gitlabCommit)
}

// splitDiverged separates the branch refspecs that can be pushed to GitHub
// without dropping commits from those that cannot. githubTips maps full
// reference names (refs/heads/...) to the current tips on GitHub; a branch
// that is not in it does not exist on GitHub yet and is pushed.
func splitDiverged(repo *git.Repository, refSpecs []config.RefSpec, githubTips map[string]plumbing.Hash) ([]config.RefSpec, []divergedBranch, error) {
	push := make([]config.RefSpec, 0, len(refSpecs))
	var diverged []divergedBranch

	for _, rs := range refSpecs {
		githubTip, onGithub := githubTips[rs.Dst(plumbing.ReferenceName(rs.Src())).String()]
		if !onGithub {
			push = append(push, rs)
			continue
		}

		ref, err := repo.Reference(plumbing.ReferenceName(rs.Src()), true)
		if err != nil {
			return nil, nil, fmt.Errorf("resolving %s: %v", rs.Src(), err)
		}

		ok, err := isFastForward(repo, githubTip, ref.Hash())
		if err != nil {
			return nil, nil, err
		}
		if ok {
			push = append(push, rs)
			continue
		}

		diverged = append(diverged, divergedBranch{
			Name:      plumbing.ReferenceName(rs.Src()).Short(),
			GithubTip: githubTip,
			GitlabTip: ref.Hash(),
		})
	}

	return push, diverged, nil
}

// splitMovedTags separates the tags that can be pushed to GitHub from those
// that already exist there on another object, for example a tag that was
// re-created on GitHub and has a release. githubTips maps full reference names
// (refs/tags/...) to the objects the tags point at on GitHub.
func splitMovedTags(tags []*plumbing.Reference, githubTips map[string]plumbing.Hash) ([]config.RefSpec, []divergedBranch) {
	push := make([]config.RefSpec, 0, len(tags))
	var moved []divergedBranch

	for _, tag := range tags {
		githubTip, onGithub := githubTips[tag.Name().String()]
		if onGithub && githubTip != tag.Hash() {
			moved = append(moved, divergedBranch{Name: tag.Name().Short(), GithubTip: githubTip, GitlabTip: tag.Hash()})
			continue
		}
		push = append(push, config.RefSpec(fmt.Sprintf("%[1]s:%[1]s", tag.Name())))
	}

	return push, moved
}

// githubRefTips lists the branches and tags of the "github" remote in one
// call, keyed by full reference name. The REST branch listing is paginated
// and cached, which is no base for deciding what a force-push may overwrite.
func githubRefTips(ctx context.Context, repo *git.Repository) (map[string]plumbing.Hash, error) {
	remote, err := repo.Remote("github")
	if err != nil {
		return nil, fmt.Errorf("looking up github remote: %v", err)
	}

	refs, err := remote.ListContext(ctx, &git.ListOptions{})
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing github references: %v", err)
	}

	tips := make(map[string]plumbing.Hash)
	for _, ref := range refs {
		if ref.Type() == plumbing.HashReference && (ref.Name().IsBranch() || ref.Name().IsTag()) {
			tips[ref.Name().String()] = ref.Hash()
		}
	}
	return tips, nil
}
