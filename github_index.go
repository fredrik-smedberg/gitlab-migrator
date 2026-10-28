package main

import (
	"context"
	"fmt"
	"regexp"
	"strconv"

	"github.com/google/go-github/v74/github"
)

var (
	mergeRequestIIDRegexp = regexp.MustCompile(`\*\*GitLab MR Number\*\* \| \[?(\d+)`)
	issueNumberRegexp     = regexp.MustCompile(`/issues/(\d+)$`)
)

// githubIndex holds the pull requests and issue comments of a GitHub
// repository, listed once per project instead of searched for per merge request.
type githubIndex struct {
	pullRequests map[int]*github.PullRequest    // key: GitLab MR IID
	comments     map[int][]*github.IssueComment // key: PR number
}

// buildGithubIndex lists all pull requests and all issue comments of the
// repository. The client paginates, so each list call returns every page.
func buildGithubIndex(ctx context.Context, owner, repo string) (*githubIndex, error) {
	index := &githubIndex{
		pullRequests: make(map[int]*github.PullRequest),
		comments:     make(map[int][]*github.IssueComment),
	}

	logger.Debug("listing pull requests", "owner", owner, "repo", repo)
	pullRequests, _, err := gh.PullRequests.List(ctx, owner, repo, &github.PullRequestListOptions{
		State:       "all",
		ListOptions: github.ListOptions{PerPage: 100},
	})
	if err != nil {
		return nil, fmt.Errorf("listing pull requests: %v", err)
	}

	for _, pr := range pullRequests {
		if pr == nil {
			continue
		}
		iid, ok := mergeRequestIIDFromBody(pr.GetBody())
		if !ok {
			continue
		}
		if existing, found := index.pullRequests[iid]; found {
			kept, dropped := existing, pr
			if pr.GetNumber() < existing.GetNumber() {
				kept, dropped = pr, existing
			}
			logger.Warn("multiple pull requests found for merge request, using the lowest number; the higher-numbered pull request is left untouched and should be removed manually", "owner", owner, "repo", repo, "merge_request_id", iid, "pr_number", kept.GetNumber(), "ignored_pr_number", dropped.GetNumber())
			index.pullRequests[iid] = kept
			continue
		}
		index.pullRequests[iid] = pr
	}

	logger.Debug("listing issue comments", "owner", owner, "repo", repo)
	comments, _, err := gh.Issues.ListComments(ctx, owner, repo, 0, &github.IssueListCommentsOptions{
		Sort:        pointer("created"),
		Direction:   pointer("asc"),
		ListOptions: github.ListOptions{PerPage: 100},
	})
	if err != nil {
		return nil, fmt.Errorf("listing issue comments: %v", err)
	}

	for _, comment := range comments {
		if comment == nil {
			continue
		}
		number, ok := issueNumberFromURL(comment.GetIssueURL())
		if !ok {
			continue
		}
		index.comments[number] = append(index.comments[number], comment)
	}

	logger.Debug("indexed GitHub repository", "owner", owner, "repo", repo, "pull_requests", len(index.pullRequests), "comments", len(comments))

	return index, nil
}

// mergeRequestIIDFromBody returns the GitLab MR IID recorded in a migrated pull request body.
func mergeRequestIIDFromBody(body string) (int, bool) {
	m := mergeRequestIIDRegexp.FindStringSubmatch(body)
	if len(m) != 2 {
		return 0, false
	}
	iid, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return iid, true
}

// issueNumberFromURL returns the issue or pull request number from an issue API URL.
func issueNumberFromURL(url string) (int, bool) {
	m := issueNumberRegexp.FindStringSubmatch(url)
	if len(m) != 2 {
		return 0, false
	}
	number, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return number, true
}
