# tclaude agent instructions

`AGENTS.md` is a symlink to this file. Keep it short and durable. Plans and
handoffs belong in the external tracker, not this file.

## Product and code ownership

`tclaude` is a Go CLI and daemon for durable agentic work across Claude Code,
Codex CLI, OpenCode and Copilot CLI. Providers are selected explicitly; no
provider is enabled by default. Linux and macOS are supported; WSL is Linux.

Both binaries use `internal/product`: `main.go` runs the client and
`cmd/tclaude-agentd/main.go` runs the daemon. The client also exposes the same
daemon command as `tclaude agentd serve`.

`internal/backend/model` defines durable concepts; `app` owns workflows and
authority; `sqlite` owns transactions; `ports` defines focused contracts.
Cohesive `providers` own native interpretation and mechanisms; `host` owns
processes, terminals and Git resources. `transport` projects authenticated public
APIs; `server` owns composition and joined background workers. The browser is in
`internal/product/browser`. See docs/architecture.md and the operating guide.

State lives in an explicitly initialized private directory. Never discover,
open or migrate the old database implicitly. Preserve logical identities and
important durable meaning through the versioned offline importer. Keep native
resource evidence, credentials and internal principals out of public DTOs.

An Agent, Conversation, Execution, Workspace and WorkRun have different
lifetimes. Queries do not repair state. Admit current authority before each
effect and settle durable results beyond caller cancellation. Unknown effects
are not replayed automatically. Report unsupported provider capabilities
explicitly; do not silently weaken requested confinement or history precision.

## Build and test

```sh
go build ./...
go test -race ./... -count=1
golangci-lint run ./...
```

When installation is explicitly intended, `go install . ./cmd/...` builds both
binaries. Ordinary development and validation should build without installing or
restarting a live daemon.

Tests use production application/SQLite/public API paths and disposable state;
double only external native boundaries as needed. Do not use live private data.
Install tmux for host tests. Installed-Chrome offline browser acceptance runs
with `TCLAUDE_BROWSER_SMOKE=1 go test -race ./internal/product/browser -count=1`.
CI runs the full suite on Linux/macOS and browser journeys on Linux. Keep native
fixture evidence distinct from authenticated native execution.

Commands use Cobra through Boa. Use platform build tags for OS-specific host
code. Keep lifecycle command tokens constant and user text out of command
injection paths. Do not opportunistically rename historical identifiers.

## UI direction

For contested layout or control choices, mock alternatives in standalone HTML,
render with Chrome using disposable XDG directories, and send the image through
human-notify before changing production UI. Let the operator choose taste
questions. Mechanical fixes and already accepted UI behavior need no new taste
approval.

## Git, commits, and PRs

When making feature or fix changes as an agent, create a feature branch unless the 
operator gives different instructions. It is fine to force-push a feature branch; 
never force-push `main`.

Do not include remote-access/session links in commits, PR descriptions, or PR
comments. In particular, do not add `Claude-Session:` trailers or
`https://claude.ai/code/...` URLs. A plain `Co-Authored-By` trailer is fine.

PR descriptions should start with a short `Background / Purpose` section that
explains why the PR exists. Then summarize the implementation and list tests or
verification.

Every PR needs a real cold review, but make a commit to the feature branch first.
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
justifications beyond what Background / Purpose already states. Triage its
findings like CodeRabbit's: fix valid issues and document any deliberate
skips. Record the review status in the PR description or a PR comment,
including who reviewed and any important follow-up.

How you obtain that fresh agent is deliberately not fixed. `tclaude agent
spawn` is the preferred mechanism, because the reviewer becomes a real peer
the operator can see and talk to. It is not the only one: when spawn
permission is unavailable, an in-harness subagent of the local harness
(Claude Code's Task tool, Codex CLI's equivalent) is a valid fallback, as is
asking the operator to arrange a reviewer. What must hold is the cold and
uninvolved property described above, not the spawn path. Say which mechanism
you used when you record the review status, so a subagent review is not
mistaken for a peer-agent one.

Do not `git add -A`; stage specific paths.

## Work tracking

The external tracker and private board details are not stored in this repo. Use
operator-provided startup context or private project memory when it is available,
and do not add private tracker URLs or credentials to committed docs.

Design intent, plans, and roadmaps live in the external tracker, not in this
repo — do not commit plan or roadmap documents. The repo carries code, the user
docs under `docs/`, and inline rationale in code comments.
