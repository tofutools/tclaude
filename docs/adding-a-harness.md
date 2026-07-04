# Adding a harness

This page is for contributors who want to teach tclaude to drive another coding
CLI (Gemini CLI, Aider, an in-house tool, …). The harness seam was built so this
is a **recipe, not a rewrite**: you implement a handful of small, focused
contracts and register a descriptor. Everything tclaude owns — tmux sessions,
the conversation index, agent coordination, the dashboard — then works for your
harness unchanged.

> Not every worked example under `pkg/claude/session` follows this recipe.
> `--harness shell` (`session/shell.go`) starts a plain, conversation-less
> interactive shell and is deliberately handled outside this registry — see
> the note in [harnesses.md](harnesses.md). It isn't a template for adding a
> real coding-harness integration.

## The shape of the seam

The seam lives in `pkg/claude/harness`. It is **deliberately not one monolithic
`Harness` interface**. The same user-facing feature is distributed differently
across each harness's internals — a "rename", for instance, is one logical idea
but Claude Code implements it by injecting `/rename` (which writes a title turn
into the conversation file) while Codex writes a row in its own title store. So
the seam models **focused, capability-segregated contracts** and lets each
harness satisfy each one however its storage/command model dictates.

A small `Harness` descriptor (`pkg/claude/harness/harness.go`)
composes the contracts and exposes capability flags. A `nil`
sub-contract means "this harness lacks that capability" — the `Supports*`
helpers fold those into booleans, and callers gate behavior on them so a pane is
never typed a command it can't parse.

```go
type Harness struct {
    Name        string          // persisted in the DB `harness` column; accepted by --harness
    DisplayName string          // human-facing label

    Spawn    Spawner            // build the in-tmux launch + resume command
    Models   ModelCatalog       // validate/normalize model + effort
    Life     Lifecycle          // name the in-pane slash commands (or report unsupported)
    Convs    ConvStore          // assemble conversation metadata from the harness's storage
    Hooks    HookInstaller      // install/check/repair the tclaude callback (+ trust)
    Sandbox  SandboxCatalog     // launch-time OS-sandbox modes (optional)
    Approval ApprovalCatalog    // launch-time approval policy (optional)
}
```

## The contracts

Implement as many as your harness needs; leave the rest `nil`. Claude Code
(`claude.go`) and Codex (`codex.go`, `codex_*.go`) are the worked examples —
read them alongside this list.

### `Spawner` — launch & resume *(required to spawn)*

```go
Binary() string                 // the executable name, e.g. "codex" (used by the process-tree walk)
BuildCommand(spec SpawnSpec) string  // the full shell command run inside the tmux pane
```

`SpawnSpec` carries everything needed to build the command: `EnvExports` (the
identity env prefix), `ResumeID` (empty = fresh; the resume form is
harness-specific — `claude --resume <id>` vs `codex resume <id>`), validated
`Model`/`Effort`, `ExtraArgs`, and the optional `SandboxMode` / `ApprovalPolicy`
/ `AutoReview` / `BypassHookTrust` knobs. Shell-quote anything you interpolate.

### `ModelCatalog` — model & effort *(required to spawn)*

```go
ValidateModel(s string) (string, error)   // normalize or reject a model token
ValidateEffort(s string) (string, error)  // normalize or reject an effort token
Models() []string                         // valid model values (for the spawn dialog)
EffortLevels() []string                   // valid effort values
```

Reject another harness's slugs with a clear message (e.g. Codex rejects
`claude-*` model names) so a mistyped `--harness` surfaces immediately instead of
failing after the pane has already launched.

### `Lifecycle` — in-pane control commands

```go
RenameCommand() string    // e.g. "/rename"; "" = no in-pane rename
CompactCommand() string   // e.g. "/compact"; "" = no in-pane compaction
SoftExitCommand() string  // e.g. "/exit" / "/quit"; "" = hard-kill the pane instead
```

Return `""` for anything your harness lacks. These tokens **must be compile-time
constants** — they are typed into a tmux pane, which is a keystroke-injection
sink; never interpolate user input into them. tclaude gates every slash
injection on the matching `Supports*` flag, and where an in-pane command is
missing it degrades (e.g. a missing rename command falls back to
`ConvStore.SetTitle`; a missing soft-exit becomes a hard kill).

### `ConvStore` — conversation metadata

```go
ListConvs(cwd string) ([]convops.SessionEntry, error)        // "" cwd = all dirs
Resolve(idPrefix, cwd string, global bool) (*ConvRef, error) // short-id → conv
Title(convID string) (string, error)                         // read the title
SetTitle(convID, title string) error                         // write the title (out-of-band rename)
```

This is where the **"the single conversation file is the whole truth"
assumption** is dropped. Claude Code assembles a `SessionEntry` from one
`.jsonl`; Codex assembles it from a date-indexed rollout file **plus** a SQLite
state DB. Implement `ListConvs`/`Resolve` against *your* harness's full storage
model. `SetTitle` is the out-of-band rename path: a harness with no in-pane
`/rename` (like Codex) renames by writing its title store here instead.

### `HookInstaller` — status hooks

```go
Install() error                                       // write the tclaude callback
Check() (installed bool, missing []string, needsRepair bool)
ConfigTarget() string                                 // path it writes (for messages)
TrustNote() string                                    // one-time trust instructions, or ""
```

Install **surgically and idempotently** — add only tclaude's callback and
preserve the user's existing hooks (byte-preserve unknown fields). `tclaude
setup --harness <name>` dispatches here when `SupportsHooks()` is true. The hook
**callback payload** is already harness-agnostic: tclaude's `HookCallbackInput`
parses Claude Code's and Codex's snake_case stdin field-for-field, so if your
harness's payload follows the same shape, live status and notifications work with
no extra code.

### `SandboxCatalog` / `ApprovalCatalog` — launch-time safety *(optional)*

```go
// SandboxCatalog
DefaultMode() string                   // secure default for daemon-spawned agents
ValidateMode(mode string) (string, error)
Modes() []string

// ApprovalCatalog
DefaultPolicy() string                 // non-escalating default for unattended panes
ValidatePolicy(policy string) (string, error)
```

Leave both `nil` if your harness configures sandboxing/approvals out of band
(like Claude Code via `settings.json`); the spawn path then passes no flag and
rejects an explicit one. Codex implements both as launch flags — see the matrix
on [Harnesses](harnesses.md) (the research lives in the
`tclaude-harness-independence` Linear project).

## Wiring it up

1. Add a `mynewharness.go` (and `mynewharness_*.go`) under `pkg/claude/harness`
   implementing the contracts you need.
2. Register the descriptor from an `init()`:

   ```go
   func init() {
       Register(&Harness{
           Name:        "mynewharness",
           DisplayName: "My New Harness",
           Spawn:       myNewSpawner{},
           Models:      myNewModels{},
           // ...the rest your harness provides; leave the others nil
       })
   }
   ```

   `Register` keys by `Name`; `Resolve`/`ResolveSpawnable` look it up, and
   `--harness mynewharness` then works everywhere. `SpawnBinaries()` picks up the
   new binary automatically, so the hook-callback's process-tree walk recognises
   it without edits.

3. **Persist nothing new.** The `harness` column already defaults to `claude`;
   your harness's conversations record `Name` on spawn and every later operation
   resolves through the descriptor.

4. **Add a simulator + flow tests.** tclaude's test harness (testharness v2)
   pins multi-step coordination through the daemon. Codex's `CodexSim` is the
   model: a sim that owns the harness's real on-disk conversation format, with
   the daemon and all production read paths exercised unchanged. Every new
   capability gets a `pkg/claude/agentd/*_flow_test.go` scenario.

## A note on naming

The codebase predates the seam, so a number of internal identifiers still carry
a `Claude`/`claude`/`TCLAUDE_` prefix (e.g. `buildClaudeCmd`, `FindClaudePID`,
the `TCLAUDE_SESSION_ID` env var) even though the code behind them is now
harness-agnostic. These names are **historical, not Claude-Code-specific** — they
operate on whatever harness a conversation records. They were left as-is
deliberately: a mass rename is high-churn, low-value, and risks a half-finished
state. If you hit a clean, contained rewrite point where renaming one of them
falls out naturally, do it there; a broad rename should be its own focused PR,
not smuggled into a feature change.

## Further reading

- **[Harnesses](harnesses.md)** — the user-facing overview + capability matrix.
- The `tclaude-harness-independence` Linear project — the design intent and
  research pool (coupling inventory, Codex CLI facts, sandbox/approval analysis).
- `pkg/claude/harness/` — the contracts and the `claude` / `codex` implementations.
