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
| tclaude's Linux egress-filter implementation (packet engine, the default) | [Linux network filtering in `tclaude-layer`](linux-network-filtering.md) |
| The alternative egress-filter engine (`network.engine: "proxy"`) | [Proxy network filtering in `tclaude-layer`](proxy-network-filtering.md) |

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

## Linux CPU and memory limits

Sandbox profiles can optionally constrain the aggregate workload process tree
with cgroup v2. `resource_limits.memory` renders to `memory.max`, while
`resource_limits.cpu` renders to `cpu.max` with a fixed 100 ms period. These
limits are orthogonal to bubblewrap: they work with any non-`off` sandbox
implementation on Linux, including `resource-only`, which exists to apply them
with no confinement at all (see below). They do not make the guest report a
smaller machine;
interfaces such as `/proc/meminfo` may still describe host memory.

Leaving both fields blank preserves the previous launch path exactly — tclaude
does not probe controllers, create a cgroup, or add a wrapper — for every
implementation except `resource-only`, which creates the cgroup either way (see
"A cgroup with no ceiling" below). Configured limits are Linux-only in this MVP.
A macOS launch, an `off` implementation, or a Linux host without usable delegated
cgroup v2 controllers refuses by default. The dashboard's existing **allow launch
without enforcement** checkbox is the operator-controlled escape hatch and
records a visible degradation notice.

### Limits without confinement: the `resource-only` implementation

`resource-only` is the sandbox implementation for operators who want a per-agent
cgroup — a CPU/memory budget, or just the accounting boundary — and no access
confinement at all. It creates and joins the same cgroup every other
implementation uses, and it uses no bubblewrap, no namespaces, and no
harness-native sandbox: the harness launches under its own no-confinement mode,
exactly as under `off`.

Reach for it when the goal is blast-radius control rather than confinement — a
runaway browser or build under one agent should not be able to exhaust host
memory and take the other agents, and `tclaude-agentd`, down with it. Note that
this is not the only way to get that: **limits are orthogonal to the confinement
layer**, so `harness-builtin` with the same `resource_limits` gives you the
budget *and* the harness's own wall. Prefer that pairing unless you specifically
want no wall; `resource-only` trades confinement away, it does not add anything
`harness-builtin` lacks.

The two unconfined implementations differ on exactly one axis:

| | access confinement | `resource_limits` | with no limits authored |
|---|---|---|---|
| `off` | none | refused | nothing |
| `resource-only` | none | enforced in a per-launch cgroup | per-launch cgroup, no ceiling |

`off` keeps meaning "tclaude enforces nothing here", which is why it refuses
limits rather than quietly applying them.

#### A cgroup with no ceiling

`resource-only` with no `resource_limits` authored anywhere in the chain is not a
no-op — that spelling exists for the cgroup itself, so the launch gets the
boundary without a ceiling. What that buys:

- per-agent accounting: `memory.current`, `memory.peak`, `cpu.stat`,
  `pids.current` for the whole workload tree, under a cgroup named for the
  session;
- OOM attribution: `memory.events`' `oom_kill` counter includes kills by the
  *host* OOM killer, so tclaude's `resource_limit_oom` exit reason fires for an
  agent the host killed under memory pressure, not only for one that hit its own
  `memory.max`;
- a kill handle for every process the agent started, however it daemonized.

Both the `cpu` and `memory` controllers are enabled in the delegated parent so
those counters exist. Unlike a ceiling, a controller the delegation does not
carry is *not* a refusal here: it costs visibility, not enforcement, so the
launch proceeds and logs which counters are unavailable. The cgroup itself is
still required — if it cannot be created, a `resource-only` launch has no
boundary at all and is refused, with **allow launch without enforcement** as the
operator's escape hatch, exactly as for a ceiling.

This changed behavior: before, `resource-only` with no limits silently created
nothing and was indistinguishable from `off`. A conversation recorded that way
now creates its cgroup on the next launch, and fails if the host has no
delegation for it.

A `resource-only` launch still resolves the sandbox-profile chain, because that
chain is what carries `resource_limits`. The chain is the whole stack — global,
then group, then the explicit profile — so a `resource-only` agent generally
inherits whatever filesystem and network rules are already assigned globally.

Those rules are **recorded but not enforced**. The launch is not refused for
them: refusing would make the implementation unreachable for anyone whose
global profile carries a network rule, while the chain still has to resolve for
the limits to arrive. Instead the launch records a `not_enforced` access notice
naming the implementation, and the access-enforcement preview reports `None` on
every axis.

This holds on all three seams that plan access — the daemon spawn path, a
direct `tclaude session new`, and `tclaude conv resume` (including watch's
auto-resume). A `resource-only` conversation that spawns under a given chain
always resumes under it; the notice is attached identically either way, so a
resumed pane never looks quieter than the one it replaced.

Two things are still refused rather than degraded, because neither is a rule
that merely fails to apply:

- **Remapped mount paths** (`mount_path`), which need a mount namespace that no
  cgroup provides — the authored sandbox path would simply be empty.
- **The implementation off Linux**, where no cgroup can be created at all.

`resource-only` is also excluded from the dashboard's **restart without
sandbox** action. That action trades access confinement away, and this
implementation has none to trade — the only thing it would remove is the
CPU/memory ceiling, which is the opposite of what the action means.

When agentd is run as a systemd service, its unit must delegate the required
controllers to an otherwise empty parent. With systemd 254 or newer, configure
`Delegate=cpu memory`, `DelegateSubgroup=tclaude-supervisor`, and
`OOMPolicy=continue`; the subgroup keeps agentd out of the constrained workload
siblings and leaves the delegated unit node process-free. A delegation or
controller failure is reported at launch with an actionable error; tclaude
never widens a configured limit without the explicit operator override.

Without such a unit, agentd derives the delegated parent from its own
`/proc/self/cgroup`. Inside a container or an unshared cgroup namespace the
unified path reads `/`, so that derivation resolves to the root of whatever is
mounted at `/sys/fs/cgroup`. That works only where the namespace's own cgroup
root is delegated and writable, as it is for systemd in a container with a
private cgroup namespace. It does not work where the host hierarchy is mounted
into the namespace instead — a bubblewrap sandbox binding the host
`/sys/fs/cgroup` under `--unshare-cgroup`, for example: the derived root is
root-owned, and the kernel's `nsdelegate` refuses writes to every cgroup outside
the namespace root, so no other path under that mount serves either. A launch in
that state fails with an error naming the cause; run agentd outside the cgroup
namespace as the unit above, or give the namespace a delegated, writable cgroup
root.

For deployments where tmux panes must survive agentd service upgrades, put the
`-L tclaude` tmux server in a separate, long-lived systemd unit with
`Delegate=cpu memory` and `DelegateSubgroup=tclaude-tmux`, then start agentd
with the delegated unit cgroup (the parent of that subgroup) as its external
root:

```bash
tclaude agentd serve \
  --resource-delegation-dir /sys/fs/cgroup/system.slice/tclaude-tmux.service
```

The same value may be supplied through
`TCLAUDE_RESOURCE_DELEGATION_DIR` or
`agent.resource_delegation_dir` in `config.json`. Precedence is CLI flag,
environment, config file, then the legacy agentd-unit derivation above. Agentd
validates that the directory is below `/sys/fs/cgroup`, is a real directory,
and exposes both the `cpu` and `memory` controllers before accepting requests.

In external mode, the tmux server must already be running. Agentd probes it and
exports the delegation setting into its global environment, but deliberately
does not start or own that server: allowing `tmux new-session` to create it
would place the server back inside agentd's service cgroup. If the external
runtime is unavailable, startup and later session creation fail with an error
naming the runtime unit rather than silently creating a replacement server.
Agentd also verifies that the server PID is already inside the configured
delegation tree.

Managed OpenCode servers are an interim exception because agentd cannot use
`clone3` to place its own child across systemd unit boundaries. In external
mode, OpenCode plus configured resource limits therefore refuses by default or
uses the existing explicit **allow launch without enforcement** degradation.
The degraded server is an ordinary agentd child and must be stopped with
agentd (for example with systemd's default `KillMode=control-group`); do not
configure it to survive agentd restarts. A tmux-mediated managed-server launch
is tracked as TCL-943 to restore both enforcement and upgrade survival.

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

`--sandbox-impl off` is the cross-harness explicit opt-out. It selects each
harness's native no-confinement posture (`off` for Claude Code and OpenCode,
`danger-full-access` for Codex), omits composed tclaude sandbox-profile policy,
and is recorded for relaunches. Because it is a real implementation value—not a
dashboard-only alias—a group or global default cannot silently put the launch
back inside `tclaude-layer`.

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
- a spawn profile's **Sandbox** field, which every agent launched through
  that profile inherits
- the dashboard spawn dialog's **Sandbox** row

The dashboard presents this implementation choice first: resolved defaults,
the harness's built-in sandbox when it has one, tclaude's built-in OS sandbox,
the experimental stacked boundary where supported, or Off. A second
**\<Harness\> sandbox mode** row appears only after explicitly selecting the
harness's built-in sandbox. This keeps Codex's managed `tclaude-agent` profile
where it belongs—as a Codex built-in sandbox mode, not a competing sandbox
implementation.

Existing profiles that stored a harness-native off mode without pinning an
implementation keep that two-axis meaning; the editor calls it out as a legacy
value rather than silently turning it into the broader implementation-level
Off choice. A legacy profile that explicitly paired the built-in implementation
with its native off mode keeps that mode visible until it is changed. Selecting
Off explicitly is what disables every OS sandbox layer.

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
traffic. Filtered OpenCode supports inspected explicit-provider configs on
Linux, while the release-owned local/model-API pack combination remains
launch-refused: they name no explicit provider endpoint to resolve, and
OpenCode exposes no effective-config read of its own loader. The Unix relay
remains Linux-only. Linux uses
`bwrap` from `PATH` and requires working unprivileged user namespaces. macOS uses
`/usr/bin/sandbox-exec` for filesystem confinement and for the
isolated-with-agentd network boundary. If any required capability is missing,
tclaude refuses the launch instead of silently falling back.

On Linux the layer does not unshare the IPC namespace. A host-open posture with
an inherited root also retains the host PID namespace; every constructed root
unshares PIDs as part of closing ambient socket access, including the host-open
constructed root, because otherwise a host process's `/proc/<pid>/root` would
lead straight back to the sockets the root just hid. Under `tclaude-layer`, the harness's own OS
sandbox is disabled inside the wrapper. The explicit Linux-only `stacked`
implementation above is the reviewed exception for Claude Code and Codex.
OpenCode's inner access profile permits all paths without compiling the sandbox
profile's path scoping; its selected approval policy and independent
tool-governance setting remain active. OpenCode has no stacked contract.

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

#### Recovering an agent stranded by a changed `XDG_DATA_HOME` or `HOME`

A private OpenCode allocation is bound to the private state parent it was
created under, which is `$XDG_DATA_HOME/tclaude/opencode-agents` (falling back
to `~/.local/share`). Changing `XDG_DATA_HOME`, or `HOME` when `XDG_DATA_HOME`
is unset, moves that parent away from the recorded allocation and the agent
stops launching. This is deliberate — the daemon will not accept a state root
it cannot re-derive for itself — but it fails closed rather than fixing itself,
so it needs an operator action.

Two ways out, in the order most operators want them:

1. **Restore the previous `XDG_DATA_HOME` or `HOME`.** If the change was
   accidental, this is the cheaper fix: it mutates nothing and the existing
   agent, its conversations and its credentials all come back.
2. **Recreate the agent.** A new agent allocates under the current parent. Its
   conversation history and its private OpenCode credentials do not come with
   it — the old state root is still on disk under the old parent and can be
   copied out by hand if it is worth keeping.

There is deliberately no command that repoints an existing allocation at the
new parent. The daemon's rule is that a per-agent state root must be one it
derives, not one it reads back out of the same database the launch spec comes
from; a repair that trusted the recorded parent would reintroduce exactly the
circularity the launch contract exists to prevent.

The refusal names the cause and both remedies at the point of failure, on every
posture. Which sentence an operator sees depends on how their agent is
launched:

* **Isolated and filtered** postures use the Unix control relay, so they refuse
  on the control root — `OpenCode control root … is outside this daemon's
  private state parent …`.
* **Host-open** has no control socket and refuses on the state root anchor
  instead, naming the config bootstrap target or the launch contract's state
  root.

**Legacy shared agents are never stranded on the control root, and never see
this remedy.** Their control root is derived from the current parent rather than
recorded, so it follows the environment change instead of being stranded by it,
and they are never told to recreate anything. A legacy shared agent moved this
way gets a new, empty control directory under the new parent rather than a
refusal on that path.

That is a narrower claim than "unaffected", deliberately. A legacy shared
agent's contract state root is `~/.opencode`, so changing `HOME` — as opposed to
`XDG_DATA_HOME` — moves that too, and the state root anchor refuses with
`… is neither an allocated per-agent state root nor this host's OpenCode state
root …`. That refusal is about the ambient OpenCode tree following `HOME`, not
about a stranded allocation, and recreating the agent is not its remedy.

The Linux host-open posture starts with a read-only view of the host root
unless the profile authors the `unix_sockets` axis; the isolated and filtered
postures, and host-open with an authored socket axis, use the constructed root
described below. Both give `/dev`,
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
case-sensitive APFS volume keeps distinct objects distinct. Newly saved
registry profiles retain those operator spellings separately from their
canonical filesystem authority. Profiles created by an older tclaude build
remain canonical-only until they are edited and saved; spellings discarded
before this metadata existed cannot be reconstructed.

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

Since TCL-798 the constructed root is no longer welded to the network posture.
A profile that leaves network access open but authors the `unix_sockets` axis
as `closed` or an allow `list` gets a **host-network constructed root** on
Linux: bubblewrap builds the same fresh root and PID namespace as the isolated
posture, binds the agentd socket and any listed sockets back, and does NOT
create a network namespace, so host IP networking, host loopback services, and
the IDE bridge keep working.

That posture is deliberately rated **partially enforced**, permanently. With the
host network namespace shared, Linux abstract-namespace Unix sockets (`@…`) are
not filesystem objects at all, so no mount plan can hide them; close network
access as well if you need those confined too. The recursive-root remainder
applies here as it does under closed network access: a socket beneath a
directory the profile makes readable or writable stays reachable.

The PID namespace is a **requirement** of this posture rather than a side
effect, and it has a cost worth knowing before you author the axis. Without it a
host process's `/proc/<pid>/root` leads straight back to the sockets the
constructed root just hid, so the posture's whole claim would be false. The
consequence is that the agent cannot see or signal host processes, and tools
that read the host process table stop working. This is stated in the launch
warning alongside the abstract-socket caveat.

It is never on by default. A profile that says nothing about `unix_sockets`, or
sets it to `open`, launches with exactly the read-only host root it launched
with before.

Two consequences of building the root reach beyond sockets, and both are stated
in the launch warning rather than left to be discovered:

- **What the agent can see narrows.** Host paths outside your filesystem grants
  and the static OS surface are no longer present at all, where a host-open
  launch without this rule showed the whole read-only host root. Before adding a
  socket rule to an existing host-open profile, check that anything the agent
  needs — toolchains under your home, `/var`, `/srv`, `/opt`-style installs — is
  actually granted.
- **Sockets on the static OS surface stay reachable.** `/usr`, `/bin`, `/sbin`,
  `/lib*`, `/etc`, and `/opt` are mounted read-only, and a read-only mount does
  not block `connect()`, so an AF_UNIX socket living under one of those paths
  remains connectable. This is the same remainder the closed-network posture
  has, named here because a host-open profile is a far more common shape.

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

If a fresh dashboard spawn requests closed network access on an implementation
that cannot enforce it, the launch normally refuses with:

```text
<harness> (<scope> scope) cannot enforce closed network access; choose a sandbox implementation that can enforce closed network access, use network open, or enable “Allow launch without enforcement” in the dashboard spawn dialog
```

The human operator may explicitly check that named dashboard escape hatch for
one fresh launch. Only this exact closed-network capability gap is widened:
network access becomes open, while enforceable filesystem and Unix-socket rules
keep their planned posture. Protected-root checks, filtered model-transport
gates, implementation availability, and every other launch refusal remain
non-overridable. Raw API callers, including agents and human-class credentials,
cannot set the override.

The authorization is deliberately neither saved nor inherited. Resume,
reincarnate, and clone of an overridden agent do not carry it forward and refuse
again unless a future fresh spawn is explicitly authorized in the dashboard.
The launch snapshot records a degradation notice with network reason
`operator_unenforced_launch_override` and effect `not_enforced`; the dashboard
renders a warning badge and the exact notice rather than claiming a sandbox
lock or changing the recorded OS-sandbox verdict.

#### Compositional network packs

The profile editor authors a network baseline first: **Deny all**, **Allow
all**, or **No override**. Deny all and Allow all can add both Allow and Deny
rules as stable references to release-owned packs or explicit manual
destinations. Expanded pack rows are visible but read-only; choose Off, Allow,
or Deny on their pack to change them. The saved shape keeps intent rather than
copying endpoints:

```json
{
  "network": {
    "baseline": "deny",
    "packs": ["net-local", "net-anthropic", "net-openai-codex"],
    "deny_packs": ["net-npm"],
    "allow": [{"domain": "example.internal", "ports": [443]}],
    "deny": [{"domain": "telemetry.example.internal", "ports": [443]}]
  }
}
```

`packs` retains the original allow-pack spelling; `deny_packs` is its
deny-mode counterpart. Traffic matching any Deny rule is denied even when it
also matches an Allow rule, and rule order does not matter. Under Deny all,
Allow rules are unlocks while non-overlapping Deny rules are redundant. Under
Allow all, Deny rules are restrictions while Allow rules are redundant. The
editor labels either redundant case instead of rejecting it. No override
carries no rows.

Network denies are enforced for Claude Code and Codex on the Linux
`tclaude-layer` filtered gateway. CIDR and local-machine denies are fully
enforced. DNS-name denies are fully enforced under Deny all; under Allow all
they are **Partial** because the broker blocks addresses it observes for the
name, while another address for the same service or encrypted DNS that bypasses
the broker can remain reachable. A blocked shared address also affects other
names until the DNS lease expires.

The authoring table carries only a neutral pointer to the Effective policy
preview. That preview places each effective deny destination in the existing
Fully supported, Partially supported, or Unsupported bucket for the selected
target. Its `?` disclosure identifies the target and explains the mechanism,
limitation, and remedy without turning an authored row into a global
enforcement claim. macOS, built-in, and stacked cells remain unsupported for
network denies, as do OpenCode's local presets wherever they run the packet
gateway — a local preset that deploys the **proxy** engine on a platform whose
proxy cells are activated does rate its deny rows. Capability planning omits
unsupported deny rows individually; a port-scoped deny is never widened into a
whole-destination block.

`net-local` provides the unscoped loopback destination for local model servers
such as Ollama, LM Studio, and llama.cpp, Codex OSS mode, OpenCode local
providers, and host-local development services. OpenCode local-provider
launches are refused **under the packet gateway** because their effective
provider endpoint is not available at the launch seam: those presets name no
explicit provider. Under an activated `network.engine: proxy` they are not:
that engine resolves nothing ahead of time, so the preset composes normally and
the launch is held to the ordinary explicit-provider contract instead.
`net-anthropic` and `net-openai-codex` independently provide the direct
Anthropic and OpenAI API-key endpoints. New drafts select these three packs
once on their first transition to Deny all; they remain ordinary editable pack
choices afterward.

Under the proxy engine, an unscoped loopback row governs `127.0.0.0/8`, `::1`,
the exact unspecified addresses `0.0.0.0` and `::`, and their IPv4-mapped
spellings. It does not authorize the rest of `0.0.0.0/8`: those addresses are
ordinary reserved destinations and are refused under Deny all unless an
explicit CIDR allow row covers them. CIDR rows such as `0.0.0.1/32` are
authorable in either polarity, so an Allow all profile can explicitly deny the
address and a Deny all profile can deliberately allow it. A broader CIDR that
also contains an address governed by loopback rows remains invalid; split the
range so loopback and CIDR authority stay explicit. Port scopes apply in both
cases.

Legacy `network.mode: list` profiles remain valid and open as Deny all with
their exact rows under manual destinations. The editor never infers a pack
reference from matching endpoints, so opening and saving a legacy policy does
not silently transfer ownership to a future release registry.

On Linux, pack-backed lists use the filtered gateway. Local host services are
reached through `host.tclaude.internal`; `127.0.0.1` and `::1` remain private
to the sandbox. On macOS, strict Local access is enforced natively by Seatbelt,
but **its scope is a port set, not an address set**. Seatbelt's network grammar
accepts only `*` or `localhost` as the host part of a rule, and `localhost`
means *every address assigned to this machine* — not the loopback interface.

So allowing Local access on port N also allows anything else **on this same
machine** listening on port N, including a service bound only to the host's LAN
address. Everything involved belongs to the machine the agent already runs on:
this is not egress, and a service bound to `0.0.0.0` was already reachable
through the Seatbelt rule in any case, since `localhost` covers whichever
address the connect names. The gap needs a second service, on the same port,
bound exclusively to a non-loopback address.

Note this is a different mechanism from the proxy engine's **loopback rows**
described above, and the `0.0.0.0/8` authority split stated there does not
apply here. Seatbelt has no row-authority concept to split: it has one host
token, `localhost`, which is why the scope is a port set. Reading the two
paragraphs as one model is the mistake to avoid — they share the word
"loopback" and share nothing else.

Ports outside the list are denied, and outbound **TCP** connections to
addresses beyond this machine are denied. Bind/inbound behavior is left alone
so local services and the IDE bridge can work. The Seatbelt outbound rule uses
`remote ip` predicates; an outbound `local ip` predicate would match the local
side of remote connections and is therefore never used for this boundary.

This is a limit of the mechanism rather than a configuration mistake: SBPL
cannot express "this port, loopback only" at all, so no policy you can author
avoids it. It is measured on a real runner rather than inferred — CI run
`30691418550`, job `91346704723` — and pinned by a characterization test, so if
Apple ever *narrows* `localhost` to the loopback interface, CI reports it
instead of the change being discovered years later. A *widening* of `localhost`
beyond this machine is not detected by that test; catching it would need a
service on the allowed port at an off-machine address, which CI cannot arrange
safely.

macOS does not yet enforce a mixed Local + model-API pack list. It follows the
same established list-degradation path as any other mixed Darwin list: the
preview and recorded launch report **Not enforced**, and outbound networking
remains open. It does not refuse and does not silently pretend to filter;
mixed-list enforcement is tracked separately in TCL-827.

Strict Local access never gains a hidden cloud endpoint. A cloud-backed
harness launch is checked through its resolved model-transport requirement and
refuses with `unsupported_filtered_model_transport` unless the authored
Access list, directly or through **Includes**, covers the provider endpoint.
A concrete provider at `host.tclaude.internal` on Linux, or real
`localhost`/`127.0.0.1`/`::1` on macOS, passes that same gate. The Anthropic
and OpenAI API packs cover first-party API-key traffic and the opt-in ChatGPT
pack covers ChatGPT-signed-in Codex; custom providers, web search, plugins, MCP
servers, and commands run by the agent need their own authored destinations.
Under the packet gateway, OpenCode remains launch-refused on the built-in
local/model-API combination with `unsupported_filtered_model_transport`, naming
the missing explicit provider and the network-open remedy; the editor does not
advertise its local-provider constituency as present-day support. That refusal
is the packet gateway's own — it exists because that gateway checks the
authored list against an endpoint resolved ahead of time — so it does not apply
to a launch whose deployed engine is an activated proxy. Such a launch still
needs an explicit provider/model and inline explicit-provider config; what it
no longer gets is a refusal naming machinery it never runs.

One operational detail of the host-network constructed root: the static OS
surface binds `/etc` read-only, but on a systemd-resolved-class host
`/etc/resolv.conf` is a symlink into `/run`, which a constructed root does not
have. tclaude reopens that one resolver **file** read-only, creating its parent
directories inside the namespace — never `/run` itself, which is exactly where
the ambient sockets this posture exists to hide tend to live. A resolver target
outside `/run` is deliberately not chased, and one that is not a regular file is
refused: silently binding an arbitrary host path — still less a directory or a
socket — to make DNS work would be a wider hole than the failure it prevents. If
an ordinary profile deny covers the resolver's parent, the reopen is repaired
after that deny, the same way the agentd socket is.

When a profile path reaches resolution with a symlinked spelling, the
constructed root recreates the highest symlinked component so tools can keep
using that spelling while the mount plan binds the resolved target. Registry
profiles store the canonical path as the sole launch authority and retain
operator spellings in separate, versioned metadata. The editor therefore shows
one row per canonical rule, with the authored spelling and a `binds →` target,
rather than presenting aliases as additional grants.

Retained spellings are revalidated during save preview and again at launch. If
a symlink or case/NFC-equivalent spelling now resolves to a different object,
tclaude refuses it and names the profile, spelling, original target, current
target, and both remedies: re-save the spelling to adopt the new target, or
remove it. Here, “re-save the spelling” means explicitly edit that filesystem
path and save, which enters the re-authoring flow; an ordinary profile save
does not adopt drift. Launch authority is never recomputed from retained
spelling metadata. Ordinary edits keep the original binding pinned and
revalidate it.

The isolated posture blocks TCP egress and host-loopback TCP. It also closes
the Linux abstract Unix-socket namespace. PID isolation prevents the harness
from escaping the constructed root through a host process's
`/proc/<pid>/root`. A filesystem Unix socket is visible only when it was
explicitly bound, or when an operator-authored filesystem grant re-exposes a
parent directory under the normal most-specific-wins policy. This is the
constructed-root posture's socket boundary; the compact badge tooltip does not
report socket fidelity. The host-network constructed root shares the filesystem
half of that boundary and the same PID isolation, but not the abstract-namespace
half: that one closes only when the network namespace does. Opt into its adjacent details chevron with
`features.recorded_sandbox_details` for recorded launch fidelity, or use
`sandbox-profiles plan` for a dry-run of explicit inputs.

The Linux `filtered` posture enforces the packet and DNS subset for exact
`tclaude-layer` Claude Code, Codex, and OpenCode launches: IPv4/IPv6 CIDR destinations,
exact DNS hosts, label-bound domains with optional subdomains, TCP/UDP
destination ports (including QUIC as UDP), and synthetic host loopback. Raw and
packet sockets, including authored ICMP access, are not part of the network-list
contract. Host, domain, CIDR, and synthetic host-loopback selectors are rated
`Full`. Host and domain rules enforce the DNS-to-IP boundary: the sandbox can
also reach another site hosted on an allowed IP until the DNS answer expires,
and existing connections may continue after expiry. This is the strongest
name boundary available for arbitrary TCP/UDP, which carries no SNI or other
application identity on the wire. `stacked` does not claim this cell.

Each launch probes bubblewrap user/network namespaces and resolves `pasta` and
`nft` through root-owned, non-group/world-writable paths. The pasta probe also
requires the exact forwarding, synthetic-address mapping, and splice controls
used by the gateway; older pasta releases that lack those controls are an
unavailable prerequisite, not a weakened fallback. A missing prerequisite
widens the filtered network rules to host-open with a persisted warning.
Consequently, an editor Full preview is explicitly prerequisite-conditional;
only the resolved launch verdict can mint enforcement.

On a positive launch, bubblewrap creates the constructed network/PID namespace
without connectivity. Rootless bubblewrap maps the invoking host user to
namespace UID/GID 0 so the pinned bootstrap can receive namespace-local
`CAP_NET_ADMIN` for the atomic nft policy and `CAP_NET_BIND_SERVICE` for the
private port-53 DNS listener; host file ownership remains mapped to the
invoking user. The final harness also runs as namespace UID/GID 0 after the
verified capability drop. This is a one-ID rootless mapping, not host root: the
invoking user's files appear owned by namespace root inside the wall, and
files the harness creates map back to the invoking host UID/GID. The bootstrap
installs the complete default-drop nftables output policy as one atomic
`nft -f` transaction and signals the outer supervisor. Only then does the
supervisor start foreground rootless `pasta` with inbound forwarding,
namespace forwarding, gateway mapping, and splice shortcuts disabled. Harness
exec stays gated until pasta's PID readiness is verified. The bootstrap drops
every capability, clears ambient capabilities, and sets no-new-privileges
before exec. Only `CAP_NET_ADMIN` is deliberately carried across the trusted
nft child exec; the harness receives neither capability. If pasta exits, the
supervisor kills the sandbox through a pinned pidfd; if the supervisor dies,
bubblewrap and pasta die with it.

Local-machine rules use `host.tclaude.internal`, mapped to fixed synthetic IPv4
and IPv6 addresses and filtered by the authored ports. Inside the sandbox,
`127.0.0.1` and `::1` refer to the sandbox itself. The sandbox's
`/etc/resolv.conf` and `/etc/hosts`
both route through the external DNS broker, so a hosts-file mapping cannot
bypass the same selector and port checks. A DNS answer containing loopback is
refused unless the profile also authors loopback; when it does, the
broker rewrites the answer to the same synthetic host-loopback addresses.
The current implementation does not reserve those synthetic addresses from
CIDR or DNS-derived rules, however, so either kind of rule can also reach host
loopback on its authored ports. See
[Linux network filtering](linux-network-filtering.md#host-loopback-mapping-and-current-reservation-gap)
for that security-boundary limitation.

Host and domain rules allow IP addresses returned by DNS. The sandbox can also
reach other sites hosted on that same IP until the DNS answer expires. The
broker follows a bounded CNAME chain, filters returned A/AAAA records for the
matching authored host/domain selector, and adds each admitted address to the
matching per-rule nft set for no longer than the observed DNS TTL. CIDR rows
are IP-literal packet authority only; they do not authorize arbitrary DNS
queries whose answers happen to fall inside the CIDR. Only a new DNS lookup
refreshes the allowed IP. There is no timer-driven self-refresh and no fixed
grace window.

Expiry has two deliberately different directions. A new TCP or UDP flow needs
a current lease, so an agent that performs no fresh lookup after expiry cannot
open another connection. An already-established conntrack flow may continue
after the DNS lease expires; this keeps a long streaming response or connected
UDP operation alive, but it is also a named residual rather than an
application-identity guarantee. Re-resolving before a later connection obtains
a fresh answer and refreshes the lease.

Filtered model traffic has no hidden bypass. Under a default-deny/list
baseline, the operator-authored allow rules must already cover every endpoint
from genuinely resolved provider/model context. Under a default-allow
baseline with denies, the same resolved endpoints must not match an authored
deny selector. tclaude never fabricates provider resolution or rewrites the
rules. Deny wins at the shared IP and port boundary, so a provider route that
later shares a denied address can still be cut even when its name passed the
selector preflight. At the executing launch seam, Claude's direct Anthropic
default and concrete `ANTHROPIC_BASE_URL` are resolved from the launch
environment; third-party provider modes and provider-changing live settings
refuse with a named remedy.

Claude Code 2.1.220 also loads **remote managed settings**: a policy-tier
settings source fetched from the API rather than read from a file. A payload
verified by a fresh authenticated fetch is exempt from the environment filter
that strips provider routing out of an unverified one, so it can carry
`ANTHROPIC_BASE_URL`, a `CLAUDE_CODE_USE_*` provider selector, proxy variables,
or CA/mTLS material. The fetch happens at Claude startup — after this preflight
— and repeats on an hourly background poll that applies changes to the running
process. Unlike Codex, therefore, a running Claude session can re-route.
tclaude inspects the locally cached copy at
`$CLAUDE_CONFIG_DIR/remote-settings.json` (or the
`CLAUDE_CODE_REMOTE_SETTINGS_PATH` override) and refuses a launch whose cached
remote policy carries provider routing, which covers the consented, persisted
case. It cannot observe a payload that has not been fetched yet. When the route
moves anyway, the unauthored destination under a list baseline is denied
fail-closed for new flows at the packet floor, and the launch notice says so.

Codex resolves its selected provider and concrete base URL from Codex's own
merged effective configuration, read at launch through the app-server
`config/read` request rather than by parsing `config.toml`. That merge includes
the layers no local file exposes — the MDM layer and the enterprise cloud-config
bundle — and reports, per key, which layer won and a content hash for it. When a
provider-routing key was won by a remotely delivered layer, the launch notice
names that layer and hash. A repository-local `.codex/config.toml` is a real
layer but may not set any provider-routing key, so repository contents cannot
move model traffic.

Because the effective route is now readable, ChatGPT sign-in is no longer
refused. A ChatGPT-authenticated launch resolves to the effective
`chatgpt_base_url` (default `https://chatgpt.com/backend-api/`) plus the
token-refresh endpoint `auth.openai.com`, which is a constant in the harness and
cannot be moved by any config layer. Both must be covered by the authored list;
the opt-in `net-openai-chatgpt` pack provides them. Provider-changing
pass-through overrides, an unresolvable effective config, a selected profile,
and credential routes tclaude cannot inspect — `CODEX_ACCESS_TOKEN` and the
keyring/ephemeral credential stores — still refuse, because they leave the
destination unknown rather than merely secret.

Codex snapshots its cloud-config bundle once at process start; its background
refresher only warms the on-disk cache for later starts and does not re-route a
running process. The residual window is therefore bounded to the gap between
this preflight and process start, where a changed route reaches an unauthored
destination and is denied fail-closed at the packet floor. Reading the effective
config runs the Codex binary before the sandbox exists, which is a launch-time
side effect including whatever bundle refresh Codex performs at start.

OpenCode filtered supports explicit-provider configs only. The launch model and
frozen profile `OPENCODE_CONFIG_CONTENT` must name exactly one provider using
`@ai-sdk/openai-compatible`, whitelist and define exactly that one launch
model, and give a concrete `options.baseURL` covered by the authored network
list. The filtered server forces OpenCode's project-config, custom-config,
model-fetch, auto-update, stored-auth, and plugin isolation inputs and replaces
the ambient XDG and `$HOME/.opencode` config sources with provider-empty
per-agent directories that are daemon-final read-only inside the executor. Their
canonical contents and persistent account/org absence are rechecked immediately
before every initial server exec and persisted restart. A model-level
`provider` override refuses because it can replace
the inspected adapter. An active persistent OpenCode account/organization also
refuses because its remote config loads after inline content; sign out or clear
the active organization,
or use network open. Managed `/etc/opencode` config, opaque or default
providers, substitutions, multiple providers/models, and other adapters refuse
with the named remedy of using the strict explicit shape or network open.
Unlike Claude/Codex, there is no implicit first-party origin
set to discover: the authored base URL is the origin authority, and the pinned
OpenCode 1.18.6 smoke proves the real server consumes it while an unauthorised
TCP/UDP endpoint remains denied.

OpenCode's sandbox-profile network access rules for built-in `webfetch` and
`websearch` are soft tool rules. They are not the filtered security boundary;
the `tclaude-layer` nft policy is the packet-enforced floor around the
tool-executing server and all of its subprocesses.

For all supported harnesses, a nonempty `HTTP_PROXY`, `HTTPS_PROXY`, or
`ALL_PROXY` (including lowercase variants) in the launch environment changes the
actual transport boundary and therefore refuses filtered launch: the real
destination sits behind a proxy this seam does not resolve, so the authored list
cannot be checked against it. Remove the proxy variable or use network open. The
same refusal covers those variables when a Claude `settings.json` authors them,
because Claude re-reads settings env while a session runs.

Under `network.engine: "proxy"` tclaude sets those variables itself, and that is
not an exception to the refusal above. The resolver inspects the
**pre-injection** environment — the host environment plus authored
`EnvironmentEntry` overrides — and tclaude's own values are injected inside the
namespace afterwards, so they are never in the inspected set. Nothing is
allowlisted by value: a foreign variable that happens to name a loopback address
refuses exactly like any other. `NO_PROXY`/`no_proxy` are the one exemption, and
they are overridden to empty rather than refused over, with an access notice
recording the override whenever the host actually carried a value. See
[Proxy network filtering](proxy-network-filtering.md#the-proxy-environment-is-tclaudes).

The built-in `net-anthropic` and `net-openai-codex` packs are backed by the
named CI origin audit against Claude Code 2.1.220 and Codex CLI 0.145.0. The
active minimal evidence set is `api.anthropic.com:443` for Claude and
`api.openai.com:443` for API-key Codex. The same audit records ChatGPT model and refresh traffic
at `chatgpt.com:443` and `auth.openai.com:443`, which back the separate opt-in
`net-openai-chatgpt` pack. Those destinations stay out of `net-openai-codex` so
an API-key profile does not silently gain them. Any
undeclared mandatory origin fails the audit.
Codex's optional plugin-marketplace synchronization is not model transport; it
may be unavailable unless the authored profile separately admits its
destinations.

The fully isolated `network_access: none` posture also severs editor
integrations that connect over a localhost WebSocket, including Claude Code's
IDE bridge, as well as host-local model servers. Strict Local access is the
explicit posture that restores those host-loopback services under the
platform-specific semantics above.

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

The remaining limitation in the host-open posture with an INHERITED root is
explicit (a host-open profile that authors the `unix_sockets` axis gets the
constructed root described above instead, and the ambient filesystem sockets go
with it):

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

A pid is not unique over a machine's lifetime, and session rows are not
pruned, so a long-dead session's row can be recorded against the same number
as a live agent's pane. When more than one row claims the pid the daemon
replaces a winner whose tmux session it can see is gone with one it can see is
alive. It is a repair of a demonstrably dead answer, not a re-ranking: with
nothing provably alive, with tmux unreachable, or with no recorded tmux session
to judge by, resolution is exactly what it was before, so no caller is refused
that would previously have been placed. It matters because picking the corpse
refuses a live agent's callbacks, and that failure sustains itself — the live
row is advanced mainly by the very callbacks being refused.

The repair covers the brokered hook and status-line paths, the
`tclaude-layer` ancestry walks, and the general pid → conv-id lookup behind
direct CLI identity. That last one is the one whose answer becomes the caller's
conversation for authorization, so a reused pid there means a caller is
authorized as whichever conversation happened to hold the freshest row. On that
path the repair may only improve WHICH conversation is named, never whether one
is named at all: a live row that has not established its conv-id yet — the
state every spawn row starts in — cannot displace a dead row that has one, nor
answer in place of an incumbent that has none.

Refusing outright on an ambiguous pid was considered and rejected: several rows
per pid is the normal case rather than the suspicious one, so refusing whenever
liveness is merely inconclusive would routinely reject legitimate live callers.
Pid reuse is also an accident rather than something a caller can steer — it
cannot choose the pid the OS hands it, and identity stays bound to host pids the
daemon itself recorded either way. The residual limitation is deliberate: a dead
incumbent with no provably live sibling still resolves as it did before.

For OpenCode, CLI identity may cross at most 16 wrapper
ancestors only when the matching runtime row explicitly records
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
server-authoritative smokes. The filesystem test starts a real wrapped `opencode serve`,
connects a real unwrapped `opencode attach`, verifies the permission patch,
executes the real bash tool across an allowed and denied path, and requires
`tclaude agent whoami` from that tool subprocess to resolve the exact managed
agent identity. The filtered activation test additionally proves the server
consumes its inspected explicit `options.baseURL`, suppresses hostile alternate
config/auth/model/plugin sources, and allows authored TCP/UDP tool traffic while
denying an adjacent unauthorised port. It also executes the deny boundary that
backs OpenCode's Linux deny capability: deny-over-allow precedence in both
overlap directions and a DNS deny whose negative lease cuts an address the
covering allow otherwise permits. That case needs the live adjacent-target
fixture the CI step provisions, and the smoke refuses to run without it.
Neither test has a user-namespace capability skip. To repeat them after
installing OpenCode (see the workflow step for the fixture the second command
expects):

```bash
TCLAUDE_OPENCODE_LAYER_SMOKE=1 \
  TCLAUDE_SANDBOX_V2_TCLAUDE_BINARY="$HOME/.cache/tclaude/tclaude-sandbox-v2-smoke" \
  go test ./pkg/claude/agentd -run '^TestOpenCodeTclaudeLayerExecutorSmoke$' -count=1 -v -timeout=120s

TCLAUDE_OPENCODE_LAYER_SMOKE=1 \
  TCLAUDE_SANDBOX_V2_TCLAUDE_BINARY="$HOME/.cache/tclaude/tclaude-sandbox-v2-smoke" \
  go test ./pkg/claude/agentd -run '^TestOpenCodeFilteredNetworkExecutorSmoke$' -count=1 -v -timeout=180s
```

The Darwin job likewise verifies that Seatbelt enforces its deny-write probe
before running the real filesystem/network smoke and the OpenCode executor
smoke, and fails if either named test does not report its explicit top-level
PASS line. `TestTclaudeLayerDarwinSmoke` includes strict Local access: an
allowed real-host loopback port connects, another listening loopback port and
public TCP egress fail with `EPERM`, and local bind remains available. It also
characterizes the **address** axis, not only the port one: a different service
on the *allowed* port at a non-loopback local address is reachable, which is
the scope limit described above. Listing only the port assertions is what let
that limit go unnoticed, so the pair is stated together.
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

## Mounting a host directory at a different sandbox path

A `read` or `write` row may carry an optional `mount_path`. The host directory
named by `path` then appears inside the sandbox at `mount_path` instead of at
its host path — a Kubernetes-style volume mount:

```json
[
  { "path": "/var/lib/shared-datasets/corpus-v3",
    "access": "read",
    "mount_path": "/data" }
]
```

The agent sees `/data`; `/var/lib/shared-datasets/corpus-v3` is not visible
inside the sandbox at all. That lets a profile give agents a stable conventional
path without exposing (or depending on) the host's real directory layout, and
lets one profile compose across machines whose host paths differ.

Rules:

- **Omitting `mount_path` is the same-path behavior every profile had before.**
  Nothing changes for a rule that does not use it.
- **`deny` rows may not carry a `mount_path`.** A deny hides a host path rather
  than projecting one, so it always applies to the host path. Authoring one is a
  validation error rather than a silently ignored field.
- **`path` stays the authority.** Symlink resolution, directory-ness and the
  protected-root wall are all decided against the host path, exactly as before.
  `mount_path` is validated syntactically (absolute, cleaned, not `/`) because
  it names a location in a namespace that does not exist yet.
- **Ordering, shadowing and most-specific-wins are evaluated on the sandbox
  side**, because that is the namespace the agent actually observes.
- Two different host paths may not claim one `mount_path` (that is a validation
  error); one host path at several mount paths is fine.
- A `mount_path` may not intersect protected tclaude state or shadow the agentd
  control socket, and — at launch — may not cover a directory the launch itself
  requires, such as the workspace.
- A host path whose most specific rule is a `deny` may not be mounted elsewhere;
  that would re-expose exactly the content the deny hides under another name. A
  path reopened by a narrower `read`/`write` beneath a broad deny is fine.
- **A missing host source is skipped and the mount simply does not appear**,
  the same rule missing paths already follow. tclaude never creates host
  directories as a side effect of launching.

### Where it is enforced

Projecting a directory onto another path requires a real mount namespace, so
this is not universally available:

| Implementation / platform | `mount_path` |
|---------------------------|--------------|
| `tclaude-layer` or `stacked`, **Linux** (bubblewrap) | ✅ enforced with `--ro-bind`/`--bind src dest` |
| `tclaude-layer` or `stacked`, **macOS** (Seatbelt) | ❌ refused — Seatbelt is a path filter over the host namespace, not a mount namespace |
| `harness-builtin` (any harness, any platform) | ❌ refused — the harness receives path lists and confines itself in the host namespace |

Where it is unsupported, tclaude **refuses the launch** with
`unsupported_sandbox_profile_mount_path` and buckets the rule as unsupported in
the effective preview. It never falls back to mounting at the host path: that
would break the authored contract in both directions, exposing a path you did
not authorize while leaving the one you did authorize empty.

One further Linux caveat. When the sandbox root is the real host root bound
read-only, bubblewrap has nowhere to create a new mount point, so the mount
point must already exist on the host; tclaude refuses with
`tclaude_layer_missing_mount_point` naming the path rather than creating it for
you. Wherever tclaude constructs the root the mount point is created inside the
namespace with nothing required on the host. Since TCL-798 that covers more
than the isolated and filtered postures: a host-open profile that authors the
`unix_sockets` axis also constructs its root, and the refusal no longer applies
to it.

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

### Spelling does not get you past a protected root

On a case-insensitive volume — the APFS default on macOS — `~/.tclaude/data`
and `~/.TCLAUDE/Data` are the *same directory*, and so are the NFC and NFD
spellings of a name containing accented characters. Rules are compared against
protected roots by filesystem identity, not by string, so every spelling of a
protected directory is refused alike. You cannot slip a write grant past the
wall by capitalising it differently.

The same rule folds rules together: two rows naming one physical directory
through different spellings persist as **one** row, with the more restrictive
access winning. Authoring a `deny` on `~/Project` and a `write` on `~/project`
on such a volume leaves you with a single denied row, not two competing ones.

On a case-*sensitive* volume — every ordinary Linux filesystem, and
case-sensitive APFS — differently spelled paths really are different
directories, and tclaude keeps treating them that way. Nothing is silently
lowercased: two directories that exist separately stay separate, because the
filesystem says so.

There is one deliberate exception, and it is worth knowing about because it can
surprise you on Linux. When a rule's path differs from a protected root *only*
by case or Unicode normalization **and that path does not exist yet**, tclaude
refuses it on every volume. It does not try to predict whether the filesystem
would have folded the two spellings had the directory been created — that
question has no reliable answer (on Linux, case folding is a per-directory
attribute, not a property of the volume), and guessing it wrong would mean
admitting a write grant over tclaude's own state.

In practice this costs you an error message in a narrow case: a not-yet-created
path whose spelling collides with a protected root. Either create the directory
first, or spell it the way it is spelled on disk, and the rule is admitted
normally. An ordinary not-yet-created path that does not collide with a
protected root is unaffected and involves no filesystem checks at all.

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
   tclaude agent sandbox-profiles plan --group <g> --sandbox-profile <name>
   tclaude agent sandbox-profiles plan --agent <selector>
   ```

   `plan` is inspection-only. The first form composes the current global,
   optional group, and optional explicit tiers for the current directory (use
   `--cwd` and `--for implementation[/harness[/platform]]` to make those inputs
   explicit), predicts access enforcement, and describes the four outer-layer
   mount precedence classes. The `--agent` form instead reads only the latest
   frozen launch row: it does not re-resolve registry profiles or mix in a
   prediction of current host capability. Because older rows did not persist
   the complete per-session launch contract, recorded mode renders only the
   row-carried effective profile, access axes, profiles, and notices. It marks
   the launch-contract and daemon-final classes
   `not recorded at launch — unavailable` and points to hypothetical mode; it
   never reconstructs those facts from mutable current state. Positive
   filesystem rows remain visible as recorded policy, but their launch-time
   `present` / `missing-would-skip` disposition is also marked unavailable:
   older rows did not persist path presence, and recorded mode never `stat`s
   them now. Hypothetical mode is the complete composed view for explicit
   current inputs.

   Harness-builtin targets report that an outer mount plan is not applicable.
   A non-host `--for` target still returns its access prediction, but mount-plan
   inspection is marked unavailable rather than labeling host paths and
   presence observations as another platform. `--json` exposes the same stable
   structure. Missing positive binds are reported as
   `missing-would-skip`; inspection never creates them.

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
