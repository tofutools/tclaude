# Testharness v2 — behavior-accurate CC + tmux simulators

## Why this redesign

The Phase 1/2 testharness mocks at the subprocess boundary and
**synthesizes** the side effects the daemon expects:
`MaterializeSpawn` writes a `SessionRow` with conv-id directly to
SQLite and flips a FakeTmux alive flag. From the daemon's perspective
the world looks the same as if `tclaude session new` had succeeded.

That's enough to pin "did the daemon's coordination logic do the right
thing in the happy path", but it misses the bug class that's actually
hurt us in production: **timing / cache / disk-write** mismatches.
Concrete examples that the synthesis approach can't catch:

- A `/rename` slash command goes out via tmux send-keys but the
  resulting `customTitle` turn never reaches the `.jsonl` (CC dropped
  it, paste-mode swallowed the Enter, etc.). The daemon sees its
  send-keys returned 0 and reports success; the web UI / `tclaude
  conv ls` later renders the old title because the `.jsonl` is the
  source of truth.
- A delete purges DB rows but the `.jsonl` lingers because
  `removeJSONLBestEffort` walks the wrong project dir; subsequent
  resume "succeeds" against the orphan file.
- The watch-mode cache reflects stale state because a `.jsonl` mtime
  changed but the conv_index refresh missed it.
- Reincarnate's `prevTitle` lookup reads from disk via
  `FreshConvRowAt`; if the on-disk title disagrees with the
  conv_index row the new title is computed against the wrong base.

These are all "the disk says X, the cache says Y, the UI says Z"
inconsistencies. To pin them, the harness has to model:

1. CC actually consuming keystrokes and writing `.jsonl` content
   (with realistic timing).
2. Tmux actually routing send-keys to the right pane (so a wrong
   target is observable, not silently swallowed by a global recorder).
3. The full stack of agentd / conv / FreshConvRow / watch reading
   that real on-disk state and refreshing their caches like in
   production.

## Goals

1. **Mock only at the subprocess boundary.**
   `clcommon.TmuxCommand` and `agentd.SpawnDetachedTclaude{New,Resume}`
   are the two boundaries. Everything else — agentd, conv, agent,
   session, watch, web, statusbar — runs production code, reads real
   files under `t.TempDir()`, refreshes caches per its normal cadence.

2. **Simulators that behave, not synthesize.**
   - `CCSim` receives keystrokes, parses slash commands, writes real
     `.jsonl` turns. Its lifecycle mirrors real CC: startup writes a
     summary turn, `/rename X` writes a `customTitle` turn, `/exit`
     shuts it down, plain text writes a user turn.
   - `TmuxSim` owns a sessions table and routes send-keys to the
     CCSim attached to the target pane. Models `has-session`,
     `kill-session`, `new-session`, `send-keys` with real semantics.

3. **Easy to encode newly-discovered quirks.**
   Each time we find a CC or tmux behavior that bit us in production,
   add it to the simulator. Over time the sims accrete the
   institutional knowledge of "things that have surprised us".
   Example: paste-mode coalescing that swallows trailing Enter — the
   sim should reproduce that quirk so a regression in
   `injectTextAndSubmit` (e.g., dropping the 500ms gap) fails the
   relevant flow test.

   v2 implements this via two opt-in CCSim hooks:
   - `OnInput(prefix, handler)` — register a custom behavior for an
     input prefix; newer registrations win, so a single test can
     shadow a default without editing the sim.
   - `SetCommandDelay(prefix, dur)` — schedule processing of a
     matching input asynchronously after `dur`. Models "CC takes a
     moment to commit this turn"; tests that depend on prod's
     "send-keys returned ⇒ turn on disk" fallacy will fail when this
     is set non-zero.

4. **All the existing flow tests still pass and gain teeth.**
   At least one assertion per scenario should now be jsonl-grounded:
   "after the daemon injects `/rename worker`, the conv's `.jsonl`
   contains a `customTitle: worker` turn that `FreshConvRow` returns
   for that conv-id".

## Non-goals (yet)

- Modeling CC's full prompt/turn lifecycle (assistant responses, tool
  use). The sims only do what scenarios require.
- A real-binary smoke test (still deferred to Phase 4 in the original
  testing-strategy doc).
- Mocking other subprocess boundaries (git, gh, dbus). Out of scope.

## Architecture

### `pkg/testharness/cc_sim.go` (full rewrite)

```go
type CCSim struct {
    ConvID    string  // claude session id, used as filename + conv_id
    Cwd       string
    JsonlPath string  // ~/.claude/projects/<encoded-cwd>/<convID>.jsonl

    // internal
    home     string
    inputCh  chan string
    done     chan struct{}
    mu       sync.Mutex
    title    string
    alive    bool
}

// NewCCSim picks a conv-id and prepares paths but does not yet write.
// Call Start to materialise.
func NewCCSim(t *testing.T, home, label, cwd string) *CCSim

// Start mirrors what production CC does on its first run: write the
// initial summary turn into the .jsonl, register the SessionRow with
// the conv-id (so the daemon's poll loop can find it), kick off the
// keystroke processing goroutine. Idempotent.
func (c *CCSim) Start(label string) error

// Receive enqueues a keystroke chunk delivered via tmux send-keys.
// Tests usually don't call this directly — TmuxSim routes here.
//
// Inputs are buffered until an "Enter" arrives, then processed as a
// single line (matches CC's input-readline semantics):
//   - "/rename X"  → write customTitle turn, update internal title
//   - "/exit"      → write end-of-conversation turn, mark dead
//   - "/compact"   → write a summary turn (post-compact CC writes)
//   - anything else → write a user turn
func (c *CCSim) Receive(text string)

// Shutdown stops the input loop and closes any open file handles.
// Auto-called via t.Cleanup.
func (c *CCSim) Shutdown()

// Title returns the current customTitle (post-/rename). Read by
// assertions; production code finds the same value by reading the
// .jsonl + conv_index.
func (c *CCSim) Title() string
```

### `pkg/testharness/tmux_sim.go` (rewrite)

```go
type TmuxSim struct {
    mu       sync.Mutex
    sessions map[string]*tmuxSession  // name → session
    sentLog  []SentKey
}

type tmuxSession struct {
    name  string
    cwd   string
    cc    *CCSim
    alive bool
}

// Command is the rewire replacement for clcommon.TmuxCommand.
// Dispatches based on args[0]:
//   - has-session -t X         → exit 0 if alive, exit 1 otherwise
//   - send-keys -t TARGET TEXT → routes to attached CCSim's Receive
//   - kill-session -t X        → mark dead, signal CCSim shutdown
//   - new-session, set-option  → no-op success (the harness creates
//                                  sessions via Register, not via this
//                                  path)
func (t *TmuxSim) Command(args ...string) *exec.Cmd

// Register attaches a CCSim to a tmux session name + marks it alive.
// Called by the SpawnDetachedTclaude{New,Resume} mocks.
func (t *TmuxSim) Register(name, cwd string, cc *CCSim)

// IsAlive answers production session.IsTmuxSessionAlive lookups via
// the attached CCSim's alive state.
func (t *TmuxSim) IsAlive(name string) bool

// Sessions returns a snapshot of registered sessions. Tests use this
// to iterate when they don't know names ahead of time.
func (t *TmuxSim) Sessions() []*tmuxSession

// WaitForSendKeys is kept for backwards-compat assertions but the
// preferred new-style assertion is to read the .jsonl directly.
func (t *TmuxSim) WaitForSendKeys(target, contains string, timeout time.Duration) bool
```

### `pkg/testharness/world.go`

Adds an exported `CCs` registry so tests can look up CCSims by label:

```go
type World struct {
    HomeDir string
    Tmux    *TmuxSim
    CCs     *CCRegistry  // label → CCSim
}
```

### Mock wiring (`pkg/claude/agentd/flow_setup_test.go`)

```go
rewire.Func(t, agentd.SpawnDetachedTclaudeNew, func(label, cwd string) error {
    cc := testharness.NewCCSim(t, w.HomeDir, label, cwd)
    if err := cc.Start(label); err != nil {
        return err
    }
    w.Tmux.Register(label, cwd, cc)
    w.CCs.Set(label, cc)
    return nil
})

rewire.Func(t, agentd.SpawnDetachedTclaudeResume, func(convID, cwd string) error {
    // Resume: locate the existing CCSim by conv-id, re-attach to a
    // tmux session. If the .jsonl exists but no in-memory CCSim does
    // (e.g. the prior CCSim was Shutdown before resume), hydrate a
    // fresh one from the .jsonl on disk.
    cc := w.CCs.GetByConvID(convID)
    if cc == nil {
        cc = testharness.HydrateCCSim(t, w.HomeDir, convID, cwd)
    }
    label := generateResumeLabel(convID)
    w.Tmux.Register(label, cwd, cc)
    return nil
})
```

## Migration: existing 4 flow tests

All four still pass after the migration. Assertions exercise the
real surfaces a human would use (`tclaude agent groups members`,
`tclaude conv ls` equivalents) — NOT the simulator's internal
.jsonl. The .jsonl is impl detail of the mock layer: the mock writes
it so the production read path (FreshConvRowResolved → ScanAndUpsertFile
→ conv_index → /v1/groups/{name}/members) finds something realistic.

| Test | New surface assertion |
|------|------------------------|
| `TestSpawn_RenamesAndResumes` | `GET /v1/groups/alpha/members` lists the new conv with alias=worker AND title=worker (after the daemon's post-spawn /rename settles). |
| `TestReincarnate_OfRN_ProducesRNplus1` | `GET /v1/groups/alpha/members` shows the new conv with alias=worker, title=worker-r-4; the old conv is no longer a member. |
| `TestClone_EmptyAlias_DerivesFromOriginalTitle` | `GET /v1/groups/alpha/members` shows BOTH the original (unchanged title) AND the clone with the computed alias=worker-c-1 + matching title. |
| `TestDelete_PurgesAllReferencingRows` | `conv.ListSessions(projectDir)` (the same scan `tclaude conv ls` runs) does not re-discover the deleted conv — guards the orphan-jsonl bug class. |

## Phasing

1. Build new `CCSim` + `TmuxSim` alongside existing files.
   Old `FakeTmux` can be deleted once tests migrate.
2. Update `flow_setup_test.go` to wire the new simulators.
3. Migrate one test at a time. Add the jsonl-grounded assertion as
   each migrates.
4. Delete dead code (`MaterializeSpawn`, `MaterializeConvID`,
   `FakeTmux`).
5. Land in the same PR (#49) as the existing harness work, since
   it's the natural next step.

## Out of scope but worth noting

- Synthesising assistant responses. CCSim treats every input as a
  user turn (or slash command). Real CC would respond. We don't need
  that to pin coordination bugs — agents that DO need it can extend
  CCSim with `WriteAssistantTurn`.
- Modelling tmux paste-mode coalescing. The flake we hit in
  injectTextAndSubmit was about paste-mode swallowing trailing
  Enter. Worth a deliberate sim quirk: if `Enter` arrives within
  some window of a paste, treat it as a paste-newline rather than a
  submit. Implement when we get a regression.
- Window/pane multiplexing. We assume one pane per session. Models
  for split panes, multiple windows, etc. — only when needed.

## What "done" looks like

- `pkg/testharness/cc_sim.go` and `pkg/testharness/tmux_sim.go`
  rewritten to the v2 design above.
- All four existing flow tests pass and have a jsonl-grounded
  assertion.
- `MaterializeSpawn` / `MaterializeConvID` / `FakeTmux` removed.
- `script/test ./...` and `./verify.sh` green.
- PR #49 description updated with a "v2 simulators" subsection in
  the Method block, and the diagrams refreshed if the spawn flow
  reads differently.
