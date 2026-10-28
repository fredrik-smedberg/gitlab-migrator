# MR-migrering: buggfixar, färre anrop och fasindelning

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Korta migreringstiden för projekt med tusentals MRs genom att fixa fyra buggar, ta bort anrop per MR, batcha git-arbetet och dela upp MR-migreringen i en parallell läsfas, en seriell git-fas och en seriell, strypt skrivfas.

**Architecture:** `migrateMergeRequests` i `project.go` delas upp i faser som arbetar på en lista av `mrWork`-poster. Lookup av befintliga PRs och kommentarer görs en gång per projekt (`github_index.go`). Git-arbetet använder bara refs, aldrig worktree, och pushar i chunkar (`gitphase.go`). Alla GitHub-skrivningar går genom en gemensam throttle i transporten (`throttle.go`).

**Tech Stack:** Go 1.27.2, go-git v5.13.2 (memory storage), go-github v74, go-gitlab v0.115.0, go-retryablehttp, golang.org/x/time/rate.

**Spec:** `parallelization-findings.md` i repots rot. Avsnitt 8–11 gäller; 9.4 ger ordningen, 11 besluten.

## Global Constraints

- PR-numren ska följa MR-ordningen (`created_at asc`). Skrivfasen mot GitHub är seriell per projekt (beslut 11.1).
- Kommentarsformatet och slutkommentaren behålls oförändrade (beslut 11.2, 11.3). PR-body, kommentarsbody och `finalCommentMarker` får inte ändras, annars uppdateras alla redan migrerade PRs vid omkörning.
- Omkörning mot ett färdigmigrerat projekt ska göra noll innehållsskapande anrop mot GitHub.
- Ctrl-C ska fungera i alla faser och städfasen ska köras ändå.
- Inga ändringar i `memory.Storage` får ske från mer än en goroutine i taget (spec 4.1, 8.9).
- Inga `Co-authored-by`-rader i commits.
- Testprojekt: GitLab `connect/scheduling/usermanagementservice` (id 377, 34 MRs: 3 öppna, 26 mergade, 5 stängda; !1, !13, !14, !17 har fler än 20 commits). GitHub-mål: `ssc-sandbox/usermanagementservice-migration-test`.

Körkommando för verifiering, kallas `KÖR` nedan. `TARGET` är GitHub-repot; GitHub-tokenen saknar `delete_repo`-scope, så "kör mot tomt repo" betyder nytt reponamn, `usermanagementservice-migration-test-t<tasknummer>`, aldrig `-delete-existing-repos`:

```bash
cd /Users/fredriksmedberg/Code/other/gitlab-migrator && go build -o /tmp/gitlab-migrator . && \
TARGET=${TARGET:-usermanagementservice-migration-test} && \
GITHUB_TOKEN=$(gh auth token) GITLAB_TOKEN=$(glab config get token --host sees-gitlab.sams.internal) \
LOG_LEVEL=TRACE /tmp/gitlab-migrator -gitlab-domain sees-gitlab.sams.internal \
  -gitlab-project connect/scheduling/usermanagementservice \
  -github-repo ssc-sandbox/$TARGET \
  -github-user Fredrik-Smedberg_SSCSpace -migrate-pull-requests -skip-invalid-merge-requests -no-prompt \
  -gitlab-ca-file $HOME/.config/gitlab-migrator/ssc-ca.pem \
  2>&1 | tee /tmp/migration-run.log
```

Kontroll av skrivningar efter en körning, kallas `RÄKNA SKRIVNINGAR`:

```bash
grep -cE '"method"="?(POST|PATCH)' /tmp/migration-run.log || true
gh pr list --repo ssc-sandbox/$TARGET --state all --limit 200 --json number | jq length
```

## Review Focus

1. MR vars ändringar redan finns i målbranchen: GitHub svarar "No commits between". Förväntat: MR:en hoppas över utan fel, temporära brancher raderas ändå. Test i Task 4.
2. Andra körningen mot ett färdigmigrerat projekt: förväntat noll POST/PATCH mot GitHub och samma antal PRs. Test i Task 2 och Task 4.
3. MR med fler än 20 commits (!17 har 103): förväntat att PR:en visar lika många commits som GitLab. Test i Task 1 och Task 3.
4. Ctrl-C under skrivfasen: förväntat att processen avslutas inom en minut, att temporära brancher raderas och att inga `migration-*`-brancher ligger kvar. Test i Task 4.
5. MR utan `diff_refs` (de äldsta i schedulinglayer, 2015): förväntat att commit-listan används som fallback och att PR:en skapas. Testprojektet har troligen `diff_refs` på alla MRs, så fallbacken verifieras först vid körningen mot schedulinglayer. Task 3 kontrollerar !1 ändå.

---

### Task 1: Buggfixar (spec 8.1, 8.3, 9.3, 4.2, 9.1 PerPage)

**Files:**
- Modify: `project.go:278-302` (MR-listning), `:485-493` (branch-kontroll), `:510-523` (commits), `:565-574` (parent-loop), `:781-784` (draft), `:800-819` (notes), `:121`, `:467` (regexp)
- Modify: `main.go:43`, `:409-410`, `:514-521`, `:537-549`; `helper.go:200-203`
- Create: `helper_test.go`

**Interfaces:**
- Produces: `func listMergeRequestCommits(projectID, iid int) ([]*gitlab.Commit, error)` i `project.go`, paginerad med `PerPage: 100`, sorterad på `CommittedDate` där nil räknas som nolltid. Används av Task 3.
- Produces: `errCount` som `atomic.Int64`, ökas bara i `sendErr`.

- [ ] **Steg 1: Skriv test för commit-sortering med nil-datum**

`helper_test.go`: `TestSortCommitsByDateHandlesNil` bygger tre `*gitlab.Commit` där mittersta har `CommittedDate == nil` och kontrollerar att `sortCommitsByDate(commits)` inte panikar och lägger nil först.

- [ ] **Steg 2: Kör testet, förvänta FAIL med "undefined: sortCommitsByDate"**

Run: `go test ./... -run TestSortCommitsByDate -v`

- [ ] **Steg 3: Implementera `sortCommitsByDate(commits []*gitlab.Commit)` och `listMergeRequestCommits` i `project.go`**

`listMergeRequestCommits` loopar på `resp.NextPage` precis som MR-listningen på rad 288–302, med `GetMergeRequestCommitsOptions{PerPage: 100, Page: page}`. Ersätt anropet på rad 510 och sorteringen på rad 521.

- [ ] **Steg 4: Kör testet, förvänta PASS**

- [ ] **Steg 5: Fixa branch-kontrollen (8.1)**

Rad 485: `p.repo.Reference(plumbing.NewBranchReferenceName(mergeRequest.SourceBranch), false)`; felkontrollen använder `plumbing.ErrReferenceNotFound`.

- [ ] **Steg 6: Sätt `PerPage: 100` på `ListProjectMergeRequestsOptions` (rad 278) och `ListMergeRequestNotesOptions` (rad 800)**

Båda har inbäddad `ListOptions`.

- [ ] **Steg 7: Ta bort draft-jämförelsen på rad 784 och `pullRequest.Draft = ...` på rad 789**

go-github kan inte PATCH:a draft (`pulls.go:341`). Lämna `Draft` i `NewPullRequest` vid skapande.

- [ ] **Steg 8: Fixa parent-loopen rad 565–574**

Byt till `startCommit.Parent(0)` utan loop; vid fel returnera felet. Loopen var död kod (spec 4.5).

- [ ] **Steg 9: Nil-skydd för `mergeRequest.CreatedAt` på rad 732**

Format "an unknown date" som i `mergeRequestFinalComment` om nil.

- [ ] **Steg 10: Flytta `regexp.MustCompile` på rad 121 och 467 till paketnivå i `project.go`**

Namn: `newlineRegexp`, `pullRequestNumberRegexp`.

- [ ] **Steg 11: `errCount` till `atomic.Int64` och ta bort dubbelräkning**

`main.go:43` typ, `helper.go:201` `errCount.Add(1)`, `main.go:390` `errCount.Load()`, ta bort `errCount++` på `main.go:409`, `:514`, `:520`.

- [ ] **Steg 12: Fixa deadlock i `-loop` (`main.go:537-549`)**

Flytta `close(queue)` till efter `if loop {...} else {...}` så att den körs i båda grenarna.

- [ ] **Steg 13: Bygg, vet, test**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: inga fel.

- [ ] **Steg 14: Kör mot testprojektet, första körningen**

KÖR. Expected: loggen visar `migrated merge requests ... successful=N failed=0`. Anteckna N och antal skipped i commit-meddelandet. Kontrollera MR !17:

```bash
gh pr list --repo ssc-sandbox/$TARGET --state all --search "GitLab MR Number** | [17]" --json number,title
gh pr view <nummer> --repo ssc-sandbox/$TARGET --json commits --jq '.commits|length'
```
Expected: 103.

- [ ] **Steg 15: Commit**

```bash
git add helper_test.go project.go main.go helper.go
git commit -m "Fix open MR branch check, MR commit pagination, loop deadlock and error counting"
```

---

### Task 2: PR- och kommentarsindex per projekt (spec 5.2, 8.5, 9.1)

**Files:**
- Create: `github_index.go`, `github_index_test.go`
- Modify: `project.go:438-482` (sökning), `:821-826` (kommentarslistning), `:275-307` (bygg index)
- Delete: `search.go`; `getGithubSearchResults`, `getGithubPullRequest` i `helper.go`; motsvarande cache-typer och metoder i `cache.go`; `gitHubAdvancedSearchModder` i `main.go:333-335`

**Interfaces:**
- Produces:
  ```go
  type githubIndex struct {
      pullRequests map[int]*github.PullRequest    // nyckel: GitLab MR IID
      comments     map[int][]*github.IssueComment // nyckel: PR-nummer
  }
  func buildGithubIndex(ctx context.Context, owner, repo string) (*githubIndex, error)
  func mergeRequestIIDFromBody(body string) (int, bool)
  func issueNumberFromURL(url string) (int, bool)
  ```
- `migrateMergeRequest(ctx, mergeRequest, index *githubIndex)`; signaturen används av Task 4.

- [ ] **Steg 1: Skriv tester**

`github_index_test.go`:
- `TestMergeRequestIIDFromBody`: tabelltest med `"| **GitLab MR Number** | [17](https://x/merge_requests/17) |"` → 17; `"| **GitLab MR Number** | 17 |"` → 17; tom body → false; `"MR Number** | [170]"` → 170, inte 17.
- `TestIssueNumberFromURL`: `"https://api.github.com/repos/o/r/issues/42"` → 42; `"garbage"` → false.

- [ ] **Steg 2: Kör testerna, förvänta FAIL med undefined**

Run: `go test ./... -run 'TestMergeRequestIIDFromBody|TestIssueNumberFromURL' -v`

- [ ] **Steg 3: Implementera `github_index.go`**

`buildGithubIndex`: `gh.PullRequests.List(ctx, owner, repo, &github.PullRequestListOptions{State: "all", ListOptions: github.ListOptions{PerPage: 100}})` (githubpagination hämtar alla sidor) och `gh.Issues.ListComments(ctx, owner, repo, 0, &github.IssueListCommentsOptions{Sort: pointer("created"), Direction: pointer("asc"), ListOptions: github.ListOptions{PerPage: 100}})`. Nummer 0 ger repots alla kommentarer. Nyckla PRs på `mergeRequestIIDFromBody(pr.GetBody())`; PRs utan markör ignoreras. Regexp: `\*\*GitLab MR Number\*\* \| \[?(\d+)`.

- [ ] **Steg 4: Kör testerna, förvänta PASS**

- [ ] **Steg 5: Byt sökningen i `migrateMergeRequest` mot `index.pullRequests[mergeRequest.IID]`**

Ta bort rad 438–482. Ta bort `gh.Issues.ListComments` på rad 822 och använd `index.comments[pullRequest.GetNumber()]`; för en nyss skapad PR är listan tom.

- [ ] **Steg 6: Bygg indexet i `migrateMergeRequests` före loopen och skicka in det**

Indexet byggs per anrop, aldrig cachat (8.5).

- [ ] **Steg 7: Ta bort död kod**

`search.go`, `getGithubSearchResults`, `getGithubPullRequest`, `githubSearchResultsCacheType`, `githubPullRequestCacheType` och deras metoder, `gitHubAdvancedSearchModder`-wrappern i `main.go` (transporten blir `&retryablehttp.RoundTripper{Client: retryClient}` direkt). Rätta de förväxlade cache-typerna i `cache.go:85-109` samtidigt (spec 4.3).

- [ ] **Steg 8: Bygg, vet, test**

Run: `go build ./... && go vet ./... && go test ./...`

- [ ] **Steg 9: Kör mot testprojektet, andra körningen (idempotens)**

KÖR, sedan RÄKNA SKRIVNINGAR. Expected: 0 POST/PATCH, samma antal PRs som efter Task 1, inga `performing search` i loggen.

- [ ] **Steg 10: Commit**

```bash
git add -A
git commit -m "Replace per-MR search with one PR and comment listing per project"
```

---

### Task 3: Refs i stället för checkout, bare clone, diff_refs (spec 5.3, 8.8, 10.3, 9.1)

**Files:**
- Modify: `project.go:139-151` (klon), `:496-600` (temp-brancher), `:183-196` (branches)

**Interfaces:**
- Produces: `func (p *project) temporaryBranchHashes(ctx context.Context, mergeRequest *gitlab.MergeRequest) (source, target plumbing.Hash, skip string, err error)` i `project.go`. `skip` är ett icke-tomt skäl när MR:en ska hoppas över (inga commits, orphan-commit). I Task 3 gör funktionen själv `fetchCommitsBySHA`; Task 4 flyttar fetchen till `pushTemporaryBranches` och funktionen returnerar då bara hashar.

- [ ] **Steg 1: Klona utan worktree och med explicita refspecs**

Ersätt `git.CloneContext` med `git.Init(memory.NewStorage(), nil)`, `CreateRemote` för `gitlab` med `Fetch: []config.RefSpec{"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"}`, sedan `FetchContext(ctx, &git.FetchOptions{RemoteName: "gitlab", Tags: git.NoTags})`. Ta bort `memfs`-importen. Då hämtas varken `refs/environments/` eller `refs/merge-requests/` (spec 10.3).

- [ ] **Steg 2: Bygg och kör; förvänta samma antal brancher och taggar pushade som innan**

Jämför `count` i loggraden `force-pushing branches` mot `glab api --hostname sees-gitlab.sams.internal "projects/377/repository/branches?per_page=1" -i | grep -i x-total`.

- [ ] **Steg 3: Implementera `temporaryBranchHashes`**

Hämta `gl.MergeRequests.GetMergeRequest(projectID, iid, nil)`. Om `DiffRefs.BaseSha` och `DiffRefs.HeadSha` är satta: target = BaseSha, source = HeadSha. Annars fallback: `listMergeRequestCommits` (Task 1), source = sista commit, target = `Parent(0)` av första; orphan ger `skip`. Anropa `p.fetchCommitsBySHA` för de två SHA:erna (fallback: alla) före `object.GetCommit`.

- [ ] **Steg 4: Ersätt de två `worktree.Checkout` (rad 581, 593) med `p.repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(name), hash))`**

Ta bort `p.repo.Worktree()` på rad 500. Efter pushen (rad 603) `RemoveReference` på båda lokala temp-refs så att de inte växer i haves-mängden (spec 9.1).

- [ ] **Steg 5: Bygg, vet, test**

- [ ] **Steg 6: Kör mot testprojektet mot tomt repo**

`TARGET=usermanagementservice-migration-test-t3` KÖR. Expected: samma antal PRs som efter Task 1 och `gh pr view` för MR !17 visar 103 commits. Kontrollera även att !1 (2015-liknande, kan sakna `diff_refs`) fick en PR: `gh pr list ... --search "GitLab MR Number** | [1]"`.

- [ ] **Steg 7: Commit**

```bash
git add project.go
git commit -m "Use refs instead of worktree checkout, bare clone with explicit refspecs, diff_refs for temp branches"
```

---

### Task 4: Fasindelning med batchad fetch och push (spec 5.4, 5.5, 8.2, 9.1)

**Files:**
- Create: `gitphase.go`, `gitphase_test.go`
- Modify: `project.go` (`migrateMergeRequests`, `migrateMergeRequest` delas i `prepareMergeRequest` och `writeMergeRequest`)

**Interfaces:**
- Produces:
  ```go
  type mrWork struct {
      mr          *gitlab.MergeRequest
      pr          *github.PullRequest // nil om ny
      sourceRef   string              // branch-namn som PR:ens head
      targetRef   string              // branch-namn som PR:ens base
      sourceHash  plumbing.Hash       // satt när temp-brancher behövs
      targetHash  plumbing.Hash
      needsBranch bool
      notes       []*gitlab.Note
      approvers   []string
      author      string
      skip        string              // icke-tomt: hoppa över med Info-logg
  }
  func (p *project) prepareMergeRequest(ctx context.Context, mr *gitlab.MergeRequest, index *githubIndex) (*mrWork, error)   // läsfas: GitLab + index, inget git, inga skrivningar
  func (p *project) pushTemporaryBranches(ctx context.Context, work []*mrWork) error                                          // git-fas
  func (p *project) writeMergeRequest(ctx context.Context, w *mrWork, index *githubIndex) (bool, error)                        // skrivfas
  func (p *project) deleteTemporaryBranches(ctx context.Context) error                                                         // städfas
  func chunk[T any](items []T, size int) [][]T
  ```
- Konstanter i `gitphase.go`: `fetchChunkSize = 200` SHA:er per fetch, `pushChunkSize = 100` refspecs per push, `deleteChunkSize = 200`.

- [ ] **Steg 1: Skriv test för `chunk`**

`gitphase_test.go`: `TestChunk`: 5 element, storlek 2 → `[[a b] [c d] [e]]`; tom lista → tom; storlek 0 → en chunk med allt.

- [ ] **Steg 2: Kör testet, förvänta FAIL**

- [ ] **Steg 3: Implementera `chunk` och `pushTemporaryBranches` i `gitphase.go`**

`pushTemporaryBranches`: samla alla `sourceHash`/`targetHash` för poster med `needsBranch`, en `fetchCommitsBySHA` per chunk om `fetchChunkSize`; `SetReference` för alla; `PushContext` per chunk om `pushChunkSize` refspecs med `Force: true`, `NoErrAlreadyUpToDate` ignoreras; `RemoveReference` lokalt efteråt.

`deleteTemporaryBranches`: lista brancher på GitHub via `gh.Repositories.ListBranches` (paginerad), välj alla som börjar med `migration-source-` eller `migration-target-`, pusha `:refs/heads/<name>` i chunkar om `deleteChunkSize`. Fångar även rester från avbrutna körningar.

- [ ] **Steg 4: Kör testet, förvänta PASS**

- [ ] **Steg 5: Dela `migrateMergeRequest` i `prepareMergeRequest` och `writeMergeRequest`**

`prepareMergeRequest` tar rad 484–493 (branch-kontroll), `temporaryBranchHashes` (Task 3) utan fetch, användare, awards och notes (rad 644–692, 799–819). `writeMergeRequest` tar rad 694–797 och 821–926 oförändrat, men läser allt från `mrWork`. "No commits between" ger `false, nil` som idag.

- [ ] **Steg 6: Skriv om `migrateMergeRequests`**

Ordning: lista MRs → `prepareMergeRequest` för varje (seriellt, Task 6 gör det parallellt) → `pushTemporaryBranches` → `defer deleteTemporaryBranches` → `writeMergeRequest` i MR-ordning. `ctx.Err()` kontrolleras mellan varje MR i varje fas. Ta bort `defer`-pushen på rad 619–636.

- [ ] **Steg 7: Bygg, vet, test**

- [ ] **Steg 8: Kör mot testprojektet mot tomt repo, kontrollera antal pushar**

`TARGET=usermanagementservice-migration-test-t4` KÖR. Expected: samma antal PRs som efter Task 1; `grep -c 'pushing temporary branches' /tmp/migration-run.log` ≤ 2; inga brancher kvar:

```bash
gh api repos/ssc-sandbox/$TARGET/branches --paginate --jq '.[].name' | grep -c '^migration-' || echo 0
```
Expected: 0.

- [ ] **Steg 9: Kör en gång till mot samma TARGET, RÄKNA SKRIVNINGAR**

Expected: 0 POST/PATCH.

- [ ] **Steg 10: Testa Ctrl-C**

Starta KÖR med `TARGET=usermanagementservice-migration-test-t4b`, skicka SIGINT när loggen visar första `creating pull request`. Expected: processen avslutas inom 60 s, loggen visar `deleting temporary branches`, kommandot i steg 8 ger 0.

- [ ] **Steg 11: Commit**

```bash
git add -A
git commit -m "Split MR migration into read, git, write and cleanup phases with batched fetch and push"
```

---

### Task 5: Gemensam skriv-throttle och retry-justeringar (spec 8.6, 9.2, 8.10)

**Files:**
- Create: `throttle.go`, `throttle_test.go`
- Modify: `main.go:206-335` (retry-klient), `go.mod` (`golang.org/x/time` direkt)

**Interfaces:**
- Produces:
  ```go
  type throttlingTransport struct { base http.RoundTripper; writes *rate.Limiter; mu sync.Mutex; pausedUntil time.Time }
  func newThrottlingTransport(base http.RoundTripper, writesPerHour int) *throttlingTransport
  ```
  Flagga: `-github-writes-per-hour` (int, default 500).

- [ ] **Steg 1: Skriv tester med en stub-`RoundTripper`**

`throttle_test.go`:
- `TestWritesAreRateLimited`: `newThrottlingTransport(stub, 3600)` (1/s), tre POST i rad tar ≥ 2 s totalt; tre GET tar < 100 ms.
- `TestRetryAfterPausesAllRequests`: stub svarar 403 med `Retry-After: 1` på första anropet; nästa anrop, även GET, startar tidigast 1 s senare.

- [ ] **Steg 2: Kör testerna, förvänta FAIL**

- [ ] **Steg 3: Implementera `throttle.go`**

`RoundTrip`: vänta tills `pausedUntil` passerats; om metoden är POST/PATCH/PUT/DELETE: `writes.Wait(req.Context())`; skicka; om svaret är 403 eller 429 med `Retry-After`, sätt `pausedUntil = now + Retry-After` under lås. Limiter: `rate.NewLimiter(rate.Every(time.Hour/time.Duration(writesPerHour)), 1)`.

- [ ] **Steg 4: Kör testerna, förvänta PASS**

- [ ] **Steg 5: Koppla in i `main.go` och justera retry**

Transportkedja: `throttlingTransport{base: &retryablehttp.RoundTripper{Client: retryClient}}`. I `CheckRetry`: vid `err != nil` returnera `retryablehttp.DefaultRetryPolicy(ctx, resp, err)` (retry:ar nätverksfel, inte cert-fel). 403 retry:as bara om `Retry-After` finns eller `errResp.Message` matchar `(?i)rate limit`. `RetryWaitMin` till 5 s. I `Backoff`: flytta `resp.Request`-läsningen in under `if resp != nil`.

- [ ] **Steg 6: Bygg, vet, test; kör mot tomt repo (`TARGET=usermanagementservice-migration-test-t5`)**

Expected: samma antal PRs som efter Task 1. Loggen får inte innehålla `waiting before retrying`.

- [ ] **Steg 7: Commit**

```bash
git add -A
git commit -m "Throttle GitHub writes globally and retry network errors"
```

---

### Task 6: Parallell läsfas (spec 5.7)

**Files:**
- Modify: `project.go` (`migrateMergeRequests`), `main.go` (flagga)

**Interfaces:**
- Flagga: `-merge-request-concurrency` (int, default 4), global `mergeRequestConcurrency`.

- [ ] **Steg 1: Kör `prepareMergeRequest` i en worker pool**

Samma mönster som `performMigration` (`main.go:498-525`): kanal med index, `sync.WaitGroup`, resultat skrivs till `work[i]` på sin egen plats. Varje worker har `defer func() { if r := recover(); r != nil { sendErr(fmt.Errorf("merge request %d: panic: %v", iid, r)) } }()`. Git-fasen och skrivfasen förblir seriella. `ctx.Err()` avbryter workers.

- [ ] **Steg 2: Kör med `-race`**

Run: `go build -race -o /tmp/gitlab-migrator . && <KÖR utan go build-delen, TARGET=usermanagementservice-migration-test-t6>`
Expected: ingen `WARNING: DATA RACE`, samma antal PRs som efter Task 1.

- [ ] **Steg 3: Uppdatera README, avsnittet "Concurrency", med de två nya flaggorna**

- [ ] **Steg 4: Commit**

```bash
git add -A
git commit -m "Prepare merge requests in parallel, keep git and GitHub writes serial"
```
