# CLI shortcuts and small CLI ergonomics (2026-05)

Bundle of small CLI surface improvements.

## `tclaude --join-group <group>`

Top-level + `session new` flag that spawns a fresh CC session via
the existing daemon `groups.spawn` endpoint, then attaches in the
foreground. `-d` flips to detached + prints the attach command.

- Optional `--alias` / `--role` / `--descr` mirror `agent spawn`.
- Tab-completion suggests existing group names.
- Conflicts with `--resume` / `--label` and rejects up front.
- No new daemon code — purely a CLI orchestration layer over
  the existing spawn endpoint.

Wired via a `session.JoinGroupHandler` function-variable hook so
the `agent → session` import direction stays clean.

## Spawn cwd defaulting (commit d7b13e6)

`tclaude agent spawn` and `tclaude --join-group` (and
`tclaude session new --join-group`) now capture `os.Getwd()`
CLI-side when `-C` / `--cwd` is omitted. Explicit `-C` overrides.
Help text updated; daemon endpoint unchanged.

Previously the new agent inherited whatever directory the daemon
was started in, which is almost never what the human wants when
typing `agent spawn` from a project tree.

## `tclaude agent delete <selector>` (commit e8e4215)

Orphan cleanup verb. Removes a conv from group memberships,
ownership, permission grants — does NOT touch the .jsonl. For
sweeping up rows left behind by external state (e.g. a CC
process killed without `/exit`).

## `agent stop` / `agent resume` (commit 63bacad)

Single-conv variants of the bulk `groups.stop` / `groups.resume`:

- `tclaude agent stop <selector>` — soft `/exit` injection, or
  `--force` for `kill-session`.
- `tclaude agent resume <selector>` — spawn detached via
  `tclaude session new -r <conv> -d --global`.

Both routed through `/v1/agent/{selector}/{verb}`, same auth
model as other cross-agent ops (slug OR owner-of-group). Slugs:
`agent.stop` / `agent.resume`. Bulk handlers refactored to call
the same `stopOneConv` / `resumeOneConv` per-conv helpers, so
the semantics match exactly between bulk and single-conv paths.

Unblocks the future dashboard wake-up / shut-down buttons.

## `--state=online|offline` filter

On `agent ls` and `agent groups ls`. Tab-completion offers the
two values with descriptions.

## Spawn injects `/rename` + welcome to materialise .jsonl
(commit bc7ec81)

Newly-spawned convs got their `.jsonl` written immediately by
injecting a `/rename` plus a welcome line, so the daemon's
poll-for-new-conv-id loop converges quickly.
