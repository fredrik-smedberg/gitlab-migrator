package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/google/go-github/v74/github"
	"github.com/xanzy/go-gitlab"
)

var (
	newlineRegexp = regexp.MustCompile("\r|\n")
)

// listMergeRequestCommits retrieves all commits of a merge request, following pagination.
func listMergeRequestCommits(ctx context.Context, projectID, iid int) ([]*gitlab.Commit, error) {
	var commits []*gitlab.Commit

	opts := &gitlab.GetMergeRequestCommitsOptions{PerPage: 100}
	for {
		result, resp, err := gl.MergeRequests.GetMergeRequestCommits(projectID, iid, opts, gitlab.WithContext(ctx))
		if err != nil {
			return nil, err
		}

		commits = append(commits, result...)

		if resp.NextPage == 0 {
			break
		}

		opts.Page = resp.NextPage
	}

	return commits, nil
}

// sortCommitsByDate sorts commits in place by committed date, oldest first. Commits without a date sort first.
func sortCommitsByDate(commits []*gitlab.Commit) {
	date := func(c *gitlab.Commit) time.Time {
		if c == nil || c.CommittedDate == nil {
			return time.Time{}
		}
		return *c.CommittedDate
	}

	sort.SliceStable(commits, func(i, j int) bool {
		return date(commits[i]).Before(date(commits[j]))
	})
}

func newProject(ctx context.Context, slugs []string) (*project, error) {
	var err error
	p := &project{}

	p.gitlabPath, p.githubPath, err = parseProjectSlugs(slugs)
	if err != nil {
		return nil, fmt.Errorf("parsing project slugs: %v", err)
	}

	logger.Info("searching for GitLab project", "name", p.gitlabPath[1], "group", p.gitlabPath[0])
	p.project, _, err = gl.Projects.GetProject(slugs[0], nil, gitlab.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("retrieving project: %v", err)
	}

	if p.project == nil {
		return nil, fmt.Errorf("no matching GitLab project found: %s", slugs[0])
	}

	p.defaultBranch = "main"
	if renameTrunkBranch != "" {
		p.defaultBranch = renameTrunkBranch
	} else if !renameMasterToMain && p.project.DefaultBranch != "" {
		p.defaultBranch = p.project.DefaultBranch
	}

	return p, nil
}

type project struct {
	project       *gitlab.Project
	repo          *git.Repository
	defaultBranch string
	gitlabPath    []string
	githubPath    []string
	bar           *progressBar // nil when progress is disabled
}

func (p *project) createRepo(ctx context.Context, homepage string, repoDeleted bool) error {
	if repoDeleted {
		logger.Warn("recreating GitHub repository", "owner", p.githubPath[0], "repo", p.githubPath[1])
	} else {
		logger.Debug("repository not found on GitHub, proceeding to create", "owner", p.githubPath[0], "repo", p.githubPath[1])
	}
	newRepo := github.Repository{
		Name:          pointer(p.githubPath[1]),
		Description:   &p.project.Description,
		Homepage:      &homepage,
		DefaultBranch: &p.defaultBranch,
		Private:       pointer(true),
		HasIssues:     pointer(true),
		HasWiki:       pointer(true),
	}
	if _, _, err := gh.Repositories.Create(ctx, p.githubPath[0], &newRepo); err != nil {
		return fmt.Errorf("creating github repo: %v", err)
	}

	return nil
}

func (p *project) migrate(ctx context.Context) (err error) {
	p.bar = progress.newBar(p.gitlabPath[1])
	defer func() { p.bar.finish(err != nil) }()

	cloneUrl, err := url.Parse(p.project.HTTPURLToRepo)
	if err != nil {
		return fmt.Errorf("parsing clone URL: %v", err)
	}

	logger.Info("mirroring repository from GitLab to GitHub", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "github_org", p.githubPath[0], "github_repo", p.githubPath[1])

	logger.Debug("checking for existing repository on GitHub", "owner", p.githubPath[0], "repo", p.githubPath[1])
	_, _, err = gh.Repositories.Get(ctx, p.githubPath[0], p.githubPath[1])

	var githubError *github.ErrorResponse
	if err != nil && (!errors.As(err, &githubError) || githubError == nil || githubError.Response == nil || githubError.Response.StatusCode != http.StatusNotFound) {
		return fmt.Errorf("retrieving github repo: %v", err)
	}

	homepage := fmt.Sprintf("%s://%s/%s/%s", gitlabScheme, gitlabDomain, p.gitlabPath[0], p.gitlabPath[1])

	if err != nil {
		// Repository not found
		if err = p.createRepo(ctx, homepage, false); err != nil {
			return err
		}
	} else if deleteExistingRepos {
		logger.Warn("existing repository was found on GitHub, proceeding to delete", "owner", p.githubPath[0], "repo", p.githubPath[1])
		if _, err = gh.Repositories.Delete(ctx, p.githubPath[0], p.githubPath[1]); err != nil {
			return fmt.Errorf("deleting existing github repo: %v", err)
		}

		if err = p.createRepo(ctx, homepage, true); err != nil {
			return err
		}
	}

	logger.Debug("updating repository settings", "owner", p.githubPath[0], "repo", p.githubPath[1])
	description := newlineRegexp.ReplaceAllString(p.project.Description, " ")
	updateRepo := github.Repository{
		Name:              pointer(p.githubPath[1]),
		Description:       &description,
		Homepage:          &homepage,
		AllowAutoMerge:    pointer(true),
		AllowMergeCommit:  pointer(true),
		AllowRebaseMerge:  pointer(true),
		AllowSquashMerge:  pointer(true),
		AllowUpdateBranch: pointer(true),
	}
	if _, _, err = gh.Repositories.Edit(ctx, p.githubPath[0], p.githubPath[1], &updateRepo); err != nil {
		return fmt.Errorf("updating github repo: %v", err)
	}

	cloneUrl.User = url.UserPassword("oauth2", gitlabToken)
	cloneUrlWithCredentials := cloneUrl.String()

	// Bare in-memory repository. Only branches and tags are fetched, so GitLab's
	// refs/merge-requests/* and refs/environments/* are not downloaded.
	logger.Debug("cloning repository", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", p.project.HTTPURLToRepo)
	p.repo, err = git.Init(memory.NewStorage(), nil)
	if err != nil {
		return fmt.Errorf("initializing repository: %v", err)
	}
	if _, err = p.repo.CreateRemote(&config.RemoteConfig{
		Name:  "gitlab",
		URLs:  []string{cloneUrlWithCredentials},
		Fetch: []config.RefSpec{"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"},
	}); err != nil {
		return fmt.Errorf("adding gitlab remote: %v", err)
	}
	p.bar.setPhase("cloning", 0)
	if err = p.repo.FetchContext(ctx, &git.FetchOptions{
		RemoteName: "gitlab",
		Tags:       git.NoTags,
		CABundle:   gitlabCABundle,
	}); err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return fmt.Errorf("cloning gitlab repo: %v", err)
	}

	if p.defaultBranch != p.project.DefaultBranch {
		if gitlabTrunk, err := p.repo.Reference(plumbing.NewBranchReferenceName(p.project.DefaultBranch), false); err == nil {
			logger.Info("renaming trunk branch prior to push", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "gitlab_trunk", p.project.DefaultBranch, "github_trunk", p.defaultBranch, "sha", gitlabTrunk.Hash())

			logger.Debug("creating new trunk branch", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "github_trunk", p.defaultBranch, "sha", gitlabTrunk.Hash())
			githubTrunk := plumbing.NewHashReference(plumbing.NewBranchReferenceName(p.defaultBranch), gitlabTrunk.Hash())
			if err = p.repo.Storer.SetReference(githubTrunk); err != nil {
				return fmt.Errorf("creating trunk branch: %v", err)
			}

			logger.Debug("deleting old trunk branch", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "gitlab_trunk", p.project.DefaultBranch, "sha", gitlabTrunk.Hash())
			if err = p.repo.Storer.RemoveReference(gitlabTrunk.Name()); err != nil {
				return fmt.Errorf("deleting old trunk branch: %v", err)
			}
		}
	}

	githubUrl := fmt.Sprintf("https://%s/%s/%s", githubDomain, p.githubPath[0], p.githubPath[1])
	githubUrlWithCredentials := fmt.Sprintf("https://%s:%s@%s/%s/%s", githubUser, githubToken, githubDomain, p.githubPath[0], p.githubPath[1])

	logger.Debug("adding remote for GitHub repository", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl)
	if _, err = p.repo.CreateRemote(&config.RemoteConfig{
		Name:   "github",
		URLs:   []string{githubUrlWithCredentials},
		Mirror: true,
	}); err != nil {
		return fmt.Errorf("adding github remote: %v", err)
	}

	logger.Debug("determining branches to push", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl)
	branches, err := p.repo.Branches()
	if err != nil {
		return fmt.Errorf("retrieving branches: %v", err)
	}

	gitlabBranches := make([]string, 0)
	refSpecs := make([]config.RefSpec, 0)
	if err = branches.ForEach(func(ref *plumbing.Reference) error {
		gitlabBranches = append(gitlabBranches, ref.Name().Short())
		refSpecs = append(refSpecs, config.RefSpec(fmt.Sprintf("%[1]s:%[1]s", ref.Name())))
		return nil
	}); err != nil {
		return fmt.Errorf("parsing branches: %v", err)
	}

	logger.Debug("force-pushing branches to GitHub repository", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl, "count", len(refSpecs))
	p.bar.setPhase("pushing branches", 0)
	if err = p.repo.PushContext(ctx, &git.PushOptions{
		RemoteName: "github",
		Force:      true,
		RefSpecs:   refSpecs,
		//Prune:      true, // causes error, attempts to delete main branch
	}); err != nil {
		if errors.Is(err, git.NoErrAlreadyUpToDate) {
			logger.Debug("repository already up-to-date on GitHub", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl)
		} else {
			return fmt.Errorf("pushing to github repo: %v", err)
		}
	}

	if trimGithubBranches {
		logger.Debug("determining old branches to trim on GitHub repository", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl)
		refSpecsToDelete := make([]config.RefSpec, 0)
		githubBranches, err := getGithubBranches(ctx, p.githubPath[0], p.githubPath[1])
		if err != nil {
			return fmt.Errorf("listing branches from GitHub: %v", err)
		}
		for _, githubBranch := range githubBranches {
			found := false
			for _, gitlabBranch := range gitlabBranches {
				if githubBranch.Name != nil && *githubBranch.Name == gitlabBranch {
					found = true
					break
				}
			}
			if !found {
				refSpecsToDelete = append(refSpecsToDelete, config.RefSpec(fmt.Sprintf(":refs/heads/%s", *githubBranch.Name)))
			}
		}

		logger.Debug("trimming old branches on GitHub repository", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl, "count", len(refSpecs))
		if err = p.repo.PushContext(ctx, &git.PushOptions{
			RemoteName: "github",
			Force:      true,
			RefSpecs:   refSpecsToDelete,
			//Prune:      true, // causes error, attempts to delete main branch
		}); err != nil {
			if errors.Is(err, git.NoErrAlreadyUpToDate) {
				logger.Debug("repository already up-to-date on GitHub", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl)
			} else {
				return fmt.Errorf("pushing to github repo: %v", err)
			}
		}
	}

	logger.Debug("force-pushing tags to GitHub repository", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl)
	p.bar.setPhase("pushing tags", 0)
	if err = p.repo.PushContext(ctx, &git.PushOptions{
		RemoteName: "github",
		Force:      true,
		RefSpecs:   []config.RefSpec{"refs/tags/*:refs/tags/*"},
	}); err != nil {
		if errors.Is(err, git.NoErrAlreadyUpToDate) {
			logger.Debug("repository already up-to-date on GitHub", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "url", githubUrl)
		} else {
			return fmt.Errorf("pushing to github repo: %v", err)
		}
	}

	logger.Debug("setting default repository branch", "owner", p.githubPath[0], "repo", p.githubPath[1], "branch_name", p.defaultBranch)
	updateRepo = github.Repository{
		DefaultBranch: &p.defaultBranch,
	}
	if _, _, err = gh.Repositories.Edit(ctx, p.githubPath[0], p.githubPath[1], &updateRepo); err != nil {
		return fmt.Errorf("setting default branch: %v", err)
	}

	if enablePullRequests {
		p.migrateMergeRequests(ctx)
	}

	return nil
}

// mrWork carries one merge request through the read, git, write and cleanup
// phases of migrateMergeRequests.
type mrWork struct {
	mr          *gitlab.MergeRequest
	pr          *github.PullRequest // nil if the pull request does not exist yet
	sourceRef   string              // branch name used as the pull request head
	targetRef   string              // branch name used as the pull request base
	sourceHash  plumbing.Hash       // set when temporary branches are needed
	targetHash  plumbing.Hash
	needsBranch bool
	// fromDiffRefs: sourceHash and targetHash are diff_refs head_sha and base_sha.
	// base_sha is the merge base, so the source is an ancestor of the target only when they are equal.
	fromDiffRefs bool
	notes        []*gitlab.Note
	approvers    []string
	author       string
	skip         string // non-empty: skip the merge request with an Info log
}

// migrateMergeRequests migrates the merge requests of the project in phases:
// read everything needed from GitLab, fetch commits and push temporary branches
// in batches, write pull requests and comments to GitHub, and finally delete
// the temporary branches.
func (p *project) migrateMergeRequests(ctx context.Context) {
	var mergeRequests []*gitlab.MergeRequest

	opts := &gitlab.ListProjectMergeRequestsOptions{
		ListOptions: gitlab.ListOptions{PerPage: 100},
		OrderBy:     pointer("created_at"),
		Sort:        pointer("asc"),
	}

	if mergeRequestsAge > 0 {
		opts.CreatedAfter = pointer(time.Now().AddDate(0, 0, -mergeRequestsAge))
	}

	logger.Debug("retrieving GitLab merge requests", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID)
	p.bar.setPhase("listing merge requests", 0)
	for {
		result, resp, err := gl.MergeRequests.ListProjectMergeRequests(p.project.ID, opts, gitlab.WithContext(ctx))
		if err != nil {
			sendErr(fmt.Errorf("retrieving gitlab merge requests: %v", err))
			return
		}

		mergeRequests = append(mergeRequests, result...)

		if resp.NextPage == 0 {
			break
		}

		opts.Page = resp.NextPage
	}

	p.bar.setPhase("indexing pull requests", 0)
	index, err := buildGithubIndex(ctx, p.githubPath[0], p.githubPath[1])
	if err != nil {
		sendErr(fmt.Errorf("indexing GitHub repository: %v", err))
		return
	}

	var successCount, failureCount int
	totalCount := len(mergeRequests)
	logger.Info("migrating merge requests from GitLab to GitHub", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "count", totalCount)
	defer func() {
		skippedCount := totalCount - successCount - failureCount
		logger.Info("migrated merge requests from GitLab to GitHub", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "successful", successCount, "failed", failureCount, "skipped", skippedCount)
	}()

	// Select the merge requests to prepare, in GitLab order.
	selected := make([]*gitlab.MergeRequest, 0, len(mergeRequests))
	for _, mergeRequest := range mergeRequests {
		if mergeRequest == nil {
			continue
		}

		if !migrateClosedMergeRequests && strings.EqualFold(mergeRequest.State, "closed") {
			logger.Debug("skipping closed merge request as -migrate-closed-merge-requests is disabled", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID)
			continue
		}

		selected = append(selected, mergeRequest)
	}

	p.bar.setPhase("preparing", len(selected))
	prepared, errs := p.prepareMergeRequests(ctx, selected, index)

	// The worker pool in prepareMergeRequests has fully finished here. It must not overlap
	// pushTemporaryBranches: the git phase writes to the in-memory go-git storage, which has no locks.
	if err := ctx.Err(); err != nil {
		sendErr(fmt.Errorf("preparing merge requests: %v", err))
		return
	}

	// Report errors and drop failed merge requests in GitLab order, so log output and counts are deterministic.
	// Prepare errors for merge requests without a pull request abort the project: creating the
	// others would shift GitHub pull request numbers away from GitLab merge request order.
	work := make([]*mrWork, 0, len(selected))
	var unnumbered []int
	for i, w := range prepared {
		if errs[i] != nil {
			sendErr(errs[i])
			failureCount++
			if index.pullRequests[selected[i].IID] == nil {
				unnumbered = append(unnumbered, selected[i].IID)
			}
			continue
		}
		if w == nil {
			continue
		}
		work = append(work, w)
	}

	if len(unnumbered) > 0 {
		sendErr(fmt.Errorf("aborting merge request migration for %s/%s to keep pull request numbers in merge request order: merge requests %v without a pull request failed to prepare; rerun after fixing the cause", p.gitlabPath[0], p.gitlabPath[1], unnumbered))
		return
	}

	// Registered before pushing so that partially pushed branches are also deleted.
	// Cleanup must run after Ctrl-C too; deleteTemporaryBranches sets its own deadlines.
	defer func() {
		p.bar.setPhase("cleaning up", 0)
		if err := p.deleteTemporaryBranches(ctx, ctx.Err() != nil); err != nil {
			sendErr(fmt.Errorf("deleting temporary branches: %v", err))
		}
	}()

	if err := p.pushTemporaryBranches(ctx, work); err != nil {
		sendErr(err)
		for _, w := range work {
			if w.skip == "" {
				failureCount++
			}
		}
		return
	}

	p.bar.setPhase("writing pull requests", len(work))
	for _, w := range work {
		if err := ctx.Err(); err != nil {
			sendErr(fmt.Errorf("migrating merge requests: %v", err))
			return
		}

		ok, err := p.writeMergeRequest(ctx, w, index)
		p.bar.advance(1)
		if err != nil {
			sendErr(err)
			failureCount++
		} else if ok {
			successCount++
		}
	}
}

// fetchCommitsBySHA ensures the given commit SHAs are present in the local
// repository, fetching any that are missing directly from the GitLab remote by
// SHA. This recovers commits that are no longer reachable from any advertised
// branch or tag (for example, the commits of a closed or merged merge request
// whose source branch has since been deleted). Commits that are already present
// are skipped. The GitLab server must permit fetching by SHA, which it does via
// the AllowReachableSHA1InWant/AllowTipSHA1InWant capabilities.
//
// The missing commits are fetched in one batch. If the batch fails, for example
// because one commit no longer exists on GitLab, they are fetched one by one
// and per-commit errors are ignored, so one bad SHA does not prevent fetching
// the others. Callers must check that the commits they need are present.
func (p *project) fetchCommitsBySHA(ctx context.Context, shas []string) error {
	missing := make([]string, 0, len(shas))
	seen := make(map[string]bool)
	for _, sha := range shas {
		if sha == "" || seen[sha] {
			continue
		}
		seen[sha] = true

		// Skip commits that are already present locally to avoid unnecessary fetches
		if _, err := object.GetCommit(p.repo.Storer, plumbing.NewHash(sha)); err == nil {
			continue
		}
		missing = append(missing, sha)
	}

	if len(missing) == 0 {
		return nil
	}

	err := p.fetchMissingCommits(ctx, missing)
	if err == nil || len(missing) == 1 || ctx.Err() != nil {
		return err
	}

	logger.Debug("fetching commits by SHA failed, fetching one by one", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "count", len(missing), "error", err)
	failed := 0
	for _, sha := range missing {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := p.fetchMissingCommits(ctx, []string{sha}); err != nil {
			failed++
			logger.Debug("fetching commit by SHA failed", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "sha", sha, "error", err)
		}
	}
	if failed > 0 {
		logger.Info(fmt.Sprintf("%d commits could not be fetched from GitLab", failed), "name", p.gitlabPath[1], "group", p.gitlabPath[0])
	}

	return nil
}

// fetchMissingCommits fetches the given commit SHAs from the GitLab remote in one fetch.
func (p *project) fetchMissingCommits(ctx context.Context, shas []string) error {
	refSpecs := make([]config.RefSpec, 0, len(shas))
	refs := make([]plumbing.ReferenceName, 0, len(shas))
	for _, sha := range shas {
		// Map the commit into a local ref so that go-git accepts the fetch
		ref := plumbing.ReferenceName("refs/migration/commits/" + sha)
		refSpecs = append(refSpecs, config.RefSpec(fmt.Sprintf("%s:%s", sha, ref)))
		refs = append(refs, ref)
	}

	// Drop the refs again; the objects stay in storage. go-git sends every local
	// ref as a "have" on each fetch, so leaving them would slow down later fetches.
	defer func() {
		for _, ref := range refs {
			if err := p.repo.Storer.RemoveReference(ref); err != nil {
				sendErr(fmt.Errorf("removing local commit ref %s: %v", ref, err))
			}
		}
	}()

	if err := p.repo.FetchContext(ctx, &git.FetchOptions{
		RemoteName: "gitlab",
		RefSpecs:   refSpecs,
		Force:      true,
		Tags:       git.NoTags,
		CABundle:   gitlabCABundle,
	}); err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return err
	}

	return nil
}

// finalCommentMarker uniquely identifies the comment describing what
// ultimately happened to the merge request on GitLab, allowing the comment to
// be updated idempotently rather than duplicated on subsequent migrations.
const finalCommentMarker = "This pull request reflects the final state of the merge request on GitLab"

// mergeRequestFinalComment builds a note-style comment describing how the
// merge request was finally resolved on GitLab (merged or closed without
// merging), including who performed the action and when. It returns an empty
// string for merge requests that are still open.
func (p *project) mergeRequestFinalComment(ctx context.Context, mergeRequest *gitlab.MergeRequest) (string, error) {
	resolveActor := func(user *gitlab.BasicUser) (string, error) {
		if user == nil {
			return "_Unknown_", nil
		}

		name := user.Name
		gitlabUser, err := getGitlabUser(ctx, user.Username)
		if err != nil {
			return "", fmt.Errorf("retrieving gitlab user: %v", err)
		}
		if gitlabUser.WebsiteURL != "" {
			name = "@" + strings.TrimPrefix(strings.ToLower(gitlabUser.WebsiteURL), "https://github.com/")
		}
		return name, nil
	}

	var summary string
	switch {
	case strings.EqualFold(mergeRequest.State, "merged"):
		actor, err := resolveActor(mergeRequest.MergedBy)
		if err != nil {
			return "", err
		}
		when := "an unknown date"
		if mergeRequest.MergedAt != nil {
			when = mergeRequest.MergedAt.UTC().Format(dateTimeFormat)
		}
		summary = fmt.Sprintf("This merge request was **merged** by %s on %s.", actor, when)
	case strings.EqualFold(mergeRequest.State, "closed"):
		actor, err := resolveActor(mergeRequest.ClosedBy)
		if err != nil {
			return "", err
		}
		when := "an unknown date"
		if mergeRequest.ClosedAt != nil {
			when = mergeRequest.ClosedAt.UTC().Format(dateTimeFormat)
		}
		summary = fmt.Sprintf("This merge request was **closed without merging** by %s on %s.", actor, when)
	default:
		return "", nil
	}

	return fmt.Sprintf(`> [!NOTE]
> %s
>
> %s`, finalCommentMarker, summary), nil
}

// temporaryBranchHashes determines the commits to use for the temporary source
// and target branches of a merged/closed merge request. It prefers the merge
// request's diff_refs (head and base SHA). Older merge requests may lack them,
// in which case the source is the last commit of the merge request and the
// target is the first parent of its first commit, read from the GitLab API.
// No git work is done here; the commits are fetched in pushTemporaryBranches.
// A non-empty skip is the reason the merge request should be skipped.
// fromDiffRefs reports whether the hashes came from diff_refs.
func (p *project) temporaryBranchHashes(ctx context.Context, mergeRequest *gitlab.MergeRequest) (source, target plumbing.Hash, fromDiffRefs bool, skip string, err error) {
	logger.Trace("retrieving diff refs for merge request", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID)
	mr, _, err := gl.MergeRequests.GetMergeRequest(p.project.ID, mergeRequest.IID, nil, gitlab.WithContext(ctx))
	if err != nil {
		return source, target, false, "", fmt.Errorf("retrieving merge request: %v", err)
	}

	if mr.DiffRefs.BaseSha != "" && mr.DiffRefs.HeadSha != "" {
		return plumbing.NewHash(mr.DiffRefs.HeadSha), plumbing.NewHash(mr.DiffRefs.BaseSha), true, "", nil
	}

	logger.Trace("merge request has no diff refs, retrieving commits", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID)
	commits, err := listMergeRequestCommits(ctx, p.project.ID, mergeRequest.IID)
	if err != nil {
		return source, target, false, "", fmt.Errorf("retrieving merge request commits: %v", err)
	}
	if len(commits) == 0 {
		return source, target, false, "merge request has no commits", nil
	}

	for _, commit := range commits {
		if commit == nil {
			return source, target, false, "", fmt.Errorf("merge request %d has a nil commit", mergeRequest.IID)
		}
	}
	// API is buggy, ordering is not respected, so we'll reorder by commit datestamp
	sortCommitsByDate(commits)
	startCommit, _, err := gl.Commits.GetCommit(p.project.ID, commits[0].ID, nil, gitlab.WithContext(ctx))
	if err != nil {
		return source, target, false, "", fmt.Errorf("retrieving start commit %s: %v", commits[0].ShortID, err)
	}
	if len(startCommit.ParentIDs) == 0 {
		// Orphaned commit, we cannot open a pull request as GitHub rejects it
		if !skipInvalidMergeRequests {
			return source, target, false, "", fmt.Errorf("start commit %s for merge request %d has no parents", commits[0].ShortID, mergeRequest.IID)
		}
		return source, target, false, "start commit has no parents", nil
	}

	// Sometimes we will be starting from a merge commit, so branch out from its first parent
	return plumbing.NewHash(commits[len(commits)-1].ID), plumbing.NewHash(startCommit.ParentIDs[0]), false, "", nil
}

// prepareMergeRequests runs prepareMergeRequest for each merge request in a pool of
// mergeRequestConcurrency workers. Result i belongs to mergeRequests[i]; each worker writes only
// its own index of work and errs. A panic while preparing one merge request becomes that merge
// request's error. Merge requests not started before ctx is canceled have neither result nor error.
func (p *project) prepareMergeRequests(ctx context.Context, mergeRequests []*gitlab.MergeRequest, index *githubIndex) ([]*mrWork, []error) {
	work := make([]*mrWork, len(mergeRequests))
	errs := make([]error, len(mergeRequests))

	concurrency := max(mergeRequestConcurrency, 1)
	concurrency = min(concurrency, len(mergeRequests))

	queue := make(chan int)
	var wg sync.WaitGroup
	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range queue {
				if ctx.Err() != nil {
					continue // drain the queue without starting new merge requests
				}
				work[i], errs[i] = p.safePrepareMergeRequest(ctx, mergeRequests[i], index)
				p.bar.advance(1)
			}
		}()
	}

	for i := range mergeRequests {
		if ctx.Err() != nil {
			break
		}
		queue <- i
	}
	close(queue)
	wg.Wait()

	return work, errs
}

// safePrepareMergeRequest calls prepareMergeRequest and converts a panic into an error.
func (p *project) safePrepareMergeRequest(ctx context.Context, mergeRequest *gitlab.MergeRequest, index *githubIndex) (w *mrWork, err error) {
	defer func() {
		if r := recover(); r != nil {
			w, err = nil, fmt.Errorf("merge request %d: panic: %v", mergeRequest.IID, r)
		}
	}()
	return p.prepareMergeRequest(ctx, mergeRequest, index)
}

// prepareMergeRequest reads everything needed to migrate a merge request from
// GitLab and the GitHub index. It pushes nothing and writes nothing to GitHub.
func (p *project) prepareMergeRequest(ctx context.Context, mergeRequest *gitlab.MergeRequest, index *githubIndex) (*mrWork, error) {
	w := &mrWork{
		mr:        mergeRequest,
		pr:        index.pullRequests[mergeRequest.IID],
		sourceRef: mergeRequest.SourceBranch,
		targetRef: mergeRequest.TargetBranch,
	}
	if w.pr != nil {
		logger.Debug("found existing pull request", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", w.pr.GetNumber())
	}

	if strings.EqualFold(mergeRequest.State, "opened") {
		if _, err := p.repo.Reference(plumbing.NewBranchReferenceName(mergeRequest.SourceBranch), false); err != nil {
			if errors.Is(err, plumbing.ErrReferenceNotFound) && skipInvalidMergeRequests {
				w.skip = fmt.Sprintf("source branch %s does not exist", mergeRequest.SourceBranch)
				return w, nil
			}
			return nil, fmt.Errorf("checking source branch for merge request: %v", err)
		}
	}

	// Temporary branches are needed when migrating a merged/closed merge request that doesn't yet have a counterpart PR in GitHub (can't create one without a branch)
	if w.pr == nil && !strings.EqualFold(mergeRequest.State, "opened") {
		logger.Trace("determining temporary branches for closed/merged merge request", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "source_branch", mergeRequest.SourceBranch)

		w.needsBranch = true
		w.sourceRef = fmt.Sprintf("migration-source-%d/%s", mergeRequest.IID, mergeRequest.SourceBranch)
		w.targetRef = fmt.Sprintf("migration-target-%d/%s", mergeRequest.IID, mergeRequest.TargetBranch)

		var err error
		w.sourceHash, w.targetHash, w.fromDiffRefs, w.skip, err = p.temporaryBranchHashes(ctx, mergeRequest)
		if err != nil {
			return nil, err
		}
		if w.skip != "" {
			return w, nil
		}
	}

	if p.defaultBranch != p.project.DefaultBranch && w.targetRef == p.project.DefaultBranch {
		logger.Trace("changing target trunk branch", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "old_trunk", p.project.DefaultBranch, "new_trunk", p.defaultBranch)
		w.targetRef = p.defaultBranch
	}

	w.author = mergeRequest.Author.Name

	author, err := getGitlabUser(ctx, mergeRequest.Author.Username)
	if err != nil {
		return nil, fmt.Errorf("retrieving gitlab user: %v", err)
	}
	if author.WebsiteURL != "" {
		w.author = "@" + strings.TrimPrefix(strings.ToLower(author.WebsiteURL), "https://github.com/")
	}

	logger.Debug("determining merge request approvers", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID)
	w.approvers = make([]string, 0)
	awards, _, err := gl.AwardEmoji.ListMergeRequestAwardEmoji(p.project.ID, mergeRequest.IID, &gitlab.ListAwardEmojiOptions{PerPage: 100}, gitlab.WithContext(ctx))
	if err != nil {
		sendErr(fmt.Errorf("listing merge request awards: %v", err))
	} else {
		for _, award := range awards {
			if award.Name == "thumbsup" {
				approver := award.User.Name

				approverUser, err := getGitlabUser(ctx, award.User.Username)
				if err != nil {
					sendErr(fmt.Errorf("retrieving gitlab user: %v", err))
					continue
				}
				if approverUser.WebsiteURL != "" {
					approver = "@" + strings.TrimPrefix(strings.ToLower(approverUser.WebsiteURL), "https://github.com/")
				}

				w.approvers = append(w.approvers, approver)
			}
		}
	}

	opts := &gitlab.ListMergeRequestNotesOptions{
		ListOptions: gitlab.ListOptions{PerPage: 100},
		OrderBy:     pointer("created_at"),
		Sort:        pointer("asc"),
	}

	logger.Debug("retrieving GitLab merge request comments", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID)
	for {
		result, resp, err := gl.Notes.ListMergeRequestNotes(p.project.ID, mergeRequest.IID, opts, gitlab.WithContext(ctx))
		if err != nil {
			return nil, fmt.Errorf("listing merge request notes: %v", err)
		}

		w.notes = append(w.notes, result...)

		if resp.NextPage == 0 {
			break
		}

		opts.Page = resp.NextPage
	}

	return w, nil
}

// writeMergeRequest creates or updates the pull request and its comments on
// GitHub from a prepared merge request. Temporary branches must already be pushed.
func (p *project) writeMergeRequest(ctx context.Context, w *mrWork, index *githubIndex) (bool, error) {
	mergeRequest := w.mr

	if w.skip != "" {
		logger.Info("skipping invalid merge request", "name", p.gitlabPath[1], "group", p.gitlabPath[0], "project_id", p.project.ID, "merge_request_id", mergeRequest.IID, "reason", w.skip)
		return false, nil
	}

	var err error
	pullRequest := w.pr
	githubAuthorName := w.author
	approvers := w.approvers

	originalState := ""
	if !strings.EqualFold(mergeRequest.State, "opened") {
		originalState = fmt.Sprintf("> This merge request was originally **%s** on GitLab", mergeRequest.State)
	}

	description := mergeRequest.Description
	if strings.TrimSpace(description) == "" {
		description = "_No description_"
	}

	slices.Sort(approvers)
	approval := strings.Join(approvers, ", ")
	if approval == "" {
		approval = "_No approvers_"
	}

	closeDate := ""
	if mergeRequest.State == "closed" && mergeRequest.ClosedAt != nil {
		closeDate = fmt.Sprintf("\n> | **Date Originally Closed** | %s |", mergeRequest.ClosedAt.UTC().Format(dateTimeFormat))
	} else if mergeRequest.State == "merged" && mergeRequest.MergedAt != nil {
		closeDate = fmt.Sprintf("\n> | **Date Originally Merged** | %s |", mergeRequest.MergedAt.UTC().Format(dateTimeFormat))
	}

	mergeRequestTitle := mergeRequest.Title
	if len(mergeRequestTitle) > 40 {
		mergeRequestTitle = mergeRequestTitle[:40] + "..."
	}

	// Prefix the pull request title to reflect the original GitLab merge request state
	pullRequestTitle := mergeRequest.Title
	switch {
	case strings.EqualFold(mergeRequest.State, "merged"):
		pullRequestTitle = "MERGED: " + pullRequestTitle
	case strings.EqualFold(mergeRequest.State, "closed"):
		pullRequestTitle = "CLOSED: " + pullRequestTitle
	}

	createdAt := "an unknown date"
	if mergeRequest.CreatedAt != nil {
		createdAt = mergeRequest.CreatedAt.UTC().Format(dateTimeFormat)
	}

	body := fmt.Sprintf(`> [!NOTE]
> This pull request was migrated from GitLab
>
> |      |      |
> | ---- | ---- |
> | **Original Author** | %[1]s |
> | **GitLab Project** | [%[4]s/%[5]s](%[12]s://%[10]s/%[4]s/%[5]s) |
> | **GitLab Merge Request** | [%[11]s](%[12]s://%[10]s/%[4]s/%[5]s/merge_requests/%[2]d) |
> | **GitLab MR Number** | [%[2]d](%[12]s://%[10]s/%[4]s/%[5]s/merge_requests/%[2]d) |
> | **Date Originally Opened** | %[6]s |%[7]s
> | **Approved on GitLab by** | %[8]s |
> |      |      |
>
%[9]s

## Original Description

%[3]s`, githubAuthorName, mergeRequest.IID, description, p.gitlabPath[0], p.gitlabPath[1], createdAt, closeDate, approval, originalState, gitlabDomain, mergeRequestTitle, gitlabScheme)

	if pullRequest == nil {
		logger.Info("creating pull request", "owner", p.githubPath[0], "repo", p.githubPath[1], "source_branch", w.sourceRef, "target_branch", w.targetRef)
		newPullRequest := github.NewPullRequest{
			Title:               &pullRequestTitle,
			Head:                &w.sourceRef,
			Base:                &w.targetRef,
			Body:                &body,
			MaintainerCanModify: pointer(true),
			Draft:               &mergeRequest.Draft,
		}
		if pullRequest, _, err = gh.PullRequests.Create(ctx, p.githubPath[0], p.githubPath[1], &newPullRequest); err != nil {
			if strings.Contains(err.Error(), "No commits between") {
				logger.Debug("skipping merge request as the change is already present in trunk branch", "owner", p.githubPath[0], "repo", p.githubPath[1], "merge_request_id", mergeRequest.IID)
				return false, nil
			}
			return false, fmt.Errorf("creating pull request for merge request %d: %v; pull request numbers are now shifted for the remaining merge requests", mergeRequest.IID, err)
		}

		if mergeRequest.State == "closed" || mergeRequest.State == "merged" {
			logger.Debug("closing pull request", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber())

			pullRequest.State = pointer("closed")
			if pullRequest, _, err = gh.PullRequests.Edit(ctx, p.githubPath[0], p.githubPath[1], pullRequest.GetNumber(), pullRequest); err != nil {
				return false, fmt.Errorf("updating pull request: %v", err)
			}
		}

	} else {
		var newState *string
		switch mergeRequest.State {
		case "opened":
			newState = pointer("open")
		case "closed", "merged":
			newState = pointer("closed")
		}

		if pullRequest.State != nil && newState != nil && *pullRequest.State != *newState {
			pullRequestState := &github.PullRequest{
				Number: pullRequest.Number,
				State:  newState,
			}

			if pullRequest, _, err = gh.PullRequests.Edit(ctx, p.githubPath[0], p.githubPath[1], pullRequestState.GetNumber(), pullRequestState); err != nil {
				return false, fmt.Errorf("updating pull request state: %v", err)
			}
		}

		if (newState != nil && (pullRequest.State == nil || *pullRequest.State != *newState)) ||
			(pullRequest.Title == nil || *pullRequest.Title != pullRequestTitle) ||
			(pullRequest.Body == nil || *pullRequest.Body != body) {
			logger.Info("updating pull request", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber())

			pullRequest.Title = &pullRequestTitle
			pullRequest.Body = &body
			pullRequest.MaintainerCanModify = nil
			if pullRequest, _, err = gh.PullRequests.Edit(ctx, p.githubPath[0], p.githubPath[1], pullRequest.GetNumber(), pullRequest); err != nil {
				return false, fmt.Errorf("updating pull request: %v", err)
			}
		} else {
			logger.Trace("existing pull request is up-to-date", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber())
		}
	}

	comments := w.notes

	// A pull request created during this run has no comments in the index
	prComments := index.comments[pullRequest.GetNumber()]
	logger.Info("migrating merge request comments from GitLab to GitHub", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber(), "count", len(comments))

	for _, comment := range comments {
		if comment == nil || comment.System {
			continue
		}

		githubCommentAuthorName := comment.Author.Name

		commentAuthor, err := getGitlabUser(ctx, comment.Author.Username)
		if err != nil {
			return false, fmt.Errorf("retrieving gitlab user: %v", err)
		}
		if commentAuthor.WebsiteURL != "" {
			githubCommentAuthorName = "@" + strings.TrimPrefix(strings.ToLower(commentAuthor.WebsiteURL), "https://github.com/")
		}

		commentBody := fmt.Sprintf(`> [!NOTE]
> This comment was migrated from GitLab
>
> |      |      |
> | ---- | ---- |
> | **Original Author** | %[1]s |
> | **Note ID** | %[2]d |
> | **Date Originally Created** | %[3]s |
> |      |      |
>

## Original Comment

%[4]s`, githubCommentAuthorName, comment.ID, comment.CreatedAt.UTC().Format(dateTimeFormat), comment.Body)

		foundExistingComment := false
		for _, prComment := range prComments {
			if prComment == nil {
				continue
			}

			if strings.Contains(prComment.GetBody(), fmt.Sprintf("**Note ID** | %d", comment.ID)) {
				foundExistingComment = true

				if prComment.Body == nil || *prComment.Body != commentBody {
					logger.Debug("updating pull request comment", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber(), "comment_id", prComment.GetID())
					prComment.Body = &commentBody
					if _, _, err = gh.Issues.EditComment(ctx, p.githubPath[0], p.githubPath[1], prComment.GetID(), prComment); err != nil {
						return false, fmt.Errorf("updating pull request comments: %v", err)
					}
				}
			} else {
				logger.Trace("existing pull request comment is up-to-date", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber(), "comment_id", prComment.GetID())
			}
		}

		if !foundExistingComment {
			logger.Debug("creating pull request comment", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber())
			newComment := github.IssueComment{
				Body: &commentBody,
			}
			if _, _, err = gh.Issues.CreateComment(ctx, p.githubPath[0], p.githubPath[1], pullRequest.GetNumber(), &newComment); err != nil {
				return false, fmt.Errorf("creating pull request comment: %v", err)
			}
		}
	}

	// Add a final comment describing what ultimately happened to the merge request on GitLab
	if finalBody, err := p.mergeRequestFinalComment(ctx, mergeRequest); err != nil {
		return false, err
	} else if finalBody != "" {
		foundExistingFinal := false
		for _, prComment := range prComments {
			if prComment == nil {
				continue
			}

			if strings.Contains(prComment.GetBody(), finalCommentMarker) {
				foundExistingFinal = true

				if prComment.Body == nil || *prComment.Body != finalBody {
					logger.Debug("updating merge request final comment", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber(), "comment_id", prComment.GetID())
					prComment.Body = &finalBody
					if _, _, err = gh.Issues.EditComment(ctx, p.githubPath[0], p.githubPath[1], prComment.GetID(), prComment); err != nil {
						return false, fmt.Errorf("updating merge request final comment: %v", err)
					}
				}
				break
			}
		}

		if !foundExistingFinal {
			logger.Debug("creating merge request final comment", "owner", p.githubPath[0], "repo", p.githubPath[1], "pr_number", pullRequest.GetNumber())
			newComment := github.IssueComment{
				Body: &finalBody,
			}
			if _, _, err = gh.Issues.CreateComment(ctx, p.githubPath[0], p.githubPath[1], pullRequest.GetNumber(), &newComment); err != nil {
				return false, fmt.Errorf("creating merge request final comment: %v", err)
			}
		}
	}

	return true, nil
}
