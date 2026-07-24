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

    Spawn          Spawner            // build the in-tmux launch + resume command      (REQUIRED)
    Models         ModelCatalog       // validate/normalize model + effort               (REQUIRED)
    Ask            Asker              // build the argv for a one-shot `tclaude ask` turn
    Life           Lifecycle          // name the in-pane slash commands (or report unsupported)
    Convs          ConvStore          // assemble conversation metadata from the harness's storage
    Hooks          HookInstaller      // install/check/repair the tclaude callback (+ trust)
    Sandbox        SandboxCatalog     // launch-time OS-sandbox modes
    Approval       ApprovalCatalog    // launch-time approval policy / permission mode
    AskTimeout     AskTimeoutCatalog  // launch-time AskUserQuestion idle-timeout override
    ToolGovernance ToolGovernanceCatalog // uniform allow/ask/deny over a built-in tool group

    // ...plus a set of plain boolean capability flags (TmuxScrollback,
    // LaunchEnrollment, SeedsFirstTurn, ServerAuthoritative, ApprovalsReviewer,
    // BackgroundShells, …). Those tune spawn/liveness behavior rather than adding
    // a contract to implement; each is documented on the field in harness.go.
}
```

## The minimum bar

Only two fields are actually required. `ResolveSpawnable` — the resolver every
spawn surface goes through (`agent spawn`, group/wave deploys, `--join-group`,
the dashboard's `buildHarnessCatalog`) — rejects a harness that lacks **either**
a `Spawn` (`Spawner`) **or** a `Models` (`ModelCatalog`); such a harness is
silently absent from the spawn dialog. Everything else is nil-able: the
`Supports*` helpers fold a nil sub-contract to `false` and callers degrade.

So the smallest useful harness is `{Name, DisplayName, Spawn, Models}` — it can
be launched and resumed, but nothing else. Here is what each nil field costs:

| Nil field        | What stops working / degrades |
|------------------|-------------------------------|
| `Spawn`          | **Not spawnable.** `ResolveSpawnable` errors; the harness never appears in a spawn UI. |
| `Models`         | **Not spawnable.** Same — no model/effort validation, so the resolver refuses it. |
| `Ask`            | `tclaude ask` refuses this harness with a clear message (`SupportsAsk` is false). |
| `Life`           | No in-pane control commands: rename falls back to `ConvStore.SetTitle`, soft-exit becomes a hard tmux kill, and compaction / remote-control are simply unavailable (`Supports{Compact,RemoteControl}` false). |
| `Convs`          | No conversation listing / resolve / title from this harness (it drops out of `conv ls`, search, the dashboard), **and** no out-of-band rename fallback — so a harness with neither `Life.RenameCommand` nor `Convs` cannot be renamed at all. |
| `Hooks`          | `tclaude setup` skips hook install with a message; live status and notifications don't light up. |
| `Sandbox`        | No launch-time `--sandbox`; an explicit `--sandbox` is rejected (the harness is assumed to configure sandboxing out of band). |
| `Approval`       | No launch-time approval/permission flag; an explicit one is rejected. |
| `AskTimeout`     | No AskUserQuestion idle-timeout override; an explicit value is rejected and the dashboard hides the selector. |
| `ToolGovernance` | No uniform built-in-tool allow/ask/deny axis (`SupportsToolGovernance` false). |

## The contracts

Implement as many as your harness needs; leave the rest `nil`. Claude Code
(`claude.go`), Codex (`codex.go`, `codex_*.go`) and OpenCode (`opencode.go`,
`opencode_*.go`) are the worked examples — read them alongside this list. Between
them they cover every contract below at least once, so for each one there is a
concrete implementation to copy.

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

### `Asker` — the `tclaude ask` surface *(optional)*

```go
BuildAskArgv(spec AskSpec) []string   // argv (binary + args) to exec for one ask turn
PreMintsConvID() bool                 // can a FRESH ask pin its conv-id up front?
NoisyCaptureStderr() bool             // does print mode write a verbose transcript to stderr?
```

`tclaude ask` puts a single foreground, terminal-attached question to the
harness against a per-`(terminal, cwd)` thread. Unlike `Spawner.BuildCommand`
(which returns a `sh -c` **string** for a tmux pane), an ask is exec'd directly
with **no shell**, so this returns an **argv** (`[]string`): `argv[0]` is the
binary and the question rides as one already-separated element, never
shell-quoted into a command line. `AskSpec` carries `ResumeID` **xor**
`SessionID` (continue vs. mint a fresh conv with a caller-chosen id), validated
`Model`/`Effort`, and the `Print`/`Stream` mode bits.

`PreMintsConvID` reports whether a fresh ask can pin its conv-id before the turn
runs (Claude Code's `--session-id`) so the `(terminal,cwd)→conv` mapping is
recorded up front; a harness that only exposes the id after the first turn
(Codex) returns false and the ask flow discovers the id from `ConvStore`
afterwards. `NoisyCaptureStderr` reports whether print mode writes a verbose
human transcript to stderr (which `tclaude ask` then hides unless `--verbose` or
the run fails). Leave `Harness.Ask` nil and callers gate on `SupportsAsk` and
fail with a clear message. `opencode_asker.go` and `codex_asker.go` are the
plain buffered implementations to copy; `claude.go` additionally implements the
streaming refinements below.

#### Optional streaming refinements

A buffered `Asker` is enough. Three optional interfaces let a human watching a
TTY see the answer build up live instead of appearing all at once (Claude Code
in `claude.go` is the only one that implements them today):

- **`StreamAsker`** (`Asker` + `StreamFilter(w, smooth, status) io.Writer`) — for
  a harness whose print mode can emit a machine-readable **event stream**. Its
  two halves are deliberately coupled: `BuildAskArgv` (given `AskSpec.Stream`)
  emits the flags that turn the stream on, and `StreamFilter` knows how to read
  exactly that wire format back, forwarding only the assistant's clean,
  incremental visible text (no JSON, reasoning, or tool chatter) to `w`.
  `tclaude ask` gates on `SupportsAskStream` and falls back to the plain
  buffered path otherwise.
- **`StreamStatus`** (`BeforeOutput()`) — an optional sink for the transient
  "working…" spinner. The filter announces each visible write; the renderer
  decides from that timing when to show/hide itself. Must be safe for concurrent
  use (the filter calls it from its parse and pacing goroutines). `nil` disables
  the indicator.
- **`AskStreamFlusher`** (`Flush() error`) — the optional flush half of the
  writer `StreamFilter` returns. `tclaude ask` type-asserts for it and calls
  `Flush` exactly once after the process exits, so the filter can surface any
  buffered final answer/error and end the line cleanly.

### `Lifecycle` — in-pane control commands

```go
RenameCommand() string         // e.g. "/rename"; "" = no in-pane rename
CompactCommand() string        // e.g. "/compact"; "" = no in-pane compaction
SoftExitCommand() string       // e.g. "/exit" / "/quit"; "" = hard-kill the pane instead
RemoteControlCommand() string  // e.g. "/remote-control"; "" = no built-in remote access
```

The interface has **four** methods. Return `""` for anything your harness
lacks. These tokens **must be compile-time
constants** — they are typed into a tmux pane, which is a keystroke-injection
sink; never interpolate user input into them. tclaude gates every slash
injection on the matching `Supports*` flag, and where an in-pane command is
missing it degrades (e.g. a missing rename command falls back to
`ConvStore.SetTitle`; a missing soft-exit becomes a hard kill; a missing
compaction or remote-control toggle simply has its affordance hidden — those
have no out-of-band fallback).

`RemoteControlCommand` names the harness's built-in remote-access **toggle**
(Claude Code's `/remote-control`, which exposes the session to claude.ai/code +
the mobile app). It is one command that turns the feature on when off and off
when on — the harness exposes no readback, so callers track the intended
direction themselves. There is no out-of-band fallback: `""` both hides the
affordance and makes the daemon's toggle endpoint refuse (Codex and OpenCode
return `""`; OpenCode's `serve --hostname` / `opencode web` is a local HTTP
surface, not a hosted relay, and must not be conflated with this).

A **server-authoritative** harness (one whose conversation lives in a
daemon-owned side server, `ServerAuthoritative: true`) still names its lifecycle
tokens here, but tclaude does not send them as pane keystrokes. OpenCode returns
`/compact` and `/exit`; the daemon uses those as the capability selector and
dispatches the equivalent managed command (`session.compact` / `app.exit`)
through the authenticated server API instead of `send-keys`. The token strings
therefore double as the switch key for that translation, so they must stay
constant and in sync with the dispatch mapping.

### `ConvStore` — conversation metadata

```go
ListConvs(cwd string) ([]convops.SessionEntry, error)        // "" cwd = all dirs
Resolve(idPrefix, cwd string, global bool) (*ConvRef, error) // short-id → conv
Title(convID string) (string, error)                         // read the title
SetTitle(convID, title string) error                         // write the title (out-of-band rename)
Exists(convID, cwd string) (bool, error)                     // is the conv still present?
```

This is where the **"the single conversation file is the whole truth"
assumption** is dropped. Claude Code assembles a `SessionEntry` from one
`.jsonl`; Codex assembles it from a date-indexed rollout file **plus** a SQLite
state DB. Implement `ListConvs`/`Resolve` against *your* harness's full storage
model. `SetTitle` is the out-of-band rename path: a harness with no in-pane
`/rename` (like Codex or OpenCode) renames by writing its title store here
instead.

`Exists` reports whether a conv-id is still present in the store. `tclaude ask`
uses it to self-heal a stale `(terminal,cwd)→conv` mapping — a recorded thread
whose conversation has vanished (a turn that died before the harness persisted
it, or one the user deleted) starts fresh instead of resuming a ghost. Its three
outcomes mirror `Resolve`: `(true, nil)` present, `(false, nil)` confirmed
absent, `(false, err)` the store couldn't be read (the caller keeps the thread
rather than nuking it on a transient error). `cwd` locates a cwd-scoped store
(Claude Code's per-project `.jsonl`); a globally-indexed store (Codex) ignores
it.

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

#### `TrustedHookInstaller` — auto-trust *(optional refinement)*

```go
TrustedHookInstaller interface {
    HookInstaller
    AutoTrustSupported() (bool, string)  // does this build know the harness's trust-key contract?
    InstallTrusted() error               // persist trust FIRST, then install declarations
    TrustInstalled() error               // trust the already-installed declarations
    Trusted() bool                       // do installed declarations match current trust?
}
```

Some harnesses gate command hooks behind a **separate executable-trust store**,
so installing the declaration is not enough — it must also be trusted before it
will run. A harness like that implements this optional extension (Codex's
`codex_hook_trust.go` is the worked example). Setup invokes the trusted path only
when the operator **explicitly selects that harness**: merely finding another
harness on `PATH` is enough to install its declarations, but never to grant it
execution trust. `InstallTrusted` deliberately persists trust *before* installing
the matching declarations — a stale trust record without a declaration is inert,
whereas the reverse order can leave a startup review gate. Leave it unimplemented
and hooks still install; they just won't carry auto-trust.

### `SandboxCatalog` / `ApprovalCatalog` / `AskTimeoutCatalog` — launch-time safety *(optional)*

The three catalogs share the same shape (name a default, list/validate/describe
the selectable values) so the dashboard, CLI, and profile editor drive their
`<select>` off any of them uniformly. Each lists **four** methods:

```go
// SandboxCatalog — Codex's --sandbox; OpenCode's soft access-control modes.
DefaultMode() string                   // secure default for daemon-spawned agents
ValidateMode(mode string) (string, error)
Modes() []string                       // selectable modes, least→most permissive
ModeHelp(mode string) string           // one-line description per mode (e.g. socket reachability); "" if unknown

// ApprovalCatalog — Codex's --ask-for-approval; Claude Code's --permission-mode.
DefaultPolicy() string                 // non-escalating default for unattended panes
ValidatePolicy(policy string) (string, error)
Modes() []string                       // selectable policies (drives the spawn dialog's approval <select>)
ModeHelp(policy string) string         // one-line description per policy (e.g. "safe unattended?"); "" if unknown

// AskTimeoutCatalog — Claude Code's askUserQuestionTimeout, via --settings.
DefaultMode() string                   // "inherit" (ValidateMode normalizes it to "") — an un-chosen spawn keeps the operator's setting
ValidateMode(mode string) (string, error)
Modes() []string                       // selectable values (inherit first)
ModeHelp(mode string) string           // one-line description per value; "" if unknown
```

`Modes` and `ModeHelp` matter because the same set must drive **both**
validation and every authoring UI, so the CLI, profiles, and dashboard can't
drift; `buildHarnessCatalog` calls `Modes()` then `ModeHelp(m)` for each to build
the spawn dialog. Keep the help copy beside the values it describes.

Leave any of the three `nil` if your harness configures that axis out of band
(Claude Code's sandbox lives in `settings.json`; Codex has no AskUserQuestion
dialog, so it leaves `AskTimeout` nil); the spawn path then passes no flag and
rejects an explicit value. Codex implements sandbox + approval as launch flags —
see the matrix on [Harnesses](harnesses.md) (the research lives in the
`tclaude-harness-independence` Linear project).

> **Adding a `SandboxCatalog` or `ApprovalCatalog` is not self-contained.**
> Approval postures are also compared across a spawn lineage (can this parent
> mint a child with *this* posture?), and that comparison lives in a
> **name-keyed switch outside the seam**: `classifyParentApprovalLineage` /
> `classifyChildApprovalLineage` in `approval_lineage.go` switch on the harness
> name and fall through to an **invalid** posture for an unclassified harness —
> so a new harness with an `Approval` catalog that isn't added there **fails
> closed**, and its agents can neither spawn children nor be spawned as one.
> `ApprovalLineageDenialHint` and `SpawnSandboxWarnings` (`sandbox.go`) are two
> more name-keyed switches you must extend so the harness gets correct denial
> hints and the right "your sandbox is weaker than it looks" warning. Grep for
> `normalizeLineageHarness` to find them all.

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

## What has no seam yet

The recipe above covers everything that is a contract today. A few features are
still wired to Claude Code / Codex by hand and do **not** yet have a descriptor
seam — a new harness inherits nothing for them, and adding support means editing
those call sites directly (and ideally lifting them into a contract):

- **Usage / cost.** There is no `Cost`/`Usage` field on `Harness`. Usage is read
  by harness-specific code the daemon calls directly (Codex's `codex_usage.go`,
  surfaced through `agentd/usage.go` and a Codex-specific DB cache row). Cost
  works differently: `agentd/costs.go` is a *generic* aggregator over the
  harness-agnostic `session_cost_daily` table, so each harness must get its own
  numbers into that table — Codex computes its virtual cost inside its hook
  projection (`codex_projection.go`, using `codex_cost.go`). Either way there is
  no descriptor seam, so a new harness's usage/cost won't appear until similar
  harness-specific code exists.
- **Statusline install.** `harness.go` names a *future* `StatuslineInstaller`
  seam that isn't factored out yet. Claude Code installs a command-backed
  renderer; Codex curates its native footer items (`statusbar/codex_install.go`).
  A new harness needs its own install path added there.
- **MCP registration.** Registering tclaude's MCP/plugin surface lives in
  `agentd/plugins.go`, not behind a harness contract.

If your harness needs one of these, prefer promoting it to a real contract in a
focused PR over bolting on another name-keyed branch.

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
- `pkg/claude/harness/` — the contracts and the `claude` / `codex` / `opencode`
  implementations.
