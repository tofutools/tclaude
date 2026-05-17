# Sandboxing tclaude agents (operator guide)

**Audience:** operators who run more than one Claude Code agent through
tclaude — i.e. anyone using `agentd` and `tclaude agent` to coordinate a
group of agents.

This guide explains the **one piece of configuration that lives outside
tclaude** and that tclaude cannot do for you: locking down the Claude Code
sandbox so agents cannot reach tclaude's daemon state directly.

## Why this matters

`agentd`'s identity and permission layer is a **coordination guardrail,
not a security boundary**. This is a deliberate, accepted design choice;
[`plans/agentd.md`](plans/agentd.md) — the `agentd` design doc — covers
the full reasoning in its security-model discussion.

The short version: the daemon resolves *which agent is calling* from the
caller's process tree and gates sensitive operations behind that identity.
That gating shapes agent behaviour and keeps a human in the loop — but it
only does so for callers that go *through the daemon*. The daemon's state
lives in plain files owned by your user account:

| Path                          | What it holds                                                              |
|--------------------------------|----------------------------------------------------------------------------|
| `~/.tclaude/db.sqlite`         | Session, group, and permission state (plus the WAL/SHM sidecar files).      |
| `~/.tclaude/config.json`       | tclaude configuration, including agent default permissions.                 |
| `~/.claude/sessions/<pid>.json`| Per-process identity files — the daemon reads these to attribute a caller.  |

An agent that can **write those files directly** does not need the daemon
at all:

- Writing `~/.tclaude/db.sqlite` lets an agent add itself to any group,
  grant itself any permission, or rewrite another agent's messages —
  every gate the daemon enforces is in that file.
- Writing `~/.claude/sessions/<pid>.json` lets an agent change the
  conv-id the daemon will attribute to a process — i.e. **forge its own
  identity or impersonate another agent**.

The OS sandbox is what makes the guardrail hold. With agents sandboxed so
those paths are read-only (or unreachable), the daemon is the *only* way
to touch that state, and the guardrail does its job. Without it, the
guardrail is advisory only.

> This is not a tclaude bug to fix in code — containing a hostile
> same-UID process is the OS sandbox's job by design. tclaude's part is to
> tell you, the operator, exactly what to deny. That is this document.

## What to lock down

Deny tclaude agents direct access to these two trees:

- **`~/.tclaude/`** — the whole directory. **Write must be denied**
  (integrity: the guardrail-bypass vector above). Read denial is
  recommended too (confidentiality: `db.sqlite` contains every group's
  messages and permission grants) — see the socket caveat below.
- **`~/.claude/sessions/`** — the whole directory. **Write must be
  denied** (identity-forgery vector). Read denial is harmless and
  recommended.

Write denial is the must-have. Read denial is defense-in-depth.

### This does not break agent ↔ daemon communication

Agents still talk to the daemon over the Unix socket at
`~/.tclaude/agentd.sock`. Connecting to that socket needs the path to
*resolve* and the socket itself — it does **not** need filesystem
**write** access to `~/.tclaude/`. So denying write leaves the socket
fully usable. (Verified: with `~/.tclaude/` mounted read-only by the
sandbox, `tclaude agent` commands and the daemon's identity resolution
work normally.) Read denial is the one to be careful with — it can
affect path resolution; see the socket caveat below.

Likewise, write-denying `~/.claude/sessions/` does **not** stop Claude
Code from maintaining its own session files or the daemon from reading
them — neither is a sandboxed agent *tool* call. Only the agent's own
Bash/file tools are restricted.

## How to configure it

Claude Code enforces filesystem restrictions through **two layers**, and
you need **both** — each covers a hole the other leaves open:

1. **`sandbox.filesystem.*`** — OS-level enforcement (bubblewrap on
   Linux, Seatbelt on macOS). Per Claude Code's docs it "applies only to
   Bash commands and their child processes" (including scripts:
   `python`, `node`, etc.). It does **not**, on its own, gate Claude
   Code's built-in `Read`/`Write`/`Edit` tools.
2. **`permissions.deny`** — tool-level rules. The two file-related rule
   names are **`Read`** and **`Edit`**: per Claude Code's permissions
   docs, *"`Edit` rules apply to all built-in tools that edit files"* —
   that is the `Write`, `Edit`, `MultiEdit`, and `NotebookEdit` tools —
   and `Read` rules apply to the file-reading tools. There is no
   separate per-path `Write(...)` rule to set; `Edit(...)` already
   covers new-file creation. These rules also apply to the file
   commands Claude Code recognizes in Bash (`cat`, `sed`, …; the docs
   call recognition best-effort) but **not** to arbitrary subprocesses —
   a `python`/`node` script that opens a file itself slips past
   `permissions.deny`. That gap is what layer 1 closes.

Configure only one and there is a hole. With only the sandbox layer, an
agent can still create or overwrite `~/.tclaude/db.sqlite` with the
built-in `Write`/`Edit` tools — the sandbox does not gate them on its
own (verified: the `Write` tool created a file under `~/.tclaude/` on a
machine whose Bash sandbox treats `~/.tclaude/` as read-only). With only
`permissions.deny`, an agent can still write the file from a `python`
one-liner in Bash. (Claude Code's docs note the two layers also
reinforce each other — `Read`/`Edit` deny rules are merged into the
sandbox boundary — but set both explicitly rather than relying on that.)

Add this to your Claude Code **`~/.claude/settings.json`** (user scope —
a deny rule there cannot be weakened by any project's `.claude/settings.json`):

```json
{
  "sandbox": {
    "enabled": true,
    "filesystem": {
      "denyWrite": ["~/.tclaude", "~/.claude/sessions"],
      "denyRead":  ["~/.claude/sessions"]
    }
  },
  "permissions": {
    "deny": [
      "Edit(~/.tclaude/**)",
      "Read(~/.tclaude/**)",
      "Edit(~/.claude/sessions/**)",
      "Read(~/.claude/sessions/**)"
    ]
  }
}
```

Notes:

- **`sandbox.enabled` must be `true`.** With the sandbox off, layer 1
  does nothing and a Bash one-liner can write anywhere your user can.
- **`~/.claude/sessions` is safe to `denyRead` directory-wide** — it
  holds no socket, so a directory read deny cannot break anything. Only
  `~/.tclaude` needs the file-scoped treatment (see the socket caveat
  below).
- **Check for paths that re-open these.** The sandbox's writable set is
  your working directory plus `permissions.additionalDirectories` plus
  `sandbox.filesystem.allowWrite`. Make sure none of those lists contains
  `~/.tclaude`, `~/.claude`, `~`, or a parent of them. Claude Code's
  permissions and sandboxing docs state that deny rules take precedence
  over allow rules, so a `denyWrite` entry should override an
  `allowWrite` for the same path — but keeping the allow-lists clean
  avoids relying on that and avoids surprises.
- **`Edit` is the write rule, `Read` is the read rule.** `Edit(...)`
  covers every built-in file-editing tool (creation included), so it is
  the must-have integrity rule; `Read(...)` is the optional
  confidentiality rule.

### Socket caveat for read-denying `~/.tclaude/`

The example above intentionally does **not** put `~/.tclaude` in
`sandbox.filesystem.denyRead`. A directory-wide OS-level read deny can
also block *path resolution* of `~/.tclaude/agentd.sock`, which would
break `tclaude agent`. The `permissions.deny` `Read(~/.tclaude/**)` rule
above already stops the agent's `Read` tool and `cat`-style Bash
commands from reading `db.sqlite` — that covers the realistic
confidentiality case without risking the socket.

If you also want OS-level read denial of `~/.tclaude/`, deny the
*specific files* rather than the directory:

```json
"sandbox": { "filesystem": { "denyRead": ["~/.tclaude/db.sqlite", "~/.tclaude/config.json"] } }
```

— or deny the directory and punch a hole for the socket with
`"allowRead": ["~/.tclaude/agentd.sock"]` (Claude Code's sandboxing docs
note that `sandbox.filesystem.allowRead` re-grants read within a denied
region). Either way, **verify** afterwards (see below) — this is the
fragile case, so confirm `tclaude agent whoami` still works.

### Multi-user / shared machines

On a shared machine, put the same `sandbox` and `permissions.deny`
blocks in **managed settings** instead
(`/etc/claude-code/managed-settings.json` on Linux, the platform
equivalent elsewhere). Managed settings sit at the top of the precedence
chain and cannot be overridden by user or project settings.

## Verifying

After updating `settings.json`, start an agent through tclaude and, from
inside that agent's session, confirm:

1. **Write is denied** — both layers:
   - Bash: `echo x > ~/.tclaude/probe` → should fail (read-only / denied).
   - The `Write` tool, targeting `~/.tclaude/probe` → should be denied.
   - Repeat both for `~/.claude/sessions/probe`.
2. **The daemon still works** — `tclaude agent whoami` returns the
   agent's own identity, and `tclaude agent inbox ls` works. This
   confirms the socket and identity resolution survived the lockdown.

If step 1 succeeds in writing a file, the sandbox is not denying that
path — re-check `sandbox.enabled`, the `allowWrite` /
`additionalDirectories` lists, and that the `permissions.deny` rules are
in a scope that applies.

## Scope — what this does and does not cover

- **Covers:** an agent's own Bash tool, the subprocesses Bash spawns, and
  the built-in `Read`/`Write`/`Edit` tools — the realistic ways a
  well-behaved-but-curious or prompt-injected agent reaches the daemon's
  files.
- **Does not cover:** a process that fully escapes the OS sandbox. The
  sandbox is the security boundary; if it is bypassed, no tclaude-side
  configuration helps. This is the same residual described in
  [`plans/agentd.md`](plans/agentd.md) — the trust boundary is the Unix
  UID, and `agentd` never claimed to contain a hostile same-UID process.
  This guide closes the *easy* path (direct file edits through ordinary
  agent tooling); it does not turn the guardrail into a boundary.

## See also

- [`plans/agentd.md`](plans/agentd.md) — `agentd` design and its
  security-model discussion (guardrail, not boundary), which this guide
  backs on the operator side.
- Claude Code sandboxing: <https://code.claude.com/docs/en/sandboxing>
- Claude Code permissions: <https://code.claude.com/docs/en/permissions>
