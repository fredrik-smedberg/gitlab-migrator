package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
)

func newGuardTestRepo(t *testing.T) *git.Repository {
	t.Helper()
	repo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatalf("initializing repository: %v", err)
	}
	return repo
}

// guardTestCommit stores a commit with an empty tree and returns its hash.
func guardTestCommit(t *testing.T, repo *git.Repository, message string, parents ...plumbing.Hash) plumbing.Hash {
	t.Helper()

	treeObj := repo.Storer.NewEncodedObject()
	if err := (&object.Tree{}).Encode(treeObj); err != nil {
		t.Fatalf("encoding tree: %v", err)
	}
	treeHash, err := repo.Storer.SetEncodedObject(treeObj)
	if err != nil {
		t.Fatalf("storing tree: %v", err)
	}

	sig := object.Signature{Name: "test", Email: "test@example.com", When: time.Unix(1700000000, 0)}
	commitObj := repo.Storer.NewEncodedObject()
	commit := &object.Commit{Author: sig, Committer: sig, Message: message, TreeHash: treeHash, ParentHashes: parents}
	if err := commit.Encode(commitObj); err != nil {
		t.Fatalf("encoding commit: %v", err)
	}
	hash, err := repo.Storer.SetEncodedObject(commitObj)
	if err != nil {
		t.Fatalf("storing commit: %v", err)
	}
	return hash
}

func guardTestBranch(t *testing.T, repo *git.Repository, name string, hash plumbing.Hash) {
	t.Helper()
	ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName(name), hash)
	if err := repo.Storer.SetReference(ref); err != nil {
		t.Fatalf("setting branch %s: %v", name, err)
	}
}

func TestIsFastForward(t *testing.T) {
	repo := newGuardTestRepo(t)
	base := guardTestCommit(t, repo, "base")
	next := guardTestCommit(t, repo, "next", base)
	side := guardTestCommit(t, repo, "side", base)

	// A commit that only GitHub has: it is not in this repository's objects.
	githubOnly := guardTestCommit(t, newGuardTestRepo(t), "merged on GitHub", next)

	// A tag object is no commit; a branch can point at one.
	tagObject := guardTestTag(t, repo, "v1", next)

	tests := []struct {
		name      string
		githubTip plumbing.Hash
		gitlabTip plumbing.Hash
		want      bool
	}{
		{"equal tips", next, next, true},
		{"GitHub behind GitLab", base, next, true},
		{"GitHub ahead of GitLab", next, base, false},
		{"GitHub tip unknown to GitLab", githubOnly, next, false},
		{"diverged, both known", side, next, false},
		{"GitLab tip is a tag object", base, tagObject, false},
		{"GitHub tip is a tag object", tagObject, next, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := isFastForward(repo, tt.githubTip, tt.gitlabTip)
			if err != nil {
				t.Fatalf("isFastForward: %v", err)
			}
			if got != tt.want {
				t.Errorf("isFastForward = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSplitDiverged(t *testing.T) {
	repo := newGuardTestRepo(t)
	base := guardTestCommit(t, repo, "base")
	next := guardTestCommit(t, repo, "next", base)
	githubOnly := guardTestCommit(t, newGuardTestRepo(t), "merged on GitHub", next)

	guardTestBranch(t, repo, "main", next)       // GitHub has a merge GitLab lacks
	guardTestBranch(t, repo, "same", next)       // equal on both sides
	guardTestBranch(t, repo, "moved", next)      // GitLab moved forward
	guardTestBranch(t, repo, "new-branch", next) // not on GitHub yet

	refSpecs := []config.RefSpec{
		"refs/heads/main:refs/heads/main",
		"refs/heads/same:refs/heads/same",
		"refs/heads/moved:refs/heads/moved",
		"refs/heads/new-branch:refs/heads/new-branch",
	}
	githubTips := map[string]plumbing.Hash{
		"refs/heads/main":        githubOnly,
		"refs/heads/same":        next,
		"refs/heads/moved":       base,
		"refs/heads/github-only": githubOnly, // not on GitLab: never part of the push
	}

	push, diverged, err := splitDiverged(repo, refSpecs, githubTips)
	if err != nil {
		t.Fatalf("splitDiverged: %v", err)
	}

	wantPush := []config.RefSpec{
		"refs/heads/same:refs/heads/same",
		"refs/heads/moved:refs/heads/moved",
		"refs/heads/new-branch:refs/heads/new-branch",
	}
	if !reflect.DeepEqual(push, wantPush) {
		t.Errorf("push = %v, want %v", push, wantPush)
	}

	wantDiverged := []divergedRef{{Name: "main", GithubTip: githubOnly, GitlabTip: next}}
	if !reflect.DeepEqual(diverged, wantDiverged) {
		t.Errorf("diverged = %v, want %v", diverged, wantDiverged)
	}
}

func TestSplitDivergedWithoutGithubTips(t *testing.T) {
	repo := newGuardTestRepo(t)
	guardTestBranch(t, repo, "main", guardTestCommit(t, repo, "base"))
	refSpecs := []config.RefSpec{"refs/heads/main:refs/heads/main"}

	// A repository that was just created on GitHub has no branches.
	push, diverged, err := splitDiverged(repo, refSpecs, nil)
	if err != nil {
		t.Fatalf("splitDiverged: %v", err)
	}
	if !reflect.DeepEqual(push, refSpecs) || len(diverged) != 0 {
		t.Errorf("push = %v, diverged = %v, want everything pushed", push, diverged)
	}
}

func TestSplitMovedTags(t *testing.T) {
	repo := newGuardTestRepo(t)
	base := guardTestCommit(t, repo, "base")
	next := guardTestCommit(t, repo, "next", base)

	tags := []*plumbing.Reference{
		plumbing.NewHashReference(plumbing.NewTagReferenceName("1.0.0"), base), // same on GitHub
		plumbing.NewHashReference(plumbing.NewTagReferenceName("1.1.0"), next), // not on GitHub yet
		plumbing.NewHashReference(plumbing.NewTagReferenceName("1.2.0"), base), // points elsewhere on GitHub
	}
	githubTips := map[string]plumbing.Hash{
		"refs/tags/1.0.0":       base,
		"refs/tags/1.2.0":       next,
		"refs/tags/github-only": next, // not on GitLab: never part of the push
		"refs/heads/1.1.0":      base, // a branch of the same name is not a tag
	}

	push, moved := splitMovedTags(tags, githubTips)

	wantPush := []config.RefSpec{
		"refs/tags/1.0.0:refs/tags/1.0.0",
		"refs/tags/1.1.0:refs/tags/1.1.0",
	}
	if !reflect.DeepEqual(push, wantPush) {
		t.Errorf("push = %v, want %v", push, wantPush)
	}

	wantMoved := []divergedRef{{Name: "1.2.0", GithubTip: next, GitlabTip: base}}
	if !reflect.DeepEqual(moved, wantMoved) {
		t.Errorf("moved = %v, want %v", moved, wantMoved)
	}
}
