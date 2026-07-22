# tclaude agent instructions

`AGENTS.md` is a symlink to this file. Keep this file short, durable, and
useful as startup context for coding agents. Do not use it as a changelog,
implementation diary, roadmap, or project-management scratchpad.

## What tclaude is

`tclaude` is a Go CLI that wraps agentic coding harnesses in tmux and adds
session management, conversation search, usage/status reporting, worktree
helpers, and multi-agent coordination.

The project is harness-agnostic. Claude Code is the default harness and OpenAI
Codex CLI is also supported via `--harness claude|codex`; the selected harness
is persisted per conversation. The harness seam lives in `pkg/claude/harness`.

Many identifiers still contain historical `Claude`/`claude`/`TCLAUDE_` names
even when the code is now harness-agnostic. Treat those names as historical,
not Claude-Code-only. Do not opportunistically mass-rename them; only rename at
a clean, contained rewrite point.

## Build and test

Common commands:

```bash
go build ./...
go test ./...
go test ./pkg/claude/conv/...
golangci-lint run ./...
go install .
```

CI runs `go test ./...` and `golangci-lint run ./...`. Prefer pushing PRs
early and letting CI do that work; do not duplicate the full test/lint suite
locally before pushing. Do make sure your changes at least build, and run
focused local tests when they help your own iteration on the code you are
changing.

Platform target: Linux and macOS. WSL is treated as Linux for practical use.
Native Windows is not a supported development target; do not design new
features around native Windows behavior unless the operator explicitly asks.

## Where to look

- Entry point: `main.go`, which calls `pkg/claude.Cmd()`.
- Root command wiring: `pkg/claude/claude.go`.
- Harness design and capability matrix: `docs/harnesses.md`.
- Adding another harness: `docs/adding-a-harness.md`.
- Agent coordination: `docs/agent.md`.
- Dashboard: `docs/dashboard.md`.
- Sessions, conversations, worktrees, tasks, status bar, notifications:
  corresponding files under `docs/`.
- Flow-test helpers and simulators: `pkg/testharness/`.
- Contributing and flow-test style: `CONTRIBUTING.md`.

Avoid maintaining exhaustive package inventories here. They drift quickly; use
the code tree and focused docs as the source of truth.

## Architecture guardrails

- Commands use Cobra through Boa (`boa.CmdT[...]`) with
  `common.DefaultParamEnricher()` unless nearby code establishes another
  pattern.
- Session and conversation state lives in SQLite under
  `~/.tclaude/data/db.sqlite`. The sibling `~/.tclaude/api/` tree is reserved
  for the agent-reachable daemon socket; private daemon state stays under
  `~/.tclaude/data/`.
  Legacy JSON files may still be written for compatibility, but SQLite is the
  source of truth for tclaude.
- Harness support is capability-based. Callers should gate behavior on the
  harness descriptor (`Supports*` / `Can*`) and degrade gracefully when a
  contract is absent.
- In-pane slash-command delivery via tmux `send-keys` is an injection sink.
  Lifecycle command tokens must be compile-time constants from the harness
  lifecycle, never interpolated user input. User-controlled titles or text sent
  toward these paths must pass the existing charset/length gates.
- Platform-specific code should use Go build tags such as `_linux.go`,
  `_darwin.go`, and `_unix.go`. Treat old `_windows.go` files as vestigial
  unless the task explicitly concerns them.

## Testing guidance

Unit tests live next to the code they cover. Flow tests live under
`pkg/claude/agentd/*_flow_test.go` and run under plain `go test ./...`.

Flow tests exercise production paths through the daemon HTTP mux. Only the
external subprocess boundaries are swapped:

- `clcommon.Default` for tmux.
- `agentd.Spawn` for `tclaude session new`.
- `agentd.runPluginShell` for plugin shell execution when needed.

Keep new tests focused on user-visible surfaces such as CLI/API results,
conversation listings, and dashboard snapshots. Avoid asserting on simulator
internals when a production read path can be exercised instead.

For manual dashboard visual smoke, first check that Linux-side Chrome/Chromium
exists, then run:

```bash
TCLAUDE_DASHSNAP=1 TCLAUDE_DASHSNAP_SHARD=1/4 go test ./pkg/claude/agentd/ -run TestDashSnap -v -count=1 -timeout 600s
```

Run shards `1/4` through `4/4` to cover the full matrix (each takes a few
minutes; an unsharded full run needs `-timeout 1800s`). See "Visual smoke
testing" in `docs/dashboard.md`.

The visual smoke harness is optional and environment-dependent; it is not wired
into CI.

## Git, commits, and PRs

When making feature or fix changes as an agent, use a git worktree and open a
PR unless the operator gives different instructions. It is fine to force-push a
feature branch; never force-push `main`.

Do not include remote-access/session links in commits, PR descriptions, or PR
comments. In particular, do not add `Claude-Session:` trailers or
`https://claude.ai/code/...` URLs. A plain `Co-Authored-By` trailer is fine.

PR descriptions should start with a short `Background / Purpose` section that
explains why the PR exists. Then summarize the implementation and list tests or
verification.

Every PR needs a real cold review, but the review must not delay the PR
itself. Open the PR first, then trigger the cold review against it. When
presenting a PR to the operator while the review is still pending, say so.
Apply fixes once the review completes: push them directly to the PR if it is
still open; if the operator chose to merge early, address findings in a
follow-up PR, and escalate anything serious (such as a security regression)
to the operator directly rather than only filing the follow-up.
CodeRabbit is enough for small/routine PRs only when it produced actual review
feedback; a green CodeRabbit check that skipped because of quota is not a
review. Larger, riskier, or more judgment-heavy PRs should get an independent
fresh-agent review even if CodeRabbit commented.

An independent review must be done by a fresh agent: one uninvolved in the
work, seeing the diff cold — cold means no exposure to how the change was
built, not context-free. Give it the diff, a review instruction, and the PR
description's Background / Purpose section (the same one every PR must open
with, which is where settled operator decisions and the larger refactor a PR
belongs to are recorded). That context is what keeps a partial or incremental
step from being mistaken for reintroducing old limitations or bugs, and keeps
review effort on defects rather than relitigating direction. What stays out is
the implementation journey: what was tried, and the implementer's own
justifications beyond what Background / Purpose already states. Prefer the
newest capable model available for the reviewer. Triage its findings like
CodeRabbit's: fix valid issues and document any deliberate skips. Record the
review status in the PR description or a PR comment, including who reviewed
and any important follow-up.

Do not `git add -A`; stage specific paths.

## Work tracking

The external tracker and private board details are not stored in this repo. Use
operator-provided startup context or private project memory when it is available,
and do not add private tracker URLs or credentials to committed docs.

Design intent, plans, and roadmaps live in the external tracker, not in this
repo — do not commit plan or roadmap documents. The repo carries code, the user
docs under `docs/`, and inline rationale in code comments.
