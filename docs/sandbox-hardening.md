# Sandboxing tclaude agents (operator guide)

**Audience:** operators who run more than one Claude Code agent through
tclaude — i.e. anyone using `agentd` and `tclaude agent` to coordinate a
group of agents.

This guide explains the **one piece of configuration that lives outside
tclaude** and that tclaude cannot do for you: locking down the Claude Code
sandbox so agents cannot reach tclaude's daemon state directly.

Codex agents use the setup-managed `tclaude-agent` permission profile by
default, which applies the equivalent private-state denial while allowing the
canonical agentd socket. This guide is for Claude Code's `settings.json`
sandbox path; see [Harnesses](harnesses.md#sandbox-approval-defaults-codex)
for Codex.

## Why this matters

`agentd`'s identity and permission layer is a **coordination guardrail,
not a security boundary**. This is a deliberate, accepted design choice;
the [Agent identity](agent.md#identity) and
[permission model](agent.md#permission-model) sections describe how callers
are attributed and gated.

The short version: the daemon resolves *which agent is calling* from the
caller's process tree and gates sensitive operations behind that identity.
That gating shapes agent behaviour and keeps a human in the loop — but it
only does so for callers that go *through the daemon*. The daemon's state
lives in plain files owned by your user account:

| Path                             | What it holds                                                              |
|----------------------------------|----------------------------------------------------------------------------|
| `~/.tclaude/data/db.sqlite`      | Session, group, and permission state (plus the WAL/SHM sidecar files).      |
| `~/.tclaude/data/config.json`    | tclaude configuration, including agent default permissions.                 |
| `~/.tclaude/data/output.log`     | The `agentd` daemon log — an identity-and-activity trace (see below).       |
| `~/.tclaude/data/…`              | All other daemon state: `operator_token`, `plugins.json`, `processes/`, `remote-access/` (CA + server keys), `exports/`. |
| `~/.claude/sessions/<pid>.json`  | Per-process identity files — the daemon reads these to attribute a caller.  |

All of tclaude's private state lives under **`~/.tclaude/data/`** by
access class: that one subtree is what must be denied. The sibling
**`~/.tclaude/api/`** holds only the agent-reachable daemon socket, and
must stay reachable (see "Keeping the daemon socket reachable" below).

An agent that can **write those files directly** does not need the daemon
at all:

- Writing `~/.tclaude/data/db.sqlite` lets an agent add itself to any group,
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

- **`~/.tclaude/data/`** — the private-state subtree. **Write must be
  denied** (integrity: the guardrail-bypass vector above). **Read should
  be denied too** (confidentiality — see below). Deny this whole
  subtree; the daemon socket lives in the sibling `~/.tclaude/api/`, so no
  child-path exception is needed — see below.
- **`~/.claude/sessions/`** — the whole directory. **Write must be
  denied** (identity-forgery vector). Read denial is harmless and
  recommended.

Write denial is the must-have. Read denial is cheap defense-in-depth —
worth doing.

> **Why `~/.tclaude/data`, not `~/.tclaude`?** The socket must stay
> reachable while all state stays denied. Claude Code's (and Codex's)
> sandbox resolves paths **deny-before-allow**, so you *cannot* deny the
> whole `~/.tclaude` tree and allow-carve the socket back in — the deny
> wins and the socket becomes unreachable. The layout instead splits
> `~/.tclaude` by access class: everything sensitive lives under
> `data/` (denied), and the socket under `api/` (allowed). That keeps the
> single rule `~/.tclaude/data/**` complete and future-proof.

### Why read-deny `~/.tclaude/data/`

`~/.tclaude/data/` holds more than `db.sqlite`, and every file in it is
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
- `operator_token`, `plugins.json`, `processes/`, `remote-access/` (which
  holds the CA and server private keys), and `exports/` (conversation
  transcripts) — all daemon-private.

Denying read on the **whole `data/` subtree** covers all of these — and
whatever the daemon adds later — with one rule and no filename list to
keep in sync. The only subtlety is the daemon socket, which lives outside
`data/`; see "Keeping the daemon socket reachable" below.

### This does not break agent ↔ daemon communication

Agents talk to the daemon over the canonical Unix socket
`~/.tclaude/api/agentd.sock`. It lives under the `api/` surface — a
sibling of the denied `data/` subtree, **not** inside it — so the private
state can be denied wholesale (`~/.tclaude/data`) without hiding the
socket. Reaching the socket still needs two things: the `socket(AF_UNIX)`
call must be permitted, and the socket file must be visible. The settings
below cover both axes. (Two pre-split sockets,
`~/.tclaude-agentd.sock` and `~/.tclaude/agentd.sock`, are still bound and
allowlisted during the upgrade window; both also sit outside `data/`.)

Likewise, write-denying `~/.claude/sessions/` does **not** stop Claude
Code from maintaining its own session files or the daemon from reading
them — neither is a sandboxed agent *tool* call. Only the agent's own
Bash/file tools are restricted.

## How to configure it

After upgrading from a version that used a pre-split socket
(`~/.tclaude-agentd.sock` or `~/.tclaude/agentd.sock`), restart
`tclaude agentd serve` before installing the updated hardening. The restart
also performs a one-time relocation of existing state into `~/.tclaude/data/`.
The installer refuses to rewrite the socket allowance while it detects a
legacy-only daemon, so it cannot strand newly sandboxed agents on an
unreachable endpoint.

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
agent can still create or overwrite `~/.tclaude/data/db.sqlite` with the
built-in `Write`/`Edit` tools — the sandbox does not gate them on its
own (verified: the `Write` tool created a file under `~/.tclaude/` on a
machine whose Bash sandbox treats it as read-only). With only
`permissions.deny`, an agent can still write the file from a `python`
one-liner in Bash. (Claude Code's docs note the two layers also
reinforce each other — `Read`/`Edit` deny rules are merged into the
sandbox boundary — but set both explicitly rather than relying on that.)

Add this to your Claude Code **`~/.claude/settings.json`** — both deny
layers, plus the `sandbox.network` block the daemon socket needs (see
"Keeping the daemon socket reachable" below). User scope means a deny
rule there cannot be weakened by any project's `.claude/settings.json`:

```json
{
  "sandbox": {
    "enabled": true,
    "network": {
      "allowUnixSockets":    ["~/.tclaude/api/agentd.sock", "~/.tclaude-agentd.sock", "~/.tclaude/agentd.sock"],
      "allowAllUnixSockets": true
    },
    "filesystem": {
      "denyWrite": ["~/.tclaude/data", "~/.claude/sessions", "~/.codex"],
      "denyRead":  ["~/.tclaude/data", "~/.claude/sessions", "~/.codex"],
      "allowRead": ["~/.tclaude/api/agentd.sock", "~/.tclaude-agentd.sock", "~/.tclaude/agentd.sock"]
    }
  },
  "permissions": {
    "deny": [
      "Edit(~/.tclaude/data/**)",
      "Read(~/.tclaude/data/**)",
      "Edit(~/.claude/sessions/**)",
      "Read(~/.claude/sessions/**)",
      "Edit(~/.codex/**)",
      "Read(~/.codex/**)"
    ]
  }
}
```

Notes:

- **`sandbox.enabled` must be `true`.** With the sandbox off, layer 1
  does nothing and a Bash one-liner can write anywhere your user can.
- **The daemon socket needs two settings, not one.**
  `sandbox.filesystem.allowRead` keeps
  `~/.tclaude/api/agentd.sock` visible. *Separately*, the `sandbox.network`
  unix-socket allowance lets a sandboxed agent open it at all —
  `allowUnixSockets` (a path list, **macOS only**) or
  `allowAllUnixSockets` (**Linux / WSL2**, all-or-nothing). Both axes are
  required; see "Keeping the daemon socket reachable" below for why, the
  trade-off, and the verification. `~/.claude/sessions` holds no socket
  and needs neither.
- **Check for paths that re-open these.** The sandbox's writable set is
  your working directory plus `permissions.additionalDirectories` plus
  `sandbox.filesystem.allowWrite`. Make sure none of those lists contains
  `~/.tclaude/data`, `~/.tclaude`, `~/.claude`, `~`, or a parent of them.
  Claude Code's
  permissions and sandboxing docs state that deny rules take precedence
  over allow rules, so a `denyWrite` entry should override an
  `allowWrite` for the same path — but keeping the allow-lists clean
  avoids relying on that and avoids surprises.
- **`Edit` is the write rule, `Read` is the read rule.** `Edit(...)`
  covers every built-in file-editing tool (creation included), so it is
  the must-have integrity rule; `Read(...)` is the confidentiality rule
  (recommended defense-in-depth).

### Keeping the daemon socket reachable

Every `tclaude agent` command connects to the daemon over the canonical
Unix socket `~/.tclaude/api/agentd.sock`. Denying `~/.tclaude/data` does
not contain that path — it is under the sibling `api/`. **Two independent
things** still have to hold, enforced by
**different** sandbox mechanisms — don't conflate them.

**1. The `socket(AF_UNIX, …)` syscall must be permitted.** With the
sandbox on, Claude Code blocks Unix-domain-socket creation by default.
Re-allowing it is a `sandbox.network` setting — *not* a filesystem one:

- **macOS:** `sandbox.network.allowUnixSockets` takes a path list;
  allow the canonical `~/.tclaude/api/agentd.sock` (plus the pre-split
  `~/.tclaude-agentd.sock` and `~/.tclaude/agentd.sock` during the upgrade
  window).
- **Linux / WSL2:** the block is a seccomp-bpf filter, which cannot
  inspect a socket's path, so per-path `allowUnixSockets` is **ignored**
  there (Claude Code's settings reference says so explicitly). The only
  available knob is `sandbox.network.allowAllUnixSockets: true`, which
  switches the filter off entirely.

  That **widens the sandbox**: with the filter off, a sandboxed agent
  can reach *any* Unix socket, not only the daemon's. Claude Code's
  sandboxing docs flag this — allowing `/var/run/docker.sock`, for one,
  "would effectively grant access to the host system through exploiting
  the docker socket." On Linux/WSL2 this is simply the price of
  `tclaude agent` working inside the sandbox; there is no narrower
  option. Accept it deliberately, and keep the *filesystem* denies tight
  so the widened socket layer is the only give.

This allowance is a **precondition**, not something this guide's
lockdown introduces: an agent that can already run `tclaude agent`
inside a sandbox already has it set. The settings block above lists both
keys so one `settings.json` works on either platform — a macOS-only
operator can drop `allowAllUnixSockets` and keep the tighter per-path
entry; on Linux/WSL2 the per-path entry is inert but harmless.

**2. The socket *file* must be visible.** This is the filesystem layer.
The socket lives under `~/.tclaude/api/`, outside the denied
`~/.tclaude/data` subtree. The generated settings still list the socket
paths under `sandbox.filesystem.allowRead` explicitly so the communication
capability remains clear and survives a broader ambient read deny.

**Verified (Linux).** Both halves were checked empirically:

- *Filesystem layer.* A `denyRead` on `~/.tclaude/data` hides `db.sqlite`,
  `db.sqlite-wal`, `db.sqlite-shm`, `config.json`, `output.log`, and
  future files under `data/`. The canonical socket remains visible
  because it is a separate path under `~/.tclaude/api/`.
- *Socket-syscall layer.* With the filesystem left fully open — the
  socket file plainly visible — a seccomp filter denying
  `socket(AF_UNIX, …)` (the same rule Claude Code's sandbox applies) was
  installed around `tclaude agent`. It failed regardless: a visible
  socket file is necessary but not sufficient; the syscall gate blocks
  the connection on its own.

So filesystem visibility exposes the socket *file*; the `sandbox.network`
unix-socket allowance restores the *syscall*. You need both. Note the
two failures look identical — `tclaude agent` reports "agentd is not
running" whether the socket file is hidden or the syscall is blocked —
so if you hit that, check both settings.

Do **not** enumerate individual files in `denyRead` instead of the
`data/` subtree. That misses the `-wal`/`-shm` sidecars (which leak recent
activity in cleartext — see "Why read-deny `~/.tclaude/data/`" above) and
`output.log`, and it must be hand-updated whenever the daemon gains a
new state file. The subtree deny plus the socket `allowRead` hole is both
safer and lower-maintenance.

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
   - Bash: `echo x > ~/.tclaude/data/probe` → should fail (read-only / denied).
   - The `Write` tool, targeting `~/.tclaude/data/probe` → should be denied.
   - Repeat both for `~/.claude/sessions/probe`.
2. **Read is denied** — both layers:
   - The `Read` tool, or `cat ~/.tclaude/data/db.sqlite` in Bash → blocked by
     `permissions.deny` (layer 2).
   - A subprocess that slips past layer 2 — e.g.
     `python3 -c "open('$HOME/.tclaude/data/db.sqlite').read(1)"` → the read
     should fail (layer 1: the OS sandbox; on Linux the file is not even
     visible).
   - Repeat for `~/.tclaude/data/output.log` and a file under
     `~/.claude/sessions/`.
3. **The daemon still works** — `tclaude agent whoami` returns the
   agent's own identity, and `tclaude agent inbox ls` works. This
   confirms both the socket-file visibility and the `sandbox.network`
   unix-socket allowance survived the lockdown.

If step 1 succeeds in writing a file, the sandbox is not denying that
path — re-check `sandbox.enabled`, the `allowWrite` /
`additionalDirectories` lists, and that the `permissions.deny` rules are
in a scope that applies. If step 3 fails with "agentd is not running"
even though the daemon is up, the socket is unreachable for one of two
reasons — check both: the `sandbox.filesystem.allowRead` entry for
`~/.tclaude/api/agentd.sock` is missing or mistyped, **or** the
`sandbox.network` unix-socket allowance is not set
(`allowAllUnixSockets` on Linux/WSL2, `allowUnixSockets` on macOS).

## Scope — what this does and does not cover

- **Covers:** an agent's own Bash tool, the subprocesses Bash spawns, and
  the built-in `Read`/`Write`/`Edit` tools — the realistic ways a
  well-behaved-but-curious or prompt-injected agent reaches the daemon's
  files.
- **Does not cover:** a process that fully escapes the OS sandbox. The
  sandbox is the security boundary; if it is bypassed, no tclaude-side
  configuration helps. The trust boundary is the Unix UID, and `agentd` never
  claimed to contain a hostile same-UID process.
  This guide closes the *easy* path (direct file edits through ordinary
  agent tooling); it does not turn the guardrail into a boundary.

## See also

- [Agent coordination](agent.md#identity) — caller attribution, operator
  identity, and the permission guardrail this guide backs on the operator side.
- Claude Code sandboxing: <https://code.claude.com/docs/en/sandboxing>
- Claude Code permissions: <https://code.claude.com/docs/en/permissions>
