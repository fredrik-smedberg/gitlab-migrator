package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// requireGit skips the test when the git binary, which go-git runs to talk to
// a local repository, is not installed.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
}

// newBareRemote creates an empty bare repository standing in for GitHub and
// returns its path.
func newBareRemote(t *testing.T) string {
	t.Helper()
	requireGit(t)
	path := filepath.Join(t.TempDir(), "github.git")
	if _, err := git.PlainInit(path, true); err != nil {
		t.Fatalf("creating bare repository: %v", err)
	}
	return path
}

// seedRemote fills the bare repository at path: build creates commits and refs
// in a scratch repository, and everything in it is then pushed.
func seedRemote(t *testing.T, path string, build func(repo *git.Repository)) {
	t.Helper()
	repo := newGuardTestRepo(t)
	build(repo)
	remote, err := repo.CreateRemote(&config.RemoteConfig{Name: "seed", URLs: []string{path}})
	if err != nil {
		t.Fatalf("creating seed remote: %v", err)
	}
	if err := remote.Push(&git.PushOptions{
		RemoteName: "seed",
		Force:      true,
		RefSpecs:   []config.RefSpec{"refs/heads/*:refs/heads/*", "refs/tags/*:refs/tags/*"},
	}); err != nil && err != git.NoErrAlreadyUpToDate {
		t.Fatalf("seeding remote: %v", err)
	}
}

// newGitlabClone returns an in-memory repository, standing in for the GitLab
// clone, with a "github" remote that is configured like the one in migrate.
func newGitlabClone(t *testing.T, githubPath string, build func(repo *git.Repository)) *git.Repository {
	t.Helper()
	repo := newGuardTestRepo(t)
	build(repo)
	if _, err := repo.CreateRemote(&config.RemoteConfig{Name: "github", URLs: []string{githubPath}, Mirror: true}); err != nil {
		t.Fatalf("creating github remote: %v", err)
	}
	return repo
}

// remoteRefs reads the branches and tags of the bare repository at path.
func remoteRefs(t *testing.T, path string) map[string]plumbing.Hash {
	t.Helper()
	repo, err := git.PlainOpen(path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	iter, err := repo.References()
	if err != nil {
		t.Fatalf("listing references: %v", err)
	}
	refs := make(map[string]plumbing.Hash)
	if err := iter.ForEach(func(ref *plumbing.Reference) error {
		if ref.Type() == plumbing.HashReference && (ref.Name().IsBranch() || ref.Name().IsTag()) {
			refs[ref.Name().String()] = ref.Hash()
		}
		return nil
	}); err != nil {
		t.Fatalf("reading references: %v", err)
	}
	return refs
}

func guardTestTag(t *testing.T, repo *git.Repository, name string, target plumbing.Hash) plumbing.Hash {
	t.Helper()
	tag := &object.Tag{
		Name:       name,
		Tagger:     object.Signature{Name: "test", Email: "test@example.com", When: time.Unix(1700000000, 0)},
		Message:    "release " + name,
		TargetType: plumbing.CommitObject,
		Target:     target,
	}
	obj := repo.Storer.NewEncodedObject()
	if err := tag.Encode(obj); err != nil {
		t.Fatalf("encoding tag: %v", err)
	}
	hash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatalf("storing tag: %v", err)
	}
	guardTestRef(t, repo, plumbing.NewTagReferenceName(name), hash)
	return hash
}

func guardTestRef(t *testing.T, repo *git.Repository, name plumbing.ReferenceName, hash plumbing.Hash) {
	t.Helper()
	if err := repo.Storer.SetReference(plumbing.NewHashReference(name, hash)); err != nil {
		t.Fatalf("setting %s: %v", name, err)
	}
}

func assertRemote(t *testing.T, path string, want map[string]plumbing.Hash) {
	t.Helper()
	if got := remoteRefs(t, path); !reflect.DeepEqual(got, want) {
		t.Errorf("refs on remote = %v, want %v", got, want)
	}
}

func names(refs []divergedRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Name)
	}
	sort.Strings(out)
	return out
}

func TestGithubRefTipsEmptyRemote(t *testing.T) {
	path := newBareRemote(t)
	repo := newGitlabClone(t, path, func(*git.Repository) {})

	tips, err := githubRefTips(context.Background(), repo)
	if err != nil {
		t.Fatalf("githubRefTips: %v", err)
	}
	if len(tips) != 0 {
		t.Errorf("tips = %v, want none", tips)
	}
}

func TestGithubRefTipsFailsClosed(t *testing.T) {
	requireGit(t)
	repo := newGitlabClone(t, filepath.Join(t.TempDir(), "does-not-exist.git"), func(*git.Repository) {})

	tips, err := githubRefTips(context.Background(), repo)
	if err == nil {
		t.Fatalf("githubRefTips = %v, nil; want an error, as no listing means no basis to decide", tips)
	}
}

func TestGithubRefTipsFiltersRefs(t *testing.T) {
	path := newBareRemote(t)
	var base plumbing.Hash
	seedRemote(t, path, func(repo *git.Repository) {
		base = guardTestCommit(t, repo, "base")
		guardTestBranch(t, repo, "main", base)
		guardTestRef(t, repo, plumbing.NewTagReferenceName("v1"), base)
		guardTestRef(t, repo, "refs/pull/1/head", base)
		guardTestRef(t, repo, "refs/notes/commits", base)
	})
	// HEAD of a bare repository is a symbolic reference to refs/heads/master.
	bare, err := git.PlainOpen(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := bare.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, "refs/heads/main")); err != nil {
		t.Fatal(err)
	}
	repo := newGitlabClone(t, path, func(*git.Repository) {})

	tips, err := githubRefTips(context.Background(), repo)
	if err != nil {
		t.Fatalf("githubRefTips: %v", err)
	}
	want := map[string]plumbing.Hash{"refs/heads/main": base, "refs/tags/v1": base}
	if !reflect.DeepEqual(tips, want) {
		t.Errorf("tips = %v, want %v", tips, want)
	}
}

// history builds base, then next on top of it, and returns both.
func history(t *testing.T, repo *git.Repository) (base, next plumbing.Hash) {
	base = guardTestCommit(t, repo, "base")
	next = guardTestCommit(t, repo, "next", base)
	return base, next
}

func TestPushToGithubTrimWithNothingToDeletePushesNothing(t *testing.T) {
	path := newBareRemote(t)
	var base, next, githubOnly plumbing.Hash
	seedRemote(t, path, func(repo *git.Repository) {
		base, next = history(t, repo)
		githubOnly = guardTestCommit(t, repo, "merged on github", base)
		guardTestBranch(t, repo, "main", githubOnly)
		guardTestBranch(t, repo, "feature", base)
	})
	repo := newGitlabClone(t, path, func(repo *git.Repository) {
		history(t, repo)
		guardTestBranch(t, repo, "main", next)
		guardTestBranch(t, repo, "feature", base)
	})

	result, err := pushToGithub(context.Background(), repo, pushOptions{trim: true})
	if err != nil {
		t.Fatalf("pushToGithub: %v", err)
	}
	// main is skipped by the guard and trimming must not force-push it behind its back.
	assertRemote(t, path, map[string]plumbing.Hash{"refs/heads/main": githubOnly, "refs/heads/feature": base})
	if got := names(result.SkippedBranches); !reflect.DeepEqual(got, []string{"main"}) {
		t.Errorf("skipped branches = %v, want [main]", got)
	}
	if len(result.Trimmed) != 0 {
		t.Errorf("trimmed = %v, want none", result.Trimmed)
	}
}

func TestPushToGithubTrimKeepsWorkThatExistsOnlyOnGithub(t *testing.T) {
	var base, next, githubOnly plumbing.Hash
	seed := func(path string) {
		seedRemote(t, path, func(repo *git.Repository) {
			base, next = history(t, repo)
			githubOnly = guardTestCommit(t, repo, "pull request on github", base)
			guardTestBranch(t, repo, "main", base)
			guardTestBranch(t, repo, "pr-branch", githubOnly) // tip GitLab does not have
			guardTestBranch(t, repo, "old-branch", base)      // tip GitLab has, branch deleted there
		})
	}
	clone := func(path string) *git.Repository {
		return newGitlabClone(t, path, func(repo *git.Repository) {
			history(t, repo)
			guardTestBranch(t, repo, "main", next)
		})
	}

	t.Run("without overwrite", func(t *testing.T) {
		path := newBareRemote(t)
		seed(path)
		result, err := pushToGithub(context.Background(), clone(path), pushOptions{trim: true})
		if err != nil {
			t.Fatalf("pushToGithub: %v", err)
		}
		assertRemote(t, path, map[string]plumbing.Hash{"refs/heads/main": next, "refs/heads/pr-branch": githubOnly})
		if got := names(result.KeptBranches); !reflect.DeepEqual(got, []string{"pr-branch"}) {
			t.Errorf("kept = %v, want [pr-branch]", got)
		}
		if !reflect.DeepEqual(result.Trimmed, []string{"old-branch"}) {
			t.Errorf("trimmed = %v, want [old-branch]", result.Trimmed)
		}
	})

	t.Run("with overwrite", func(t *testing.T) {
		path := newBareRemote(t)
		seed(path)
		result, err := pushToGithub(context.Background(), clone(path), pushOptions{trim: true, overwrite: true})
		if err != nil {
			t.Fatalf("pushToGithub: %v", err)
		}
		assertRemote(t, path, map[string]plumbing.Hash{"refs/heads/main": next})
		sort.Strings(result.Trimmed)
		if !reflect.DeepEqual(result.Trimmed, []string{"old-branch", "pr-branch"}) {
			t.Errorf("trimmed = %v, want [old-branch pr-branch]", result.Trimmed)
		}
		if len(result.KeptBranches) != 0 {
			t.Errorf("kept = %v, want none", result.KeptBranches)
		}
	})
}

func TestPushToGithubWithoutTrimLeavesGithubOnlyBranches(t *testing.T) {
	path := newBareRemote(t)
	var base, next, githubOnly plumbing.Hash
	seedRemote(t, path, func(repo *git.Repository) {
		base, next = history(t, repo)
		githubOnly = guardTestCommit(t, repo, "pull request on github", base)
		guardTestBranch(t, repo, "main", base)
		guardTestBranch(t, repo, "pr-branch", githubOnly)
	})
	repo := newGitlabClone(t, path, func(repo *git.Repository) {
		history(t, repo)
		guardTestBranch(t, repo, "main", next)
	})

	if _, err := pushToGithub(context.Background(), repo, pushOptions{}); err != nil {
		t.Fatalf("pushToGithub: %v", err)
	}
	assertRemote(t, path, map[string]plumbing.Hash{"refs/heads/main": next, "refs/heads/pr-branch": githubOnly})
}

func TestPushToGithubOverwriteFlag(t *testing.T) {
	var base, next, githubOnly plumbing.Hash
	seed := func(path string) {
		seedRemote(t, path, func(repo *git.Repository) {
			base, next = history(t, repo)
			githubOnly = guardTestCommit(t, repo, "merged on github", base)
			guardTestBranch(t, repo, "main", githubOnly)
			guardTestRef(t, repo, plumbing.NewTagReferenceName("v1"), githubOnly)
		})
	}
	clone := func(path string) *git.Repository {
		return newGitlabClone(t, path, func(repo *git.Repository) {
			history(t, repo)
			guardTestBranch(t, repo, "main", next)
			guardTestRef(t, repo, plumbing.NewTagReferenceName("v1"), base)
		})
	}

	t.Run("off leaves branch and tag", func(t *testing.T) {
		path := newBareRemote(t)
		seed(path)
		result, err := pushToGithub(context.Background(), clone(path), pushOptions{})
		if err != nil {
			t.Fatalf("pushToGithub: %v", err)
		}
		assertRemote(t, path, map[string]plumbing.Hash{"refs/heads/main": githubOnly, "refs/tags/v1": githubOnly})
		if !reflect.DeepEqual(names(result.SkippedBranches), []string{"main"}) || !reflect.DeepEqual(names(result.SkippedTags), []string{"v1"}) {
			t.Errorf("skipped = %v, %v; want main and v1", result.SkippedBranches, result.SkippedTags)
		}
	})

	t.Run("on overwrites both", func(t *testing.T) {
		path := newBareRemote(t)
		seed(path)
		result, err := pushToGithub(context.Background(), clone(path), pushOptions{overwrite: true})
		if err != nil {
			t.Fatalf("pushToGithub: %v", err)
		}
		assertRemote(t, path, map[string]plumbing.Hash{"refs/heads/main": next, "refs/tags/v1": base})
		if len(result.SkippedBranches)+len(result.SkippedTags) != 0 {
			t.Errorf("skipped = %v, %v; want none", result.SkippedBranches, result.SkippedTags)
		}
	})
}

func TestPushToGithubNewRepositoryAndNoTags(t *testing.T) {
	path := newBareRemote(t)
	var next plumbing.Hash
	repo := newGitlabClone(t, path, func(repo *git.Repository) {
		_, next = history(t, repo)
		guardTestBranch(t, repo, "main", next)
	})

	if _, err := pushToGithub(context.Background(), repo, pushOptions{}); err != nil {
		t.Fatalf("pushToGithub: %v", err)
	}
	assertRemote(t, path, map[string]plumbing.Hash{"refs/heads/main": next})

	// A second run has nothing to push, with and without tags.
	if _, err := pushToGithub(context.Background(), repo, pushOptions{}); err != nil {
		t.Fatalf("second pushToGithub: %v", err)
	}
}

func TestPushToGithubEverythingSkipped(t *testing.T) {
	path := newBareRemote(t)
	var githubOnly plumbing.Hash
	seedRemote(t, path, func(repo *git.Repository) {
		base, _ := history(t, repo)
		githubOnly = guardTestCommit(t, repo, "merged on github", base)
		guardTestBranch(t, repo, "main", githubOnly)
	})
	repo := newGitlabClone(t, path, func(repo *git.Repository) {
		_, next := history(t, repo)
		guardTestBranch(t, repo, "main", next)
	})

	result, err := pushToGithub(context.Background(), repo, pushOptions{trim: true})
	if err != nil {
		t.Fatalf("pushToGithub: %v", err)
	}
	assertRemote(t, path, map[string]plumbing.Hash{"refs/heads/main": githubOnly})
	if len(result.Rejected) != 0 || len(result.Trimmed) != 0 {
		t.Errorf("result = %+v, want nothing rejected or trimmed", result)
	}
}

func TestPushToGithubTrunkRename(t *testing.T) {
	path := newBareRemote(t)
	var base, next plumbing.Hash
	seedRemote(t, path, func(repo *git.Repository) {
		base, next = history(t, repo)
		guardTestBranch(t, repo, "master", base)
	})
	// As on GitHub, the default branch is main already: a default branch cannot be deleted.
	bare, err := git.PlainOpen(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := bare.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, "refs/heads/main")); err != nil {
		t.Fatal(err)
	}
	repo := newGitlabClone(t, path, func(repo *git.Repository) {
		history(t, repo)
		guardTestBranch(t, repo, "master", next)
	})
	if err := renameTrunkRef(repo, "master", "main"); err != nil {
		t.Fatalf("renameTrunkRef: %v", err)
	}

	// main is new on GitHub, master is no longer on GitLab and its tip is known there.
	result, err := pushToGithub(context.Background(), repo, pushOptions{trim: true})
	if err != nil {
		t.Fatalf("pushToGithub: %v", err)
	}
	assertRemote(t, path, map[string]plumbing.Hash{"refs/heads/main": next})
	if !reflect.DeepEqual(result.Trimmed, []string{"master"}) {
		t.Errorf("trimmed = %v, want [master]", result.Trimmed)
	}
}

func TestPushToGithubTags(t *testing.T) {
	var base plumbing.Hash
	// side builds a tag named name on base: annotated or lightweight.
	side := func(repo *git.Repository, name string, annotated bool) plumbing.Hash {
		if annotated {
			return guardTestTag(t, repo, name, base)
		}
		guardTestRef(t, repo, plumbing.NewTagReferenceName(name), base)
		return base
	}

	cases := []struct {
		name                 string
		githubTag, gitlabTag string // "", "light" or "annotated"
		wantSkipped          bool
	}{
		{"new lightweight", "", "light", false},
		{"new annotated", "", "annotated", false},
		{"same lightweight", "light", "light", false},
		{"same annotated", "annotated", "annotated", false},
		{"annotated on GitHub, lightweight on GitLab", "annotated", "light", true},
		{"lightweight on GitHub, annotated on GitLab", "light", "annotated", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := newBareRemote(t)
			var githubTip plumbing.Hash
			seedRemote(t, path, func(repo *git.Repository) {
				base = guardTestCommit(t, repo, "base")
				guardTestBranch(t, repo, "main", base)
				if tc.githubTag != "" {
					githubTip = side(repo, "v1", tc.githubTag == "annotated")
				}
			})
			var gitlabTip plumbing.Hash
			repo := newGitlabClone(t, path, func(repo *git.Repository) {
				guardTestCommit(t, repo, "base")
				guardTestBranch(t, repo, "main", base)
				gitlabTip = side(repo, "v1", tc.gitlabTag == "annotated")
			})

			result, err := pushToGithub(context.Background(), repo, pushOptions{})
			if err != nil {
				t.Fatalf("pushToGithub: %v", err)
			}
			want := gitlabTip
			if tc.wantSkipped {
				want = githubTip
			}
			if got := remoteRefs(t, path)["refs/tags/v1"]; got != want {
				t.Errorf("tag on remote = %s, want %s", got, want)
			}
			if skipped := len(result.SkippedTags) == 1; skipped != tc.wantSkipped {
				t.Errorf("skipped tags = %v, want skipped = %v", result.SkippedTags, tc.wantSkipped)
			}
		})
	}
}
