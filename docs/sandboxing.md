# Sandboxing

tclaude can confine an agent inside an operating-system sandbox: a wall around
the harness process that decides which directories it can read or write, which
network destinations it can reach, which Unix sockets it can connect to, and
how much CPU and memory its whole process tree may consume. The policy is
authored once, as a harness-neutral **sandbox profile**, and enforced by
whichever backend can faithfully carry it — or the launch is refused.

Three ideas carry this page:

- A **profile** is what you author: a JSON capability bundle.
- **`--sandbox`** is the harness's *own* sandbox mode — a per-harness setting.
- **`--sandbox-impl`** is *who enforces* confinement — the harness itself,
  tclaude's own OS layer, both stacked, a resource-only cgroup, or nobody.

Egress filtering has its own page ([Network filtering](network-filtering.md)),
and credential-less workflows that make strict profiles livable have theirs
([Credential proxies](proxies.md)).

## Who enforces: `--sandbox-impl`

The sandbox implementation is selected at spawn and recorded with the
conversation — it is durable relaunch intent, so a flagless resume uses the
same layer. The values:

- **`harness-builtin`** — the harness confines itself with its own OS sandbox.
  Valid as an explicit pin only for harnesses that really own one: Claude Code
  (bubblewrap on Linux, Seatbelt on macOS) and Codex. OpenCode refuses the pin
  — its access control is a command filter, not confinement — and Copilot's
  descriptor declares no built-in OS sandbox. Leaving the implementation
  *unset* is different from pinning `harness-builtin`: unset falls through the
  precedence chain and preserves each harness's historical behavior (for
  OpenCode, the command filter plus an explicit no-confinement warning).
- **`tclaude-layer`** — tclaude wraps the tool-executing harness process in its
  own wall: bubblewrap mount/PID (and optionally network) namespaces on Linux,
  Seatbelt (`sandbox-exec`) on macOS. The harness's own OS sandbox is forced
  off inside it (Claude Code mode `off`, Codex `danger-full-access`; Copilot
  has no off-flag tclaude can set, so its configuration is verified instead and
  an unverifiable posture refuses). Supported for Claude Code, Codex, OpenCode,
  and Copilot, on Linux and macOS. For OpenCode the wrapped process is the
  agentd-owned `opencode serve` executor; the attach pane stays outside.
  Requires `bwrap` and working unprivileged user namespaces on Linux; a missing
  capability refuses the launch — never a silent fallback — from whichever tier
  selected the layer.
- **`stacked`** (experimental, Linux only, Claude Code and current-backend
  Codex) — both walls at once: tclaude's outer sandbox with the harness's real
  inner sandbox kept active (Claude Code forced `on` with
  `enableWeakerNestedSandbox: false`; Codex forced onto a managed profile with
  legacy Landlock disabled). Every fresh launch and resume probes the real
  inner engine inside the exact outer spec before the pane is committed; the
  harness executable is copied to launch-owned staging, hashed, and
  descriptor-bound so the final process executes the proved bytes. Any gap
  refuses with `stacked requested — refused: missing capability <name>…;
  refusing rather than falling back`. A successful stack shows the `🔒²` badge
  and reports its implementation as `CC+TClaude` or `Codex+TClaude`. Stacked
  refuses on stock Ubuntu 24.04+, whose `bwrap-userns-restrict` AppArmor policy
  denies the nested bubblewrap its in-namespace `CAP_SYS_ADMIN`; the documented
  workaround is host-wide and costs Ubuntu's user-namespace defense-in-depth.
  OpenCode and macOS nested Seatbelt have no stacked contract and refuse.
- **`resource-only`** (Linux only) — no access confinement at all: the harness
  runs in its native no-confinement mode, but every launch creates and joins a
  per-launch cgroup. With `resource_limits` authored you get CPU/memory
  ceilings; without them you still get per-agent accounting (`memory.peak`,
  `cpu.stat`), host-OOM attribution (`resource_limit_oom` exit reason), and a
  kill handle for everything the agent started. The profile chain still
  resolves — it carries the limits — and its access rules are recorded but not
  enforced, disclosed with a `not_enforced` notice. Remapped `mount_path` rules
  and non-Linux hosts refuse. A fresh launch that cannot create its cgroup
  refuses; a relaunch of a ceiling-free boundary degrades with a
  `resource_cgroup_unavailable` notice instead (ceilings always fail closed).
- **`off`** — the cross-harness explicit opt-out: each harness's native
  no-confinement posture (Claude Code and OpenCode `off`, Codex
  `danger-full-access`), composed profile policy omitted, resource limits
  refused. Because it is recorded, a group or global default cannot silently
  re-sandbox the conversation on a later relaunch.

Select it wherever agents are spawned: `tclaude session new --sandbox-impl`,
`tclaude agent spawn --sandbox-impl`, a spawn profile's Sandbox field, or the
dashboard spawn dialog — which presents the implementation choice first and
shows a "\<Harness\> sandbox mode" row only after you pick the harness's
built-in sandbox. Precedence, highest first: explicit flag or dialog selection,
then `--profile`, then the group's default spawn profile, then the global
default profile, then the harness default.

### Reassigning an existing agent

```bash
tclaude agent stop <agent>
tclaude agent sandbox-impl set <agent> resource-only
tclaude agent resume <agent>
```

`tclaude agent sandbox-impl show|set` takes a positional selector (there is no
self form and no `--target`), refuses while the agent is online, validates the
assignment against the chain the *relaunch* will resolve, and probes cgroup
creation for cgroup-needing implementations. It needs the `agent.sandbox-impl`
permission, which group ownership deliberately does not confer and which is not
granted by default. The dashboard equivalent is **🧩 sandbox implementation…**
in the agent's row menu. A harness mode that was merely derived from the old
implementation is not carried forward; the harness default is recorded instead
unless `--sandbox` pins one.

## The harness's own mode: `--sandbox`

`--sandbox` on `tclaude session new` sets the harness-native sandbox mode. It
is per harness, persisted per conversation, and orthogonal to who enforces:

- **Claude Code**: `inherit` (default — the `settings.json` posture is left
  untouched, which is what the unsandboxed-autonomy warning is about) | `on`
  (force the OS sandbox via a settings overlay; outranked only by managed
  policy) | `off`.
- **Codex**: `tclaude-agent` (the setup-managed profile — workspace-write plus
  the agentd socket plus denies on tclaude's private state; the daemon spawn
  default) | `workspace-write` | `read-only` | `danger-full-access`.
- **Copilot**: `inherit` | `off` only — deliberately no `on`, because tclaude
  has no lever to enable Copilot's sandbox.
- **OpenCode**: no built-in OS sandbox; `tclaude-layer` is its real confinement
  mode.

Under `tclaude-layer` the mode is derived (forced off/danger inside the wall),
not an operator escape hatch.

## The profile model

A sandbox profile is an operator-authored, harness-neutral JSON document —
there is no YAML form. It is Kubernetes-flavored in two concrete ways: memory
limits accept k8s-like quantities (`4GB`, `1.5GiB`, `512MiB`; decimal K/M/G/T
and binary Ki/Mi/Gi/Ti, case-insensitive), and `mount_path` is an explicit
Kubernetes-style volume-mount projection.

The axes:

- **`filesystem`** — rows of `{path, access, mount_path?}` with `access` one of
  `read` | `write` | `deny`. Rows are directories only; a non-directory path is
  rejected, so home-level dotfiles such as `~/.gitconfig` cannot be reopened
  individually under a denied Home. `~` expands to the daemon's home; paths are
  symlink-resolved and case/NFC-canonicalized, and duplicate spellings of one
  physical directory fold into a single row with deny > write > read. Missing
  paths are retained with a warning and skipped at launch — tclaude never
  creates operator-authored host directories.
- **Carve-outs work in both directions.** A narrower row shadows a broader one:
  a narrow `write` reopens beneath a `deny`, and a narrow `deny` hides beneath
  an allow. This most-specific-wins shape is the whole strictness mechanism —
  see below.
- **`mount_path`** — a `read`/`write` row may project its host directory at a
  different sandbox path (`{"path": "/srv/corpus-v3", "access": "read",
  "mount_path": "/data"}`). `deny` rows may not carry one (a deny hides a path,
  it never projects). Two host paths cannot claim one guest path; one host path
  at several guest paths is fine; a host path whose most specific rule is a
  deny may not be remapped. Ordering and most-specific-wins evaluate in
  guest-path space. Enforcement needs a real mount namespace, so it works only
  under `tclaude-layer`/`stacked` on Linux; Seatbelt (a path filter, not a
  mount namespace) and `harness-builtin` refuse the launch with
  `unsupported_sandbox_profile_mount_path` rather than falling back to the
  host path.
- **`environment`** — `{name, value}` pairs, non-secret. Reserved names are
  refused: `HOME`, `PATH`, `SHELL`, `TMPDIR`, `CLAUDE_CONFIG_DIR`,
  `XDG_CONFIG_HOME`, `TMUX` and friends, plus the `TCLAUDE_`, `CLAUDE_CODE_`,
  `CODEX_`, `LD_`, and `DYLD_` prefixes — with the single exception
  `TCLAUDE_OFFLINE_MODEL`.
- **`agent_directories`** — environment-variable names bound to
  tclaude-materialized private per-agent directories.
- **`filesystem_root`** — `inherit` prefers the read-only host root, `separate`
  requests the minimal constructed root even with open network, omitted means
  automatic derivation. Composes monotonically: `separate` anywhere in the
  chain wins, and `inherit` cannot weaken a rule whose enforcement itself
  requires a constructed root.
- **`network`** — baseline `open` | `closed` | `list`, allow/deny rows, packs,
  and a `namespace` selector (the legacy `network_access: internet|none`
  spelling maps to open/closed). Covered on the
  [network-filtering](network-filtering.md) page.
- **`unix_sockets`** — `mode: open|closed|list` plus `path`/`path_glob` entries
  (`**` is refused). The agentd socket is a non-removable floor. Authoring this
  axis on an otherwise host-open profile switches the launch to a constructed
  root; see [Network filtering](network-filtering.md#namespace-and-the-unix-socket-axis).
- **`resource_limits`** — `memory` → cgroup `memory.max`, `cpu` (cores ≥ 0.01)
  → `cpu.max` at a 100 ms period; Linux cgroup v2, whole workload tree,
  orthogonal to confinement, works with any non-`off` implementation. Both
  blank means no cgroup probing at all, except under `resource-only`, which
  always creates its cgroup. macOS, `off`, and hosts without delegated
  controllers refuse by default; the dashboard's "allow launch without
  enforcement" checkbox is the one operator escape hatch and records a visible
  degradation notice.
- **`darwin_allow_mach_register`** — a Seatbelt `(allow mach-register)` opt-in
  for browser/XPC workloads on macOS.
- **`pre_launch`** — named, ordered operator-authored shell fragments that run
  *inside* the sandbox, after profile environment export and before the harness
  starts (≤ 32 blocks, ≤ 64 KiB of script). They hold no extra authority and
  take no part in lineage containment.
- **`includes`** — compose other profiles by name (≤ 32 references, include
  depth ≤ 16, acyclic). Included profiles apply first in listed order, then the
  including profile's own rows; for the same exact path or environment name the
  later layer wins, so a local grant *can* override an included deny. That is
  authoring convenience inside one registry — cross-scope composition below is
  deliberately not last-wins.

### Protected roots

`~/.tclaude/data` and `~/.claude/sessions` are an absolute wall. No profile,
include, launch contract, or flag reopens them, and any read/write rule that
*intersects* one is refused — ancestors count, so an ordinary row over
`~/.claude` is rejected because it contains `sessions`. Comparison is by
filesystem identity, not string: case or Unicode-normalization spellings of a
protected root are refused alike, on every volume, including for paths that do
not exist yet. `~/.codex` is not protected and must be reopened under a denied
Home, which makes a strict-Home profile materially easier to run under Codex
than under Claude Code.

### Reference: scoping `playwright-cli` to one agent

The worked example, and the reason `pre_launch` exists. It gives Playwright
private XDG directories, a per-agent session, and a fixed browser, without any
of that leaking into the agent's other tools. Pair it with
`"agent_directories": ["TCLAUDE_PW_HOME"]` so the directory is per-agent and
writable.

```bash
pw="$TCLAUDE_PW_HOME"
# Resolve the real binary with our own wrapper directory removed from PATH.
# Blocks re-run on every launch including resume, so by the second launch the
# wrapper may already be first on PATH; a plain lookup would find the wrapper
# and wrap it in itself, an exec loop that hangs with no output. Excluding the
# directory makes a re-run idempotent instead of fatal.
pw_search=""
IFS=: read -ra pw_parts <<< "$PATH"
for pw_entry in "${pw_parts[@]}"; do
  [ "$pw_entry" = "$pw/bin" ] && continue
  pw_search="${pw_search:+$pw_search:}$pw_entry"
done
real="$(PATH="$pw_search" command -v playwright-cli || true)"
if [ -z "$real" ]; then
  echo "playwright-cli is not installed on this host" >&2
  false  # abort the launch; tclaude names this block in the failure
fi
export TCLAUDE_PW_REAL="$real"
mkdir -p "$pw"/{config,cache,data,bin}
# A QUOTED heredoc: nothing is interpolated at write time, so a directory
# containing $, backtick, backslash or quote cannot corrupt the wrapper. The
# wrapper reads both values from its environment at run time instead.
cat > "$pw/bin/playwright-cli" <<'WRAPPER'
#!/bin/bash
XDG_CONFIG_HOME="$TCLAUDE_PW_HOME/config" \
XDG_CACHE_HOME="$TCLAUDE_PW_HOME/cache" \
XDG_DATA_HOME="$TCLAUDE_PW_HOME/data" \
exec "$TCLAUDE_PW_REAL" "$@"
WRAPPER
chmod +x "$pw/bin/playwright-cli"
export PATH="$pw/bin:$PATH"
export PLAYWRIGHT_CLI_WRAPPER_DIR="$pw/bin"
# Playwright's downloaded browser bundles live under $XDG_CACHE_HOME, which we
# just made per-agent — so without this every agent would see an empty browser
# registry and re-download hundreds of MB. Keep the registry shared.
export PLAYWRIGHT_BROWSERS_PATH="${PLAYWRIGHT_BROWSERS_PATH:-$HOME/.cache/ms-playwright}"
export PLAYWRIGHT_CLI_SESSION="$(basename "$pw")"
export PLAYWRIGHT_MCP_BROWSER=chrome
export PLAYWRIGHT_MCP_SANDBOX=false
```

Declare `"exports": ["PLAYWRIGHT_CLI_WRAPPER_DIR", "PLAYWRIGHT_CLI_SESSION",
"PLAYWRIGHT_MCP_BROWSER", "PLAYWRIGHT_MCP_SANDBOX"]`. Note what is *not*
there: `PATH` is always set, so declaring it would prove nothing — the check
is set-ness. The wrapper-directory sentinel is set alongside it and is
genuinely absent if the block did not get that far.

Five things are load-bearing:

- **The wrapper, not a global export.** XDG is exported *inside* the wrapper,
  so only Playwright sees it. Exporting it into the agent would take `gh`,
  `git`, `codex` and `claude` with it.
- **Resolve the real binary with the wrapper directory excluded from `PATH`.**
  Blocks re-run on every launch including resume; a plain lookup on the second
  launch would find the wrapper and wrap it in itself — an exec loop that
  hangs with no output. Excluding the directory makes a re-run a no-op.
- **A quoted heredoc.** Nothing is interpolated when the wrapper is written,
  so a directory containing `$`, a backtick, a backslash or a quote cannot
  corrupt it; the wrapper reads both values from its environment at run time.
- **`PLAYWRIGHT_BROWSERS_PATH` stays shared.** The block just made
  `$XDG_CACHE_HOME` per-agent; without re-sharing the browser registry every
  agent would re-download hundreds of megabytes of browser bundles.
- **`false`, not `exit 1`.** Letting the command fail trips tclaude's `ERR`
  trap, which names the block in the failure message; an explicit `exit`
  bypasses it and the operator gets an unattributed exit code.

One limitation worth knowing: `playwright-cli` refuses the `file:` protocol
unless `allowUnrestrictedFileAccess` is set, and every way of setting it
grants the browser unrestricted filesystem reads (the global config also
lives in the agent's real `$HOME`, which this wrapper deliberately does not
redirect). Serve local pages over loopback instead.

This block is not prose nobody runs:
`pkg/claude/session/pre_launch_playwright_reference_test.go` executes it on
every test run and compares the fenced script above against the one it
executes, so the two cannot drift.

## Strictness is a shape, not a mode

There is no "strict mode" switch. Strictness is composed from ordinary rows:

```json
[
  { "path": "~",               "access": "deny"  },
  { "path": "~/git/myproject", "access": "write" },
  { "path": "~/go",            "access": "read"  }
]
```

A `read`/`write` grant strictly beneath a `deny` — a **reopen-under-deny** — is
detected as a shape, and that shape is capability-gated at launch: a backend
that does not honor most-specific-wins would run this strict-looking profile
with a broad baseline, so tclaude refuses the launch
(`unsupported_sandbox_profile_reopen_under_deny`) instead. The combinations
that can faithfully enforce it:

| Backend | Reopen-under-deny |
|---------|-------------------|
| Claude Code, sandbox `on` | supported |
| Claude Code, sandbox `inherit` / `off` | refused — the deny and the reopen may both be dropped |
| Codex, managed `tclaude-agent` profile on Linux, split-policy probe verified | supported |
| Codex on macOS | refused — the deny mask dominates narrower reopens ([openai/codex#21081](https://github.com/openai/codex/issues/21081)) |
| Codex legacy Landlock, or a raw `--sandbox` mode | refused |
| Any other harness under `harness-builtin` | refused |

The `tclaude-layer` and `stacked` implementations enforce the shape themselves
on both platforms. The gate keys on the rules tclaude will *emit*, not just the
rows you authored: a bare `deny ~` with no reopens of your own still becomes a
split policy, because the launch contract adds its own reopens.

### What tclaude reopens for you

Under a broad deny, tclaude pairs read reopens automatically for a short,
closed list: the workspace and daemon-verified git admin paths, the profile's
own `write` grants, declared `agent_directories`, the agentd socket, and — on
Codex only, when a probe proves it needed — the Codex executable. Everything
else is yours to enumerate. In particular, tclaude's own binary is *not*
implicitly reopened: under `deny ~` an agent can reach the agentd socket and
still get `tclaude: command not found` until you reopen the directory holding
the binary (commonly `~/go/bin` or a version-manager install root).

Gotchas worth knowing before you debug: on Linux, a write under a deny can fail
*silently* (exit 0, file gone by the next command — the write landed in a
throwaway mount layer), and `ls ~` shows only reopened paths; on macOS,
Seatbelt returns ordinary permission errors and denied names stay enumerable.
`command not found` for a tool plainly on `$PATH` usually means its install
root is denied — and reopening toolchain *caches* is not enough to build if the
compiler binary itself sits under the deny. Claude Code's built-in denies mask
some paths with `/dev/null` device nodes on Linux, which makes `git add -A`
fail ("can only add regular files"); stage specific paths instead.

## Attachment: global, group, explicit

Profiles attach at three tiers: a **global default**, a **group assignment**,
and an **explicit per-spawn profile** (`--sandbox-profile` on
`tclaude session new` — human-only — plus spawn profiles, the dashboard, and
`tclaude agent spawn`). `--omit-sandbox-profiles` (human-only) omits all three
tiers, and the omission is recorded in the launch snapshot so a resume or
reincarnation cannot resurrect the ambient tiers.

Cross-scope composition is **not** last-wins:

- **Filesystem**: a canonical-path union where deny dominates write dominates
  read, regardless of tier — an explicit profile cannot un-deny a global deny.
  A strictly narrower later row survives as a reopen-under-deny and then meets
  the capability gate above.
- **Environment**: last scope wins (global → group → explicit).
- **Network**: allow authority intersects across scopes while deny authority
  unions; `closed` dominates. Unix-socket lists intersect.
- **`network.namespace: private`** and **`filesystem_root: separate`** compose
  monotonically — a later scope cannot widen them back to host/inherit.
- **`network.engine`** is the one precedence field: most explicit wins
  (explicit > group > global), with a disclosure when composition was
  overridden.

## The `sandbox-profiles` CLI

```bash
tclaude agent sandbox-profiles ls
tclaude agent sandbox-profiles show <name>
tclaude agent sandbox-profiles create --file profile.json
tclaude agent sandbox-profiles edit <name> --file profile.json
tclaude agent sandbox-profiles rm <name>
tclaude agent sandbox-profiles default show|set|clear
tclaude agent sandbox-profiles group show|set|clear <group>
tclaude agent sandbox-profiles export [name…]
tclaude agent sandbox-profiles import --file bundle.json
tclaude agent sandbox-profiles plan --group <g> --sandbox-profile <name>
tclaude agent sandbox-profiles plan --agent <selector>
```

Renames go through `edit` (change the JSON `name`; stable-ID assignments
follow). `import` is transactional, with assignment application opt-in.
`draft --token --file` is the scribe seam: a dashboard-summoned drafting agent
holding only `sandbox-profiles.draft` can propose a validated profile for human
preview but can never save, assign, or launch. Payload reads (`show`) and all
writes require `sandbox-profiles.manage` — human-only by default, and
deliberately not implied by `profiles.manage`; reading the global and group
*assignments* does not.

`plan` is inspection-only, in two modes. Hypothetical mode composes the current
global, group, and explicit tiers for a directory (`--cwd`,
`--for impl[/harness[/platform]]`), predicts access enforcement, and describes
the four outer-layer mount precedence classes. `--agent` mode reads only the
latest frozen launch row — it never re-resolves registry profiles or `stat`s
paths, and marks classes the row did not record as unavailable. Both support
`--json`; missing positive binds report `missing-would-skip`.

The dashboard carries a full profile editor with a common-rules catalog — a
portable deny tier for SSH keys, GnuPG, cloud and container credentials, VCS
tokens, toolchain caches (with a build-breakage warning), browser profiles, and
a "Deny access to the Home directory" rule that inserts an ordinary editable
`deny ~` row.

## Lineage and spawn write-proofs

Confinement is heritable, in one direction only. Resume, reincarnation, and
agent-initiated child spawns can never weaken a deny or introduce a reopen the
recorded parent lacked; both count as widening and are refused. The check is
specificity-aware — a broad parent read above a deny does not "cover" a child's
reopen beneath it. Launch snapshots are revalidated on resume, and drifted
profile rows or spellings refuse with `sandbox_profile_changed`. Humans are the
trust root and bypass lineage.

A second guardrail closes the anchor-point escape: a sandboxed parent choosing
a child cwd its own sandbox cannot write. agentd answers such a spawn with a
single-use challenge (`write_proof_required`); the parent must create the
dot-prefixed proof file in each target directory *from inside its own sandbox*
and retry, and agentd verifies and deletes the proof. Humans, fully open
parents, and children that take no cwd write are exempt. The full guardrail
list lives in [Spawning and lifecycle](spawning-and-lifecycle.md).

## Hardening tclaude's own state

`tclaude setup --install-sandbox-hardening` protects the daemon's trust anchors
— `~/.tclaude/data` (database, config, operator token, remote-access CA keys)
and `~/.claude/sessions` (identity forgery) — from the agents it coordinates.
agentd's identity and permission layer is a coordination guardrail, not a
security boundary; the OS sandbox is what makes it hold.

The installer writes an append-only, idempotent block into user-scope
`~/.claude/settings.json` covering **both Claude Code layers**, because neither
alone is enough:

- **Layer 1, the OS sandbox** (`sandbox.enabled: true`,
  `failIfUnavailable: true`, `allowUnsandboxedCommands: false`, filesystem
  deny-read/deny-write on both protected trees with the agentd socket
  re-allowed, and `allowedDomains: github.com, api.github.com`). A kernel
  boundary — but it applies only to Bash commands and their children, not to
  the built-in `Read`/`Write`/`Edit` tools. That gap is verified: the `Write`
  tool created a file under `~/.tclaude/` on a machine whose Bash sandbox
  treated it as read-only.
- **Layer 2, permission rules** (`permissions.deny` `Read(…)`/`Edit(…)`
  mirrors). These gate the built-in tools — and are string matching, not a
  boundary, so an arbitrary subprocess that opens a file itself slips past
  them. Layer 1 contains what layer 2 cannot see; layer 2 gates what layer 1
  does not reach.

The installed block, in full — user scope, `~/.claude/settings.json`, so no
project can weaken it (`tclaude setup --install-sandbox-hardening` merges it
append-only and idempotently; a test keeps this block and the installer's
in-code spec identical):

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

Re-run the installer after upgrades — older installs lack newer keys such as
`failIfUnavailable`. Conflicting scalars are preserved and reported. On shared
machines, use managed settings with `forbidUnsandboxedCommands: true`. tclaude
also applies a per-launch overlay denying the exact tmux socket path, so agents
cannot `send-keys` at tclaude's tmux server.

### Honestly residual holes

- **MCP bypasses both layers entirely.** MCP servers run in the harness host
  process over their own transport, outside the Bash sandbox and the permission
  rules. An agent that cannot see `~/.config/gh` may still file a GitHub issue
  through an MCP server. Control MCP where MCP servers are configured, not in
  the profile.
- **Linux Unix-socket widening.** Claude Code's Linux seccomp filter cannot see
  socket paths, so the only knob is `allowAllUnixSockets: true` — a deliberate
  widening the installer accepts to keep the agentd socket reachable (the
  path-list form works on macOS only).
- **Reopen-under-deny is layer-1-only.** Leaf denies are mirrored to layer 2,
  but a deny *with a reopen beneath it* cannot be expressed as Claude Code
  permission rules (deny rules carry no exceptions), so under a `deny ~`
  profile the built-in `Read` tool can still read denied paths unless you add
  user-scope leaf rules yourself. tclaude logs a warning naming each deny it
  could not mirror.
- The GitHub domain allowlist is itself an exfiltration path; full sandbox
  escape is out of scope (the trust boundary is the Unix UID); and the sandbox
  badge is a launch-time verdict — resume re-resolves it.

The dashboard badges the posture per agent: `🔒` sandbox on, `🔒²` stacked,
`⚠` off or unknown, nothing for plain inherit. An unsandboxed-autonomy warning
fires wherever auto-approval pairs with an unprovable sandbox. The
[credential proxies](proxies.md) are what make denying `~/.ssh` and
`~/.config/gh` survivable for agents that still need to push branches.

## Symptom → cause

| Symptom | Likely cause |
|---------|--------------|
| Files written, exit 0, gone next command | Silent write under a deny (Linux bubblewrap) |
| `command not found` for a tool on `$PATH` | Install root under a deny and not reopened |
| Builds fail despite readable caches | Toolchain binary root denied, not just the cache |
| `tclaude: command not found`, socket fine | tclaude's binary dir not reopened — it is never implicit |
| Git loses identity / credential helper | `~/.gitconfig` is a file; files cannot be reopened |
| `git add -A`: "can only add regular files" | Claude Code masks a denied path with a `/dev/null` device node — stage specific paths |
| Launch refused, `…reopen_under_deny` | Claude Code not sandbox `on`, or Codex not Linux managed-profile with a verified probe |
| `stacked` refused on Ubuntu 24.04+ | The `bwrap-userns-restrict` AppArmor policy denies nested bubblewrap |
| Profile looks strict, nothing is denied | Claude Code sandbox `inherit`/`off` — the deny rows are emitted but the sandbox never engages |
| Agent read a denied path with the `Read` tool | Expected under `deny ~` — that shape reaches layer 1 only |
| Agent reached something the profile denied | Check MCP, which bypasses the sandbox |

## See also

- [Network filtering](network-filtering.md) — the two egress engines, packs,
  and the unix-socket axis.
- [Credential proxies](proxies.md) — git, GitHub, and Linear without secrets in
  the sandbox.
- [Spawning and lifecycle](spawning-and-lifecycle.md) — the full spawn
  guardrail list.
- [Permissions and audit](permissions-and-audit.md) — the slug model behind
  `sandbox-profiles.manage` and `agent.sandbox-impl`.
- Claude Code sandboxing: <https://code.claude.com/docs/en/sandboxing>
