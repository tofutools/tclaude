# Run the fleet

Keep linked nodes healthy: update them, manage harnesses and logins, run scripts, and recover identities.

## Updating local and linked nodes

`tclaude update` checks the latest stable release. Use `--apply` to install it,
`--version vX.Y.Z` to pin a release, or `--rollback` to restore the preceding
binaries. `--node <label-or-instance-id>` performs the same operation on a
trusted node; that node downloads its own update. Installed `tclaude`,
`tclaude-agentd`, and `tclaude-hub` binaries are included. Agentd gracefully
restarts and reconnects; existing tmux sessions continue running. A running
standalone hub needs its own service restart after its binary is replaced.

Official release binaries report their real release version and the `release`
install marker. Release archives must match the official SHA-256 checksums.
Go-installed binaries use `go install` at the selected version. Source/dev or
unmarked builds report their method explicitly and warn that updating replaces
local modifications; their default is `go install @latest`. A staged binary
must report the expected release version and the same federation protocol before
any installed binary is replaced. Unknown current versions have a nullable
update-available hint rather than a guessed ordering. A new update creates a
verified backup; rollback refuses to overwrite files changed outside the updater.

`node.update` is an **instance-wide**, default-off peer permission. Neither
`node.read` nor `node.harnesses.read` implies it; only unrestricted trust implies
it automatically. It allows reading updater status/jobs as well as checking,
applying and rolling back updates. Permission is checked again before each
replacement. Update requests and results appear in the federation audit.

The local dashboard API is `GET /api/node/update` (status), `POST
/api/node/update` with `{ "action": "check" | "apply" | "rollback", "version":
"vX.Y.Z" }` (optional version, never with rollback), and `GET
/api/node/update/jobs/{id}`. Starting a job returns HTTP 202 and its durable ID;
concurrent jobs return HTTP 409 `update_busy`. The corresponding operator-only
CLI API uses `/v1/node/update`; remote dashboard calls use
`/api/peer/{instance_id}/node/update` and its `/jobs/{id}` suffix.

Poll a started job about once per second. Its states are `running`, `restarting`,
`succeeded` and `failed`; `phase`, `warnings`, and optional `error` explain its
progress. During `restarting`, tolerate the normal daemon/peer disconnect and
resume polling the same ID after reconnecting. `tclaude update --no-wait` returns
that ID immediately; `--job <id>` reads it later, including after a restart.
Node summaries expose cached `version`, optional `latest_version`, nullable
`update_available`, and optional `update_checked_at` without release requests
in the polling path. While federation runs, release metadata is refreshed in
the background hourly: the federation runtime starts the check when it starts
(at daemon start or on a later `tclaude federation connect`) and stops it when
federation is disabled or disconnected. After a failed check, restarts of the
runtime wait 10 minutes before checking again. A standalone daemon never starts
these Fleet release checks; explicit local update and harness-install requests
still fetch metadata on demand.

In the dashboard, each fleet map card shows the node's tclaude version and ↑
when a newer release is known; its update…/manage… link opens that node's
update dialog (the Fleet page's identity bar opens it for this node). The
dialog shows the version, install method, latest release, binaries and
warnings, and offers Check now, Update to the latest and Roll back; update and
rollback confirm that the binaries are replaced and the daemon restarts. It
follows the job through the status read, so the restart does not lose it. A
peer that has not granted you `node.update` says so.

## Custom update scripts

Every machine is kept up to date its own way — OS packages, dotfiles, a
language toolchain, a harness installed through a package manager. A node's
operator describes those in `~/.tclaude/data/config.json`, and the fleet shows and
runs them:

```json
{
  "updates": {
    "scripts": [
      {
        "name": "apt",
        "label": "System packages",
        "status": "~/bin/apt-status.sh",
        "update": "sudo -n apt-get upgrade -y",
        "update_timeout_seconds": 1800
      },
      { "name": "dotfiles", "status": "git -C ~/dotfiles fetch -q && git -C ~/dotfiles status -sb | head -1", "update": "git -C ~/dotfiles pull --ff-only" }
    ],
    "harnesses": {
      "codex": { "update": "brew upgrade codex" }
    }
  }
}
```

Each script has a `name` (lowercase letters, digits, `-`, `_`), an optional
`label`, and a `status` command, an `update` command or both. Commands run with
`/bin/sh -c` as the agentd user from its home directory, with
`TCLAUDE_UPDATE_SCRIPT` and `TCLAUDE_UPDATE_ACTION` (`status` or `update`) set.
Status checks time out after `status_timeout_seconds` (default 60) and updates
after `update_timeout_seconds` (default 1800).

A status command reports what is pending in one of two forms:

- **JSON** on stdout: `{"summary": "3 pending", "update_available": true,
  "details": "…", "items": [{"name": "openssl", "current": "3.0.1", "latest":
  "3.0.2", "update_available": true, "detail": "security"}]}`. Every field is
  optional; without `update_available`, the items' own flags decide.
- **Plain text**: the first line is the summary and the rest the details. Exit
  0 means up to date and exit 100 means updates are pending (as with `dnf
  check-update`).

Any other exit code is a failed check. Output is bounded (64 KiB per stream,
200 items, 200 characters per field). Status scripts run when first read, on
Check, after their update finishes, and hourly while federation runs. One
update runs at a time per node; a finished update keeps its exit code and the
last 16 KiB of its output, in memory for the daemon's lifetime.

`updates.harnesses.<name>.update` replaces that harness's default update
command (its built-in updater) for ⚙ Fleet config › Harnesses and `tclaude harness
update`; the job reports the recipe as `custom`. An invalid `updates` block
is ignored, so harnesses fall back to their default update commands.

The commands come only from this node's config: requests select a script by
name and never carry a command, and `updates` never travels in config bundles.
The scripts sit under the `node.update` grant: a peer allowed to update this
node may read their status, the command text, their output (the status details
and the last update output) and run them; a peer holding
`node.harnesses.install` likewise sees a harness override's command. Keep
secrets out of the commands and their output — read tokens from files or the
environment instead. Update runs are audited
as `federation.node.update`.

In the dashboard, ⚙ Fleet config › Harnesses lists each node's scripts under **Custom
updates** with the summary, a ↑ when updates are pending, the reported items,
and Check / Update… buttons. The API is `GET /api/node/update-scripts`
(scripts with their last status, and recent jobs), `POST
/api/node/update-scripts` with `{"action": "check", "name"?: "…"}` or
`{"action": "update", "name": "…"}` (HTTP 202; a second concurrent update is
HTTP 409 `update_busy`), and `GET /api/node/update-scripts/jobs/{id}`. Peers
reach them through `/api/peer/{instance_id}/node/update-scripts`.

## Installing and updating harnesses

`tclaude harness install <name> [--node <peer>]` runs a fixed official npm
package recipe as the daemon user, with no sudo. New installs live under
`~/.local/share/tclaude/harnesses/npm`; the daemon includes its `bin` directory
in subsequent probes and launches. The supported names are `claude`, `codex`,
`opencode`, `copilot`, `gemini`, and `pi`, using their vendors' latest stable npm
packages. The recipes link to the official vendor instructions.

`tclaude harness update <name|--all> [--node <peer>|--all-nodes]` runs the
harness's own updater, which knows how it was installed (native installer, npm,
Homebrew, …): `claude update`, `codex update`, `opencode upgrade`, `copilot
update` and `pi update --self`. Gemini CLI has no updater of its own; it is
updated through npm in its existing prefix, and other Gemini installation
methods, missing npm and unwritable prefixes require a manual command. Root
execution of these default recipes requires a manual command; the daemon
never elevates privileges. A node's operator can replace any harness's update command with
their own in `updates.harnesses` (below). `--all-nodes` includes this node and trusted
linked nodes, and reports failures per node. Cached npm metadata adds optional
`latest_version` and nullable `update_available` fields to harness availability.

Active sessions require an explicit choice: `--now` acknowledges the warning,
or `--when-idle` waits until the selected harness has no busy sessions. With
neither option, a busy harness returns HTTP 409 `harness_workers_busy`. A node
runs one harness job at a time. `--no-wait` returns the job ID; `--job <id>` reads
it later. Jobs contain stored phase/result logs, never vendor stdout, environment
values or credential contents. An interrupted job is marked failed after daemon
restart; inspect availability before retrying it.

Remote installs and updates require the instance-wide `node.harnesses.install`
permission, default off and implied only by unrestricted trust. This grant does
not follow from `node.harnesses.read`, and it does not authorize credential drops.
Both grants can be assigned through node profiles/pools. Operations are audited.

For a **single remote install**, `--copy-credentials` explicitly pushes the
operator's own file credentials to the installing node. Its agents can then act
as the operator with that provider. Copy is never enabled by default, never pulls
from a peer, and additionally requires `node.credentials.receive` on the receiver.
That receiving grant is also instance-wide and default off except unrestricted
trust. Existing credentials require `--overwrite-credentials`; every write first
creates a private, owner-only backup and reports its ID/location. Symlink targets
are refused and new auth files have mode 0600.

File copy supports Claude's `.credentials.json`, Codex's `auth.json`, OpenCode's
`auth.json`, Gemini's OAuth/account files, and pi's `auth.json` in their standard directories.
Configured Codex/Gemini/XDG directories on each node are respected. Claude capture
honors the sender configuration; receiving uses tclaude's pinned `~/.claude` state root. Keychain-only,
Copilot, and environment-only credentials need the target's own login flow;
secret stores and ambient environment variables are never exported. Bundles are
bounded to 18 KiB. The local daemon captures files only after explicit operator
opt-in and sends them over the existing end-to-end encrypted peer transport.
The browser, CLI output, job records, and audit never receive credential contents.
Availability checks report file presence without reading those contents;
provider usability remains unknown until the harness authenticates.

The dashboard API uses `GET /api/harnesses/operations` for recipes/options,
`POST /api/harnesses/operations` with `action`, either `harness` or update `all`,
optional `mode` (`now` or `when_idle`), and opt-in `copy_credentials` /
`overwrite_credentials`. POST returns HTTP 202 with a job; poll `GET
/api/harnesses/operations/jobs/{id}` about once per second until `succeeded` or
`failed` (intermediate states: `running`, `waiting_idle`). A job includes
`results`, safe `log`, `warnings`, and final `availability`. Concurrent requests
return HTTP 409 `harness_operation_busy`. Operator CLI routes mirror these under
`/v1`; remote dashboard calls use `/api/peer/{instance_id}/harnesses/operations`.

Credential copy during install is a third way to prepare a teleport target,
alongside the target's own login and a model gateway. It does not change teleport's
credential choice or defaults.

## Standalone credential push and restore

An operator can refresh an existing node's file-based harness login without
installing a harness:

```sh
tclaude harness credentials push codex --node laptop --confirm-share
tclaude harness credentials push claude,gemini --node laptop --confirm-share
tclaude harness credentials ls codex --node laptop
tclaude harness credentials restore codex --node laptop --backup BACKUP_ID
tclaude harness credentials backup codex
tclaude harness credentials restore codex
```

`--confirm-share` explicitly acknowledges that the remote node's agents will act
as you with those providers. The authenticated local daemon captures only your
selected standard auth files and sends them over the encrypted federation
channel. Clients cannot supply credential contents or arbitrary paths. Locally configured
HOME and harness-directory symlinks are resolved once before opening a pinned
canonical directory; credential-file symlinks are refused. There is
no API for pulling a peer's credentials. Copilot, keychain logins, and environment
secrets require the target harness's own login flow.

Standalone operations require only `node.credentials.receive` on the receiving
node, independently of `node.harnesses.install`. This node-wide grant defaults
off and is implied only by unrestricted trust. Each push writes a timestamped,
owner-only backup before replacement; a failed backup refuses the replacement.
Restore also backs up the current files first. Omitting `--backup` selects the
newest matching backup, including backups created by install-copy. Restore
removes a copied file if that file was absent in the selected backup. Backup and
restore work locally when `--node` is omitted; push requires a remote node.
Chosen-set pushes run separately per harness and report individual failures.

The dashboard API uses `GET /api/harnesses/credentials/backups?harness=codex`,
`POST /api/harnesses/credentials/push`, `POST /api/harnesses/credentials/backup`,
and `POST /api/harnesses/credentials/restore`. Use
`/api/peer/{instance_id}/harnesses/credentials/{action}` for a remote node.
Local CLI routes have the same tails under `/v1`; remote CLI routes use
`/v1/federation/peer/{node}/harnesses/credentials/{action}`.

POST bodies contain `harness`; push also requires `confirm_share: true`, and
restore optionally accepts `backup` (the opaque 32-character backup ID).
Success returns `receipt` with `backup_id`, `backup_location`, and `copied`;
restore adds `restored_from` and `restored`. It also returns fresh `availability`.
Backup listings return `backups` containing `id`, `harness`, `created_at`, and
`location`, plus `credential_share_warning`. Failed file operations return a
409 `credential_operation_failed` and any safety-backup receipt. Credential
contents never appear in responses, logs, or audits. Audits record the harness,
operator/peer, backup ID, action, and result. File presence can update after a
push; usability remains unknown until the harness actually authenticates.

## Harness availability

Inspect the daemon's registered harnesses, including missing binaries and the
shell harness, with:

```bash
tclaude harness ls
tclaude harness ls --refresh --json
tclaude harness ls --node bob --json
tclaude federation grant bob node.harnesses.read
```

The local dashboard reads `GET /api/harnesses/availability`; the operator CLI
reads `GET /v1/harnesses/availability`. Remote reads use the same peer-view
proxy with tail `harnesses/availability`. These are operator reads locally;
remote peers need `node.harnesses.read` or unrestricted trust. Existing
`node.read` catalog harness versions remain a coarse capability summary and
do not reveal binary paths or credential presence.

Responses contain `schema: 1`, `observed_at`, `refresh_after`, and `harnesses`.
Each harness row includes `name`, `display_name`, `binary`, `installed`,
optional `path` and `version`, `version_status`, nullable `credential_present`,
and nullable `usable`. Version status is `known`, `unknown`, `not_installed`,
`not_spawnable`, or `path_unavailable`. Remote responses also include the
normal `peer_view` metadata with concept `node.harnesses`.

Paths resolve against agentd's PATH. Version subprocesses use fixed
`--version` arguments, bounded output, and deadlines. Credential presence is
only a boolean check for descriptor-declared ambient credential variables;
credential values are never returned. Presence does not prove that a key is
valid. Missing ambient credentials mean unknown, since native login and
provider configuration may still work. The probe does not read credential
files, run login commands, or contact authentication services. Missing
binaries are unusable; installed credential-free shell is usable; other
installed harnesses report unknown usability.

Results cache for five minutes. `--refresh` or `?refresh=1` requests a fresh
probe, with a ten-second minimum between probes and a single shared probe at
a time. The first uncached read can take up to eight seconds. Do not put this
probe on the fast snapshot or map-summary poll; request it when opening a
node's harness details or explicitly refreshing them.

## Operator scripts on nodes

`node.exec` grants full remote code execution as the agentd user. It is an
unscoped node permission, default off and implied by unrestricted trust.
Remote execution additionally requires the receiving node's local
`accept_remote_scripts` setting, **including for unrestricted peers**. Enable
that switch locally; peers cannot change it. Local operator runs require the
usual dashboard/CLI authority and neither remote lock.

```sh
tclaude federation scripts --accept-remote on
tclaude federation scripts --memory 1GiB --pids 256
tclaude federation run --node laptop --node desktop --file maintenance.sh --timeout 2m
tclaude federation run --all -- echo 'hello from the fleet'
tclaude federation run -- printf '%s\n' 'local only'
tclaude federation run --node laptop --job JOB_ID
tclaude federation run --node laptop --job JOB_ID --log stdout
```

Scripts are bounded to 16 KiB and their encoded request to 32 KiB. They run as
private script files passed as argv to `/bin/sh` in a detached one-shot tmux
session on Linux (a managed one-shot subprocess on macOS), with the existing
process-group cleanup, timeout (exit 124), output
limit (4 MiB per stream, exit 125 on overflow), and Linux cgroup resource-limit
runner. This is code execution, not a filesystem/network sandbox. Root daemon
execution is refused. Default Linux limits are 1 GiB memory and 256 processes;
configured limits require working cgroup delegation and fail closed if it is
unavailable. macOS uses process-group and deadline cleanup, with no cgroups.
The local operator can change receiver-owned limits in settings. Jobs execute
in the daemon user's canonical HOME, never a peer-selected working directory.

`--all` selects this node and trusted linked nodes, reporting offline nodes as
skipped; explicit nodes that are offline are also skipped. No offline queue is
created. `--no-wait` returns durable per-node job IDs; later `--job` reads one.
The CLI collects result summaries and bounded output tails. Failed, canceled,
interrupted, or skipped outcomes return a nonzero command status. Re-run only
failed nodes by selecting their `--node` values again.

Dashboard/API routes (same tails under `/v1` for local CLI) are:

- `GET /api/node/run`: settings/availability including the receiving switch.
- `POST /api/node/run`: `{script, timeout_seconds?}`, returns 202 job metadata.
- `GET /api/node/run/jobs/{id}`: durable result with state, exit code,
  duration_ms, stdout_tail and stderr_tail (8 KiB each), hash and size.
- `GET /api/node/run/jobs/{id}/logs?stream=stdout&offset=0`: bounded 64 KiB
  chunks, `{data, next_offset, eof}` (`data` is base64); stderr is analogous.
- Local-only `GET/PUT /api/node/run/settings`: `{accept_remote_scripts?,
  resource_limits?: {memory?, cpu?, pids?}}`, with the full-code-execution warning.

Remote data routes use `/api/peer/{instance_id}/node/run...`, or
`/v1/federation/peer/{node}/node/run...` for CLI. The UI can fan out the same
per-node requests for checked nodes and poll their jobs independently. Active
jobs are canceled when either receiving lock is withdrawn. Peer jobs/logs are
readable only by their initiating peer and the receiving local operator.
Scripts and bounded logs remain private on the receiver; audits on each side
record operator, peer/node, script hash and size, job ID and result. The sender
records results when they are observed through job polling. After daemon
restart, unfinished jobs are marked interrupted and never replayed.

## Identity rotation and key-loss recovery

Instance IDs derive from public keys. Rotation therefore creates a linked
successor ID, rather than changing the key behind an existing ID:

```bash
tclaude federation identity rotate             # preview consequences
tclaude federation identity rotate --apply     # stage and publish successor
tclaude federation identity rotations          # local and peer transition state
```

The public transition contains both keys and IDs and signatures from both keys.
Peers verify it from their own pinned predecessor. The hub independently verifies
it before transferring admission and spaces. Each authority starts a detection
window at first observation (default ten minutes); replay does not shorten it.
Set `federation.identity_rotation_seconds` in local configuration or hub
`serve --identity-rotation-window 10m` to change the window. A competing signed
successor freezes automatic acceptance until explicit recovery. The original
key remains current during the window. An operator notification shows both
fingerprints and the statement issue date when a change is observed and accepted.

The local daemon stages the replacement key privately and journals activation.
Activation closes federation streams and re-establishes handshakes. Pending
sealed mail is refused with an instruction to resend; ciphertext and history
are never rewritten. Existing model credentials and requester-paid leases are
revoked immediately, with an audit reason. Live execution reservations remain
charged until their existing lifecycle proves the worker ended. Paused teleport
backup leases and their linked routing records follow the successor without
changing epochs, sequence numbers, deadlines or historical origin provenance.
Normal teleport `--home` resolves accepted successors for routing.

Four public transition hops are retained. A fifth rotation refuses with an
instruction to re-pair offline peers. Offline peers can verify a retained chain
from their pinned key and start their own detection window when they return.
Older peers and hubs lacking rotation support need explicit re-pairing.

A missing previously recorded local key refuses ordinary federation startup;
it does not silently create another identity. To create an unlinked replacement
explicitly after key loss or suspected compromise:

```bash
tclaude federation identity recover-local              # preview
tclaude federation identity recover-local --apply      # new ID and fingerprint
```

Recovery journals its replacement before retiring capabilities or changing the key.
Startup resumes an interrupted recovery with that same replacement. Explicit
recovery also abandons a pending signed rotation and archives its public journal,
so lost staged keys do not block recovery.

Local peer grants and historical records remain. The replacement needs explicit
hub admission recovery and explicit confirmation at every peer that trusted the
old identity. Verify the replacement fingerprint out of band, then preview and
apply on each trust authority:

```bash
tclaude-hub identity recover OLD_ID NEW_ID --db /path/hub.sqlite
tclaude-hub identity recover OLD_ID NEW_ID --db /path/hub.sqlite \
  --fingerprint NEW_FINGERPRINT --apply

tclaude federation identity recover-peer OLD_ID NEW_ID
tclaude federation identity recover-peer OLD_ID NEW_ID \
  --fingerprint NEW_FINGERPRINT --apply
```

Peer recovery displays the old label and trust level, direct grants, pool
memberships, profile assignment and rebind rules. Apply preserves these and
updates exact peer scopes; it refuses merging two already trusted identities.
Unrestricted authority stays unrestricted, as explicitly displayed in the
preview. No default node profile or config offer is applied. Historical activity
and consumed enrollment records stay under the old identity; enrollment tokens
cannot resurrect the predecessor. Hub recovery replaces the replacement's
spaces with the predecessor's spaces and revokes the old admission.

If a transition is unexpected, revoke its predecessor on the relevant peer and
hub (both commands preview unless `--apply` is present):

```bash
tclaude federation identity revoke-old OLD_ID --apply
tclaude-hub identity revoke-old OLD_ID --db /path/hub.sqlite --apply
```

Revocation blocks pending rotation and old-key replay. An already accepted
successor remains current; revocation does not roll it back. If the old signing
key was stolen, the attacker can also sign a valid rotation. The detection window
helps expose competing successors; it cannot prove which signer is the owner.
An undetected winning successor inherits authority. For immediate containment,
revoke that accepted successor itself on every trusting peer and on the hub:

```bash
tclaude federation identity revoke-old SUCCESSOR_ID --apply
tclaude-hub identity revoke-old SUCCESSOR_ID --db /path/hub.sqlite --apply
```

Despite the command name, `revoke-old` can revoke the currently trusted successor.
It removes that identity's trust and closes its remaining capabilities. Pair a
separately verified replacement and explicitly restore the intended authority;
revocation does not preserve an authority assignment for later automatic recovery.
Identity management commands are operator-only.
