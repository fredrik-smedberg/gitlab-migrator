package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
)

// pushOptions are the inputs of pushToGithub.
type pushOptions struct {
	// overwrite restores the force-push of every branch and tag, and the
	// trimming of every branch that GitLab lacks.
	overwrite bool
	// trim deletes branches on GitHub that are not on GitLab.
	trim bool
	// progress receives the messages of the git server; may be nil.
	progress io.Writer
	// afterListing is called when the refs on GitHub have been listed. Tests use it
	// to change GitHub between the listing and the push; may be nil.
	afterListing func()
	// phase is called with the name of the phase that starts; may be nil.
	phase func(name string)
}

// pushResult tells what pushToGithub did not do as asked.
type pushResult struct {
	// SkippedBranches and SkippedTags are left as they are on GitHub.
	SkippedBranches, SkippedTags []divergedRef
	// KeptBranches are branches on GitHub that trimming left alone.
	KeptBranches []divergedRef
	// Rejected are the branches that GitHub refused, see isolateRejected.
	Rejected []config.RefSpec
	// Trimmed are the branches that were deleted on GitHub.
	Trimmed []string
}

func (o pushOptions) setPhase(name string) {
	if o.phase != nil {
		o.phase(name)
	}
}

// pushToGithub pushes the branches and tags of repo to its "github" remote,
// then trims branches if asked to. Without opts.overwrite it never removes
// commits from GitHub that repo does not have.
func pushToGithub(ctx context.Context, repo *git.Repository, opts pushOptions) (*pushResult, error) {
	result := &pushResult{}

	branches, err := repo.Branches()
	if err != nil {
		return nil, fmt.Errorf("retrieving branches: %v", err)
	}
	gitlabBranches := make(map[string]bool)
	refSpecs := make([]config.RefSpec, 0)
	if err = branches.ForEach(func(ref *plumbing.Reference) error {
		gitlabBranches[ref.Name().Short()] = true
		refSpecs = append(refSpecs, config.RefSpec(fmt.Sprintf("%[1]s:%[1]s", ref.Name())))
		return nil
	}); err != nil {
		return nil, fmt.Errorf("parsing branches: %v", err)
	}

	// A branch that has moved on GitHub since the last run, for example main after pull
	// requests were merged there, holds commits GitLab does not have. Force-pushing the
	// GitLab tip would drop them, so such branches are left as they are on GitHub.
	var githubTips map[string]plumbing.Hash
	if !opts.overwrite || opts.trim {
		if githubTips, err = githubRefTips(ctx, repo); err != nil {
			return nil, err
		}
	}
	if opts.afterListing != nil {
		opts.afterListing()
	}
	if !opts.overwrite {
		if refSpecs, result.SkippedBranches, err = splitDiverged(repo, refSpecs, githubTips); err != nil {
			return nil, fmt.Errorf("comparing branches with github repo: %v", err)
		}
	}

	opts.setPhase("pushing branches")
	// GitHub refuses the whole push when any branch is rejected, for example for a file over
	// 100 MB (GH001). Isolate the rejected branches and push the rest, so that one branch does
	// not block the migration of the project.
	var movedBranches []divergedRef
	movedBranches, result.Rejected, err = pushGuarded(ctx, repo, refSpecs, opts.overwrite, opts.progress)
	if err != nil {
		return nil, fmt.Errorf("pushing to github repo: %v", err)
	}
	result.SkippedBranches = append(result.SkippedBranches, movedBranches...)

	if opts.trim {
		var toDelete []config.RefSpec
		toDelete, result.KeptBranches, err = splitTrim(repo, githubTips, gitlabBranches, opts.overwrite)
		if err != nil {
			return nil, fmt.Errorf("determining branches to trim: %v", err)
		}
		if err = pushRefSpecs(ctx, repo, toDelete, true, nil); err != nil {
			return nil, fmt.Errorf("trimming branches on github repo: %v", err)
		}
		for _, rs := range toDelete {
			result.Trimmed = append(result.Trimmed, strings.TrimPrefix(rs.Dst("").String(), "refs/heads/"))
		}
	}

	opts.setPhase("pushing tags")
	tagRefSpecs := []config.RefSpec{"refs/tags/*:refs/tags/*"}
	if !opts.overwrite {
		tagIter, err := repo.Tags()
		if err != nil {
			return nil, fmt.Errorf("retrieving tags: %v", err)
		}
		var tags []*plumbing.Reference
		if err = tagIter.ForEach(func(ref *plumbing.Reference) error {
			tags = append(tags, ref)
			return nil
		}); err != nil {
			return nil, fmt.Errorf("parsing tags: %v", err)
		}
		tagRefSpecs, result.SkippedTags = splitMovedTags(tags, githubTips)
	}
	movedTags, rejectedTags, err := pushGuarded(ctx, repo, tagRefSpecs, opts.overwrite, nil)
	if err != nil {
		return nil, fmt.Errorf("pushing tags to github repo: %v", err)
	}
	if len(rejectedTags) > 0 {
		return nil, fmt.Errorf("pushing tags to github repo: rejected by GitHub: %v", rejectedTags)
	}
	result.SkippedTags = append(result.SkippedTags, movedTags...)

	return result, nil
}

// pushRefSpecs pushes refSpecs to the "github" remote. An empty list pushes
// nothing: go-git would otherwise push every branch (refs/heads/*), forced
// when force is set. A push that has nothing to update is not an error.
func pushRefSpecs(ctx context.Context, repo *git.Repository, refSpecs []config.RefSpec, force bool, progress io.Writer) error {
	if len(refSpecs) == 0 {
		return nil
	}
	err := repo.PushContext(ctx, &git.PushOptions{
		RemoteName: "github",
		Force:      force,
		RefSpecs:   refSpecs,
		Progress:   progress,
	})
	if errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil
	}
	return err
}

// errMoved is returned by the push function of pushGuarded for a ref that moved
// on GitHub after it was listed.
var errMoved = errors.New("ref moved on GitHub")

// isMoveRefusal reports whether err, from a push without Force, can mean that
// a ref on GitHub is not where the listing said. go-git compares the old
// commit in the advertisement of the push itself with the new one, and refuses
// anything that is not a fast-forward ("non-fast-forward update: <ref>", no
// sentinel error exists), also a tag that appeared on another object. For a
// tag object as the new tip it cannot read a commit and reports "object not
// found" instead.
func isMoveRefusal(err error) bool {
	return strings.Contains(err.Error(), "non-fast-forward update: ") || errors.Is(err, plumbing.ErrObjectNotFound)
}

// pushGuarded pushes refSpecs. With overwrite every ref is forced. Without it
// nothing is forced: the guard has let through only new refs and fast-forwards,
// so go-git's own fast-forward check, against the refs that GitHub advertises
// for the push itself, closes the window between the listing and the push. A
// ref that go-git refuses because it moved meanwhile is returned in moved and
// is not an error. Rejections by the server are returned in rejected, see
// isolateRejected. Any other error is returned as it is.
func pushGuarded(ctx context.Context, repo *git.Repository, refSpecs []config.RefSpec, overwrite bool, progress io.Writer) (moved []divergedRef, rejected []config.RefSpec, err error) {
	movedSpecs := make(map[config.RefSpec]divergedRef)
	isRejection := func(err error) bool { return errors.Is(err, errMoved) || isRefRejection(err) }

	rejected, err = isolateRejectedBy(refSpecs, func(rs []config.RefSpec) error {
		err := pushRefSpecs(ctx, repo, rs, overwrite, progress)
		if err == nil || overwrite || !isMoveRefusal(err) {
			return err
		}
		if len(rs) > 1 {
			return errMoved // find out which of them
		}

		// A refusal is only taken for a moved ref when GitHub confirms it.
		ref, rerr := repo.Reference(plumbing.ReferenceName(rs[0].Src()), true)
		if rerr != nil {
			return err
		}
		tips, lerr := githubRefTips(ctx, repo)
		if lerr != nil {
			return lerr
		}
		tip, ok := tips[rs[0].Dst(plumbing.ReferenceName(rs[0].Src())).String()]
		if !ok || tip == ref.Hash() {
			return err
		}
		movedSpecs[rs[0]] = divergedRef{Name: plumbing.ReferenceName(rs[0].Src()).Short(), GithubTip: tip, GitlabTip: ref.Hash()}
		return errMoved
	}, isRejection)
	if err != nil {
		return nil, nil, err
	}

	var serverRejected []config.RefSpec
	for _, rs := range rejected {
		if d, ok := movedSpecs[rs]; ok {
			moved = append(moved, d)
		} else {
			serverRejected = append(serverRejected, rs)
		}
	}
	return moved, serverRejected, nil
}

// splitTrim returns the delete refspecs for the branches on GitHub that are not
// in gitlabBranches. Without overwrite a branch whose tip repo does not have is
// kept and returned as such: it is work that exists only on GitHub, for example
// a pull request branch created there.
func splitTrim(repo *git.Repository, githubTips map[string]plumbing.Hash, gitlabBranches map[string]bool, overwrite bool) ([]config.RefSpec, []divergedRef, error) {
	names := make([]string, 0, len(githubTips))
	for name := range githubTips {
		if plumbing.ReferenceName(name).IsBranch() {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	var toDelete []config.RefSpec
	var kept []divergedRef
	for _, name := range names {
		short := plumbing.ReferenceName(name).Short()
		if gitlabBranches[short] {
			continue
		}
		if !overwrite {
			err := repo.Storer.HasEncodedObject(githubTips[name])
			if errors.Is(err, plumbing.ErrObjectNotFound) {
				kept = append(kept, divergedRef{Name: short, GithubTip: githubTips[name]})
				continue
			}
			if err != nil {
				return nil, nil, fmt.Errorf("looking up %s: %v", githubTips[name], err)
			}
		}
		toDelete = append(toDelete, config.RefSpec(":"+name))
	}
	return toDelete, kept, nil
}

// renameTrunkRef renames the branch from to to in repo, if repo has it.
func renameTrunkRef(repo *git.Repository, from, to string) error {
	old, err := repo.Reference(plumbing.NewBranchReferenceName(from), false)
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return nil
		}
		return err
	}
	if err = repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(to), old.Hash())); err != nil {
		return fmt.Errorf("creating trunk branch: %v", err)
	}
	if err = repo.Storer.RemoveReference(old.Name()); err != nil {
		return fmt.Errorf("deleting old trunk branch: %v", err)
	}
	return nil
}

// skippedRefNames returns the sorted names of refs.
func skippedRefNames(refs []divergedRef) []string {
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		names = append(names, ref.Name)
	}
	sort.Strings(names)
	return names
}

// skippedBranchesOf returns those of the source and target branch of a merge
// request that were not pushed to GitHub, without duplicates.
func (p *project) skippedBranchesOf(source, target string) []string {
	var skipped []string
	for _, branch := range []string{source, target} {
		if p.skippedBranches[branch] && !slices.Contains(skipped, branch) {
			skipped = append(skipped, branch)
		}
	}
	return skipped
}
