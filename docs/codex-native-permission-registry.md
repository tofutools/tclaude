# Codex native permission registry setup

Codex app-server launches that use Codex's built-in `tclaude-agent` sandbox
need an administrator requirements file to keep the exact generated permission
profile selected across TUI thread and turn reloads. tclaude manages that
profile catalog in a private user-owned directory, but it never creates or
replaces administrator policy under `/etc`.

This setup is required only for the Codex app-server drive with the
`harness-builtin` sandbox implementation. The normal send-keys drive and
app-server launches using a tclaude-level outer sandbox do not use this
registry.

## One-time setup

First confirm that `/etc/codex` does not already exist:

```bash
sudo ls -ld /etc/codex
```

If it exists, stop here. It may be enterprise or administrator-managed Codex
policy. tclaude deliberately refuses to adopt, edit, rename, or replace it.
Ask the administrator whether this integration is appropriate for the host.

If it is absent, create the guarded target as the user who runs tclaude, then
ask an administrator to create the single root-owned symlink:

```bash
mkdir -p "$HOME/.tclaude/data/codex-sb-cfg"
chmod 700 "$HOME/.tclaude/data/codex-sb-cfg"
sudo ln -s "$HOME/.tclaude/data/codex-sb-cfg" /etc/codex
```

The target and the `~/.tclaude` and `~/.tclaude/data` path components must be
real directories owned by the current user, without group/world write access.
The target must have mode `0700`. `/etc/codex` must be a root-owned symlink
whose literal absolute target is exactly the directory above. tclaude-created
`config.toml`, `requirements.toml`, and `registry.lock` files are user-owned
regular files with mode `0600`; nested symlinks are refused.

Restart `tclaude-agentd` after setup, or make an applicable launch. The daemon
then atomically creates the managed catalog. Ordinary Codex sessions retain
the built-in `:workspace` default.

## Verification

Check the link and target before launching:

```bash
sudo ls -ld /etc/codex
readlink /etc/codex
ls -ld "$HOME/.tclaude" "$HOME/.tclaude/data" "$HOME/.tclaude/data/codex-sb-cfg"
find "$HOME/.tclaude/data/codex-sb-cfg" -maxdepth 1 -mindepth 1 -ls
```

After an applicable launch, `config.toml` should default to `:workspace` and
define the launch's exact `tclaude-agent-<id>` profile. `requirements.toml`
should allow the three bundled profiles (`:read-only`, `:workspace`, and
`:danger-full-access`) plus every currently retained generated profile.

Use the drive diagnostic on a launched agent:

```bash
tclaude agent codex-app-server status --target <agent> --json
```

The model tool shell should still be unable to list or read
`~/.tclaude/data` or the host tmux socket directory. `tclaude agent whoami`,
`tclaude agent ls`, and inbox commands should continue to work through the
separate `~/.tclaude/api` socket.

## Repair

A launch error and the dashboard warning distinguish a missing symlink, wrong
target, unsafe owner, unsafe mode, and unmanaged/conflicting files. Repair the
named condition; do not delete policy until you have established who owns it.

For a tclaude-managed target, restore directory and file modes with:

```bash
chmod 700 "$HOME/.tclaude/data/codex-sb-cfg"
chmod 600 "$HOME/.tclaude/data/codex-sb-cfg/config.toml" \
  "$HOME/.tclaude/data/codex-sb-cfg/requirements.toml" \
  "$HOME/.tclaude/data/codex-sb-cfg/registry.lock"
```

Only run the file command for files that exist and begin with their
`Managed by tclaude` marker. If an unmarked file exists, tclaude treats it as
administrator configuration and refuses to overwrite it. Move or replace it
only after the responsible administrator has explicitly decided how that
policy should be handled.

After repairing the path, restart agentd. Its durable SQLite registrations are
reconciled back into the catalog before recovered launches proceed.

## Rollback

Stop applicable agents or explicitly move each stopped agent back to send-keys:

```bash
tclaude agent resume <agent> --send-keys
```

Then stop agentd and have an administrator remove only the symlink after
verifying its exact identity:

```bash
test "$(readlink /etc/codex)" = "$HOME/.tclaude/data/codex-sb-cfg"
sudo unlink /etc/codex
```

The user-owned target can be retained as a rollback backup. Remove it only
after confirming it contains solely the three tclaude-managed files. Removing
the symlink restores Codex's behavior when no `/etc/codex` administrator
requirements are installed; it does not restore or merge pre-existing
enterprise policy.
