# Operator sandbox guidance + setup advisory

**Shipped.** Operator-facing documentation, plus an unconditional advisory
at `tclaude setup` time, for sandboxing Claude Code agents so `agentd`'s
permission layer (a coordination *guardrail*, not a security boundary —
see [`../agentd.md`](../agentd.md), "Security model") is actually backed
by OS-level containment.

## Background

`agentd` resolves caller identity from the process tree and gates
sensitive operations behind it. That guardrail only holds for callers
that go *through* the daemon. The daemon's state is plain user-owned
files:

- `~/.tclaude/db.sqlite` / `config.json` — session, group, permission state.
- `~/.claude/sessions/<pid>.json` — per-process identity files agentd reads.

An agent that can write those files bypasses the daemon entirely (forge
group membership, grant itself permissions, forge its own identity). The
fix is operator-side: sandbox agents so those paths are denied. tclaude's
job is to tell the operator exactly what to deny.

## What shipped

### 1. Documentation — `docs/sandbox-hardening.md` (new)

Operator guide covering:

- Why it matters (guardrail vs. boundary; the bypass vectors).
- The two trees to lock down — `~/.tclaude/` and `~/.claude/sessions/` —
  write-deny as the must-have, read-deny as defense-in-depth.
- Claude Code's **two enforcement layers** and why both are needed:
  `sandbox.filesystem.*` (covers the Bash tool + subprocesses) and
  `permissions.deny` (covers the built-in `Read`/`Write`/`Edit` tools).
  The sandbox does **not** restrict the file tools — verified
  empirically: the `Write` tool created a file under `~/.tclaude/` on a
  machine whose Bash sandbox treats `~/.tclaude/` as read-only.
- A complete recommended `~/.claude/settings.json` snippet.
- The socket caveat: write-denying `~/.tclaude/` does not break
  `~/.tclaude/agentd.sock` (connect is a network op, not an FS write —
  verified); directory-wide read-deny can, so read-deny the specific
  files instead.
- A verification procedure and an explicit scope statement (this closes
  the easy path — direct file edits via ordinary tooling — it does not
  turn the guardrail into a boundary).

Cross-referenced from `docs/plans/agentd.md` (File map) and
`docs/index.md` (Documentation list).

### 2. `tclaude setup` advisory

`tclaude setup` and `tclaude setup --check` now print an `=== Agent
Sandbox ===` section pointing operators at the hardening doc.

It is an **unconditional pointer, not a detection.** A detection-based
warning was investigated and deliberately *not* shipped — see below.

## Why no detection-based warning

`tclaude setup` cannot reliably determine whether the operator's sandbox
denies those paths:

- It only sees `~/.claude/settings.json`. The effective config merges
  user + project + `.local` + managed scopes; project/managed scopes are
  invisible to setup, and a project `allowWrite` can re-open a path.
- The real gap is the file-tool-vs-Bash split: `sandbox.enabled: true`
  is necessary but not sufficient (the `Write`/`Edit` tools bypass the
  sandbox), so a "sandbox is on" check would give false reassurance.
- A correct check would have to reconstruct gitignore-pattern matching
  over merged `sandbox.*` + `permissions.deny`, which is CC-version-
  dependent.

Per the task's own guidance ("a wrong warning is worse than none"), the
shipped behavior is a zero-false-positive advisory pointer. A genuine
detector remains possible as future work if CC exposes a resolved-config
query.

## Files

- `docs/sandbox-hardening.md` — new operator guide.
- `docs/plans/agentd.md` — File-map pointer to the guide.
- `docs/index.md` — Documentation-list pointer to the guide.
- `pkg/claude/setup/setup.go` — `sandboxAdvisory()` + `sandboxHardeningDoc{Path,URL}`
  consts; printed from `runSetup` and `checkStatus`.
- `pkg/claude/setup/setup_test.go` — `TestSandboxAdvisory_NamesPathsAndDoc`
  (advisory content), `TestSandboxHardeningDocExists` (doc-path drift guard).

## Follow-ups / notes

- The doc cross-references `agentd.md`'s "Security model — a guardrail,
  not a boundary" section, added by PR #167. Once #167 merges, a
  one-line pointer to `docs/sandbox-hardening.md` could also be added at
  the end of that section; this PR kept its `agentd.md` edit to the
  conflict-free File map.
