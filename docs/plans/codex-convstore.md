# JOH-152 — Codex ConvStore (rollout parser + date-scan + state-DB read → SessionEntry)

**Status:** design complete, verified against real data + CodexSim; **no code written yet.**
Worktree `codex-convstore` off `main-codex` (base 18dab5d). PR target: `main-codex`.
This doc is the handoff from the design/exploration pass — implement against it.

## Task (from lead-r-2, msg #1293)
Implement Codex's **ConvStore** (assemble a `convops.SessionEntry` from Codex's full
storage model) and register it on a Codex `Harness` descriptor's `Convs` field.

ConvStore contract is **MERGED (#316, b3b8932 on main-codex)** —
`pkg/claude/harness/convstore.go` is live. **READ IT FIRST** for exact signatures; the brief's
shape was:
- `ListConvs(cwd string) ([]convops.SessionEntry, error)` — `cwd==""` ⇒ all convs across cwds.
- `Resolve(idPrefix, cwd string, global bool) (*ConvRef, error)` — `(nil,nil)`=no match;
  `(nil,err)`=I/O-fail OR ambiguous(>1); exact id beats prefix.
- `Title(convID string) (string, error)` — unknown ⇒ `("",nil)`.
- `ConvRef{ConvID, ProjectPath(=REAL cwd), Harness}`.
- **#316 also ADDED `SessionEntry.Model`** ⇒ the earlier "no Model field" contract-bite is
  RESOLVED; map Model←threads.model (or turn_context.model). Confirm the field name in convstore.go.

**Sequencing:** REBASE the worktree onto fresh `main-codex` first (gets #316). Then build the
bulk (rollout parse + state-DB read + scanner) AND wire it straight against the live interface:
the ConvStore methods + `Convs` field + `var _ ConvStore = codexConvs{}` + the Codex `Harness`
registration. After JOH-152: JOH-153 (conv ls/search Codex, read-only), then JOH-170 (telemetry).

## ⚠️ Load-bearing constraint (verified)
**CodexSim (`pkg/testharness/codex_sim.go`, #312) writes ONLY the rollout `.jsonl` — it does
NOT write `state_5.sqlite`.** It mirrors title in memory; comment says "when the parser
(JOH-152) decides whether it reads that DB, the writer slots into SetTitle (JOH-165)."
⇒ **The ConvStore MUST assemble fully from the rollout file**, with the threads DB as
*optional enrichment*. This is both the testable design and the correct "file ± sidecar" model.

## Verified data formats

### Rollout file: `~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl` (+ `.jsonl.zst` cold)
- Date dir + filename ts are **local time**; filename uses `-` not `:`. The **uuid in the
  filename == session id == threads.id**. Timestamp INSIDE the file is UTC ms (`...Z`).
- Each line: `{"timestamp","type","payload"}`, `type` ∈ {`session_meta`,`event_msg`,`response_item`,`turn_context`}.
- `session_meta.payload`: `id`, `timestamp`, `cwd`, `originator`, `cli_version`, `source`,
  `thread_source`, `model_provider`, `base_instructions{text}`. Line 1. **cwd lives here.**
- `event_msg.payload.type` ∈ {`task_started`,`user_message`,`agent_message`,`token_count`,`task_complete`}.
  The first `user_message` event's `.message` = first user prompt (→ derived title / FirstPrompt).
- `turn_context.payload`: `model` (e.g. "gpt-5.5"), `cwd`, `approval_policy`, `sandbox_policy`, …
- `response_item.payload`: `type:message`, `role`, `content[{type,text}]`.

### State DB: `~/.codex/state_5.sqlite` table `threads` (real schema, verified)
Cols of interest: `id` (PK, = rollout uuid), `rollout_path`, `cwd`, `title` (NOT NULL),
`git_branch` (nullable), `model`, `reasoning_effort`, `first_user_message`, `preview`,
`tokens_used`, `created_at`/`updated_at` (sec int), `created_at_ms`/`updated_at_ms`,
`archived`(int 0/1), `archived_at`(nullable int).
**Observed (un-renamed session):** `title == first_user_message == preview == "hello"`.
⇒ **rename heuristic: `title != "" && title != first_user_message`** ⇒ native rename.
(Pragmatic signal; the rename-detection spike is JOH-161. An auto-summary title for a long
convo could read as "renamed" — acceptable for v1, document it.)
Read **read-only** (`file:…?mode=ro`); the repo driver is `modernc.org/sqlite` (pure Go).

## SessionEntry mapping (pinned by brief)
`convops.SessionEntry` fields: SessionID, FullPath, FileMtime, FirstPrompt, Summary,
CustomTitle, MessageCount, Created, Modified, GitBranch, GitBranchStartup, ProjectPath,
IsSidechain, Harness, FileSize, ArchivedAt, BranchHistory, LastTurnInterrupted.

| SessionEntry | Codex source |
|---|---|
| SessionID | rollout uuid (filename / session_meta.id) |
| FullPath | rollout path |
| FileMtime | file mtime (Unix) — like CC |
| FirstPrompt | threads.first_user_message, else rollout first `user_message` (codexPreview-trim) |
| CustomTitle | threads.title IFF rename heuristic true; else "" |
| ProjectPath | **REAL cwd** = threads.cwd (present) else session_meta.cwd |
| GitBranch | threads.git_branch (nullable→"") ; "" without threads |
| GitBranchStartup | "" (Codex has one branch field) |
| Created | session_meta.timestamp (or threads.created_at); CC uses RFC3339 UTC string |
| Modified | file mtime as `time.RFC3339` UTC string (mirror CC convops.go:685) |
| MessageCount | 0 for v1 (not pinned; document) |
| Model | threads.model, else turn_context.model — **#316 added SessionEntry.Model** (confirm name) |
| Harness | **"codex"** — set on the entry AND on the conv_index INSERT |
| ArchivedAt | threads.archived/archived_at → RFC3339 when archived, else "" |

**conv_index INSERT:** set `harness="codex"` on the row (db.ConvIndexRow.Harness, schema v56,
already exists). The **ON-CONFLICT survival** of that tag (so a CC-blind rescan doesn't clobber
it) is **codex-seam's parallel durability slice — DO NOT implement that db-plumbing.** Just SET
harness on INSERT. An end-to-end durability flow-test assertion may need codex-seam's slice
merged first — **coordinate with the lead before relying on it.**

## Assembly design (the algorithm)
Enumerate rollout files; **threads enriches, rollout is source-of-truth + fallback.**
1. `loadCodexThreads(home) → map[id]threadRow` (state DB; **DB-absent ⇒ empty map, no error**).
2. Walk `~/.codex/sessions/**/rollout-*.jsonl{,.zst}`. Per file: id from filename.
   - **threads row present:** assemble from threads (cwd, title, git_branch, model,
     first_user_message, tokens, created/updated, archived) + file mtime + path. No file read
     (⇒ `.zst` handled transparently).
   - **no threads row:** parse the rollout head — session_meta + first `user_message` +
     a `turn_context.model`; short-circuit once all found. (`.zst`: decompress-stream — see below.)
3. Filter by cwd (`cwd==""` ⇒ all). Map → `SessionEntry`.
- **Resolve(idPrefix,cwd,global):** scan; collect matches whose id has the prefix (exact id wins
  outright); 0⇒(nil,nil); >1⇒(nil, ambiguous err); honor cwd unless global.
- **Title(convID):** threads.title if rename-heuristic, else derived first_user_message; unknown⇒("",nil).
- **Perf (deferred, "consider"):** a cwd→ids SQLite index, self-healing on rescan. NOT v1 — note as TODO.

### `.zst` handling
`github.com/klauspost/compress/zstd` (pure Go) is **in the module cache** (v1.18.x) but NOT in
go.mod. The threads-enrichment path already covers cold `.zst` sessions without decompressing.
Only a **threads-less `.zst`** needs decompression — add the dep for completeness:
try `GOPROXY=off GOFLAGS=-mod=mod go get github.com/klauspost/compress@v1.18.5` (offline from
cache; network proxy is blocked, but github.com is allowed if needed). If the dep proves
painful, fall back to **skip-with-warning for threads-less `.zst`** and note it (rare).

## File plan (package `harness`, mirroring claude.go living there)
- `codex_rollout.go` — rollout envelope structs + `parseCodexRolloutHead(path) (*codexRollout, error)`
  (+ `.zst` aware); `scanCodexRollouts(home) ([]string, error)` (walk).
- `codex_state.go` — `threadRow` struct + `loadCodexThreads(home) (map[string]threadRow, error)`.
- `codex_convstore.go` — the scanner merging threads+rollout → `[]convops.SessionEntry`, plus
  interface-independent `resolveCodex(...)` / `codexTitle(...)` helpers. Keep these so the
  eventual ConvStore methods are THIN wrappers.
- `codex.go` — register `&Harness{Name:"codex", DisplayName:"Codex CLI", Convs: codexConvs{}}`
  + `var _ ConvStore = codexConvs{}` (in scope now that #316 is merged). Caution: a Codex harness
  with nil Spawn/Models/Life — verify nothing assumes registered harnesses have a Spawner (spawn
  is a later issue; `Resolve("codex")` may now succeed). May register Convs-only for this PR.

## Testing
- **Rollout path:** drive `CodexSim` (NewCodexSim/WriteExchange) to lay down real rollout files
  under a temp HOME (t.Setenv HOME + USERPROFILE), then assert ListConvs/Resolve/Title.
- **Threads-enrichment path:** CodexSim has no threads DB → write a small test helper that
  inserts a `threads` row into `~/.codex/state_5.sqlite` (modernc driver). Cover: rename
  (title≠first_user_message ⇒ CustomTitle), un-renamed (title==first_user_message ⇒ derived),
  git_branch, archived, cwd filtering.
- Edge: DB-absent (empty map, no error), threads-less rollout, multiple cwds, prefix vs exact in
  Resolve, ambiguous prefix.
- **Flow test:** brief notes an end-to-end durability flow-test may need codex-seam's ON-CONFLICT
  slice first — coordinate. The `e2e/sim test per feature` norm applies; if the durability
  assertion is blocked, land the unit/CodexSim coverage and note the flow-test follow-up.

## Gate / process
`go build ./...`, `golangci-lint run ./...`, `go test ./...` clean before PR. PR base
`main-codex` (CI runs on main-codex PRs since #311). CodeRabbit usually rate-limit-SKIPS (green
≠ reviewed) → get an independent cold review (fresh sub-agent, `isolation: worktree`, diff-only)
before merge-ready. Report to lead (conv 1985b1dc / its reincarnation) at milestones only.
Watch context; reincarnate before ~40%.

## Key references
- SessionEntry + CC parser: `pkg/claude/common/convops/convops.go` (Created/Modified at :679-686).
- conv_index harness col + Upsert: `pkg/claude/common/db/convindex.go`.
- Harness seam + claude impl pattern: `pkg/claude/harness/{harness,claude}.go`.
- CodexSim + fixture: `pkg/testharness/codex_sim.go`, `pkg/testharness/testdata/codex_rollout_v0139.jsonl`.
- Real data: `~/.codex/sessions/...` + `~/.codex/state_5.sqlite` (Codex v0.139.0 installed locally).
