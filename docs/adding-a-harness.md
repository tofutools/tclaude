# Adding a harness

This page is for contributors who want to teach tclaude to drive another
coding CLI (Gemini CLI, Aider, an in-house tool, …). The harness seam was
built so this is a recipe, not a rewrite: you implement a handful of small,
focused contracts and register a descriptor. Everything tclaude owns — tmux
sessions, the conversation index, agent coordination, the
[dashboard](dashboard.md) — then works for your harness unchanged. Four
harnesses are registered today: Claude Code, Codex CLI, OpenCode, and Copilot
CLI. Between them they cover every contract below at least once, so for each
one there is a concrete implementation to copy.

!!! note
    `--harness shell` (`session/shell.go`) starts a plain, conversation-less
    interactive shell and is deliberately handled outside this registry — see
    [Sessions](sessions.md). It is not a template for adding a real
    coding-harness integration.

## The shape of the seam

The seam lives in `pkg/claude/harness`. It is deliberately not one monolithic
`Harness` interface. The same user-facing feature is distributed differently
across each harness's internals — a "rename" is one logical idea, but Claude
Code implements it by injecting `/rename` (which writes a title turn into the
conversation file) while Codex writes a row in its own title store. So the
seam models focused, capability-segregated contracts and lets each harness
satisfy each one however its storage and command model dictate.

A small `Harness` descriptor (`pkg/claude/harness/harness.go`) composes the
contracts and exposes capability flags. A `nil` sub-contract means "this
harness lacks that capability": the `Supports*` helpers fold nils into
booleans, and callers gate behavior on them so a pane is never typed a command
it can't parse. An unknown harness name fails closed everywhere.

```go
type Harness struct {
    Name        string // persisted in the DB `harness` column; accepted by --harness
    DisplayName string // human-facing label

    Spawn          Spawner            // in-tmux launch + resume command       (REQUIRED)
    Models         ModelCatalog       // validate/normalize model + effort     (REQUIRED)
    Ask            Asker              // argv for a one-shot `tclaude ask` turn
    Life           Lifecycle          // in-pane slash commands (or unsupported)
    Convs          ConvStore          // conversation metadata from the harness's storage
    Hooks          HookInstaller      // install/check/repair the tclaude callback (+ trust)
    Sandbox        SandboxCatalog     // launch-time sandbox-mode catalog
    TclaudeLayerMode string           // reviewed posture when tclaude-layer is the single OS wall
    BuiltinOSSandbox bool             // true only for a real harness-owned OS sandbox
    Approval       ApprovalCatalog    // launch-time approval policy / permission mode
    AskTimeout     AskTimeoutCatalog  // launch-time AskUserQuestion idle-timeout override
    ToolGovernance ToolGovernanceCatalog // uniform allow/ask/deny over a built-in tool group

    // ...plus plain boolean capability flags (TmuxScrollback,
    // LaunchEnrollment, SeedsFirstTurn, ServerAuthoritative,
    // ApprovalsReviewer, BackgroundShells, …). These tune spawn/liveness
    // behavior rather than adding a contract; each is documented on its
    // field in harness.go.
}
```

## The minimum bar

Only two fields are required. `ResolveSpawnable` — the resolver every spawn
surface goes through (`agent spawn`, group and wave deploys, `--join-group`,
the dashboard's spawn catalog) — rejects a harness that lacks either `Spawn`
or `Models`; such a harness is silently absent from the spawn dialog.
Everything else is nil-able, and callers degrade:

| Nil field        | What stops working / degrades |
|------------------|-------------------------------|
| `Spawn`          | Not spawnable. `ResolveSpawnable` errors; the harness never appears in a spawn UI. |
| `Models`         | Not spawnable. No model/effort validation, so the resolver refuses it. |
| `Ask`            | `tclaude ask` refuses this harness with a clear message. |
| `Life`           | No in-pane control commands: rename falls back to `ConvStore.SetTitle`, soft exit becomes a hard tmux kill, compaction and remote control are unavailable. |
| `Convs`          | No conversation listing/resolve/title (the harness drops out of `conv ls`, search, the dashboard), and no out-of-band rename fallback — a harness with neither `Life.RenameCommand` nor `Convs` cannot be renamed at all. |
| `Hooks`          | `tclaude setup` skips hook install with a message; live status and notifications don't light up. |
| `Sandbox`        | No launch-time `--sandbox`; an explicit `--sandbox` is rejected (the harness is assumed to configure sandboxing out of band). |
| `Approval`       | No launch-time approval/permission flag; an explicit one is rejected. |
| `AskTimeout`     | No AskUserQuestion idle-timeout override; an explicit value is rejected and the dashboard hides the selector. |
| `ToolGovernance` | No uniform built-in-tool allow/ask/deny axis. |

So the smallest useful harness is `{Name, DisplayName, Spawn, Models}` — it
can be launched and resumed, and nothing else. Copilot (`copilot.go`,
`copilot_spawner.go`, `copilot_models.go`) is the worked example of starting
at exactly that minimum, and of why you might deliberately stop there for a
while: its first wave was written from published CLI documentation with no
binary available to record fixtures, so it claimed only the contracts a
documented flag list actually proves — `Spawn`, `Models`, `Life`, plus the
`LaunchEnrollment` capability flag, because `copilot --session-id <uuid>`
proves the conv-id is knowable before the pane starts.

Resist the temptation to fill in the rest from plausible inference: a caller
can detect an absent contract through the `Supports*` helpers and degrade,
but it cannot detect one that is present and wrong. Ship the minimum bar,
then add each further contract in its own fixture-backed slice — which is
what happened here. `copilot.go` has since grown `Ask`, `Convs`, `Hooks`,
`Sandbox`, `Approval`, `ModelTransport`, and `DirTrust`, plus
`TclaudeLayerMode`, `BuiltinOSSandboxAbsenceReason`, and several capability
flags, each added once it could be proved against a real binary. Read the
first wave as the starting point; `copilot.go` itself is the current state.

### Sandbox-related descriptor fields

`Sandbox` and `BuiltinOSSandbox` answer different questions. A harness can
offer meaningful sandbox-mode choices without owning an OS boundary: OpenCode
uses its catalog for `access-control` / `tclaude-layer` / `off`, while its
`BuiltinOSSandbox` stays false because `access-control` is a command filter,
not confinement. Set `BuiltinOSSandbox: true` only when the harness itself
launches under a real OS-enforced sandbox covering its complete action
surface — its own file-editing tools included, not only the shell it spawns;
otherwise an explicit `sandbox_implementation=harness-builtin` is rejected.

When a harness ships something sandbox-shaped that misses that bar, set
`BuiltinOSSandboxAbsenceReason` to the sentence the refusal should state. An
operator who can see the feature in their own CLI reads a flat "has no
built-in OS sandbox" as a gap in tclaude; the reason names the property
actually missing. OpenCode's says its access control is a command filter;
Copilot's says its built-in file edits are checked in-process rather than by
the OS. Leave it empty for a harness with nothing of the kind.

`TclaudeLayerMode` is the separate opt-in for the single-wall
`sandbox_implementation=tclaude-layer` topology (see
[Sandboxing](sandboxing.md)). Set it to the reviewed harness-native launch
posture that must be recorded inside the outer wall (Claude Code uses `off`,
Codex uses `danger-full-access`, OpenCode uses `tclaude-layer`); the value is
validated through the harness's `Sandbox` catalog. Leave it empty to fail
closed with a capability error and a named remedy rather than guessing how a
new harness should launch.

### API-backed harnesses

A harness can be API-backed and still own exactly one pane process. OpenCode
runs a managed server supervised separately from the attached TUI, with its
own lifecycle, handshake, and endpoint discovery. Copilot has the opposite
topology: `copilot --ui-server` runs the interactive TUI and an embedded
JSON-RPC server in one process, so there is no side process to supervise, and
tclaude's existing "harness process under the pane" liveness anchoring,
reaper, and stop-escalation ladder all keep working unchanged. The practical
consequences for a new harness in that single-process shape:

- **The `Spawner` contract is enough — but the launch's first turn leaves the
  argv.** The server is just a flag on the same command line
  (`copilot_spawner.go` renders `--ui-server --host 127.0.0.1 --port <n>`),
  so nothing needs a launch-and-supervise seam. What is easy to miss is that
  the spawner must then suppress `-i`: the drive opens its own session under
  the conversation id after launch, so a prompt delivered on the command line
  would run visibly in the pane and then be discarded. If your harness's API
  opens the session, the launch prompt belongs to the API too.
- **Whoever consumes the API must hold the endpoint before the process
  exists.** agentd allocates the port and passes it down rather than
  discovering it after the fact — Copilot publishes its chosen port only to a
  log line.
- **Connectedness is not a launch record.** "This launch chose the API" is
  durable and decides routing; "a connection exists right now" is derived
  from the live handle. Collapsing the two is how a channel that is merely
  starting up gets treated as a channel that was never asked for.
- **No silent keystroke fallback.** A harness driven over an API opted out of
  the pane-injection sink. When the channel is down, hold and retry the
  delivery; typing it in returns the agent to the sink at the worst possible
  moment. Both API-backed harnesses follow this rule.

`SupportsCopilotAPI` is deliberately named for its harness rather than
generalised into an "API-backed" capability: two examples with genuinely
different process shapes are not enough to know which abstraction is worth
having. Note that a drive can be partial on purpose — Copilot's soft exit
stays on keystrokes because its shutdown RPCs end a session without ending
the process. See [Harnesses](harnesses.md) for the user-facing contract.

## The contracts

Implement as many as your harness needs; leave the rest `nil`. Claude Code
(`claude.go`), Codex (`codex.go`, `codex_*.go`), OpenCode (`opencode.go`,
`opencode_*.go`), and Copilot (`copilot*.go`) are the worked examples — read
them alongside this list.

### `Spawner` — launch and resume *(required to spawn)*

```go
Binary() string                      // executable name, e.g. "codex" (used by the process-tree walk)
BuildCommand(spec SpawnSpec) string  // full shell command run inside the tmux pane
```

`SpawnSpec` carries everything needed to build the command: `EnvExports` (the
identity env prefix), `ResumeID` (empty = fresh; the resume form is
harness-specific — `claude --resume <id>` vs `codex resume <id>`), validated
`Model`/`Effort`, `ExtraArgs`, and the optional `HarnessBuiltinMode` /
`ApprovalPolicy` / `AutoReview` / `BypassHookTrust` knobs. Shell-quote
anything you interpolate. `HarnessBuiltinMode` is your harness's *own*
sandbox setting — never a claim that the process is confined, since a
`tclaude-layer` launch stands the inner sandbox down while tclaude's wall
enforces (see [Sandboxing](sandboxing.md)).

### `ModelCatalog` — model and effort *(required to spawn)*

```go
ValidateModel(s string) (string, error)   // normalize or reject a model token
ValidateEffort(s string) (string, error)  // normalize or reject an effort token
Models() []string                         // valid model values (for the spawn dialog)
EffortLevels() []string                   // valid effort values
```

Reject another harness's slugs with a clear message (Codex rejects `claude-*`
model names) so a mistyped `--harness` surfaces immediately instead of
failing after the pane has launched.

### `Asker` — the `tclaude ask` surface *(optional)*

```go
BuildAskArgv(spec AskSpec) []string   // argv (binary + args) to exec for one ask turn
PreMintsConvID() bool                 // can a FRESH ask pin its conv-id up front?
NoisyCaptureStderr() bool             // does print mode write a verbose transcript to stderr?
```

[`tclaude ask`](ask.md) puts a single foreground, terminal-attached question
to the harness against a per-`(terminal, cwd)` thread. Unlike
`Spawner.BuildCommand` (a shell-command string interpreted by bash in a tmux
pane), an ask is exec'd directly with no shell, so this returns an argv:
`argv[0]` is the binary and the question rides as one already-separated
element, never shell-quoted into a command line. `AskSpec` carries `ResumeID`
xor `SessionID` (continue vs. mint a fresh conv with a caller-chosen id),
validated `Model`/`Effort`, and the `Print`/`Stream` mode bits.

`PreMintsConvID` reports whether a fresh ask can pin its conv-id before the
turn runs (Claude Code's `--session-id`) so the `(terminal,cwd)→conv`
mapping is recorded up front; a harness that only exposes the id after the
first turn (Codex) returns false, and the ask flow discovers the id from
`ConvStore` afterwards. `NoisyCaptureStderr` reports whether print mode
writes a verbose human transcript to stderr, which `tclaude ask` hides
unless `--verbose` or the run fails. `opencode_asker.go` and
`codex_asker.go` are the plain buffered implementations to copy; `claude.go`
additionally implements the streaming refinements.

#### Optional streaming refinements

A buffered `Asker` is enough. Three optional interfaces let a human watching
a TTY see the answer build up live (Claude Code is the only implementor
today):

- **`StreamAsker`** (`Asker` + `StreamFilter(w, smooth, status) io.Writer`) —
  for a harness whose print mode can emit a machine-readable event stream.
  Its two halves are deliberately coupled: `BuildAskArgv` (given
  `AskSpec.Stream`) emits the flags that turn the stream on, and
  `StreamFilter` reads exactly that wire format back, forwarding only the
  assistant's clean incremental visible text (no JSON, reasoning, or tool
  chatter) to `w`. `tclaude ask` gates on `SupportsAskStream` and falls back
  to the buffered path otherwise.
- **`StreamStatus`** (`BeforeOutput()`) — an optional sink for the transient
  "working…" spinner. The filter announces each visible write; the renderer
  decides from that timing when to show or hide itself. Must be safe for
  concurrent use. `nil` disables the indicator.
- **`AskStreamFlusher`** (`Flush() error`) — the optional flush half of the
  writer `StreamFilter` returns. `tclaude ask` type-asserts for it and calls
  `Flush` exactly once after the process exits, so the filter can surface a
  buffered final answer or error and end the line cleanly.

### `Lifecycle` — in-pane control commands *(optional)*

```go
RenameCommand() string         // e.g. "/rename"; "" = no in-pane rename
CompactCommand() string        // e.g. "/compact"; "" = no in-pane compaction
SoftExitCommand() string       // e.g. "/exit" / "/quit"; "" = hard-kill the pane instead
RemoteControlCommand() string  // e.g. "/remote-control"; "" = no built-in remote access
```

Return `""` for anything your harness lacks. These tokens must be
compile-time constants — they are typed into a tmux pane, which is a
keystroke-injection sink; never interpolate user input into them. tclaude
gates every slash injection on the matching `Supports*` flag, and where an
in-pane command is missing it degrades: a missing rename falls back to
`ConvStore.SetTitle`, a missing soft exit becomes a hard kill, and a missing
compaction or remote-control toggle simply has its affordance hidden (those
have no out-of-band fallback).

`RemoteControlCommand` names the harness's built-in remote-access toggle
(Claude Code's `/remote-control`; see [Remote](remote.md)). It is one command
that flips the feature either way — the harness exposes no readback, so
callers track the intended direction themselves. Codex and OpenCode return
`""`; OpenCode's `serve --hostname` / `opencode web` is a local HTTP surface,
not a hosted relay, and must not be conflated with this.

A server-authoritative harness (one whose conversation lives in a
daemon-owned side server, `ServerAuthoritative: true`) still names its
lifecycle tokens here, but tclaude does not send them as pane keystrokes.
OpenCode returns `/compact` and `/exit`; the daemon uses those as the
capability selector and dispatches the equivalent managed command
(`session.compact` / `app.exit`) through the authenticated server API instead
of `send-keys`. The token strings double as the switch key for that
translation, so they must stay constant and in sync with the dispatch
mapping.

The same holds per launch rather than per harness for Copilot's opt-in API
drive: an agent launched with `--copilot-api` has compaction dispatched as
`session.history.compact` keyed off the same `CompactCommand()` token, while
an agent on the default send-keys drive is typed the literal `/compact`. Two
properties follow, and both are load-bearing: the dispatch is a fixed switch
on compile-time tokens, never a command interpreter — an unmapped token fails
closed rather than reaching for keystrokes — and soft exit stays on
keystrokes for that harness even when the channel is up. A partial drive is a
legitimate outcome; do not assume every token your harness names must move at
once.

### `ConvStore` — conversation metadata *(optional)*

```go
ListConvs(cwd string) ([]convops.SessionEntry, error)        // "" cwd = all dirs
Resolve(idPrefix, cwd string, global bool) (*ConvRef, error) // short-id → conv
Title(convID string) (string, error)                         // read the title
SetTitle(convID, title string) error                         // write the title (out-of-band rename)
Exists(convID, cwd string) (bool, error)                     // is the conv still present?
```

This is where the "one conversation file is the whole truth" assumption is
dropped. Claude Code assembles a `SessionEntry` from one `.jsonl`; Codex
assembles it from a date-indexed rollout file plus a SQLite state DB.
Implement `ListConvs`/`Resolve` against *your* harness's full storage model.
`SetTitle` is the out-of-band rename path: a harness with no in-pane
`/rename` (Codex, OpenCode) renames by writing its title store here.

`Exists` reports whether a conv-id is still present. `tclaude ask` uses it to
self-heal a stale `(terminal,cwd)→conv` mapping — a recorded thread whose
conversation has vanished starts fresh instead of resuming a ghost. Its three
outcomes mirror `Resolve`: `(true, nil)` present, `(false, nil)` confirmed
absent, `(false, err)` the store couldn't be read (the caller keeps the
thread rather than nuking it on a transient error). `cwd` locates a
cwd-scoped store (Claude Code's per-project `.jsonl`); a globally-indexed
store (Codex) ignores it.

### `HookInstaller` — status hooks *(optional)*

```go
Install() error                                       // write the tclaude callback
Check() (installed bool, missing []string, needsRepair bool)
ConfigTarget() string                                 // path it writes (for messages)
TrustNote() string                                    // one-time trust instructions, or ""
```

Install surgically and idempotently — add only tclaude's callback and
preserve the user's existing hooks (byte-preserve unknown fields). `tclaude
setup --harness <name>` dispatches here when `SupportsHooks()` is true. The
hook callback payload is already harness-agnostic: tclaude's
`HookCallbackInput` parses Claude Code's and Codex's snake_case stdin
field-for-field, so if your harness's payload follows the same shape, live
status and notifications work with no extra code.

#### `TrustedHookInstaller` — auto-trust *(optional refinement)*

```go
TrustedHookInstaller interface {
    HookInstaller
    AutoTrustSupported() (bool, string) // can this environment attempt authoritative trust discovery?
    InstallTrusted() error              // install, discover identity, then persist trust
    TrustInstalled() error              // trust the already-installed declarations
    Trusted() bool                      // do installed declarations match current trust?
}
```

Some harnesses gate command hooks behind a separate executable-trust store,
so installing the declaration is not enough — it must also be trusted before
it runs. Codex's `codex_hook_trust.go` is the worked example. Setup invokes
the trusted path only when the operator explicitly selects that harness:
finding another harness on `PATH` is enough to install its declarations,
never to grant it execution trust. When authoritative identity is available
only for installed declarations, `InstallTrusted` may install first, discover
identity, then persist trust — but it must roll the declaration back on any
discovery or trust failure so it cannot leave an invisible startup review
gate. Leave it unimplemented and hooks still install; they just won't carry
auto-trust.

### The catalogs — launch-time safety *(optional)*

`SandboxCatalog`, `ApprovalCatalog`, and `AskTimeoutCatalog` share the same
four-method shape (name a default, validate, list, describe), so the
dashboard, CLI, and profile editor drive their selectors off any of them
uniformly:

```go
// SandboxCatalog — Codex's --sandbox; OpenCode's soft access-control modes.
DefaultMode() string                   // secure default for daemon-spawned agents
ValidateMode(mode string) (string, error)
Modes() []string                       // selectable modes, least→most permissive
ModeHelp(mode string) string           // one-line description per mode; "" if unknown

// ApprovalCatalog — Codex's --ask-for-approval; Claude Code's --permission-mode.
DefaultPolicy() string                 // non-escalating default for unattended panes
ValidatePolicy(policy string) (string, error)
Modes() []string                       // selectable policies (drives the spawn dialog)
ModeHelp(policy string) string         // one-line description per policy; "" if unknown

// AskTimeoutCatalog — Claude Code's askUserQuestionTimeout, via --settings.
DefaultMode() string                   // "inherit" (ValidateMode normalizes it to "")
ValidateMode(mode string) (string, error)
Modes() []string                       // selectable values (inherit first)
ModeHelp(mode string) string           // one-line description per value; "" if unknown
```

`Modes` and `ModeHelp` matter because the same set must drive both validation
and every authoring UI, so the CLI, profiles, and dashboard can't drift; the
dashboard's catalog builder calls `Modes()` then `ModeHelp(m)` for each value.
Keep the help copy beside the values it describes.

Leave any of the three `nil` if your harness configures that axis out of band
(Claude Code's sandbox lives in `settings.json`; Codex has no AskUserQuestion
dialog, so it leaves `AskTimeout` nil); the spawn path then passes no flag
and rejects an explicit value. See the capability matrix on
[Harnesses](harnesses.md).

!!! warning "Adding a catalog is not self-contained"
    Approval postures are also compared across a spawn lineage (can this
    parent mint a child with *this* posture?), and that comparison lives in a
    name-keyed switch outside the seam: `classifyParentApprovalLineage` /
    `classifyChildApprovalLineage` in `approval_lineage.go` switch on the
    harness name and fall through to an **invalid** posture for an
    unclassified harness — so a new harness with an `Approval` catalog that
    isn't added there fails closed, and its agents can neither spawn children
    nor be spawned as one. `ApprovalLineageDenialHint` and
    `SpawnSandboxWarnings` (`sandbox.go`) are two more name-keyed switches
    you must extend so the harness gets correct denial hints and the right
    "your sandbox is weaker than it looks" warning. Grep for
    `normalizeLineageHarness` to find them all.

## Wiring it up

1. Add a `mynewharness.go` (and `mynewharness_*.go`) under
   `pkg/claude/harness` implementing the contracts you need.
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
   `--harness mynewharness` then works everywhere. `SpawnBinaries()` picks up
   the new binary automatically, so the hook callback's process-tree walk
   recognises it without edits.

3. **Persist nothing new.** The `harness` column already defaults to
   `claude`; your harness's conversations record `Name` on spawn, and every
   later operation resolves through the descriptor.

4. **Add a simulator and flow tests.** tclaude's test harness pins multi-step
   coordination through the daemon. Codex's `CodexSim` is the model: a sim
   that owns the harness's real on-disk conversation format, with the daemon
   and all production read paths exercised unchanged. Every new capability
   gets a `pkg/claude/agentd/*_flow_test.go` scenario.

## What has no seam yet

A few features are still wired to specific harnesses by hand and do not have
a descriptor seam — a new harness inherits nothing for them, and adding
support means editing those call sites directly (and ideally lifting them
into a contract):

- **Usage / cost.** There is no `Cost`/`Usage` field on `Harness`. Usage is
  read by harness-specific code the daemon calls directly (Codex's
  `codex_usage.go`, surfaced through `agentd/usage.go` and a Codex-specific
  DB cache row). Cost works differently: `agentd/costs.go` is a generic
  aggregator over the harness-agnostic `session_cost_daily` table, so each
  harness must get its own numbers into that table — Codex folds its virtual
  cost from appended rollout records in agentd's durable telemetry follower
  (`codex_telemetry_follower.go`, using `codex_cost.go`). Either way, a new
  harness's usage and cost won't appear until similar harness-specific code
  exists.
- **Statusline install.** `harness.go` names a future `StatuslineInstaller`
  seam that isn't factored out yet. Claude Code installs a command-backed
  renderer; Codex curates its native footer items
  (`statusbar/codex_install.go`). A new harness needs its own install path
  added there.
- **MCP registration.** Registering tclaude's MCP/plugin surface lives in
  `agentd/plugins.go`, not behind a harness contract.

If your harness needs one of these, prefer promoting it to a real contract in
a focused PR over bolting on another name-keyed branch.

## A note on naming

The codebase predates the seam, so many internal identifiers still carry a
`Claude`/`claude`/`TCLAUDE_` prefix (`buildClaudeCmd`, `FindClaudePID`, the
`TCLAUDE_SESSION_ID` env var) even though the code behind them is
harness-agnostic. These names are historical, not Claude-Code-specific — they
operate on whatever harness a conversation records (see
[History](history.md)). A mass rename is high-churn and low-value; if you hit
a clean, contained rewrite point where renaming one falls out naturally, do
it there, and keep a broad rename in its own focused PR.

## Further reading

- **[Harnesses](harnesses.md)** — the user-facing overview and capability
  matrix.
- **[Architecture](architecture.md)** — where the seam sits in the system.
- `pkg/claude/harness/` — the contracts and the `claude` / `codex` /
  `opencode` / `copilot` implementations.
- `pkg/claude/copilotapi/` — the JSON-RPC client for Copilot's embedded
  server, if you are adding a harness with a similar single-process API
  shape.
