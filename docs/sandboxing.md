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
  own wall: bubblewrap mount/IPC/cgroup/PID (and optionally network) namespaces
  on Linux, Seatbelt (`sandbox-exec`) on macOS. The harness's own OS sandbox is forced
  off inside it (Claude Code mode `off`, Codex `danger-full-access`; Copilot
  has no off-flag tclaude can set, so its configuration is verified instead and
  an unverifiable posture refuses). Supported for Claude Code, Codex, OpenCode,
  and Copilot, on Linux and macOS. For OpenCode the wrapped process is the
  agentd-owned `opencode serve` executor; the attach pane stays outside.
  Requires `bwrap` and working unprivileged user namespaces on Linux; a missing
  capability refuses the launch — never a silent fallback — from whichever tier
  selected the layer. The `bwrap` found on `PATH` must also pass the trust walk
  described under [An untrusted `bwrap`](#an-untrusted-bwrap). On Linux the layer also creates and joins a per-launch
  cgroup even with no `resource_limits` authored: tclaude already owns and forks
  this workload, so the same accounting `resource-only` gives (`memory.peak`,
  `cpu.stat`, host-OOM attribution, one kill handle for the whole tree) comes
  along. That boundary is a bonus, not the posture — a host with no delegated
  cgroup gets a `resource_cgroup_unavailable` notice and the wall it asked for,
  where `resource-only` refuses. An authored ceiling still fails closed.
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
creation for implementations that cannot launch without one. It needs the
`agent.sandbox-impl` permission, which group ownership deliberately does not
confer and which is not granted by default. The dashboard equivalent is
**🧩 sandbox implementation…** in the agent's row menu. A harness mode that was
merely derived from the old implementation is not carried forward; the harness
default is recorded instead unless `--sandbox` pins one.

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
- **`harness_config`** — `read` | `write`, omitted meaning the default
  read-only floor over the harness's own config surface. See
  [The harness-config floor](#the-harness-config-floor) below. Composes
  strictest-wins: an explicit `read` anywhere in the chain cannot be widened
  back to `write` by a later scope or include.
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
- **`network`** — an authored `baseline` of `inherit` | `allow` | `deny`,
  allow/deny rows, packs, and a `namespace` selector (the legacy
  `network_access: internet|none` spelling is still accepted). The composed
  result resolves to an open, closed, or list-filtered launch. Covered on the
  [network-filtering](network-filtering.md) page. On Linux packet-filtered
  launches, `preserve_caller_identity: true` opts the harness into the invoking
  numeric UID/GID; omitted/false retains the historical namespace-root identity.
- **`unix_sockets`** — `mode: open|closed|list` plus `path`/`path_glob` entries
  (`**` is refused). The agentd socket is a non-removable floor. Authoring this
  axis on an otherwise host-open profile switches the launch to a constructed
  root; see [Network filtering](network-filtering.md#namespace-and-the-unix-socket-axis).
- **`resource_limits`** — `memory` → cgroup `memory.max`, `cpu` (cores ≥ 0.01)
  → `cpu.max` at a 100 ms period; Linux cgroup v2, whole workload tree,
  orthogonal to confinement, works with any non-`off` implementation. Both
  blank means no cgroup probing at all, except under `resource-only` (which
  always creates its cgroup) and under a Linux `tclaude-layer`/`stacked` launch
  (which tries, and degrades to a notice if the host cannot). macOS, `off`, and
  hosts without delegated controllers refuse by default every cgroup a launch
  cannot proceed without — an authored ceiling under any implementation, and
  `resource-only` even with no ceiling; the dashboard's "allow launch without
  enforcement" checkbox is the one operator escape hatch and records a visible
  degradation notice. Only the opportunistic layer boundary degrades instead.
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

### The harness-config floor

Under `tclaude-layer` the launch contract binds the harness's state root
read-write — `~/.claude`, `$CODEX_HOME`/`~/.codex`, `$COPILOT_HOME`/`~/.copilot`,
`~/.opencode` plus OpenCode's XDG roots — because that is where the harness
keeps state it genuinely must write: transcripts, project records, todos,
history, account/onboarding data. The same tree also holds the harness's
**policy and persistent-code surface**, and writing that is an escape:

- `~/.claude/settings.json` carries the block
  `tclaude setup --install-sandbox-hardening` installs. Strip it and the wall
  around `~/.tclaude/data` and `~/.claude/sessions` is gone for every
  `harness-builtin` launch and for the human's own ambient `claude`.
- `~/.codex/tclaude-agent.config.toml` *is* the managed profile that provides
  Codex's `harness-builtin` confinement.
- A hook, skill, agent, or command dropped under `~/.claude/hooks` (and
  friends) runs in the human's **next, unsandboxed** session.

So tclaude binds a closed per-harness catalog of those paths read-only by
default, materializing missing ones first — an absent `~/.claude/hooks` under a
writable state root is just a directory the agent can create. The floor is
enforced by the ordinary read-grant path, so it appears in
`sandbox-profiles plan` as `launch-contract / harness-config-floor` on both
bubblewrap and Seatbelt.

| harness | floored |
|---|---|
| claude | `hooks/`, `skills/`, `agents/`, `commands/`, `output-styles/`, `plugins/`, `workflows/`, `routines/`, `rules/`, `local/`, `cowork_plugins/`, `settings.json`, `settings.local.json`, `CLAUDE.md`, `keybindings.json` |
| codex | `hooks/`, `prompts/`, `config.toml`, `hooks.json`, `AGENTS.md`, `tclaude-agent.config.toml` |
| copilot | `hooks/`, `settings.json`, `config.json`, `mcp-config.json` |
| opencode | nothing — its config tree is already bound read-only by OpenCode's own state layout, in both legacy-shared and private modes |

Claude Code's own sandbox deny-writes a broadly similar set for its Bash tool;
without the floor tclaude's outer wall was *weaker* than the harness's own
default.

Materialization writes only what is indistinguishable from absent: an empty
directory, or `{}` for a JSON entry. An empty JSON file is **not** equivalent
to a missing one — tclaude's own readers and Claude Code alike treat a missing
file as `{}` but report an existing empty one as unparseable — so the seed
matters. Existing content is never touched.

**Symlinked entries are skipped**, with a warning naming the path. Dotfile
managers commonly point `~/.claude/skills` at a repo, and there is no faithful
way to floor that: binding the resolved target leaves the *name* an ordinary
symlink inside the writable state root, which the agent can unlink and replace
with a real directory of its own. Skipping is disclosed; flooring the target
would be a false claim. Point the symlink the other way — make `~/.claude/hooks`
the real directory — if you want it floored.

**What it costs.** Unlike Claude Code's Bash-only deny list, a bubblewrap
read-only bind blocks everyone inside the wall, the harness process included.
In-pane writes to a floored file therefore fail: Claude Code's `/config` and
user-scope `/permissions`, Codex's `/model` persistence, Copilot's
trust-folder record. `/model` and directory trust for Claude Code land in
`.claude.json`, which stays writable.

**Two escape hatches**, least blunt first:

1. An explicit profile write row **at exactly one floored path** drops that
   single entry — `{"path": "~/.claude/plugins", "access": "write"}` reopens
   plugin installs and nothing else. A broader `~` or `~/.claude` write does
   not count: the operator has to name the surface. A row *beneath* a floored
   directory (`~/.claude/hooks/mine`) reopens only that path and leaves the
   floor over the rest of the directory intact. Rows are directories only, so
   a floored *file* cannot be reopened this way.
2. `"harness_config": "write"` turns the whole floor off, restoring the
   pre-floor posture — but only when nothing else in the chain pins it.
   Composition is strictest-wins, so an explicit `"read"` in any included,
   global, group, or explicit profile outranks a `"write"` anywhere else. That
   asymmetry is deliberate: it lets an operator pin the floor globally and know
   a per-spawn profile cannot quietly undo it.
3. `--harness-config read|write` at spawn time, for granting one agent the
   posture without authoring a profile for it:

   ```bash
   tclaude agent spawn <group> --harness-config write
   ```

   Unlike the profile chain this is a launch contract, not a fourth scope: it
   overrides the composed value outright, the same way `--omit-sandbox-profiles`
   overrides the ambient tiers rather than merging with them. A human may set
   it directly; an **agent** needs the `sandbox.harness-config` permission,
   which is not default-granted and which group ownership deliberately does not
   confer — lifting the floor lets the launched agent rewrite the policy that
   confines it. `write` cannot be combined with `--omit-sandbox-profiles`,
   whose snapshot records "no profile tier applied at all"; `read` is accepted
   there as the no-op it is, since the floor is already what an absent value
   means.

   The slug gates *selection*, never enforcement. The floor is a mount frozen
   at launch, so no permission makes a running agent's config surface writable,
   and [lineage](#lineage-and-spawn-write-proofs) still refuses a child posture
   wider than its recorded parent's — an agent holding the slug can pin the
   floor on a child, but can only pass `write` down if it already has it.

The floor applies where tclaude owns the wall — `tclaude-layer` and `stacked`.
Under `harness-builtin` the harness's own policy governs and the axis is
inert; under `resource-only`/`off` nothing is enforced by design. Lineage
treats it like any other containment rule: a floored parent cannot spawn an
unfloored child.

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

## Exporting an agent's debug configuration

When a sandbox behaves differently from the operator's shell, export the
configuration recorded for that specific agent instead of trying to recreate
its launch from current defaults:

```bash
tclaude agent debug-export <agent> --file agent-debug.json
```

An agent can omit `<agent>` to export its own configuration. Exporting another
agent requires `agent.debug-export`, the group-scoped
`groups.members.debug-export`, or ownership covering the target's active
groups. The Groups dashboard exposes the same download as **export debug info**
in each agent's cog menu, including for offline agents.

The versioned JSON deliberately separates three configurations:

- `requested` — the original spawn parameters, including which values were
  omitted;
- `resolved` — durable resume/relaunch values and the exact composed sandbox
  snapshot, including applied-profile provenance, filesystem grants,
  environment values, harness-config posture, network/socket rules, resource
  limits, and pre-launch blocks;
- `running` — the latest recorded launch's harness mode, sandbox
  implementation, model/effort, status/error detail, OS-sandbox verdict,
  approval posture, effective policy, and a versioned execution-boundary
  record. The boundary includes the resolved harness executable/runtime roots,
  the host `tclaude` source and its sandbox mount target, automatic static-root
  and daemon-final mounts, the materialized outer-layer render input, PATH
  construction and pre-launch mutation caveats, and host-to-sandbox UID/GID
  mapping.

The export records agentd's current UID/GID/groups separately from the launch
boundary's host and sandbox identities. On Linux it also reads the recorded
live harness process through `/proc` and reports its host-visible real,
effective, saved, and filesystem UID/GID values, supplementary groups, and
the kernel's live `uid_map`/`gid_map`, actual executable path, and current
`PATH`. A stopped process, an unreadable `/proc` entry, or a
platform without that observation is reported explicitly as unavailable or
unsupported; the export never substitutes agentd's environment for the
agent's.

The export also records the tclaude version and host platform. Environment
values and local paths are intentionally included because they are often the
cause of sandbox failures, so treat the file as sensitive. Initial task text
and one-time authorization tokens are redacted.

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
      "allowUnixSockets":    ["~/.tclaude/api/agentd-socket/agentd.sock", "~/.tclaude/api/agentd.sock", "~/.tclaude-agentd.sock", "~/.tclaude/agentd.sock"],
      "allowAllUnixSockets": true,
      "allowedDomains":      ["github.com", "api.github.com"]
    },
    "filesystem": {
      "denyWrite": ["~/.tclaude/data", "~/.claude/sessions"],
      "denyRead":  ["~/.tclaude/data", "~/.claude/sessions"],
      "allowRead": ["~/.tclaude/api/agentd-socket", "~/.tclaude/api/agentd.sock", "~/.tclaude-agentd.sock", "~/.tclaude/agentd.sock"]
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

- **`~/.claude/.claude.json` stays writable, and it carries `mcpServers`.**
  Under `tclaude-layer` the harness-config floor cannot cover it: Claude Code
  writes that file continuously (project records, directory trust, history),
  and `CLAUDE_CONFIG_DIR` puts it inside the same writable state root. An agent
  that appends an `mcpServers` entry there gets that command executed by the
  next tclaude-launched Claude pane — including a `harness-builtin` one with no
  outer wall. This is the residual member of the same escalation family the
  floor closes, and closing it needs a different mechanism than a read-only
  bind.
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

## Deep dives

### Namespaces every Linux posture takes

Network and PID namespaces are posture-dependent — they cost real capability
(no host IP, no host process table) and are paid only where a posture's claims
need them. Two others are requested in every posture, including the
walking-skeleton host-open one that keeps the host network namespace and the
read-only host root. Only one of the two is *guaranteed*, and the difference
matters:

- **IPC.** This closes System V IPC *and* POSIX message queues. Neither is a
  filesystem object: `shmget`/`semget`/`msgget` take integer keys, and
  `mq_open` resolves its name against the IPC namespace's own internal mount
  rather than any path — `/dev/mqueue` only makes queues browsable, it is not
  what makes them work. Both are permission-checked by uid, so a sandboxed
  process running as the invoking user would otherwise reach every segment,
  semaphore, and message queue that user owns on the host, in either direction,
  with nothing a mount plan could do about it. This is the one flag here that
  an enforcement claim rests on, so it is a hard requirement: a kernel that
  cannot build an IPC namespace refuses the launch.
- **cgroup**, via `--unshare-cgroup-try`. An information boundary, not a
  containment one, and a partial one — nothing claims otherwise. Escaping a
  cgroup needs a writable `/sys/fs/cgroup`, which no posture provides, so
  there is nothing here to contain. What it closes is `/proc/self/cgroup`
  naming agentd's delegated node and the session inside it, and it closes that
  fully only under a **constructed root**, which binds no `/sys` at all. Under
  the walking skeleton's recursive read-only host root, `/sys/fs/cgroup` is
  still mounted from the parent namespace — a cgroup namespace re-roots
  `/proc/PID/cgroup` and freshly mounted cgroup2 filesystems, not an inherited
  mount — and that posture has no PID namespace either, so the layout is still
  recoverable by finding your own host PID under `cgroup.procs`. The `-try`
  spelling follows from that: cgroup namespaces need kernel 4.6, and refusing a
  launch over a partial disclosure fix that backs no enforcement claim would be
  the wrong trade. **So this one is best-effort, not guaranteed:** on a kernel
  or under an outer confinement that refuses the namespace, the launch
  continues without it and `/proc/self/cgroup` reads the host path as before.
  Nothing else about the posture changes, because nothing else depends on it —
  but do not treat cgroup hiding as a property you can rely on. Your workload's
  resource ceiling is unaffected either way — `resource-limit-exec` runs
  outside bubblewrap and puts the workload in the cgroup before the launch
  command execs.

Two consequences worth knowing:

- System V IPC and POSIX message queues are now **absent** in every posture,
  including the default one. `ipcs` shows nothing, and anything relying on
  shared-memory IPC with a host process — X11 MIT-SHM against a host X server
  is the classic case — degrades or fails. This is why `docker run --ipc=host`
  exists. Harmless for terminal-driven harnesses, which is what this layer
  wraps.
- A **nested** `tclaude` run *inside* a sandbox sees `0::/` and cannot derive a
  delegated cgroup parent from it — when the cgroup namespace was created at
  all; see the best-effort note above. With no `resource_limits` authored that
  degrades with a diagnosis. With a ceiling authored it fails closed, exactly
  as an undelegated host does — there is no flag to wave it through; the
  dashboard's "Allow launch without enforcement" fresh-spawn control is the
  only thing that widens it.

UTS is deliberately not unshared. The sandbox already cannot change a hostname
— that needs `CAP_SYS_ADMIN` in the user namespace owning the host's UTS
namespace, which is the initial one, and bubblewrap drops all capabilities. A
synthetic hostname would be a concealment feature rather than a boundary, and
it would need a synthesized `/etc/hosts` in every posture to avoid breaking
`getaddrinfo(gethostname())`, since `/etc` is bound whole from the host.

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
  them. The canonical `~/.tclaude/api/agentd-socket/` directory is bound
  read-only as a launch-contract path, so socket replacement remains visible
  across agentd restarts.

Since TCL-798 the constructed root is no longer welded to the network posture.
A sandbox profile can select the filesystem root explicitly with
`filesystem_root`: omit it for **Automatic**, use `inherit` to prefer the
read-only host root, or use `separate` to request the minimal constructed root
even when network and Unix sockets remain open. Explicit separation is
supported by Linux `tclaude-layer` for Claude Code, Codex, OpenCode, and
Copilot; other targets refuse it during preview/spawn rather than ignoring it.

The setting composes monotonically. `separate` in any included, global, group,
or explicit profile wins. `inherit` cannot weaken a private/restricted network
or Unix-socket rule whose enforcement itself requires a constructed root. This
keeps existing profiles unchanged: an omitted setting retains the same
automatic derivation they had before the control existed.

A profile that leaves network access open but authors the `unix_sockets` axis
as `closed` or an allow `list` gets a **host-network constructed root** on
Linux for Claude Code, Codex, OpenCode, and Copilot: bubblewrap builds the same fresh
root and PID namespace as the isolated posture, binds the agentd socket and any
listed sockets back, and does NOT create a network namespace, so host IP
networking, host loopback services, and the IDE bridge keep working. For
OpenCode, the attach pane remains outside while its agentd-owned tool server is
wrapped by that root.

That posture is deliberately rated **partially enforced**, permanently. With the
host network namespace shared, Linux abstract-namespace Unix sockets (`@…`) are
not filesystem objects at all, so no mount plan can hide them; close network
access as well if you need those confined too. The sibling non-filesystem
channels — System V IPC and POSIX message queues — *are* closed here, since
every posture unshares the IPC namespace, so abstract sockets are the
remainder rather than one example of a class. The recursive-root remainder
applies here as it does under closed network access: a socket beneath a
directory the profile makes readable or writable stays reachable.

The PID namespace is a **requirement** of this posture rather than a side
effect, and it has a cost worth knowing before you author the axis. Without it a
host process's `/proc/<pid>/root` leads straight back to the sockets the
constructed root just hid, so the posture's whole claim would be false. The
consequence is that the agent cannot see or signal host processes, and tools
that read the host process table stop working. This is stated in the launch
warning alongside the abstract-socket caveat.

It is never newly enabled by an omitted setting. A profile that says nothing
about `filesystem_root`, says nothing about `unix_sockets` (or sets sockets to
`open`), and has no network rule requiring construction launches with exactly
the read-only host root it launched with before.

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

### An untrusted `bwrap`

**Symptom.** Every `tclaude-layer` launch refuses with `tclaude-layer could not
resolve a trusted bubblewrap (bwrap)`, naming a path component and one of
*group/world writable*, *not a regular executable*, or *is not a directory*.
`bwrap` itself runs fine from a shell.

**Cause.** tclaude resolves `bwrap` from `PATH`, follows it to its real path,
and walks that path and every parent directory up to `/`. A launch is only as
trustworthy as the binary that builds its sandbox, so a `bwrap` anyone can
replace is refused rather than exec'd. The same walk applies to the
filtered-network helpers (`pasta`, `nft`, `nsenter`) — see
[network filtering](network-filtering.md).

The walk requires that no component is group- or world-writable, that the
target is a regular file with an execute bit, and that every parent is a
directory. **Ownership is not checked**, so a `bwrap` you built and installed
under your own prefix is fine; a world-writable directory anywhere above it is
not.

**Fix.** Tighten the offending component (`chmod go-w`), or install `bwrap`
somewhere that already satisfies the walk. The refusal names the exact
component, so there is no guessing.

**Not the cause.** This is unrelated to unprivileged user namespaces: the walk
runs before the capability probe, so a trust refusal means the probe never
ran. A namespace failure reads *cannot create the bubblewrap … namespace*
instead.

### Where the tclaude-layer capability probe runs

`tclaude-layer` refuses rather than falling back, and that promise is only as
good as the pre-flight probe behind it. On Linux the probe and the launch used
to stand in different places, so on some hosts the probe could pass a launch
that could not run.

**The two places.** The probe is reached from `tclaude session new`: a child of
`tclaude agentd` for a dashboard spawn, or of your shell for a CLI launch. The
process that really execs `bwrap` is several hops away — tmux server → pane
bootstrap shell → dir-proof guard shell → exit-gate shell → (whenever the
launch has a cgroup) `session resource-limit-exec` → `tclaude …
tclaude-layer-winch-relay` → `bwrap` — and the tmux server inherits its
confinement from whatever first auto-started it. Any per-process confinement
that differs between the two (an AppArmor profile per binary, an SELinux
domain, a seccomp filter, a differing `no_new_privs`) is invisible to a probe
that never runs under it.

**What tclaude does now.** When a tmux server is already running, the probe is
executed *through* it: `tmux run-shell` forks the same `tclaude` capability
probe from the server, one `sh` and one `tclaude` exec away from `bwrap`, so a
profile transition keyed on either executable applies to the probe exactly as
it will to the launch. A passing posture is remembered briefly (keyed on the
server pid, so a restart re-asks, and expiring on its own so a prerequisite you
remove stops being reported as present); a failing one is never remembered, so
installing the missing capability takes effect on the next launch.

The hops are close, not identical. The launch also crosses the guard and gate
shells and, whenever the launch has one, a per-session cgroup that a tmux job
does not join — so a confinement expressed as **cgroup policy** is not
reproduced by this probe. What it reproduces is the per-process confinement
inherited from the tmux server, which is the case observed.

When no tmux server is running yet, the probe runs in-process. That is usually
exact rather than merely safe: this process is the one that will auto-start the
server, so its confinement is the one the pane inherits. The exception is a
profile that transitions on the tmux binary itself (`/usr/bin/tmux Px -> tmux`,
or an SELinux `type_transition`), which puts the server in a domain the
launching process is not in.

**When the probe cannot reach the server, it says so.** If the round trip
cannot be made — the staging path is unreachable because agentd runs under
systemd `PrivateTmp=yes` while tmux does not, the confined server cannot write
it, the job publishes nothing — tclaude falls back to probing in the preparing
process and logs a warning naming the reason. Nothing is refused on a failed
round trip, so without that line a permanently disabled probe would look
exactly like the original bug. If you are debugging "why did it not refuse?",
that warning is the first thing to grep for.

**The residual cases still fail closed at the pane.** A confinement that changes
between probe and launch, a tmux server that restarts under a different
profile, or any of the fallbacks above can still leave the exec denied. The
relay reports that denial as a named refusal —
`tclaude-layer requested — refused: the host denied this process permission to
execute bubblewrap …` — instead of the bare `fork/exec …: operation not
permitted` at exit 125 it used to print. Nothing runs unconfined; but the pane
dies rather than being refused pre-flight, so this is evidence that names the
cause, not a restored pre-flight contract.

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
| `could not resolve a trusted bubblewrap (bwrap)` | A group/world-writable component above `bwrap` — see [an untrusted `bwrap`](#an-untrusted-bwrap) |
| Pane dies instantly, `refused: the host denied this process permission to execute bubblewrap` | The tmux server's confinement forbids the exec — see [where the probe runs](#where-the-tclaude-layer-capability-probe-runs) |
| Profile looks strict, nothing is denied | Claude Code sandbox `inherit`/`off` — the deny rows are emitted but the sandbox never engages |
| Agent read a denied path with the `Read` tool | Expected under `deny ~` — that shape reaches layer 1 only |
| Agent reached something the profile denied | Check MCP, which bypasses the sandbox |
| `/config`, `/permissions`, or `/model` write fails in the pane | The harness-config floor — reopen the one path, or set `harness_config: "write"` |

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
