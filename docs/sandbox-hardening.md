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

There is also an escape hatch around the OS layer:
`dangerouslyDisableSandbox: true` asks Claude Code to run a Bash command
outside the sandbox and fall back to the normal permission flow. A
`permissions.deny` rule such as `Read(path)` binds reliably to file tools, not
to every arbitrary program that can read the same bytes. Set
`sandbox.allowUnsandboxedCommands` to `false` so the agent cannot request that
bypass. Because this also removes the fallback for ordinary network tools, the
recommended block first allows the exact GitHub domains needed to push a
branch, open a PR, and inspect its checks inside the sandbox.

Add this to your Claude Code **`~/.claude/settings.json`** — both deny
layers, the disabled escape hatch, and the `sandbox.network` allowances the
daemon socket and GitHub workflow need (see "Keeping the daemon socket
reachable" below). User scope means a deny rule there cannot be weakened by
any project's `.claude/settings.json`:

```json
{
  "sandbox": {
    "enabled": true,
    "failIfUnavailable": true,
    "allowUnsandboxedCommands": false,
    "network": {
      "allowUnixSockets":    ["~/.tclaude/api/agentd.sock", "~/.tclaude-agentd.sock", "~/.tclaude/agentd.sock"],
      "allowAllUnixSockets": true,
      "allowedDomains":      ["github.com", "api.github.com"]
    },
    "filesystem": {
      "denyWrite": ["~/.tclaude/data", "~/.claude/sessions"],
      "denyRead":  ["~/.tclaude/data", "~/.claude/sessions"],
      "allowRead": ["~/.tclaude/api/agentd.sock", "~/.tclaude-agentd.sock", "~/.tclaude/agentd.sock"]
    }
  },
  "permissions": {
    "deny": [
      "Edit(~/.tclaude/data/**)",
      "Read(~/.tclaude/data/**)",
      "Edit(~/.claude/sessions/**)",
      "Read(~/.claude/sessions/**)"
    ]
  }
}
```

`tclaude setup --install-sandbox-hardening` installs this block
append-only and idempotently. **Existing installations must re-run that command
after upgrading**: settings written by an older tclaude do not acquire
`failIfUnavailable: true`, `allowUnsandboxedCommands: false`, or the GitHub
domains until the installer runs again.

Notes:

- **`sandbox.enabled` must be `true`.** With the sandbox off, layer 1
  does nothing and a Bash one-liner can write anywhere your user can.
- **`sandbox.failIfUnavailable` must be `true`.** Claude Code otherwise warns
  and runs Bash unsandboxed when the platform sandbox cannot start (for
  example, because bubblewrap or socat is unavailable). Hardening must fail
  closed instead of silently losing its OS boundary.
- **`allowUnsandboxedCommands` must be `false`.** Otherwise an agent can set
  `dangerouslyDisableSandbox: true` on a Bash call and ask the permission layer
  to run it outside every filesystem and network boundary above.
- **The GitHub allowlist is deliberately narrow.** `github.com` covers this
  repository's Git transport and `api.github.com` covers `gh pr` / `gh run`.
  They are trusted but still provide an exfiltration path; operators whose
  agents never push or open PRs can omit them manually.
- **The daemon socket needs two settings, not one.**
  `sandbox.filesystem.allowRead` keeps
  `~/.tclaude/api/agentd.sock` visible. *Separately*, the `sandbox.network`
  unix-socket allowance lets a sandboxed agent open it at all —
  `allowUnixSockets` (a path list, **macOS only**) or
  `allowAllUnixSockets` (all platforms, and the only available option on
  **Linux / WSL2**). Both axes are required; see "Keeping the daemon socket
  reachable" below for why, the trade-off, and the verification.
  `~/.claude/sessions` holds no socket and needs neither.
- **Do not deny the whole `~/.codex` tree.** Standalone Codex installs its
  executable below `~/.codex/packages` and re-executes it for tool commands;
  a whole-tree deny strands managed Codex agents. Narrower state boundaries
  require separate runtime/state analysis. Sandbox profiles that deny `$HOME`
  (or any ancestor of `~/.codex`) and reopen narrower paths beneath it handle
  this through a capability gate: on Linux, tclaude first runs an isolated
  Codex behavioral probe that proves a denied parent can retain narrower child
  reopens and determines whether the exact executable leaf must be reopened.
  Codex macOS is refused — see openai/codex#21081, where a deny mask dominates
  narrower reopens beneath it.
  Before merging changes to that contract, run the host-only smoke outside any
  nested agent sandbox:

  ```bash
  TCLAUDE_CODEX_SPLIT_SMOKE=1 go test ./pkg/claude/harness -run TestCodexSplitPolicyHostSmoke -count=1 -v
  ```

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

### User setting or managed policy?

The setup command deliberately writes the user-level
`~/.claude/settings.json`. It is non-privileged, preserves the existing
append-only/conflict behavior, and is appropriate for a single-user
workstation where the operator controls that file. If
`allowUnsandboxedCommands` is already `true`, setup reports
`sandbox.allowUnsandboxedCommands: hardening wants false ... left unchanged
(fix it manually)` and does not overwrite the operator's choice.

This is strong default configuration, not an administrator lock: a project or
local setting with higher precedence can change a scalar setting, and Claude
Code can hot-reload such a change during a running `inherit` session. For a
single tclaude launch that must pin the setting above project/local scope, use
sandbox `on`; its command-line settings block is outranked only by managed
policy. For a shared machine or a policy that agents must never be able to
relax, use Claude Code's root-owned managed policy settings and add top-level
`"forbidUnsandboxedCommands": true`. `tclaude setup` does not write system
policy files or invoke privilege escalation.

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

`allowAllUnixSockets` is also honored on macOS. The cross-platform settings
block therefore permits all Unix sockets there too, including an SSH agent
socket used by `git push`; a macOS-only operator can remove that broad key and
add every required socket path explicitly for a tighter policy.

This allowance is a **precondition**, not something this guide's
lockdown introduces: an agent that can already run `tclaude agent`
inside a sandbox already has it set. The settings block above lists both
keys so one `settings.json` works on either platform; on Linux/WSL2 the
per-path entry is inert but harmless.

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
blocks in **managed settings** instead, together with top-level
`"forbidUnsandboxedCommands": true`. On Linux, that file is
`/etc/claude-code/managed-settings.json`; use the platform equivalent
elsewhere. Managed settings sit at the top of the precedence chain and cannot
be overridden by user or project settings.

## The unsandboxed-autonomy warning

tclaude's spawn defaults pair an **autonomous approval posture** with a
**confined filesystem**: an agent in a detached tmux pane must not block on a
prompt nobody will answer, which is only a safe trade because its writes stay
inside a sandbox. For Codex tclaude enforces both halves — the spawn default is
the managed `tclaude-agent` permission profile. For Claude Code it enforces only
the first: the sandbox default is `inherit`, which by design leaves your
`settings.json` posture exactly as you wrote it.

So if you have *not* configured a Claude Code sandbox, a default Claude spawn
runs `--permission-mode auto` with the supervisor classifier as the only thing
between the agent and your machine.

tclaude does not silently fix this for you — forcing the sandbox on would
override your `settings.json` on the one axis it promises not to touch. It tells
you instead. Whenever a launch pairs a permission mode that runs commands
unattended (`auto`, `bypassPermissions`) with a sandbox tclaude cannot prove is
active, you get a warning:

- `tclaude agent spawn` prints it as a `Warning:` line in the resolved-launch
  echo;
- `tclaude session new` prints it to stderr;
- the dashboard spawn dialog shows it under the permission-mode select, live as
  you change the harness, sandbox, permission mode, or CWD;
- a group-template deploy attaches it to the per-agent notes.

To decide whether the sandbox is really active, tclaude reads the same settings
files Claude Code will, in the same precedence order: managed policy settings,
then `.claude/settings.local.json` and `.claude/settings.json` found by walking
up from the launch directory, then `~/.claude/settings.json`. A file it cannot
read or parse is reported alongside the verdict rather than treated as an
all-clear.

Two ways to make the warning go away, both of which are the actual fix:

- install the hardening above (`tclaude setup --install-sandbox-hardening`), so
  every Claude agent is sandboxed by default; or
- spawn with sandbox `on` for that one agent.

## Reading an agent's sandbox badge

Each agent row in the dashboard's Groups tab carries a sandbox badge — a single
glyph at the end of the harness/model line, next to the 📱 remote indicator —
and it describes what actually confines the agent, not which mode was requested.
The glyph is the whole on-screen surface; hovering it gives the full verdict,
including which mode was requested and which settings file decided.

That distinction matters because the mode usually cannot answer the question.
`inherit`, the Claude Code default, means "whatever `settings.json` says", so a
badge driven off the mode alone would look identical for an agent locked down by
your global hardening and one running completely unconfined.

So tclaude resolves the verdict once, at launch, using the same precedence chain
as the warning above, and records it on the session row. The badge then reads:

| Badge | Tooltip says | Meaning |
| --- | --- | --- |
| `🔒` | `Sandbox: on` | The OS sandbox confined this agent. The tooltip says whether it was forced on for this launch, inherited from your settings, or forced on by managed policy over an explicit `off` — and which file decided it. |
| *(none)* | — | Nothing confines the agent, and nothing was asked for — the plain `inherit` launch with no sandbox configured. This is the posture the unsandboxed-autonomy warning is about. |
| `⚠` | `Sandbox: off` | The sandbox was forced **off** for this launch; the agent's Bash runs unconfined. The tooltip names who chose it (see below) — it is not necessarily a human's explicit opt-in. |
| `⚠` | `Sandbox: off — this launch asked for the OS sandbox to be ON, but …` | The launch asked for the sandbox to be **on** and did not get it — only managed policy settings can do this. The agent is unconfined despite what was requested. |
| `⚠` | `Sandbox: on (unverified)` | The sandbox looks active, but a settings file **outranking** the one that decided could not be read or parsed, so a policy tclaude never saw may say otherwise. Treat it as unproven and fix the unreadable file. |

Experimental `tclaude-layer` verdicts are a different case and say so
differently. Their boundary is established — the launch probes the exact frozen
outer spec before committing the pane — so their tooltip reads `Sandbox: on`,
claims `Bash is confined.`, and then names the known limits of that boundary in
a `⚠ Partial fidelity:` sentence. The Linux host-open tooltip says filesystem
mounts are enforced while ambient host Unix sockets remain connectable. The
macOS tooltip is deliberately different: Seatbelt enforces filesystem
operations, but there is no mount namespace, hidden paths remain enumerable,
and the host network plus ambient Unix sockets remain reachable. Neither is the
`(unverified)` case above — that word is reserved for a posture tclaude could
not establish, and partial fidelity is a boundary it did.

For OpenCode on macOS, the partial disclosure also names the XDG-config
remainder: mutable per-agent privacy covers data/cache/state, while the config
base is not redirected. OpenCode's global config directory is read-only, but a
non-OpenCode config write from inside the wall targets the real host config
base and is controlled only by the filesystem policy.

Every warning shares the one ⚠ glyph, so a row tells you at a glance that
something is off; hover for which of the three it is. The one exception to
purely informational warnings is an active temporary sandbox override: its
badge is clickable and restores the agent's preserved normal sandbox
configuration. That badge is normally ⚠, but remains 🔒 if higher-precedence
policy keeps the sandbox on. Ordinary unconfined and unverified warnings do not
dispatch that action.

Because the verdict is resolved at launch, it describes what the *running* agent
got. Editing `settings.json` afterwards does not change an existing agent's
badge. Claude Code may hot-reload a project/local scalar such as
`allowUnsandboxedCommands`, so the badge is a launch-time OS-sandbox verdict,
not proof that a later settings edit left every escape hatch disabled. A resume
re-resolves the OS-sandbox verdict and picks up the new posture. For a running
agent, the member ⚙ menu's ordinary **↻ restart** performs that resume in place;
it also re-resolves the agent's assigned tclaude sandbox profiles.

Agents older than this feature, and Codex agents, record no verdict: a Codex
launch's `--sandbox` mode *is* its posture, so its badge reports the mode
directly (`🔒` for a confining mode, `⚠` for `danger-full-access`, with the mode
named in the tooltip). One caveat: that holds for agents tclaude spawns, where
the daemon applies its managed-profile default. A bare `tclaude session new --harness codex`
with no `--sandbox` records no mode at all and gets no badge — its real posture
comes from `~/.codex/config.toml`, which tclaude does not read.

### Temporary sandbox-off restart

For a short debugging task that cannot be completed under the normal sandbox,
the dashboard member ⚙ menu can restart the same agent with its sandbox off.
This is a reversible override on the stable `agent_id`; it does not replace the
agent's normal launch posture, and the restore action restarts under that
preserved posture. Conversation rotations keep the override, while clones do
not inherit it.

Attached tmux clients are carried across the stop/resume gap through a
short-lived bridge session. The carry is best-effort so a stale client or tmux
switch failure cannot prevent the operator-requested posture change. The bridge
self-expires after five minutes as a fallback for daemon exit or failed
cleanup.

The daemon refuses either transition unless the agent is online and fully idle:
the main status is `idle`, no background agents remain, and no background shell
commands remain. Claude Code's customizable statusline leads with a red
`⚠ SB-OFF` while the override is active. Codex has no corresponding
customizable statusline surface, so use the dashboard's sandbox warning and
restore menu state there.

### Who chose the sandbox, and which profile shaped it

Two questions the verdict alone cannot answer ride in the same tooltip.

**Who chose the mode.** `sandbox: on` reaches a launch either because someone
passed it or because a spawn profile carried it — a named `--profile`, the
group's default, or the global default. The resolved verdict is identical, so
the badge used to call both "forced ON for this launch", crediting the operator
with a decision a default profile made. It now names the tier:

| Chosen by | Tooltip |
| --- | --- |
| an explicit `--sandbox on` / the spawn dialog | ``forced ON by this launch (sandbox `on`)`` |
| a default or named spawn profile | ``forced ON by global default profile "agents" (sandbox `on`)`` |
| a profile that chose `off` | ``forced OFF by group default profile "loose" (sandbox `off`)`` |

An `off` is attributed the same way an `on` is, and for a stronger reason: a
default profile can opt an agent *out* of containment as silently as it can opt
one in, and that is the claim an operator is least likely to question.

The attribution is recorded at launch (`sessions.sandbox_mode_source`) and
replayed by the durable relaunch posture — on a CLI resume, and on every daemon
relaunch (crash recovery, reincarnation, clone) — so it survives a restart
rather than going anonymous. Only a LAUNCH-decided verdict is
attributed: where a settings file decided, who chose the mode did not affect the
outcome, and naming a profile there would be a fresh false attribution.

A harness whose mode *is* its posture (Codex) records no verdict to fold the
tier into, so its badge appends the attribution as its own sentence —
``Chosen by global default profile "wide-open".`` — from the same column.

**Which profile shaped it.** The glyph reports the sandbox *state*. A tclaude
**sandbox profile** is orthogonal to that: it never decides whether the agent is
sandboxed, it supplies the *rules*. For a Claude agent the profile's filesystem
grants are compiled into Claude Code's own `sandbox.filesystem.*` through
`--settings`, so they take effect only while the sandbox is enabled — and are
not emitted at all for a launch that requested `off` — while the profile's
environment entries are plain environment variables that apply either way.

| Situation | Tooltip adds |
| --- | --- |
| profile applied, rules in force | `Customized by tclaude sandbox profile “x” (global default).` — one clause per applied tier, in `global` → `group` → `explicit` order |
| profile applied, rules withheld | the same clause continued: `Customized by tclaude sandbox profile “x” (global default) — its filesystem rules are not in force (…); any environment entries it defines still apply.` The reason is either that the sandbox is off, or that the launch requested `off` so the rules were never emitted — including when managed policy then forced the sandbox back on |
| unverified verdict | the profile is named, with no claim either way: the hedge above already says the posture is unproven |
| launch resolved to no profile | `No tclaude sandbox profile applied.` |
| the caller omitted the profiles (spawn dialog "none", `--omit-sandbox-profiles`) | `No tclaude sandbox profile — this launch omitted them.` |
| the launch mode cannot carry them (Codex `danger-full-access`) | `tclaude sandbox profiles do not apply under this launch mode.` |
| agent older than the recorded policy | *(nothing — an absence tclaude never observed is not reported as one)* |

The clause says "customized by", not "rules from": the dashboard receives
profile *names* only, so it cannot know whether a given profile contributes
filesystem rules, environment entries, or both.

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
4. **The bypass is disabled and GitHub still works** —
   `git ls-remote origin HEAD`, `gh pr list --limit 1`, and
   `gh run list --limit 1` run inside the sandbox. A Bash request carrying
   `dangerouslyDisableSandbox: true` remains sandboxed instead of exposing the
   denied state paths.

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

## Agentd runtime dependency invariant

Agentd-owned production paths do not launch Python, directly or through a
tclaude-authored shell command. In particular, the stacked Claude capability
probe uses a Go-native, launch-owned Messages stub and a kernel-sealed copy of
the running tclaude image for its inner AF_UNIX/seccomp assertion. A host
without Python therefore has the same agentd startup and launch behavior.

This is a production-ownership boundary, not a ban on Python elsewhere in the
repository or on the machine. Tests, documentation examples, and CI tooling
may use it. Interactive agent sandboxes are unaffected: when Python is
installed and the selected sandbox profile permits it, agents may invoke it as
an ordinary tool. A human may also put any command they choose in the
dashboard's human-managed plugin registry or an interactive terminal; those
commands are operator input, not an agentd runtime dependency.

## See also

- [Sandboxing](sandboxing.md) — the operator mental model for sandbox profiles:
  enforcement layers, deny + reopen, and the failure modes to expect.
- [Agent coordination](agent.md#identity) — caller attribution, operator
  identity, and the permission guardrail this guide backs on the operator side.
- Claude Code sandboxing: <https://code.claude.com/docs/en/sandboxing>
- Claude Code permissions: <https://code.claude.com/docs/en/permissions>
