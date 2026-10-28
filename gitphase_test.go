package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/hashicorp/go-hclog"
	"github.com/xanzy/go-gitlab"
)

func TestChunk(t *testing.T) {
	items := []string{"a", "b", "c", "d", "e"}

	got := chunk(items, 2)
	want := [][]string{{"a", "b"}, {"c", "d"}, {"e"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("chunk(items, 2) = %v, want %v", got, want)
	}

	if got := chunk([]string{}, 2); len(got) != 0 {
		t.Errorf("chunk(empty, 2) = %v, want empty", got)
	}

	got = chunk(items, 0)
	want = [][]string{{"a", "b", "c", "d", "e"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("chunk(items, 0) = %v, want %v", got, want)
	}
}

// newFetchTestProject returns a project whose "gitlab" remote is a local
// on-disk repository with one commit, and that commit's hash. go-git serves
// local remotes with the system git-upload-pack, so the test needs git on PATH.
func newFetchTestProject(t *testing.T) (*project, plumbing.Hash) {
	t.Helper()

	if _, err := exec.LookPath("git-upload-pack"); err != nil {
		t.Skip("git-upload-pack not found on PATH")
	}

	if logger == nil {
		logger = hclog.NewNullLogger()
	}

	dir := t.TempDir()
	source, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("initializing source repository: %v", err)
	}
	// Allow fetching by SHA, as GitLab does
	cfg, err := source.Config()
	if err != nil {
		t.Fatalf("reading source config: %v", err)
	}
	cfg.Raw.Section("uploadpack").SetOption("allowAnySHA1InWant", "true")
	if err = source.SetConfig(cfg); err != nil {
		t.Fatalf("writing source config: %v", err)
	}

	worktree, err := source.Worktree()
	if err != nil {
		t.Fatalf("opening worktree: %v", err)
	}
	if err = os.WriteFile(filepath.Join(dir, "file"), []byte("content"), 0o644); err != nil {
		t.Fatalf("writing file: %v", err)
	}
	if _, err = worktree.Add("file"); err != nil {
		t.Fatalf("adding file: %v", err)
	}
	hash, err := worktree.Commit("initial", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("committing: %v", err)
	}

	repo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatalf("initializing repository: %v", err)
	}
	if _, err = repo.CreateRemote(&config.RemoteConfig{Name: "gitlab", URLs: []string{dir}}); err != nil {
		t.Fatalf("adding remote: %v", err)
	}

	return &project{repo: repo, gitlabPath: []string{"group", "name"}}, hash
}

func TestFetchCommitsBySHAFallsBackToSingleFetches(t *testing.T) {
	p, hash := newFetchTestProject(t)
	missingSHA := strings.Repeat("1", 40)

	if err := p.fetchCommitsBySHA(context.Background(), []string{missingSHA, hash.String()}); err != nil {
		t.Fatalf("fetchCommitsBySHA returned error: %v", err)
	}

	if _, err := object.GetCommit(p.repo.Storer, hash); err != nil {
		t.Errorf("commit %s not present after fetch: %v", hash, err)
	}

	refs, err := p.repo.References()
	if err != nil {
		t.Fatalf("listing references: %v", err)
	}
	_ = refs.ForEach(func(ref *plumbing.Reference) error {
		if strings.HasPrefix(ref.Name().String(), "refs/migration/commits/") {
			t.Errorf("reference %s left after fetch", ref.Name())
		}
		return nil
	})
}

func TestFetchCommitsBySHASingleMissingReturnsError(t *testing.T) {
	p, _ := newFetchTestProject(t)

	if err := p.fetchCommitsBySHA(context.Background(), []string{strings.Repeat("1", 40)}); err == nil {
		t.Error("expected an error when the only commit cannot be fetched")
	}
}

// storeCommit stores a commit with an empty tree and the given parents and returns its hash.
func storeCommit(t *testing.T, repo *git.Repository, msg string, parents ...plumbing.Hash) plumbing.Hash {
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
	commit := &object.Commit{Author: sig, Committer: sig, Message: msg, TreeHash: treeHash, ParentHashes: parents}
	commitObj := repo.Storer.NewEncodedObject()
	if err := commit.Encode(commitObj); err != nil {
		t.Fatalf("encoding commit: %v", err)
	}
	hash, err := repo.Storer.SetEncodedObject(commitObj)
	if err != nil {
		t.Fatalf("storing commit: %v", err)
	}
	return hash
}

func TestPushTemporaryBranchesSkipsSourceWithoutNewCommits(t *testing.T) {
	if logger == nil {
		logger = hclog.NewNullLogger()
	}

	repo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatalf("initializing repository: %v", err)
	}
	base := storeCommit(t, repo, "base")
	head := storeCommit(t, repo, "head", base)

	const reason = "source branch has no commits beyond the target branch"
	work := []*mrWork{
		{mr: &gitlab.MergeRequest{IID: 1}, needsBranch: true, sourceHash: base, targetHash: head, sourceRef: "s1", targetRef: "t1"},
		{mr: &gitlab.MergeRequest{IID: 2}, needsBranch: true, sourceHash: head, targetHash: head, sourceRef: "s2", targetRef: "t2"},
		// diff_refs path: equal hashes are still skipped without an ancestor walk.
		{mr: &gitlab.MergeRequest{IID: 3}, needsBranch: true, fromDiffRefs: true, sourceHash: head, targetHash: head, sourceRef: "s3", targetRef: "t3"},
	}

	// All commits are present and every merge request is skipped, so nothing is fetched or pushed
	// and no remotes are needed.
	p := &project{repo: repo, gitlabPath: []string{"group", "name"}, githubPath: []string{"owner", "repo"}, project: &gitlab.Project{}}
	if err := p.pushTemporaryBranches(context.Background(), work); err != nil {
		t.Fatalf("pushTemporaryBranches: %v", err)
	}

	for _, w := range work {
		if w.skip != reason {
			t.Errorf("merge request %d: skip = %q, want %q", w.mr.IID, w.skip, reason)
		}
	}
	refs, err := repo.References()
	if err != nil {
		t.Fatal(err)
	}
	_ = refs.ForEach(func(ref *plumbing.Reference) error {
		if ref.Name().IsBranch() {
			t.Errorf("unexpected local branch %s", ref.Name())
		}
		return nil
	})
}
