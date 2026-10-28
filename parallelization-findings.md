# Parallellisering av MR-migreringen: findings

Underlag för granskning. Kodreferenser gäller nuvarande `HEAD` (`873bd33`) i `gitlab-migrator`. Varje påstående är märkt:

- **[verifierat]**: kontrollerat i koden, i go-git v5.13.2-källan eller i GitHubs dokumentation.
- **[antagande]**: bygger på min kunskap och är inte kontrollerat i den här sessionen.

Ingen kod är ändrad och inget är mätt.

## 1. Problem

Migrering av projekt med hundratals eller tusentals MRs tar mycket lång tid. Målet är att köra MR-migreringen parallellt eller på annat sätt korta tiden.

## 2. Nuläge

### 2.1 Var parallellism finns idag

- **[verifierat]** `-max-concurrency` (default 4) styr bara hur många projekt som migreras parallellt: `main.go`, `performMigration`, en worker pool över `[]Project`.
- **[verifierat]** Inom ett projekt körs MRs sekventiellt i `migrateMergeRequests`, en `for`-loop i `project.go:307`.
- **[verifierat]** README säger själv att rate limiting kan göra att parallellism inte ger någon märkbar vinst (avsnittet "Concurrency").

### 2.2 Arbete per MR (`migrateMergeRequest`, `project.go:429`)

| Steg | Kod | Typ | Gäller |
|---|---|---|---|
| GitHub issue search efter befintlig PR | `project.go:444` → `helper.go` `getGithubSearchResults` | GitHub Search API | alla MRs |
| `PullRequests.Get` per sökträff | `project.go:469` | GitHub REST GET | alla MRs med träff |
| Kontroll att källbranchen finns | `project.go:485` | lokal git | öppna MRs |
| `GetMergeRequestCommits` | `project.go:510` | GitLab REST | stängda/mergade utan PR |
| `fetchCommitsBySHA`, en `fetch` per MR | `project.go:544`, `:337` | git-nätverk mot GitLab | stängda/mergade utan PR |
| `worktree.Checkout` med `Force: true`, två gånger | `project.go:581`, `:593` | lokal git, skriver ut hela trädet i memfs | stängda/mergade utan PR |
| Push av två temporära brancher | `project.go:603` | git-nätverk mot GitHub | stängda/mergade utan PR |
| `getGitlabUser` för author och approvers | `project.go:646`, `:669` | GitLab REST (cachat) | alla |
| `ListMergeRequestAwardEmoji` | `project.go:661` | GitLab REST | alla |
| `PullRequests.Create` och sedan `Edit` (stäng) | `project.go:744`, `:756` | GitHub POST + PATCH | nya PRs |
| `PullRequests.Edit` vid diff | `project.go:776`, `:791` | GitHub PATCH | befintliga PRs |
| `ListMergeRequestNotes`, paginerat | `project.go:807` | GitLab REST | alla |
| `Issues.ListComments` | `project.go:822` | GitHub REST GET | alla |
| `Issues.CreateComment` eller `EditComment` per note | `project.go:870`, `:884` | GitHub POST/PATCH | per note |
| Slutkommentar | `project.go:906`, `:919` | GitHub POST/PATCH | stängda/mergade |
| Push som raderar temporära brancher (defer) | `project.go:621` | git-nätverk mot GitHub | stängda/mergade utan PR |

En ny stängd MR med N notes kostar alltså ungefär:

- GitHub REST: 1 sökning, 2 för att skapa och stänga PR, 1 kommentarslista och N+1 kommentarer.
- Git: 1 fetch, 2 pushar och 2 fulla checkouts.
- GitLab: 3–4 anrop.

## 3. GitHubs rate limits

Källa: <https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api>

- **[verifierat]** Högst 100 samtidiga anrop.
- **[verifierat]** Högst 900 poäng per minut för REST. GET kostar 1 poäng, POST/PATCH/PUT/DELETE kostar 5.
- **[verifierat]** Innehållsskapande anrop: generellt högst 80 per minut och 500 per timme. Vissa endpoints har lägre gränser.
- **[verifierat]** Primär gräns för PAT: 5000 anrop per timme.
- **[antagande]** Search API: 30 anrop per minut för autentiserade användare. Sidan ovan anger ingen siffra, bara att gränserna är hårdare.
- **[antagande]** Det är oklart om `PullRequests.Edit` (stängning) och `Issues.EditComment` räknas som innehållsskapande. `Create` gör det med säkerhet.
- **[verifierat, användaren]** Målet är GitHub Enterprise Cloud på github.com, inte GHES.
- **[verifierat]** Dokumentationen listar en enda uppsättning sekundära gränser, utan undantag för Enterprise Cloud. 80/min, 500/h och 900 poäng/min gäller alltså fullt ut och kan inte ändras av oss.
- **[verifierat]** Enterprise Cloud höjer bara den primära gränsen till 15 000 per timme, och bara för GitHub App-installationstoken, user tokens via en GitHub App eller OAuth app som ägs av Enterprise Cloud-organisationen, och `GITHUB_TOKEN` i Actions. En PAT ligger kvar på 5000 per timme.

### 3.1 Konsekvens

Räkneexempel med 1000 stängda MRs och i snitt 3 notes:

- Ungefär 5 innehållsskapande anrop per MR (create, 3 kommentarer, slutkommentar), utan stängningen.
- Totalt ungefär 5000 innehållsskapande anrop. Med 500 per timme blir det minst 10 timmar, oavsett antal workers.
- Search API med 30 per minut ger ett tak på 1800 MRs per timme. Det binder inte så länge 500/h-gränsen gäller.

Slutsats: eftersom målet är github.com ger det sannolikt begränsad effekt att bara köra MR-loopen parallellt, när man väl ligger på den sekundära gränsen. Den största vinsten bör komma från färre innehållsskapande anrop (5.6), färre anrop totalt (5.2) och mindre git-arbete per MR (5.3–5.5). Parallellism hjälper bara upp till gränsen, alltså för läsningar och git-arbete och för skrivningar så länge man ligger under 80/min och 500/h. Det bör mätas först (punkt 5.1).

### 3.2 Retry-hantering idag

- **[verifierat]** `retryablehttp` med `RetryMax: 8`, `RetryWaitMin: 30s` och `RetryWaitMax: 300s` (`main.go`).
- **[verifierat]** Backoff respekterar `Retry-After` och `X-Ratelimit-Reset`. 403 och 429 görs om, utom vid SAML-fel.
- **[verifierat]** Retry-klienten används bara för GitHub. GitLab-klienten har ingen egen retry-konfiguration, så `go-gitlab` använder sin default.
- **[verifierat]** `github.BypassRateLimitCheck` är satt, så go-github gör ingen proaktiv strypning.

## 4. Hinder för att köra MRs parallellt i nuvarande kod

### 4.1 Delat go-git-repo är inte trådsäkert

- **[verifierat]** `p.repo` klonas med `memory.NewStorage()` och en delad `memfs` (`project.go:140-148`).
- **[verifierat]** I go-git v5.13.2 är `storage/memory.ObjectStorage` vanliga maps utan mutex (`storage/memory/storage.go:85`). `ReferenceStorage` är `map[plumbing.ReferenceName]*plumbing.Reference` (`:246`).
- **[verifierat]** `Remote.PushContext` skriver till lokal ref-storage via `updateRemoteReferenceStorage` (`remote.go:234`, `:351`). En push är alltså inte bara läsning.
- **[verifierat]** `worktree.Checkout` ändrar `HEAD` och filerna i den delade memfs.
- Slutsats: `fetchCommitsBySHA`, `Checkout`, `SetReference` och `Push` på samma `p.repo` får inte köras samtidigt utan lås.

### 4.2 Race på `errCount`

- **[verifierat]** `errCount` är en vanlig `int`. Den ökas i `sendErr` (`helper.go`) och i `performMigration` och `printReport` (`main.go`), utan synkronisering. Det är redan en data race mellan projekt-workers.
- **[verifierat]** I `performMigration` räknas fel dubbelt, eftersom `errCount++` följs av `sendErr(err)` som också ökar räknaren.

### 4.3 Cachen

- **[verifierat]** `objectCache` använder `sync.RWMutex` och klarar samtidiga anrop.
- **[verifierat]** `getGithubUser`/`setGithubUser` använder `gitlabUserCacheType` och tvärtom (`cache.go`). Det är ofarligt eftersom nycklarna aldrig blandas, men förvirrande.
- **[antagande]** Get-then-set utan lås ger dubbla API-anrop när två workers missar samma nyckel samtidigt. Det ger fler anrop men inga fel.

### 4.4 PR-ordning

- **[verifierat]** MRs hämtas sorterade på `created_at asc` och PRs skapas i den ordningen. PR-numren på GitHub följer alltså MR-ordningen idag.
- Om PRs skapas parallellt försvinner den ordningen. Om det spelar roll är det användarens beslut.

### 4.5 Övrigt i MR-loopen

- **[verifierat]** `mergeRequest.SourceBranch`/`TargetBranch` skrivs om (`project.go:506-507`). Varje MR är sitt eget objekt, så det är ofarligt vid parallellisering.
- **[verifierat]** Loopen som letar parent-commit i `project.go:565-574` använder alltid `startCommit.Parent(0)` i stället för `Parent(i)`, och `continue` sist i loopen gör ingenting. Det hör inte till parallelliseringen, men noteras.
- **[verifierat]** `Issues.ListComments` görs en gång per PR. Kommentarer som skapas under samma körning syns inte i `prComments`, men varje note jämförs bara mot sitt eget Note ID, så det ger inga dubbletter.

## 5. Förslag i prioritetsordning

### 5.1 Mät först

Kör med `LOG_LEVEL=TRACE` på ett representativt projekt med 50–100 MRs:

- Räkna `waiting before retrying failed API request` och summera `sleep`. Om de dominerar står verktyget mest still på rate limits, och då hjälper främst 5.2 och 5.6.
- Om de inte dominerar går tiden åt till git-arbete och latens, och då hjälper 5.3–5.5 och 5.7.
- Notera vilken status som triggar väntan (403/429), om `Retry-After` finns och vilken URL det gäller (search eller något annat).

### 5.2 Ersätt sökning per MR med en listning

- Hämta alla PRs en gång per projekt med `PullRequests.List(state=all)`. Den paginerade klienten hämtar 100 per sida.
- Bygg en `map[int]*github.PullRequest` från MR IID genom att parsa `**GitLab MR Number** | ...` ur bodyn, samma matchning som idag (`project.go:474-475`).
- Effekt: ingen Search API-användning, inget `PullRequests.Get` per MR och ungefär `antal_PRs/100` GET-anrop totalt.
- Bieffekt: mer robust idempotens. **[antagande]** Sökindexet uppdateras med fördröjning, så nyss skapade PRs kan saknas i sökresultatet vid snabb omkörning.

### 5.3 Byt `worktree.Checkout` mot `SetReference`

- För temporära brancher räcker `p.repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(name), hash))`.
- Effekt: inga fulla checkouts i minnet, vilket är dyrt för stora repon, och inget beroende av delat worktree-tillstånd.
- **[antagande]** Inget annat i flödet kräver ett utcheckat worktree. Granskaren bör bekräfta detta.

### 5.4 Hämta alla saknade commits i en fetch

- Samla alla commit-SHA:er för alla stängda/mergade MRs utan PR och anropa `fetchCommitsBySHA` en gång, eventuellt i chunkar.
- Förutsätter att `GetMergeRequestCommits` görs i en egen fas före den här, och den fasen kan köras parallellt mot GitLab.

### 5.5 Pusha temporära brancher i batchar

- Skapa refs för många MRs och pusha dem i chunkar om 100–200 refspecs. Radera på samma sätt efter att PRs har skapats och stängts.
- Effekt: från 2 pushar per MR till ett fåtal pushar totalt.
- **[antagande]** GitHub har en gräns för antal refs per push. Jag vet inte siffran, så chunkstorleken bör verifieras.
- Risk: fler temporära brancher finns samtidigt på GitHub. Om målrepot har workflows med `on: push` triggas de, men det gäller redan idag, bara utspritt över tid.
- Risk: om körningen avbryts ligger fler temporära brancher kvar. Städningen bör vara idempotent och kunna köras separat.

### 5.6 ~~Valfritt: färre kommentarer per PR~~ Struken, se avsnitt 11

- En ny flagga som slår ihop alla notes till en kommentar per PR, eller lägger dem i PR-bodyn.
- Effekt: från N+1 till 1–2 innehållsskapande anrop per PR. Det minskar påverkan av 500/h-gränsen mest av alla förslag.
- Nackdel: formatändring. Idempotensen per Note ID måste lösas på annat sätt, till exempel med en hash av innehållet.

### 5.7 Parallellisera MRs i faser

Först efter 5.2–5.5:

1. **Läsfas, parallell med N workers:** GitLab-anrop per MR (commits, award emoji, notes, users) och GitHub-läsningar som `ListComments`.
2. **Git-fas, seriell per projekt:** en fetch, `SetReference` för alla temporära brancher och batchade pushar.
3. **Skrivfas mot GitHub, seriell eller 2–3 workers:** skapa/stäng PR, kommentarer. Seriell skapning bevarar PR-ordningen.
4. **Städfas:** batchad radering av temporära brancher.

Gör också:

- `errCount` atomisk (`atomic.Int64`) och ta bort dubbelräkningen.
- Respektera `ctx` i alla faser, eftersom Ctrl-C ska fungera som idag.
- Lägg till en egen flagga för MR-concurrency, separat från `-max-concurrency` för projekt. Totalt antal samtidiga GitHub-anrop är projekt × MR-workers.

### 5.8 Utanför scope men värt att känna till

- **[verifierat]** En GitHub App installerad i Enterprise Cloud-organisationen får 15 000 anrop per timme i stället för 5000 med PAT. Det hjälper bara om den primära gränsen binder, eftersom de sekundära gränserna gäller ändå. Verktyget stöder idag bara PAT via `GITHUB_TOKEN`, och pushar görs med `githubUser:githubToken`.
- **[antagande]** GitHub Enterprise Importer (`gh gei`) stöder inte GitLab som källa och är därför inget alternativ.

## 6. Öppna frågor

1. ~~Är målet github.com eller GitHub Enterprise Server?~~ Besvarat: GitHub Enterprise Cloud på github.com. De sekundära gränserna binder, och den primära gränsen kan höjas till 15 000/h med en GitHub App.
2. ~~Spelar det roll att PR-numren följer MR-ordningen?~~ Besvarat: ja, ordningen bevaras.
3. ~~Är det acceptabelt att slå ihop kommentarer (5.6)?~~ Besvarat: nej, dagens format behålls.
4. ~~Hur fördelar sig MRs mellan öppna och stängda/mergade, och hur många notes har de i snitt?~~ Besvarat i 10.2 för schedulinglayer.

## 7. Att granska särskilt

- Är slutsatsen i 3.1 rimlig, det vill säga att rate limits och inte sekventiell körning är den egentliga flaskhalsen på github.com?
- Är 5.3 säker? Behövs ett utcheckat worktree någonstans?
- Finns fler delade tillstånd än de i avsnitt 4 som blir problem vid parallellisering?
- Är prioritetsordningen i avsnitt 5 rätt?

## 8. Kompletterande granskning

Kontrollerat mot koden, go-git v5.13.2, go-gitlab v0.115.0 samt GitHubs och GitLabs dokumentation. Samma märkning som ovan.

### 8.1 Öppna MRs migreras aldrig (bugg, påverkar 5.3)

- **[verifierat]** `p.repo.Branch()` i `project.go:485` läser branch-config, inte refs (`repository.go:682` i go-git).
- **[verifierat]** En mirror-klon skapar ingen branch-config alls (`repository.go:978`, villkoret `!o.Mirror`).
- Följd: alla öppna MRs får `ErrBranchNotFound`. De hoppas över med `-skip-invalid-merge-requests`, annars fel. Inte kört, bara läst.
- Rätt kontroll: `p.repo.Reference(plumbing.NewBranchReferenceName(name), false)`.
- Svar på frågan i avsnitt 7: inget i flödet behöver ett worktree. 5.3 är säker.

### 8.2 Varje push traverserar hela repot i minnet (stärker 5.5)

- **[verifierat]** `revlist.ObjectsWithStorageForIgnores` (`plumbing/revlist/revlist.go:40`) expanderar först ignore-mängden till alla objekt nåbara från GitHubs samtliga refs, innan den beräknar vad som ska skickas. Det är O(hela repot) per push, två gånger per stängd MR. Raderingspushen slipper (`allDelete`).
- För stora repon är detta sannolikt den dominerande git-kostnaden, mer än checkout. En batchad push gör traverseringen en gång.
- Mätningen i 5.1 bör ta tid per "pushing branches" i trace-loggen.

### 8.3 `GetMergeRequestCommits` paginerar inte (bugg, hör ihop med 5.4)

- **[verifierat]** `GetMergeRequestCommitsOptions` är en alias för `ListOptions` och anropas utan `PerPage` eller `Page` (`project.go:510`).
- **[antagande]** Endpointen pagineras med GitLabs default 20 per sida. En MR med fler än 20 commits får då bara första sidan, fel slutcommit och en PR med ofullständig diff, utan felmeddelande.
- Fixa när koden flyttas till läsfasen: `PerPage: 100` och loopa på `NextPage`.

### 8.4 MR-head-refs följer med i mirror-klonen (påverkar 5.4)

- **[verifierat]** Mirror-refspecen är `+refs/*:refs/*` (`repository.go:1009`) och GitLab annonserar `refs/merge-requests/:iid/head`.
- **[verifierat]** GitLab raderar dessa refs 14 dagar efter close/merge.
- Följd: för nyare MRs finns commits redan lokalt och `fetchCommitsBySHA` är en no-op. För äldre krävs fetch. Vinsten av 5.4 beror på åldersfördelningen, se fråga 4 i avsnitt 6.

### 8.5 Cachen är stale i `-loop` (påverkar 5.2)

- **[verifierat]** Sökresultat och PR-objekt cachas per process och invalideras aldrig (`helper.go:86`, `helper.go:64`).
- Följd: andra varvet hittar inte PRs som skapades i första varvet, försöker skapa dubbletter och får 422.
- PR-mappen i 5.2 ska byggas per projektvarv, inte cachas.

### 8.6 Retry-budgeten är per anrop, inte per timfönster (krav för 5.7)

- **[verifierat]** `RetryMax: 8` med `Retry-After` ger vid 500/h-taket ungefär 8 minuters väntan och sedan fel för den MR:en. Nästa MR går samma väg. Resultatet blir en kaskad av fel i stället för väntan.
- **[verifierat]** GitHubs docs: fortsatta anrop under rate limit kan ge ban. Best practices: gör anrop seriellt, vänta minst en sekund mellan skrivningar.
- Slutsats: skrivfasen bör vara seriell med en global paus-på-429 som alla workers delar, inte per-request-retry. Ersätter "2–3 workers" i 5.7 steg 3.

### 8.7 Samtidiga pushar till samma GitHub-repo

- **[antagande]** GitHub låser refs per repo, så parallella pushar ger "cannot lock ref"-fel.
- Ett argument till för att git-fasen hålls seriell per projekt, även om man skulle klona per worker.

### 8.8 Minne

- **[verifierat]** Klonen gör `Reset` till memfs även vid `Mirror: true` (`repository.go:940`). Hela repot ligger i RAM två gånger per projekt, gånger `-max-concurrency`.
- Med 5.3 kan `NoCheckout: true` sättas och worktreet försvinner helt.

### 8.9 Map-race är krasch, inte bara race

- **[verifierat]** Samtidig läsning och skrivning av go-gits memory-maps ger `fatal error: concurrent map read and map write`, som inte kan fångas med `recover`. Förtydligar 4.1.

### 8.10 Mindre

- **[verifierat]** 403 som inte är rate limit, till exempel ruleset eller behörighet, retry:as 8 gånger med upp till 27 minuters väntan per MR (30+60+120+240+4×300 s). Kan förvränga mätningen i 5.1.
- **[verifierat]** Search-gränsen är 30 anrop/min för autentiserade anrop enligt GitHubs search-docs. Uppgraderar antagandet i avsnitt 3.
- **[verifierat]** go-gitlab-default: `RetryMax: 5`, 100–400 ms, respekterar `RateLimit-Reset` (`gitlab.go:342-348`).
- **[antagande]** PR-body och kommentarer har ett tak på 65 536 tecken. Begränsar 5.6 för MRs med många eller långa notes.

### 8.11 Ändrade svar på avsnitt 7

- 3.1 är rimlig. GitHubs egna best practices bekräftar att skrivningar ska serialiseras.
- 5.3 är säker, se 8.1.
- Fler delade tillstånd: cachen i `-loop` (8.5) och retry-klienten utan global throttle (8.6).
- Prioritetsordningen håller, men 8.2 gör 5.5 till det tyngsta git-förslaget, före 5.3.

## 9. Andra granskningen

Nya punkter från en oberoende genomgång av koden, plus två egna. Allt i avsnitt 8 bekräftades.

### 9.1 Färre anrop

- **[verifierat]** `Issues.ListComments` med issue-nummer 0 listar alla kommentarer i hela repot (`issues_comments.go:58` i go-github). En listning per projekt ersätter en per PR, samma mönster som 5.2. Nya PRs behöver ingen listning alls.
- **[verifierat]** GitLabs MR-lista har redan `sha` (head) och ett enskilt MR-GET har `diff_refs.base_sha` (merge-basen). Det kan ersätta `GetMergeRequestCommits`, datumsorteringen och parent-sökningen, och ger rätt bas även för MRs som börjar på en merge-commit. Kostar samma antal GitLab-anrop som idag. **[antagande]** `diff_refs` kan vara tomt för mycket gamla MRs; behåll dagens väg som fallback.
- **[verifierat]** `ListProjectMergeRequests` (`project.go:278`) och `ListMergeRequestNotes` (`project.go:800`) saknar `PerPage`, så GitLab ger 20 per sida. `PerPage: 100` ger fem gånger färre GitLab-anrop. Samma fix som 8.3.
- **[verifierat]** `fetchCommitsBySHA` öppnar en upload-pack-session per MR. go-git bygger "haves" genom att gå upp till 100 commits bakåt per lokal ref (`remote.go:58`, `:928`). Mirror-klonen har också alla `refs/merge-requests/*`, så tusentals lokala refs ger hundratusentals commit-laddningar per fetch. Stärker 5.4.
- **[verifierat]** "No commits between"-fallet (`project.go:745`) lämnar ingen markör, så varje omkörning upprepar fetch och två pushar för de MR:erna.

### 9.2 Retry och rate limits, tillägg till 8.6

- **[verifierat]** Nätverksfel retry:as aldrig: `CheckRetry` returnerar false när `err != nil` (`main.go:272`). Connection reset ger direkt fel, vilket blir vanligare med fler samtidiga anslutningar.
- **[verifierat]** Backoff utan `Retry-After` och utan `X-Ratelimit-Remaining: 0`, vilket är typiskt för 900 poäng/min, faller till 30, 60, 120, 240, 300 s. Överdimensionerat för ett minutfönster.
- **[verifierat]** `Backoff` läser `resp.Request` (`main.go:218`) före nil-kontrollen (`:229`). Nås bara om `CheckRetry` retry:ar med `resp == nil`, vilket den inte gör idag.
- **[verifierat]** go-gitlab-klienten har en delad `rate.Limiter` som ställs in från `RateLimit`-headers (`gitlab.go:577`, `:868`). Läsfasen mot GitLab behöver ingen egen throttle.
- **[verifierat]** `hclog`, `go-github`-klienten och `objectCache` är trådsäkra.

### 9.3 Buggar utanför parallelliseringen

- **[verifierat]** Deadlock vid Ctrl-C i `-loop`: `queue` stängs bara i icke-loop-grenen (`main.go:537-549`). Workers som väntar i `range queue` vaknar aldrig och `wg.Wait()` hänger.
- **[verifierat]** `Draft` kan inte ändras via PATCH: `pullRequestUpdate` i go-github saknar fältet (`pulls.go:341`). Jämförelsen på `project.go:784` ger därför en verkningslös `Edit` vid varje omkörning för varje MR vars draft-status skiljer sig. En innehållsskapande skrivning i onödan, se 8.6.
- **[verifierat]** `sort.Slice` på `project.go:521` derefererar `CommittedDate` utan nil-kontroll, och `mergeRequest.CreatedAt.UTC()` på `:732` likaså. En panic i en MR-goroutine tar ner hela processen. 5.7 bör ha `recover` per MR-worker. Försvinner om 9.1 andra punkten görs.
- **[verifierat]** `regexp.MustCompile` i `project.go:467` och `:121` kompileras per anrop. Flytta till paketnivå.

### 9.4 Reviderad slutsats

Parallellisering av MR-loopen kräver lås eller en storer per worker (4.1, 8.9), men ger liten vinst så länge 5.2, 5.3, 5.5 och 8.6 kvarstår. Störst effekt, i ordning:

1. En PR-listning och en kommentarslistning per projekt i stället för en sökning och en listning per MR (5.2, 9.1).
2. `SetReference` i stället för `Checkout`, och `NoCheckout` på klonen (5.3, 8.8).
3. Batchad fetch och batchade pushar (5.4, 5.5, 8.2, 9.1).
4. `PerPage: 100` och `diff_refs` mot GitLab (8.3, 9.1).
5. Färre skrivningar per MR och en gemensam skriv-throttle (5.6, 8.6, 9.3 draft).
6. Först därefter parallell läsfas (5.7).

## 10. Mätt mot en riktig instans

Kontrollerat 2026-10-28 med `glab` mot `connect/scheduling/schedulinglayer` (projekt 376) på den interna GitLab-instansen, samt `git ls-remote` på samma repo.

### 10.1 Antaganden som nu är verifierade

- **[verifierat]** 8.3: commits-endpointen pagineras med 20 per sida. MR !2841 har 59 commits (`X-Total: 59`, `X-Total-Pages: 3`), anropet utan `per_page` returnerar 20. I ett urval av 78 MRs hade 6 fler än 20 commits, alltså ungefär 8 %. På 2 858 MRs blir det i storleksordningen 200 PRs med fel diff om 8.3 inte fixas.
- **[verifierat]** 9.1: `diff_refs` är `null` för de äldsta MR:erna (!1 och !2 från 2015) men finns från åtminstone !100 (2017). Fallback till dagens väg behövs för de första MR:erna.
- **[verifierat]** 8.4: endast 94 `refs/merge-requests/*/head` annonseras för 2 858 MRs, i praktiken de öppna. Nästan alla stängda och mergade MRs måste hämtas per SHA. 5.4 är därmed viktig, inte valfri.

### 10.2 Projektets fördelning, svar på fråga 4 i avsnitt 6

| Mått | Värde |
|---|---|
| MRs totalt | 2 858 |
| Öppna | 66 |
| Mergade | 2 486 |
| Stängda | 306 |
| Notes per MR, snitt (urval 78) | 9 totalt, varav 1 icke-system |
| MRs med fler än 20 commits | ca 8 % |

- Kommentarer är få. 5.6 ger liten effekt här; de innehållsskapande anropen domineras av create, close och slutkommentar, ungefär 3–4 per MR.
- 2 792 stängda eller mergade MRs ger därmed ungefär 8 000–11 000 innehållsskapande anrop, alltså 17–22 timmar vid 500 per timme, oavsett parallellism. Det bekräftar slutsatsen i 3.1.
- Alla 66 öppna MRs träffas av buggen i 8.1.

### 10.3 Refs i repot

| Namnrymd | Antal |
|---|---|
| `refs/environments/` | 2 280 |
| `refs/tags/` | 1 034 |
| `refs/heads/` | 597 |
| `refs/merge-requests/` | 183 |
| `refs/pipelines/` | 6 |

- Mirror-klonen hämtar alla 4 101 refs, inklusive 2 280 `refs/environments/` som aldrig används. De pushas inte, eftersom push-refspecen är `refs/heads/*` och `refs/tags/*`, men de ingår i go-gits "haves"-beräkning vid varje fetch (9.1, upp till 100 commits per ref) och tar minne.
- 1 034 taggar och 597 brancher på GitHub-sidan ingår i ignore-mängden vid varje push (8.2).
- Förslag: klona med en explicit refspec för `refs/heads/*` och `refs/tags/*` i stället för `Mirror: true`, så att varken `refs/environments/` eller `refs/merge-requests/` hämtas. MR-head-refs ger ändå bara 94 träffar här.

## 11. Beslut 2026-10-28

1. PR-numren ska följa MR-ordningen. Skrivfasen mot GitHub är seriell per projekt.
2. Kommentarsformatet behålls. 5.6 är struken.
3. Slutkommentaren bakas inte in i PR-bodyn. Den förblir en egen kommentar.
4. Buggarna i 8.1, 8.3 och 9.3 fixas som en del av arbetet, oberoende av parallelliseringen.
5. Steg 5.1, mät först, hoppas över för schedulinglayer. Avsnitt 10 är underlaget.

Kvar att genomföra, i den ordning 9.4 anger: 5.2 och 9.1, 5.3 och 8.8, 5.4 och 5.5, 8.3 och `diff_refs`, 8.6, sedan 5.7.
