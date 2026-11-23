## Changelog

### Unreleased

- A rerun no longer overwrites what has changed on GitHub: a branch is pushed only when it is new on GitHub or its tip there is the GitLab tip or an ancestor of it, a tag only when it is new or points at the same object. Other branches and tags are left as they are and logged as warnings, with one summary at the end of the project and a warning on open merge requests that use a skipped branch. Skipped refs do not change the exit status
- Add `-overwrite-diverged-refs` to restore the previous behaviour of force-pushing every branch and tag
- `-trim-branches-on-github` no longer deletes a branch on GitHub whose tip is a commit that GitLab does not have, unless `-overwrite-diverged-refs` is set, and no longer force-pushes all branches when there is nothing to delete
- Trimming uses the complete list of branches on GitHub instead of the first page of the REST listing

### v0.9.0

- Update `github.com/google/go-github` to v74
- Use [Advanced Issue Search](https://github.blog/changelog/2025-03-06-github-issues-projects-api-support-for-issues-advanced-search-and-more/) to reduce the resultset when trying to match an existing PR, to try to avoid missing existing PRs and then creating a duplicate
- Support subgroups in GitLab
- Support renaming trunk branch in GitLab repo to any arbitrary branch name on the GitHub repo

### v0.8.0

- Attempt to log the message from the response for a failed API request to GitHub

### v0.7.0

- Ensure repo description has no newlines/returns
- Correctly match "already up-to-date" error
- Always create temporary source/target branches for closed MRs, otherwise we will inadvertently try to create a PR against current trunk branch (which will already have the changes)
- Set the PR state before attempting to update it, to avoid GH erroring with "Cannot change the base branch of a closed pull request"

### v0.6.0

- Push tags from gitlab to github
- Don't retry when receiving a 403 with SAML enforcement error

### v0.5.0

- Follow tags when pushing trunk branch

### v0.4.0

- Don't set 'maintainer_can_modify' when updating a PR, since only the author can do that and we might be using a different user's token
- Emit an error when we encounter an orphaned MR starting commit
- Attempt to find a suitable base commit from which to branch when scaffolding a PR branch, should now handle merge commits

### v0.3.0

- Return non-zero exit status when errors occurred during a migration

### v0.2.0

- Disable cgo when building

### v0.1.0

- Initial release
