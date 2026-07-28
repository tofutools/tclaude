# How sandboxing works (operator mental model)

**Audience:** operators who want to understand what a tclaude sandbox profile
actually does to a running agent — before they author one, and while they are
debugging one that misbehaves.

This is the **mental model and troubleshooting** guide. The reference material
lives elsewhere and is not repeated here:

| For | Read |
|-----|------|
| Profile wire shape, deny/reopen rules, protected roots, the CLI | [Agent coordination → sandbox profiles](agent.md#sandbox-profiles) |
| Per-session sandbox modes (Claude `inherit`/`on`/`off`, Codex) | [Harnesses](harnesses.md#sandbox-at-spawn-claude-code) |
| The full harness capability matrix | [Harnesses](harnesses.md#capability-matrix) |
| Locking agents out of agentd's own state | [Sandbox hardening](sandbox-hardening.md) |
| The dashboard editor and the sandbox scribe | [Dashboard → Sandbox Profiles](dashboard.md#sandbox-profiles) |

Start here, then go to those.

## The two layers, and what each one actually covers

An agent's filesystem access is shaped by two mechanisms that are easy to
conflate. They have **different guarantees**, and — this is the part that trips
people up — **neither one covers everything**. You need both.

### Layer 1 — the OS sandbox

bubblewrap on Linux/WSL2, Seatbelt on macOS. This is what tclaude sandbox
profiles render into: Claude Code's `sandbox.filesystem.*` block, or Codex's
managed permission profile.

Within its scope it is a real boundary, enforced by the kernel and inherited by
every child process. No shell trick reaches around it — the policy applies to
the resolved path, however the string was constructed.

**But its scope is Bash commands and their children.** Per Claude Code's
sandboxing docs it "applies only to Bash commands and their child processes",
which includes scripts (`python`, `node`, …). It does **not**, on its own, gate
Claude Code's built-in `Read` / `Write` / `Edit` tools. That gap is real and
verified: the `Write` tool created a file under `~/.tclaude/` on a machine whose
Bash sandbox treated it as read-only. Layer 2 is what closes it.

### Layer 2 — permission rules

Claude Code's `permissions.allow` / `permissions.deny` rules for `Bash`,
`Read`, and `Edit`. These are evaluated **before** a command runs, by matching
the command string and argument structure. `Read` and `Edit` rules gate the
built-in file tools — the hole layer 1 leaves open.

The trade-off is that string matching is not a boundary. It is best-effort at
recognizing file access in Bash commands, and an arbitrary subprocess that
opens a file itself never passes through it at all — a `python`/`node`
one-liner slips straight past. See upstream
[anthropics/claude-code#45200](https://github.com/anthropics/claude-code/issues/45200)
for the discrepancies between the documented and actual matching behavior.

**So: configure both, and understand what each buys.** Layer 1 contains the
subprocess that layer 2 cannot see. Layer 2 gates the built-in tools that layer
1 does not reach. [Sandbox hardening](sandbox-hardening.md) walks through
setting up both for agentd's own state.

### Which denies reach both layers

A profile's rows always render into `sandbox.filesystem.*` (layer 1). tclaude
*also* mirrors them onto layer 2 as `permissions.deny` `Read(…)` / `Edit(…)`
rules — **but only the denies it can express there**, and the exception matters.

**A leaf deny reaches both layers.** `deny ~/.ssh` emits the OS-sandbox rule
*and* `Read(//home/you/.ssh/**)` + `Edit(//home/you/.ssh/**)`, so the built-in
`Read` tool cannot open `~/.ssh/id_rsa` either. This covers the whole portable
common-rule tier — SSH keys, GnuPG, cloud credentials, VCS tokens, browser
profiles.

**A deny with a reopen beneath it reaches layer 1 only.** This is not an
oversight — it is unrepresentable. Claude Code evaluates permission rules
*"in order: deny, then ask, then allow"*, and *"rule specificity doesn't change
the order"*: a deny rule cannot carry allowlist exceptions. So mirroring
`deny ~` would deny the built-in tools **everything** beneath it — including the
agent's own workspace — with no allow rule able to reopen it. The OS sandbox
resolves the same overlap by most-specific-wins, which is exactly why the
deny + reopen shape works there and cannot work here.

> ⚠️ **Under `deny ~`, that broad deny reaches layer 1 only.** Layer 1 does not
> gate the built-in `Read`/`Write`/`Edit` tools on its own, so a
> reopen-under-deny profile does **not** stop the `Read` tool from reading a
> denied path — those tools are left unconfined for it. tclaude logs a warning
> naming each deny it could not mirror.

If you need a specific path to bind the built-in tools under a `deny ~` profile,
add a leaf rule to your own `settings.json` at user scope, where no project's
settings can weaken it:

```json
{
  "permissions": {
    "deny": ["Read(~/.ssh/**)", "Edit(~/.ssh/**)"]
  }
}
```

Note the path syntax differs between the two surfaces. Layer 1 takes plain
directory paths; layer 2 takes gitignore-style patterns where a **single**
leading slash anchors at the settings source, not the filesystem root — so an
absolute rule needs `//`, and `~/` is home-relative. tclaude emits the `//`
form; hand-written user-settings rules are usually clearest as `~/`.

The two protected roots (`~/.tclaude/data`, `~/.claude/sessions`) are defended
on both layers independently of any profile, via the static block
`tclaude setup --install-sandbox-hardening` writes.

### What neither layer covers: MCP

**MCP tools bypass the Bash sandbox entirely.** MCP servers run in the harness
host process over their own transport, not through the sandboxed Bash
filesystem/egress boundary. An agent that cannot see `~/.config/gh` and has no
reachable `gh` binary may still be able to file a GitHub issue through an MCP
server, and an agent with `network_access: none` may still reach the network
through one.

If MCP reachability matters to your threat model, control it where MCP servers
are configured — not in the sandbox profile.

## Experimental `tclaude-layer` on Linux and macOS

`tclaude session new --sandbox-impl tclaude-layer` runs the tool-executing
harness process inside a tclaude-owned operating-system sandbox: a bubblewrap
mount namespace on Linux or a Seatbelt profile on macOS. For Claude Code and
Codex this is the pane harness itself. For OpenCode on Linux and macOS it is
the agentd-owned `opencode serve` executor; the attach pane stays outside. The
default remains
`harness-builtin`, which preserves the current harness-native behavior exactly.
The implementation choice is recorded with the conversation, so a flagless
resume uses the same layer.

That legacy name is not a claim that every harness contains an OS sandbox.
Claude Code and Codex do; OpenCode does not. For OpenCode, leaving the
implementation unset preserves its historical command filter + warning, while
an explicit `harness-builtin` pin is invalid: its access-control mode is a
command filter, not confinement. Use `tclaude-layer` on a supported host, or
spawn with the sandbox off.

### Selecting it per spawn

The same choice is available wherever an agent is spawned, so the layer does not
have to be driven through `session new` by hand:

- `tclaude agent spawn --sandbox-impl tclaude-layer`
- a spawn profile's **Sandbox impl** field, which every agent launched through
  that profile inherits
- the dashboard spawn dialog's **Sandbox impl** row

Like every other launch field it resolves through one precedence chain, highest
first: the explicit flag or dialog selection, then `--profile`, then the group's
default spawn profile, then the global default profile, then the harness
default. Leaving it unset everywhere keeps the field unpinned, so a spawn that
says nothing behaves exactly as it did before the field existed. Claude Code
and Codex then keep their built-in sandbox behavior; OpenCode keeps its command
filter plus the explicit no-confinement warning.

Unset and `harness-builtin` are different values on purpose. Unset falls through
to the next tier; an explicit `harness-builtin` **pins** the legacy
implementation, which is how one spawn opts out of a group default that would
otherwise put it on the experimental layer. That explicit pin is accepted only
when the selected harness really owns an OS sandbox; OpenCode refuses it rather
than manufacturing a confinement claim.

An unavailable **host** refuses the launch outright, from whichever tier the
value came from, naming the missing capability. It never falls through to
`harness-builtin`. The dashboard discloses availability up front, but that
disclosure never replaces the refusal: the option stays selectable, because
authoring a profile that pins the layer for another machine — or for after
`bwrap` is installed — is legitimate.

### Experimental `stacked` on Linux

`--sandbox-impl stacked` is the explicit two-wall option for Claude Code and
current-backend Codex on Linux. It uses the same frozen
`TclaudeLayerLaunchSpec` as `tclaude-layer`, but keeps the harness's real OS
sandbox active underneath it:

- Claude Code is forced to sandbox `on` with
  `enableWeakerNestedSandbox: false`. A launch-owned, credential-free loopback
  stub drives the exact pinned Claude CLI through one deterministic Bash tool,
  proving its embedded SRT's bwrap/seccomp behavior without calling a model.
- Codex is forced onto a launch-unique managed profile with
  `features.use_legacy_landlock=false`; launch requires a real
  `codex sandbox -P …` bwrap round-trip. Legacy Landlock is not a substitute.

Each fresh launch and resume probes live *inside the exact outer mount/network
spec* before the pane is committed. The probe must write an allowed marker and
fail a denied write; the Claude probe also verifies SRT's AF_UNIX seccomp deny.
The harness executable is copied into launch-owned staging, probed there, then
reopened and re-hashed immediately before bubblewrap. The outer relay binds the
open descriptor at a private read-only executable path, so the final process
executes the proved bytes rather than resolving `PATH` again. For Claude, every
managed-policy JSON file is likewise snapshotted and descriptor-bound into a
fresh read-only `/etc/claude-code`; unreadable policy and any override that
disables or weakens the required inner posture fail closed. The successful
lock is recorded only after the relay reports that this final binding has been
materialized. A missing engine, changed executable, unprovable effective
policy, failed round-trip, OpenCode selection, or macOS nested Seatbelt attempt
produces:

```text
stacked requested — refused: missing capability <name>: <detail>; refusing rather than falling back to tclaude-layer or harness-builtin
```

The dashboard always shows the option, warns inline when the selected
host/harness is incapable, and never clears an explicit stacked selection
during a harness switch. Its short-lived availability result is disclosure
only; the live launch probe is authoritative.

A successful stack shows `🔒²`. Its compact tooltip reports `Status: ON` and
identifies the implementation as `CC+TClaude` or `Codex+TClaude`; the detailed
mechanism, posture, and caveats remain in this guide rather than the row hover.
Linux host-open retains the ambient-host-Unix-socket caveat; isolated uses its
constructed root and network/PID isolation. Known namespace gaps are warned
about, never repaired by silently widening the outer policy.

Claude Code and Codex are supported on Linux and macOS. OpenCode is supported
on both platforms in the host-open posture: agentd wraps its server rather
than its attach pane. Its Linux control-plane engine can cross an isolated namespace
through an owned Unix relay, without exposing the socket path inside the server
wall or routing agentd through host TCP. That engine does not enable a posture:
OpenCode isolated profiles still refuse because OpenCode requires hosted model
traffic, filtered remains reserved because no proxy-backed applier exists, and
the Unix relay remains Linux-only. Linux uses `bwrap` from `PATH` and requires
working unprivileged user namespaces. macOS uses
`/usr/bin/sandbox-exec` for filesystem confinement and for the
isolated-with-agentd network boundary. If any required capability is missing,
tclaude refuses the launch instead of silently falling back.

On Linux the layer does not unshare the IPC namespace. The host-open posture
also retains the host PID namespace; the isolated posture unshares PIDs as part
of closing ambient socket access. Under `tclaude-layer`, the harness's own OS
sandbox is disabled inside the wrapper. The explicit Linux-only `stacked`
implementation above is the reviewed exception for Claude Code and Codex.
OpenCode's ordered tool permission rules remain enabled as defense in depth,
but OpenCode has no stacked contract.

### OpenCode state under `tclaude-layer`

A new OpenCode agent using `tclaude-layer` receives durable per-agent mutable
state under
`$XDG_DATA_HOME/tclaude/opencode-agents/<agent-id>/` (falling back to
`~/.local/share`). The launch contract hides the parent and reopens only that
raw, validated stable-agent-id child. It also hides the ambient OpenCode data,
cache, and state directories. On Linux all four XDG bases point under the
private root, with ambient global config mounted at the private config
location. On macOS data/cache/state remain private, while `XDG_CONFIG_HOME`
names the real canonical host config base when an ambient global OpenCode
config directory exists, because Seatbelt cannot project one path onto another.
With no ambient global config, the empty private config base remains in use.
The OpenCode global config directory, when present, and ambient `~/.opencode`
install tree are daemon-final read-only on both platforms, so project config
keeps OpenCode's native higher precedence. In the ambient-config case,
non-OpenCode config writes inside the wall are not redirected to the private
root; they target the real host config base and remain governed by the
filesystem policy.

OpenCode 1.18.5 and 1.18.6 try to create `~/.opencode/.gitignore` during
instance bootstrap, and 1.18.6 aborts when that missing-file write meets the
read-only install mount. As a compatibility shim, the daemon creates only that
one file before composing the read-only bind, with OpenCode's exact contents
and mode `0600`. An existing regular file is left byte-for-byte untouched;
symlinks and other file types refuse the launch. Agents still receive the
entire install tree read-only.

On macOS the same 1.18.6 bootstrap also runs for the exact global config app
directory selected by the launch contract before Seatbelt makes it read-only.
It creates only the missing `.gitignore`, never overwrites an existing entry,
and refuses the launch if that prerequisite cannot be established. When this
one-time compatibility write touches the real ambient host config, agentd logs
the path explicitly.

On first allocation, regular ambient `auth.json` and `mcp-auth.json` files are
copied once into the private data root with mode `0600`. Symlinks are refused
and an existing private credential is never overwritten. This avoids an
initial login while keeping credentials out of the cross-agent mutable surface.
Credential refresh state then belongs to that agent. In particular, a provider
using rotating, single-use refresh tokens can make another agent's older
private copy require a new login; tclaude does not synchronize or broker those
tokens.

OpenCode agents created before this isolation contract retain their existing
shared XDG state so their conversations remain usable. Their resolved launch
disclosure is:
`OpenCode state isolation: legacy shared XDG retained for this existing conversation; start a new agent for per-agent-private state.`
Missing or corrupt durable allocation state refuses a replay instead of
silently returning to shared state. Harness-builtin OpenCode launches are
unchanged.

The Linux host-open posture starts with a read-only view of the host root; the
isolated posture uses the constructed root described below. Both give `/dev`,
`/proc`, and `/tmp` fresh sandbox views. Both platform appliers enforce four
load-bearing precedence classes:

1. Reopen the launched harness's state root, workspace/Git administration
   paths, and declared agent directories for writing. These launch-contract
   paths survive an ordinary deny on an ancestor such as Home.
2. Replay the resolved profile's ordered mount plan exactly. An ordinary rule
   at or below the selected harness's state root is refused rather than
   launching a harness that cannot persist.
3. Keep `sandboxpolicy.ProtectedPaths()` hidden above launch-contract repairs,
   so `~/.tclaude/data` and `~/.claude/sessions` stay private. Nothing reopens
   beneath a protected root: `normalizeFilesystem` refuses any read/write rule
   that intersects one, so no such plan entry can exist. TCL-791 removed
   break-glass, the one former exception.
4. Hide the tclaude tmux socket directory last, so no rule can grant host tmux
   control.

Later plan entries shadow earlier ones, allowing a more-specific allow to
reopen beneath a deny and a more-specific deny to hide beneath an allow.
Missing read/write bind sources are skipped without creating anything on the
host; hide entries are still applied. Harness-owned state is the deliberate
exception: before a wrapped OpenCode server starts, tclaude materializes the
frozen `~/.opencode` and XDG data/cache/config/state directories that the launch
contract explicitly names. It never creates operator-authored profile paths.
On Linux the wrapper also starts a new terminal session to prevent
terminal-input injection. A tclaude process outside bubblewrap stays in the
pane's foreground process group and relays only `SIGWINCH` to bubblewrap's
disconnected process group, so TUIs still learn when the inherited terminal's
size changes. The relay does not proxy terminal I/O or give the sandbox a
controlling terminal. Seatbelt does not detach the process from its terminal
session and needs no relay.

### Stacked refuses on AppArmor-restricted hosts

On a stock Ubuntu 24.04 or newer host, `--sandbox-impl stacked` refuses at
launch even though everything it needs looks installed. This is the host, not a
broken install, and the refusal is the correct outcome.

**Symptom.** The launch ends with a named capability refusal —
`stacked_claude_inner_policy` or `stacked_claude_srt_probe` for Claude Code
(which one depends on how far the inner harness got), `stacked_codex_bwrap_backend`
for Codex — and the detail carries an inner `bwrap` complaint along the lines
of *No permissions to create a new namespace*. Ordinary single-layer
`tclaude-layer` on the same host works fine: the outer wall is not the problem.

**Cause.** Ubuntu ships and enforces an AppArmor policy,
`/etc/apparmor.d/bwrap-userns-restrict`, whose whole purpose is to let `bwrap`
create a user namespace while denying capabilities to what runs inside it. The
outer bwrap is permitted; its children are pivoted into the policy's
`unpriv_bwrap` child profile, whose rule is `audit deny capability,` — all
capabilities, not one. The inner bwrap needs `CAP_SYS_ADMIN` *within the user
namespace it just created* — normally free, because it owns that namespace —
but AppArmor's `capable` check applies to the profile regardless of namespace
ownership. So the second wall can never be built while that policy is
enforcing. The kernel audit line, which names whichever capability was asked
for first, is the proof:

```text
apparmor="DENIED" operation="capable" class="cap" profile="unpriv_bwrap" comm="bwrap" capname="sys_admin"
```

**What is not the cause.** The userns sysctls are a red herring here.
`kernel.apparmor_restrict_unprivileged_userns=0` alone does *not* fix it, and
the denial reproduces with `kernel.unprivileged_userns_clone=1`: the deny is
profile-local. CI is blind to this failure because its runners do not ship the
policy, so a green nested-namespace assumption pin says nothing about your
laptop.

**Diagnose it without root.** If the policy file exists and nothing disables
it, this is almost certainly what you are hitting:

```bash
ls -l /etc/apparmor.d/bwrap-userns-restrict   # present on stock Ubuntu 24.04+
ls -l /etc/apparmor.d/disable/                # an entry here means it is unloaded
ls -l /etc/apparmor.d/force-complain/         # an entry here means it only logs
grep -n 'flags=(' /etc/apparmor.d/bwrap-userns-restrict   # `complain` here means the same
```

Confirm it, with root, from the audit log and the loaded-profile list:

```bash
sudo dmesg | grep -F 'profile="unpriv_bwrap"'
sudo aa-status | grep -F bwrap
```

**Workaround, and what it costs.** Both halves are required, and the reason is
worth understanding before you run them: the policy is also what *grants*
`/usr/bin/bwrap` its `userns` permission. Unload it and bwrap becomes
unconfined — at which point the global unprivileged-userns restriction, which
the profile had been standing in front of, applies to it. So unloading alone
breaks the outer wall too, and the sysctl alone leaves the child-profile deny
in place. Persist the sysctl and unload the policy:

```bash
echo 'kernel.apparmor_restrict_unprivileged_userns = 0' \
  | sudo tee /etc/sysctl.d/99-tclaude-userns.conf
sudo sysctl --system
sudo ln -s /etc/apparmor.d/bwrap-userns-restrict /etc/apparmor.d/disable/
sudo apparmor_parser -R /etc/apparmor.d/bwrap-userns-restrict
```

This is a **host-wide security trade-off, not a tclaude setting**: it removes
Ubuntu's defence-in-depth around unprivileged user namespaces for every process
on the machine, including ones that have nothing to do with tclaude. Stacked is
experimental; single-layer `tclaude-layer` needs none of this. Decide
accordingly, and prefer the temporary form when you only want to observe
stacked once:

```bash
sudo aa-complain /etc/apparmor.d/bwrap-userns-restrict
sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0
# … run the stacked launch …
sudo aa-enforce /etc/apparmor.d/bwrap-userns-restrict
sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=1
```

**Reversal** of the persistent form is symmetric — reload the policy and drop
the sysctl file:

```bash
sudo rm /etc/apparmor.d/disable/bwrap-userns-restrict
sudo apparmor_parser -r /etc/apparmor.d/bwrap-userns-restrict
sudo rm /etc/sysctl.d/99-tclaude-userns.conf
sudo sysctl --system
```

Expect the dashboard's stacked *availability* to look green on such a host
anyway: that disclosure resolves the harness engine only, and the live nested
round-trip at launch is the authority. What the dashboard does add is a warning
that stacked is *likely* blocked when it sees this shape — the policy file
present, with no `disable/` or `force-complain/` entry — with a link here, and
the launch refusal names the same likely cause when its own output matches. Both
are guesses made from what an unprivileged daemon can read, not determinations:
agentd cannot read `dmesg` or `aa-status`. The supported posture for these hosts
is still open; what stands today is that stacked fails closed and says so.

### macOS Seatbelt filesystem posture

The Darwin applier compiles the final four-class result into a
`sandbox-exec` profile. Seatbelt rule selection depends on predicate
specificity—a specific allow can reopen a broader deny—so it does not replay
the plan as a textual sequence of deny and allow rules. Instead it emits
deny-only read/write regions whose predicates carve out the final narrower
reopens. This is how a writable child beneath a hidden ancestor keeps the same
later-shadows-earlier meaning as the Linux mount plan.

The profile otherwise uses `allow default`. In the host-open posture, host
networking and ambient Unix sockets retain their host behavior. In the isolated
posture, deny rules block outbound connections and listener binds, with only
the canonical agentd Unix socket and its surviving alias spellings excepted
from outbound denial. A `network-inbound` deny is intentionally absent:
current hardware does not make it a reliable AF_UNIX reply block, and reply
suppression is not part of the isolated contract. Inbound listener prevention
therefore rests on the bind deny. A listening descriptor passed through the
trusted agentd daemon with `SCM_RIGHTS` is outside this boundary's threat model.
The earlier reply-loss finding did not reproduce on current CI macOS; see
TCL-777's hardware evidence before proposing an inbound deny. Mach services
remain outside this slice. The filesystem baseline is read-only, with narrow
launch-contract write roots plus the Darwin process runtime paths
(`/dev/null`, tty/pty paths, `/dev/fd`, and the canonical `$TMPDIR` beneath
`/private/var/folders`). Those runtime exceptions apply only to the baseline;
an operator-authored read-only or hidden region over one of them still wins.
Protected tclaude/harness state and the tmux socket are denied for reads and
writes, while the canonical agentd socket remains readable and connectable so
hook/status-line brokering continues to work.

A hidden path remains present in the host directory tree. Seatbelt denies
opens, writes, and Unix-socket connects at that path with `EPERM`; it does not
make a directory listing omit the name. The same descendant reopens apply to
reads and connects, which keeps the agentd socket reachable beneath an
ordinary ancestor hide while the final tmux hide has no exception. This is the
intended fail-loud behavior: a hidden database path cannot accept writes into
phantom state even though its name may still be enumerated.

Profile paths are passed through `sandbox-exec -D` parameters rather than
interpolated into SBPL. Resolved symlink targets and any `MountPlan.Aliases`
that survived profile resolution are both covered; no constructed-root symlink
is created on Darwin. Case/NFC-equivalent spellings are merged only after
Darwin `lstat` file identity confirms matching device and inode, so a
case-sensitive APFS volume keeps distinct objects distinct. Persisted registry
profiles may already have discarded their operator spelling; that separate
limitation remains tracked by TCL-762.

The Darwin boundary is partial rather than unverified: Seatbelt filesystem
policy is active and paths remain enumerable. The compact row tooltip does not
repeat that caveat. Host-open retains the host network and ambient Unix sockets;
isolated blocks network operations except the agentd socket but still has no
PID isolation or constructed root. `sandbox-exec` is deprecated but still
functional and is the mechanism for this experimental slice. A future
replacement would call libsandbox/`sandbox_init` directly; the fallback is not
part of this slice.

### Isolated-with-agentd network posture

The profile's `network_access` field maps differently because the outer layer
wraps the whole harness process, not only its tool executions:

- omitted (`inherit`) and `internet` use the **host-open** posture. This
  preserves the walking skeleton: the host network namespace and read-only
  host root remain visible, including host localhost services and ambient
  filesystem Unix sockets. `internet` therefore means more here than a
  tool-only Internet switch; the launch record calls this `host network` rather
  than repeating the profile word.
- `none` requests **isolated-with-agentd**. Bubblewrap creates a fresh network
  namespace (with loopback up), a fresh PID namespace, and a constructed
  filesystem root. Bubblewrap remains PID 1 so orphaned harness subprocesses
  are reaped. There is no blanket bind of `/`: the static OS surface is
  read-only (`/usr`, `/bin`, `/sbin`, `/lib*`, `/etc`, and `/opt`, accounting
  for merged-usr symlinks); `/dev` and `/proc` are fresh, `/tmp` is tmpfs, and
  `/run`, `/var`, `/srv`, `/media`, `/mnt`, `/boot`, and `/root` are absent.
  Home paths exist only where the launch contract or ordered profile plan binds
  them. The canonical `~/.tclaude/api/agentd.sock` is bound read-only as a
  launch-contract path.

The isolated posture has an explicit platform delta:

| Property | Linux (`bubblewrap`) | macOS (`Seatbelt`) |
| --- | --- | --- |
| Public and host-loopback connectivity | Unavailable outside the private network namespace | Refused by Seatbelt network operations |
| Harness-internal localhost server | Works on the private namespace's own loopback | Refused: Darwin loopback is host loopback, so reopening it would also reopen the IDE-bridge/host-service surface |
| Agentd | Canonical socket bind-mounted into the constructed root | Canonical socket and surviving aliases are the only outbound Unix-socket exceptions |
| Processes and filesystem root | Isolated PIDs and a constructed root | No PID isolation and no constructed root; hidden paths remain enumerable |

The stricter Darwin localhost behavior is deliberate. If a real harness later
requires an internal localhost server, the remedy is a harness-descriptor
capability plus launch-time refusal for Darwin isolated mode. It must not gain a
loopback exception that silently reopens host services.

When a profile path reaches resolution with a symlinked spelling, the
constructed root recreates the highest symlinked component so tools can keep
using that spelling while the mount plan binds the resolved target. There is a
temporary authoring limitation: registry profiles are canonicalized when they
are saved, so their original symlink spellings are not yet preserved for later
resolution. Aliases are therefore materialized only when the spelling is still
present in the value passed to resolution; host-open continues to inherit
aliases from its read-only host root.

The isolated posture blocks TCP egress and host-loopback TCP. It also closes
the Linux abstract Unix-socket namespace. PID isolation prevents the harness
from escaping the constructed root through a host process's
`/proc/<pid>/root`. A filesystem Unix socket is visible only when it was
explicitly bound, or when an operator-authored filesystem grant re-exposes a
parent directory under the normal most-specific-wins policy. This is the
constructed-root posture's socket boundary; the compact badge does not report
socket fidelity. The reserved `filtered` posture will eventually cover
proxy-backed host/domain and host-loopback allowlists; no proxy is implemented
today.

Host-loopback isolation also severs editor integrations that connect over a
localhost WebSocket, including Claude Code's IDE bridge. Choosing this posture
therefore gives up that integration as well as host-local model servers.

`network_access: none` also isolates the harness's own model transport.
Claude Code and other hosted-only harnesses are refused because they would be
dead on arrival. Codex proceeds only when the resolved sandbox profile contains
`TCLAUDE_OFFLINE_MODEL=1`. That value is a precise operator assertion that the
model transport **functions across the selected platform's isolated
boundary**—for example the allowlisted agentd-style Unix-socket transport, an
inherited file descriptor, or a workflow that needs no model traffic. It does
not mean merely “a local model exists”: host-TCP Ollama/LM Studio-style servers
remain unreachable, across Linux's new network namespace or behind Darwin's
Seatbelt network denies. tclaude deliberately does not infer the assertion from
Codex `--oss` or a model name.

The remaining limitation in the host-open posture is explicit:

- Ambient host Unix sockets remain connectable through the read-only root.
  Privileged daemon sockets such as `docker.sock` or containerd-class sockets
  are host-root-equivalent escapes when present. This is partial fidelity even
  though the established outer implementation earns the row's `🔒`; the compact
  tooltip does not restate the caveat. The open posture deliberately does not
  maintain a misleading dangerous-socket blocklist.

## Hook and status-line callbacks under the layer

Installed hook processes and the status line both write the tclaude
database, which the layer's baseline necessarily hides. A `tclaude-layer`
launch therefore exports `TCLAUDE_HOOK_BROKER=agentd`, and both callbacks
POST to the daemon over `~/.tclaude/api/agentd.sock` instead of writing
state themselves — hook events to `/v1/whoami/hook`, status-line renders to
`/v1/whoami/statusline`. agentd applies each one host-side by calling the
same function a direct callback would, so a wrapped agent's status,
sub-agent ledger, directory tracking, desktop notifications, context meter,
model, effort, cost and dashboard location behave as they do for any other
launch. Every other launch still writes directly and is byte-for-byte
unaffected.

The daemon identifies the calling session from host pids it recorded at
spawn, never from anything the caller sends; a `TCLAUDE_SESSION_ID` in the
request is accepted only as a cross-check and a disagreement is refused.

A pid is not unique over a machine's lifetime, so a long-dead session's row
can be recorded against the same number as a live agent's pane. On the
brokered paths, when more than one row claims the pid the daemon replaces a
winner whose tmux session it can see is gone with one it can see is alive.
It is a repair of a demonstrably dead answer, not a re-ranking: with nothing
provably alive, with tmux unreachable, or with no recorded tmux session to
judge by, resolution is exactly what it was before. It matters because
picking the corpse refuses a live agent's callbacks, and that failure
sustains itself — the live row is advanced mainly by the very callbacks
being refused.

The repair covers the brokered hook and status-line paths and the
`tclaude-layer` ancestry walks. For OpenCode, CLI identity may cross at most 16
wrapper ancestors only when the matching runtime row explicitly records
`tclaude-layer`; the candidate still has to pass the server endpoint-ownership
proof. Harness-builtin OpenCode rows retain the exact/one-parent lookup.

A run of refusals is surfaced on the dashboard rather than only logged: the
agent's row carries a `🚫` badge saying the rest of the row has stopped
being updated. It is always attributed to the row the daemon resolved,
never the session id the refused request claimed. See
[Groups](dashboard.md#groups).
For the status line that resolved identity also replaces the environment
variable the direct path trusts, so which *session row* a brokered render
may write is decided by evidence the caller cannot assert. The second half
of the gate — whether the *conversation* the render names is one that row
tracks — is unchanged from the direct path, including its deliberate
fail-soft when the payload names no conversation at all. That fail-soft is
not an escalation: the only row reachable is the caller's own, which its
legitimate status line already writes.

A status line re-renders several times a second, so it is not brokered
per render. A render whose payload is byte-identical to the last one sent
records nothing, because it would record exactly what is already there; a
render whose payload differs goes out immediately, with no minimum
interval in front of it — the pre-compact guard reads the context snapshot
this path writes, so throttling the write side would hand the guard stale
evidence. The two cosmetic reads it needs back (the auto-compaction pin
and the temporary-sandbox badge) are cached for a few seconds in the
pane's own `/tmp`, which inside the layer is a private tmpfs that dies
with the pane. Nothing correctness-bearing reads from that cache.

Both endpoints share a per-agent ceiling — 20 requests per second, 10 MiB
per request — keyed on the resolved session row, so one agent in excess
cannot starve its peers. Enforcement is opt-in via `broker.enforce_limits`
in `config.json` (or the dashboard's config tab). With it off, which is the
default, excess is still measured and logged saying what it *would* have
refused. These are denial-of-service ceilings, not traffic shaping.

The routing is driven by the launch marker rather than by catching a write
error, and the fallback probe for a launch that arrived without the marker
looks for the absence of the database file rather than for a failure. That
was originally the only workable test: the hidden `~/.tclaude/data` used to
be an empty *writable* tmpfs, so a direct write from inside the wall
silently populated a throwaway database instead of failing. Hidden
protected roots are now remounted read-only, so such a write fails
outright and no throwaway database can appear.

## Platform assumption tests

`pkg/claude/sandboxassumptions` is the executable inventory of bubblewrap and
Seatbelt behavior that production relies on. Each named subtest records the
specific production functions whose correctness depends on that behavior and
exercises the operating-system mechanism directly. It never calls tclaude's
mount-plan, bubblewrap-argument, Seatbelt-profile, or smoke-test renderers to
make an assumption pass.

An assumption test is appropriate when production depends on behavior supplied
by bubblewrap, the Linux kernel, or Seatbelt: for example non-recursive
read-only remounts, `--new-session` terminal semantics, or the way a Seatbelt
file deny remains separate from an AF_UNIX connect deny. Pure argument/profile
rendering remains an ordinary unit test. A tclaude composition or
harness-distribution regression remains beside the code or in an end-to-end
smoke. Do not turn a scheduler race or one observed errno into a platform
promise: assert the stable operation or round-trip production needs.

The behavioral suites are env-gated outside their platform jobs. In CI they run
under the same prerequisites as the real smokes and are hard gates: an unrun or
skipped suite is red, as is a missing/renamed top-level test or a command that
exits successfully without the explicit top-level `--- PASS:` line. Helpers
are Go test re-execs with bounded handshakes; they add no interpreter
dependency and do not use sleeps as correctness evidence.

Run the Linux assumptions on compatible hardware with:

```bash
TCLAUDE_SANDBOX_ASSUMPTIONS=1 \
  go test ./pkg/claude/sandboxassumptions \
    -run '^TestBubblewrapAssumptions$' -count=1 -v -timeout=180s
```

Run the equivalent `TestSeatbeltAssumptions` command on macOS. Darwin CI is the
authoritative hardware route for Seatbelt changes. When a probe discovers a
new platform behavior that production will rely on, preserve the mechanism
claim here instead of leaving it only in a throwaway workflow; keep the
production-level round-trip in its smoke when both layers answer different
questions.

Both platform smokes are hard CI gates. The Linux job disables Ubuntu's
AppArmor restriction on unprivileged user namespaces for its ephemeral runner,
verifies bubblewrap can create the namespace, and runs the real bubblewrap smoke. If
the unlock or capability probe stops working, the job fails with runner
diagnostics instead of skipping. To repeat the smoke on a compatible Linux host
with `bwrap` installed:

```bash
mkdir -p "$HOME/.cache/tclaude"
go build -o "$HOME/.cache/tclaude/tclaude-sandbox-v2-smoke" .
TCLAUDE_SANDBOX_V2_SMOKE=1 \
  TCLAUDE_SANDBOX_V2_TCLAUDE_BINARY="$HOME/.cache/tclaude/tclaude-sandbox-v2-smoke" \
  go test ./pkg/claude/session -run '^TestTclaudeLayerHostSmoke$' -count=1 -v -timeout=120s
```

The same Linux CI job installs a pinned OpenCode binary and hard-gates the
server-authoritative smoke. That test starts a real wrapped `opencode serve`,
connects a real unwrapped `opencode attach`, verifies the permission patch,
executes the real bash tool across an allowed and denied path, and requires
`tclaude agent whoami` from that tool subprocess to resolve the exact managed
agent identity. It has no user-namespace capability skip. To repeat it after
installing OpenCode:

```bash
TCLAUDE_OPENCODE_LAYER_SMOKE=1 \
  TCLAUDE_SANDBOX_V2_TCLAUDE_BINARY="$HOME/.cache/tclaude/tclaude-sandbox-v2-smoke" \
  go test ./pkg/claude/agentd -run '^TestOpenCodeTclaudeLayerExecutorSmoke$' -count=1 -v -timeout=120s
```

The Darwin job likewise verifies that Seatbelt enforces its deny-write probe
before running the real filesystem smoke and the OpenCode executor smoke, and
fails if either named test does not report its explicit top-level PASS line.
The CI job installs the deliberately pinned `opencode-ai@1.18.6` used by the
Linux executor smoke. To repeat the filesystem smoke on a macOS host:

```bash
go build -o "$TMPDIR/tclaude-sandbox-v2-smoke" .
TCLAUDE_SANDBOX_V2_SMOKE=1 \
  TCLAUDE_SANDBOX_V2_TCLAUDE_BINARY="$TMPDIR/tclaude-sandbox-v2-smoke" \
  go test ./pkg/claude/session -run '^TestTclaudeLayerDarwinSmoke$' -count=1 -v -timeout=120s
```

After installing that pinned OpenCode binary, repeat the Darwin executor smoke
with:

```bash
TCLAUDE_OPENCODE_LAYER_SMOKE=1 \
  TCLAUDE_SANDBOX_V2_TCLAUDE_BINARY="$TMPDIR/tclaude-sandbox-v2-smoke" \
  go test ./pkg/claude/agentd \
    -run '^TestOpenCodeTclaudeLayerDarwinExecutorSmoke$' -count=1 -v -timeout=120s
```

## The shape that does the work: deny + reopen

There is exactly one mechanism, the profile's `filesystem` table. Strictness is
composed from ordinary rows:

```json
[
  { "path": "~",                "access": "deny"  },
  { "path": "~/git/myproject",  "access": "write" },
  { "path": "~/go",             "access": "read"  }
]
```

A `read`/`write` row strictly beneath a `deny` row is a **reopen-under-deny**.
It is the interesting shape, and it is **capability-gated at launch**: not every
harness/mode combination honors "most specific rule wins", and one that does not
would run your strict-looking profile with a broad baseline. tclaude refuses
that launch instead.

| Harness | Reopen-under-deny |
|---------|-------------------|
| Claude Code, sandbox `on` | ✅ supported |
| Claude Code, sandbox `inherit` / `off` | ❌ refused — the deny and the reopen may both be dropped |
| Codex, managed `tclaude-agent` profile, **Linux**, split-policy probe verified | ✅ supported |
| Codex, macOS | ❌ refused — a deny mask dominates narrower reopens ([openai/codex#21081](https://github.com/openai/codex/issues/21081)) |
| Codex, legacy Landlock, or a raw `--sandbox` mode | ❌ refused |
| Any other harness | ❌ refused |

Two consequences that surprise people:

- **The gate keys on the rules tclaude will *emit*, not the rows you authored.**
  A bare `deny ~` with no reopens of your own still becomes a split policy,
  because the launch contract adds its own reopens (below). So `deny ~` alone is
  gated exactly like a hand-written reopen.
- **A deny row is not a promise by itself.** Under Claude `inherit`, the rule is
  emitted but the sandbox is only enabled if your own `settings.json` enables
  it. Under `off` it is dropped. This is why the gate insists on `on`.

The dashboard's **Add common rule → Deny access to the Home directory** inserts
the `deny ~` row for you. It stores nothing: afterwards it is an ordinary,
editable row.

## What tclaude reopens for you, and what you must author

When a deny covers paths tclaude needs to keep usable, it pairs read reopens
automatically. That list is **short and closed**:

- the workspace / cwd, and the git worktree write dirs (narrowed, under a deny,
  to the workspace plus the daemon-verified git common/admin paths);
- the profile's own `write` grants;
- the agent-owned directories declared in `agent_directories`, at their
  materialized paths under tclaude's cache tree;
- the agentd Unix socket, so `tclaude agent …` keeps working (allowed
  unconditionally, by a separate per-harness mechanism);
- on Codex only, the Codex executable itself — and only when the isolated
  split-policy probe proves the reopen is required.

**Everything else is yours to enumerate.** In particular:

> ⚠️ **tclaude's own binary is not implicitly reopened.** Under `deny ~`, if
> `tclaude` lives somewhere in Home that you did not reopen, the agent will be
> able to reach the agentd socket and still get `tclaude: command not found`.
> Reopen whatever directory holds the binary — commonly `~/go/bin`, or the
> version-manager install root.

## Gotchas worth knowing before you debug one

### Writes under a deny can fail *silently* (Linux)

Observed under bubblewrap: a write to a denied path returned **exit 0**, stayed
visible to the rest of that same command invocation, and was gone by the next
one — no `EPERM`, no `EROFS`. The write landed in a throwaway layer of the mount
view rather than being refused.

The practical damage: a build that writes into `$HOME` reports success and
loses its output. If an agent's work keeps evaporating with no error, suspect
this first. On macOS, Seatbelt denies the syscall instead, so expect an ordinary
permission error there.

### `ls ~` shows only what you reopened (Linux)

Under `deny ~` on bubblewrap, listing home shows the reopened paths and nothing
else. The rest of home is not *hidden* — it is **not mounted**; bubblewrap
bind-mounts the allowed paths and builds the view from those.

Seatbelt has no mount namespace: it filters syscalls against a path policy, so
on macOS directory entries can still be enumerable while access to them is
refused. Do not use "the listing looks short" as your macOS confirmation that a
deny is in effect — try to read something.

### `$PATH` is a string; the sandbox policy decides

`command not found` for a tool that is plainly on `$PATH` is the normal symptom
of a denied install root, not a broken profile. Version-manager installs are the
usual casualty: under `deny ~`, everything under `~/.local/share/mise/installs`
(and the equivalents for nvm, pyenv, asdf) disappears — taking `go`,
`golangci-lint`, `node`, `gh`, `kubectl`, `terraform`, `gcloud`, and friends
with it.

**Reopening the caches is not enough to build.** `~/.cache/go-build` and
`~/go/pkg/mod` being readable does not help if the `go` binary itself is under
the deny. Reopen the toolchain install root too when the agent must build or
lint.

Note the tension with the **Deny audited default toolchain-cache locations**
common rule, which denies `~/.local/share/mise` among others: it is the right
default for an agent that only reads code, and the wrong one for an agent that
compiles it.

### Rows are directories, not files

A non-directory path is rejected outright. Home-level dotfiles —
`~/.gitconfig`, `~/.netrc`, `~/.npmrc`, shell rc files — therefore **cannot be
reopened individually** under `deny ~`; they stay denied. Losing `~/.gitconfig`
(and with it Git's identity and credential helper) is the usual first symptom.
Relocate the configuration into a directory you reopen, or supply it through the
profile's `environment`.

### Git sees Claude Code's built-in denies as device nodes (Linux)

Claude Code's own sandbox (independent of any tclaude profile) denies some
paths by bind-mounting `/dev/null` over them on Linux: `.git/config.worktree`
and `.git/config.lock`, `.gitmodules`, ambient host dotfiles, and its
protected `.claude/` settings/runtime paths (`settings.json`,
`settings.local.json`, `loop.md`, …). (Other protected paths, such as
`.git/hooks` and `.git/config` itself, stay ordinary files and directories —
those denies are enforced without a stub.) Git then sees character device nodes
where it expects regular files, which produces three symptoms inside a
sandboxed agent:

- Harmless warnings on some commands, e.g. `git fetch`:
  `warning: unable to access '….git/config.worktree': Permission denied` and
  the same for `.gitmodules`.
- Stubs not covered by an ignore rule show up as untracked in `git status`.
- `git add -A` and `git add .` **fail hard**
  (`error: … can only add regular files, symbolic links or git-directories`).

Git itself works — fetch, commit, diff, stash, worktree operations are all
fine. These denies are Claude Code built-ins, enforced by the OS: a tclaude
profile cannot reopen them, and the only documented off-switch disables *all*
of Claude Code's filesystem isolation, which tclaude deliberately relies on.

Mitigation: stage specific paths instead of bulk-adding. In the tclaude repo
itself the stubs are covered by the committed `.gitignore` (see the "Sandbox
artifacts" block there), so `git status` stays clean and `git add -A` works;
in arbitrary other repos, "stage specific paths" is the practical rule.

### You cannot reopen a directory containing a protected root

`~/.claude` contains `~/.claude/sessions`, which is protected, so an ordinary
row over `~/.claude` is rejected — ancestors count. Reopen the specific children
the harness needs (`~/.claude/plugins`, `~/.claude/skills`, …), and expect to
find that list empirically.

`~/.codex` is *not* protected and can be reopened normally — and must be, under
a denied Home, or managed Codex agents are stranded.

The practical consequence of these last two: **a denied Home is materially
easier to run under Codex than under Claude Code today.**

## Composition: which profile wins

Two independent layering steps.

**Within one profile — `includes`.** Included profiles apply first in listed
order, then the including profile's own rows. For the *same exact path or env
name*, the later layer wins — so a local grant can override an included deny.
This is authoring convenience inside one operator-owned registry.

**Across scopes — global default → group → explicit per-spawn.** This is
**not** last-wins:

- **Filesystem:** a canonical-path union where **`deny` dominates `write`
  dominates `read`, independent of tier.** A per-spawn profile cannot un-deny
  what the global denied at the same path. Layering a stricter profile over a
  broader one is therefore safe.
- **Environment:** last scope wins (global → group → explicit).

A *strictly narrower* row from a later scope is not an override — it survives as
a reopen-under-deny, and is then subject to the capability gate above.

Resume, reincarnation, and agent-initiated child spawns can never weaken a deny
or introduce a reopen the recorded parent lacked; both count as widening and are
refused.

## Authoring a restrictive profile without wasting an afternoon

1. **Start from a throwaway agent.** Spawn one with the candidate profile and a
   trivial task, and let it tell you what is missing. Do not attach a real task
   to a profile's first launch.
2. **Get it launching before you get it strict.** Under `deny ~`, confirm in
   order: the harness starts → `tclaude agent whoami` works (socket + binary
   reachable) → the toolchain runs → the build passes.
3. **Assume the failure is silent.** Check for *missing output*, not for error
   messages.
4. **Introspect, don't guess:**

   ```bash
   tclaude agent sandbox-profiles show <name>      # what you authored
   tclaude agent sandbox-profiles default show     # global assignment
   tclaude agent sandbox-profiles group show <g>   # group assignment
   ```

   Reading a profile's payload (`show`) requires `sandbox-profiles.manage`,
   which is human-only by default and deliberately not implied by
   `profiles.manage`. Reading the global and group *assignments* does not.
5. **Let the scribe draft it.** The dashboard's **🤖 configure with agent**
   button on the sandbox-profile editor summons a scribe that holds only
   `sandbox-profiles.draft` — it can propose a validated profile but cannot
   save, assign, or launch anything. You review and save it yourself.

An agent inside a sandbox can see its own *effective* policy through its Bash
tool's sandbox description, but that view is post-merge and lossy: it shows what
is allowed and denied, not which profile or scope each row came from. The
authored profiles live under `~/.tclaude/data`, which a sandboxed agent cannot
read.

## Symptom → cause

| Symptom | Likely cause |
|---------|--------------|
| Files written, exit 0, gone next command | Write under a deny — silent, see above |
| `command not found` for a tool on `$PATH` | Install root under a deny and not reopened |
| Builds fail despite readable caches | Toolchain *binary* root denied, not just the cache |
| `tclaude: command not found`, socket otherwise fine | tclaude's binary dir not reopened — it is never implicit |
| `tclaude agent` reports "agentd is not running" | Socket file hidden **or** the `AF_UNIX` syscall blocked — [check both](sandbox-hardening.md#keeping-the-daemon-socket-reachable) |
| Git loses identity / credential helper | `~/.gitconfig` is a file and cannot be reopened under `deny ~` |
| `git add -A` fails: "can only add regular files" | Claude Code masks a denied path with a `/dev/null` device node — stage specific paths ([above](#git-sees-claude-codes-built-in-denies-as-device-nodes-linux)) |
| `warning: unable to access '….git/config.worktree'` | Same device-node masking; harmless, git still works |
| `stacked` refused, inner bwrap cannot create a namespace (Ubuntu 24.04+) | Ubuntu's `bwrap-userns-restrict` AppArmor policy denies nested bwrap — [above](#stacked-refuses-on-apparmor-restricted-hosts) |
| Launch refused, `unsupported_sandbox_profile_reopen_under_deny` | Claude not in sandbox `on`, or Codex not on Linux managed-profile with a verified probe |
| Profile looks strict but nothing is denied | Claude sandbox `inherit`/`off`, or a legacy `read_baseline` profile (silently dropped — re-express as deny rows) |
| An agent read a denied path with the `Read` tool, but not from Bash | Expected under a `deny ~` profile — that shape reaches layer 1 only ([above](#which-denies-reach-both-layers)) |
| An agent reached something the profile denied | Check whether it went through MCP, which bypasses the Bash sandbox |

## See also

- [Agent coordination → sandbox profiles](agent.md#sandbox-profiles) — the full
  profile reference, protected roots, and CLI.
- [Harnesses](harnesses.md) — per-session sandbox modes and the capability matrix.
- [Sandbox hardening](sandbox-hardening.md) — protecting agentd's own state.
- Claude Code sandboxing: <https://code.claude.com/docs/en/sandboxing>
- Claude Code permissions: <https://code.claude.com/docs/en/permissions>
