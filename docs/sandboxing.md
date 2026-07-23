# How sandboxing works (operator mental model)

**Audience:** operators who want to understand what a tclaude sandbox profile
actually does to a running agent — before they author one, and while they are
debugging one that misbehaves.

This is the **mental model and troubleshooting** guide. The reference material
lives elsewhere and is not repeated here:

| For | Read |
|-----|------|
| Profile wire shape, deny/reopen rules, break-glass, the CLI | [Agent coordination → sandbox profiles](agent.md#sandbox-profiles) |
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

### A sandbox profile currently drives layer 1 only

⚠️ This one deserves to be stated plainly, because a profile reads stricter than
it enforces.

Your profile's `deny` / `read` / `write` rows render into
`sandbox.filesystem.*` — layer 1 — and nothing else. tclaude does **not**
derive the matching `permissions.deny` `Read(…)` / `Edit(…)` rules from them.

So a profile carrying `deny ~/.ssh` confines Bash, but the agent can still read
`~/.ssh/id_rsa` with the built-in `Read` tool, and still write under the denied
path with `Write`/`Edit`. The same goes for `deny ~`. The editor,
`sandbox-profiles show`, and the resolved launch echo all show the deny; only
one of the two layers is actually carrying it.

**The two protected roots are the exception.** `~/.tclaude/data` and
`~/.claude/sessions` *are* defended on both layers, because
`tclaude setup --install-sandbox-hardening` writes a static `permissions.deny`
block for exactly those paths. Everything you author yourself gets layer 1 only.

Until this is closed in tclaude, treat it as an operator task: if a deny in your
profile needs to bind the built-in tools too, add the matching rules to your own
`settings.json` by hand —

```json
{
  "permissions": {
    "deny": ["Read(~/.ssh/**)", "Edit(~/.ssh/**)"]
  }
}
```

— at user scope, where no project's settings can weaken them. Tracked as
**TCL-666**.

### What neither layer covers: MCP

**MCP tools bypass the Bash sandbox entirely.** MCP servers run in the harness
host process over their own transport, not through the sandboxed Bash
filesystem/egress boundary. An agent that cannot see `~/.config/gh` and has no
reachable `gh` binary may still be able to file a GitHub issue through an MCP
server, and an agent with `network_access: none` may still reach the network
through one.

If MCP reachability matters to your threat model, control it where MCP servers
are configured — not in the sandbox profile.

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
| Launch refused, `unsupported_sandbox_profile_reopen_under_deny` | Claude not in sandbox `on`, or Codex not on Linux managed-profile with a verified probe |
| Profile looks strict but nothing is denied | Claude sandbox `inherit`/`off`, or a legacy `read_baseline` profile (silently dropped — re-express as deny rows) |
| An agent read a denied path with the `Read` tool, but not from Bash | Expected today — a profile drives layer 1 only ([above](#a-sandbox-profile-currently-drives-layer-1-only)) |
| An agent reached something the profile denied | Check whether it went through MCP, which bypasses the Bash sandbox |

## See also

- [Agent coordination → sandbox profiles](agent.md#sandbox-profiles) — the full
  profile reference, break-glass, and CLI.
- [Harnesses](harnesses.md) — per-session sandbox modes and the capability matrix.
- [Sandbox hardening](sandbox-hardening.md) — protecting agentd's own state.
- Claude Code sandboxing: <https://code.claude.com/docs/en/sandboxing>
- Claude Code permissions: <https://code.claude.com/docs/en/permissions>
