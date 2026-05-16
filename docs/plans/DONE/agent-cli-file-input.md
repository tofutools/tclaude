# Agent-CLI file-input flag — shipped 2026-05

Several `tclaude agent` subcommands take a free-text body on the command
line. Passing a non-trivial body inline is awkward and fragile: shell
quoting, length, and — most sharply — **backticks, which the shell eats
as a command substitution before tclaude ever sees the string**.

Every such command now accepts `--file <path>` (short `-f`) to read the
body from a file instead. A path of `-` reads stdin, so a body can be
piped in. The file content is used exactly as the inline value would be.

## CLI surface

| Command | Inline source | New file flag |
|---------|---------------|---------------|
| `tclaude agent spawn` | `--initial-message` / `-m` | `--file` / `-f` |
| `tclaude agent reincarnate` | positional follow-up | `--file` / `-f` |
| `tclaude agent clone` | positional follow-up | `--file` / `-f` |
| `tclaude agent cron add` | `--body` | `--file` / `-f` |
| `tclaude agent message` | positional body | `--file` / `-f` *(pre-existing)* |
| `tclaude agent reply` | positional body | `--file` / `-f` *(pre-existing)* |
| `tclaude agent groups set-context` | positional context | `--file` / `-f` *(pre-existing)* |

`message`, `reply`, and `groups set-context` already had `--file`; this
change added the flag to `spawn`, `reincarnate`, `clone`, and `cron add`,
and made `--file -` (read stdin) work uniformly across all of them.

**Flag name:** `--file` / `-f` was chosen over the human's tentative
`--context-from-file` because three commands already used `--file` —
it is the established, discoverable convention. One scheme everywhere.

## Semantics

- **Mutually exclusive.** Passing both the inline value and `--file`
  fails with `Error: pass <inline> OR --file, not both` (rcInvalidArg).
  Any non-empty inline value counts as "given", whitespace included.
- **`-` means stdin.** `--file -` reads the body from stdin.
- **Missing / unreadable file** fails with a clear, file-named error
  (rcIOFailure) *before* anything is spawned or sent.
- **Length cap respected.** `spawn` / `reincarnate` / `clone` route the
  resolved body through the existing `isValidInitialMessage`
  (16384-byte cap), so the file path enforces the same cap with the
  same error — `--file` is not a way to smuggle an oversize brief past
  the limit.
- **No behaviour change when neither is given.**

## Implementation

- `pkg/claude/agent/bodyinput.go` — `resolveBodyInput(inline, file,
  inlineName, stdin, stderr)` is the shared resolver: mutual exclusion,
  `-`/stdin, missing-file error. Used by `spawn`, `reincarnate`,
  `clone`, `cron add`, and `groups set-context` (the last refactored
  off its bespoke copy).
- `message` / `reply` keep their own three-way `readBody` (it also has
  the older explicit `--stdin` bool) but gained `--file -` stdin
  support so the dash convention is universal.
- `RunSpawn`, `runReincarnate`, `runClone`, `runCronAdd`, and
  `runGroupsSetContext` gained a `stdin io.Reader` parameter.
- No daemon changes, no DB migration — the CLI resolves the body
  client-side and sends the same request payload as before.

## Tests

- `pkg/claude/agent/bodyinput_test.go` — the resolver: passthrough,
  file read (verbatim, backticks/newlines), `-`/stdin, mutual
  exclusion, missing file.
- `pkg/claude/agent/spawn_test.go` — `RunSpawn` rejects an oversize
  `--file` brief (cap enforced on the file path), the mutual-exclusion
  error, and a missing file — all before the daemon is contacted.
- `pkg/claude/agentd/spawn_cli_flow_test.go` —
  `TestSpawnCLI_InitialMessageFromFile` and
  `…FromStdin` spawn end-to-end through the daemon mux and assert the
  file/stdin-sourced brief reaches the new agent's inbox verbatim.

## Skills

`agent-coord`, `agent-lifecycle`, and `agent-schedule` SKILL.md files
now document `--file` and recommend it for long / multi-line / code-
heavy bodies, explicitly tied to the backtick gotcha — a body loaded
from a file is read verbatim, immune to shell re-interpretation.

## Source files

- `pkg/claude/agent/bodyinput.go` (new), `bodyinput_test.go` (new),
  `spawn_test.go` (new)
- `pkg/claude/agent/spawn.go`, `reincarnate.go`, `clone.go`, `cron.go`,
  `message.go`, `reply.go`, `groups.go`
- `pkg/claude/agentd/spawn_cli_flow_test.go`
- `pkg/claude/agent/skills/{agent-coord,agent-lifecycle,agent-schedule}/SKILL.md`
