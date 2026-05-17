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
| `~/.tclaude/output.log`        | The `agentd` daemon log — an identity-and-activity trace (see below).       |
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
  (integrity: the guardrail-bypass vector above). **Read should be
  denied too** (confidentiality — see below); deny the whole directory
  and punch one hole for the daemon socket, as the config below does.
- **`~/.claude/sessions/`** — the whole directory. **Write must be
  denied** (identity-forgery vector). Read denial is harmless and
  recommended.

Write denial is the must-have. Read denial is cheap defense-in-depth —
worth doing.

### Why read-deny `~/.tclaude/`

`~/.tclaude/` holds more than `db.sqlite`, and every file in it is
readable by any process running as your user:

- `db.sqlite` — every group's messages, permission grants, and identity
  rows. It runs in **WAL mode**: `db.sqlite-wal` holds recently-committed
  pages in cleartext until the next checkpoint, so denying read on
  `db.sqlite` alone still leaks recent activity through the `-wal`
  sidecar (`-shm` is the WAL index).
- `config.json` — tclaude configuration, including agent default
  permissions.
- `output.log` — the `agentd` daemon log. It carries no message
  *bodies*, but it is a detailed identity-and-activity trace: per-agent
  conv-ids, which agent called which endpoint when, the working
  directories agents run in, message IDs, and permission-request events.

Denying read on the **whole directory** covers all of these — and
whatever the daemon adds later — with one rule and no filename list to
keep in sync. The only subtlety is the daemon socket; see
"Read-denying `~/.tclaude/`" below.

### This does not break agent ↔ daemon communication

Agents still talk to the daemon over the Unix socket at
`~/.tclaude/agentd.sock`. Connecting to that socket needs the path to
*resolve* and the socket file itself — it does **not** need filesystem
**write** access to `~/.tclaude/`, so denying write leaves the socket
fully usable. Denying *read* of the whole directory is also fine
**provided you re-allow the socket** with `allowRead` — see
"Read-denying `~/.tclaude/`" below for the details and how it was
verified.

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
      "denyRead":  ["~/.tclaude", "~/.claude/sessions"],
      "allowRead": ["~/.tclaude/agentd.sock"]
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
- **`allowRead` re-opens the socket.** `denyRead` of `~/.tclaude` would
  otherwise also hide `~/.tclaude/agentd.sock`, which every `tclaude
  agent` command connects through. The `allowRead` entry re-grants read
  access to that one path; Claude Code's docs state `allowRead` takes
  precedence over `denyRead`. `~/.claude/sessions` holds no socket, so it
  needs no such hole. See "Read-denying `~/.tclaude/`" below.
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
  the must-have integrity rule; `Read(...)` is the confidentiality rule
  (recommended defense-in-depth).

### Read-denying `~/.tclaude/`

`~/.tclaude/` contains the `agentd` Unix socket (`agentd.sock`) that
every `tclaude agent` command connects through. A directory-wide
`denyRead` must therefore re-allow that one socket, or agents lose
contact with the daemon. The config above does exactly that —
`denyRead: ["~/.tclaude", …]` plus `allowRead: ["~/.tclaude/agentd.sock"]`
— and **this is verified to work.**

On Linux, Claude Code's sandbox implements a directory `denyRead` by
mounting an empty `tmpfs` over the directory: the files inside don't
just become unreadable, they become *invisible* — `open()` returns "No
such file or directory". An `allowRead` entry for a path inside is a
read-only bind-mount of that path back on top of the tmpfs. So:

- `db.sqlite`, `db.sqlite-wal`, `db.sqlite-shm`, `config.json` and
  `output.log` all disappear from the agent's view — and so does any
  future file the daemon writes there.
- `agentd.sock` is bind-mounted back, so it stays present and
  connectable. A read-only bind-mount does not block `connect()` (the
  kernel exempts sockets from the read-only-mount write check), and the
  tmpfs directory itself stays traversable, so path resolution to the
  socket still succeeds.

How this was checked: the exact `bwrap` invocation a sandboxed tclaude
agent runs was reproduced with the `denyRead`/`allowRead` flags above
spliced in. Inside it, reads of `db.sqlite`, `db.sqlite-wal`,
`db.sqlite-shm`, `config.json` and `output.log` all failed, while
`tclaude agent whoami` and `tclaude agent inbox ls` kept working. A
control run that denied the directory **without** the socket `allowRead`
broke `tclaude agent` ("agentd is not running") — confirming the
`allowRead` hole is what keeps the daemon reachable.

Do **not** be tempted to enumerate individual files in `denyRead`
instead of the directory. That misses the `-wal`/`-shm` sidecars (which
leak recent activity in cleartext — see "Why read-deny `~/.tclaude/`"
above) and `output.log`, and it has to be hand-updated whenever the
daemon gains a new state file. The directory deny plus the one socket
hole is both safer and lower-maintenance.

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
2. **Read is denied** — both layers:
   - The `Read` tool, or `cat ~/.tclaude/db.sqlite` in Bash → blocked by
     `permissions.deny` (layer 2).
   - A subprocess that slips past layer 2 — e.g.
     `python3 -c "open('$HOME/.tclaude/db.sqlite').read(1)"` → should
     fail with "No such file or directory" (layer 1: the tmpfs makes the
     file invisible).
   - Repeat for `~/.tclaude/output.log` and a file under
     `~/.claude/sessions/`.
3. **The daemon still works** — `tclaude agent whoami` returns the
   agent's own identity, and `tclaude agent inbox ls` works. This
   confirms the socket and identity resolution survived the lockdown.

If step 1 succeeds in writing a file, the sandbox is not denying that
path — re-check `sandbox.enabled`, the `allowWrite` /
`additionalDirectories` lists, and that the `permissions.deny` rules are
in a scope that applies. If step 3 fails with "agentd is not running"
even though the daemon is up, the socket `allowRead` is missing or
mistyped — confirm the `allowRead` entry resolves to the same path as
`~/.tclaude/agentd.sock`.

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
