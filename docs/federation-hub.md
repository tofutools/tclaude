# Running a hub

`tclaude-hub` is a separate binary, included in the release archives, or
built with `go install github.com/tofutools/tclaude/cmd/tclaude-hub@latest`.

The full command tree is also available as **`tclaude hub ...`**: for example,
`tclaude hub serve`, `tclaude hub admin rotate-claim`, and `tclaude hub invite`.
Replace the `tclaude-hub` prefix in any command below with `tclaude hub` to use
the main binary. Both forms share the same flags, database, authorization and
script guardian. The standalone binary remains available for existing services.
When running `tclaude hub serve --supervised`, hub updates replace the serving
`tclaude` executable and keep the `hub` prefix when restarting its worker;
`tclaude-hub serve --supervised` continues to update the standalone executable.
Run it anywhere every instance can reach: a team server, a cloud VM, or one
of your own machines.

```bash
tclaude-hub serve --listen 0.0.0.0:8470 --tls-cert hub.crt --tls-key hub.key
```

Without `--tls-cert` it serves plain HTTP, which clients accept only over
loopback. Otherwise front it with a TLS-terminating proxy or use the flags
above. Its state is a SQLite file under `$TCLAUDE_HUB_DIR` (default
`~/.tclaude/data/hub/` for the user running the hub). This private subtree
is denied to tclaude agent sandboxes. An existing `~/.tclaude-hub/` directory
keeps working, with a hint to move its contents while the hub is stopped; it is
never moved automatically. Explicit `--db` and `TCLAUDE_HUB_DIR` keep their
existing behavior. A dedicated systemd hub user remains supported.
It stores admitted instances, spaces and invites, plus encrypted board content
and board metadata, never messages.

As a systemd service:

```ini
# /etc/systemd/system/tclaude-hub.service
[Unit]
Description=tclaude federation hub
After=network-online.target

[Service]
User=tclaude-hub
Environment=TCLAUDE_HUB_DIR=/var/lib/tclaude-hub
ExecStart=/usr/local/bin/tclaude-hub serve --listen 0.0.0.0:8470 \
  --tls-cert /etc/tclaude-hub/hub.crt --tls-key /etc/tclaude-hub/hub.key
Restart=on-failure
StateDirectory=tclaude-hub

[Install]
WantedBy=multi-user.target
```

Admitting instances:

```bash
tclaude-hub admit inst_… --space team-a     # by instance id
tclaude-hub invite --space team-a --ttl 24h # or a single-use invite token
tclaude-hub invite --space team-a --url wss://hub.example:8470 [--ca-file hub.crt]
                                            # the same invite as a tchj_ join code
tclaude-hub ls                              # instances, spaces, last seen
tclaude-hub spaces inst_… team-a,team-b     # replace an instance's spaces
tclaude-hub revoke inst_…                   # drop it (live within ~15s)
```

Instances see each other only if they share a **space**. Admin commands edit
the database directly. A running hub picks up changes at its next policy
refresh (`--policy-refresh`, default 15s). Per-instance send limits are
`--frames-per-minute` (default 600) and `--bytes-per-minute` (default 100 MiB).
`--open` admits anyone who proves key possession and is for development only.

## Remote hub administration

A node operator can administer its connected hub with `tclaude federation hub`
(or ⚙ Fleet config › Hub). This is separate from peer trust and node grants: these routes
are local human-only, and peers cannot proxy them. The hub must already admit the
claiming instance, either through its normal admission list or a hub invite.

On an unclaimed hub, `tclaude-hub serve` writes a one-use claim token to
`admin-claim.token` next to the hub database with mode `0600`. Startup,
`tclaude-hub admin rotate-claim`, and `tclaude-hub admin reset` print the private file path and a shell-quoted
`sudo cat -- 'PATH'` command to retrieve it on the hub host.

These commands accept `--print-claim-token=auto|always|never` (default `auto`).
Auto prints the token only when **both stdout and stderr are terminals**.
Never prints only private-file guidance. Always explicitly forces the token
into the output, including redirected output: **under systemd this puts the
bearer secret in the journal, readable by local users and their agents**.
Use auto or never for services and captured output.

Redeem it using
`tclaude federation hub claim TOKEN`. It expires after 24 hours. Restarting a hub
with no admins retains a valid claim, or regenerates a lost/expired claim. The
hub stores only the token hash and deletes the token file after redemption.
Treat this token as a credential; do not paste it into shared messages.

The claim grants the redeeming instance's signing key the explicit capabilities
`hub.admins.manage`, `hub.admissions.manage`, `hub.invites.manage`,
`hub.spaces.manage`, `hub.settings.manage`, `hub.identity.manage`,
`hub.boards.manage`, `hub.health.read`, and `hub.logs.read`. Additional admins receive only the named
capabilities you select. Unknown capabilities are refused. `hub.exec` is elevated and never granted by bootstrap or admin status.
A remote admin can delegate it only if it already holds that capability.
Removing the last admitted admin with `hub.admins.manage`, stripping that
capability, revoking its admission, or recovering its lost key is refused. If all
admin keys are lost, the hub host can run the shell-only command
`tclaude hub admin reset --db PATH` (or `tclaude-hub admin reset --db PATH`)
to clear admin authority and create a fresh
claim. There is no remote reset endpoint.

To replace a leaked or unwanted **unredeemed** claim without clearing admins,
run `tclaude hub admin rotate-claim --db PATH` (equivalently,
`tclaude-hub admin rotate-claim --db PATH`) on the hub host. It immediately
invalidates the old claim, writes a fresh private token valid for 24 hours, and
uses the same `--print-claim-token` output rule. Expired unredeemed claims can
also be rotated. If the claim was redeemed, admins already exist, or no claim
was prepared, rotation refuses without changing admin authority. It cannot add
another administrator; use an existing admin to delegate access. There is no
remote rotation endpoint.

Commands (JSON unless noted; `hub status` prints a table without `--json`):

```bash
tclaude federation hub status
tclaude federation hub admins list
tclaude federation hub admins add --instance inst_ID --capabilities hub.health.read,hub.logs.read
tclaude federation hub admins remove --instance inst_ID
tclaude federation hub admissions list
tclaude federation hub admissions admit --instance inst_ID --spaces team
tclaude federation hub admissions revoke --instance inst_ID
tclaude federation hub cleanup-stale --days 30 --preview
tclaude federation hub cleanup-stale --days 30
tclaude federation hub invites create --space team --ttl 1h
tclaude federation hub invites list
tclaude federation hub invites revoke --token-hash HASH
tclaude federation hub spaces list
tclaude federation hub spaces set --instance inst_ID --spaces team,other
tclaude federation hub settings get
tclaude federation hub settings set --key max_connections --value 1024
tclaude federation hub settings reset --key max_connections
tclaude federation hub identity recover --old inst_OLD --new inst_NEW
tclaude federation hub identity recover --old inst_OLD --new inst_NEW --fingerprint FP --apply
tclaude federation hub identity revoke-old --instance inst_OLD
tclaude federation hub identity revoke-old --instance inst_OLD --fingerprint FP --apply
tclaude federation hub health
tclaude federation hub logs tail --max-entries 100
```

**Stale admission cleanup** defaults to 30 days. The dashboard's Hub admissions
section previews names, IDs and last-seen times before confirmation. The CLI
prints the same preview and asks you to type `yes`; `--preview` stops without
asking. Cleanup revokes admission using the individual revoke path; it does not
delete node data or change node trust. Admins, the requesting node and connected
instances are excluded. To remove a protected instance, explicitly use the
individual revoke action (the last-admin safeguard still applies). Instances
never seen use their admission date. Each batch is limited to 250; repeat for
remaining candidates. Confirmation applies only to the previewed IDs and cutoff,
and skips instances seen again or made admins in the meantime.


Built-in hub rate limits are 600 frames/minute, 100 MiB/minute of sent data,
and 10 MiB/second of relayed stream bandwidth per instance. Existing explicit
DB settings are preserved when upgrading.

Settings precedence is **built-in defaults < serve flags < persisted hub DB
settings**, independently per field. Startup logs report each DB override with
both its boot and effective value. Status lists explicitly overridden flags;
setting descriptors include `effective`, `boot`, `source` (`default`, `flag`, or
`db`), `flag_overridden`, bounds/unit, and `restart_required:false`. Resetting a
setting removes its DB override and restores the boot flag/default. Changes apply
live; reducing a capacity prevents new connections/streams without killing
unrelated existing streams. Rotation-window changes affect new statements and
never change an already promised activation time. Host security settings such as
listen address, TLS, open admission, and script acceptance are not writable.

The local `/api/federation/hub/` dashboard routes and matching
`/v1/federation/hub/` CLI routes are:

| Tail | Methods | Body / result |
| --- | --- | --- |
| `status` | GET | Connected hub, admin capabilities, overridden flags |
| `claim` | POST | `{token}` → `{claimed:true}` |
| `admins` | GET, POST | `{admins:[...]}`; POST `{instance,capabilities}` |
| `admins/{instance}` | DELETE | Remove an admin |
| `admissions` | GET, POST | `{admissions:[...]}`; POST `{instance,spaces}` |
| `admissions/{instance}` | DELETE | Revoke admission |
| `admissions/cleanup` | POST | Preview `{days:30}`; confirm with `{days,cutoff,instances:[IDs],apply:true}` |
| `invites` | GET, POST | `{invites:[...]}`; POST `{space,ttl_seconds}` returns bearer once, with `join_code` when this node has a hub address |
| `invites/{token_hash}` | DELETE | Revoke invitation |
| `spaces` | GET, PUT | `{spaces:[...]}`; PUT `{instance,spaces}` |
| `settings` | GET, PATCH | `{settings:{key:descriptor}}`; PATCH `{overrides:{key:integer_or_null}}` |
| `identity/recover` | POST | `{old,new,apply?,fingerprint?}` |
| `identity/revoke-old` | POST | `{instance,apply?,fingerprint?}` |
| `health` | GET | Counts, portable runtime load, uptime, recent errors |
| `logs` | GET | `{entries:[{at,level,message}],next_cursor}` |

Admission/invite/space lists accept opaque `cursor` and `max_entries` (default
100, maximum 250), returning `next_cursor` when more rows remain. Logs use an
opaque cursor with a maximum of 200 entries per response, from a bounded,
redacted in-memory event ring. They expose no arbitrary files or node content.
Errors use `{code,error}`. Identity operations preview by default; apply requires
the exact displayed fingerprint. Key-loss recovery inherits the predecessor's
spaces, revokes its admission and admin authority, and does **not** automatically
grant the replacement admin capabilities. Accepted dual-signed key rotation
preserves the predecessor's exact capability set.

RPC requests use the existing authenticated hub connection. Each signature binds
the hub identity, connection challenge, persistent admin generation, request ID,
method, payload digest, and a validity period of at most one minute. The hub
persists a replay marker before executing/responding and records a bounded audit
without payloads or tokens. Mutations are never automatically retried: if the
connection drops after submission, inspect current state before explicitly
submitting another change.

## Scripts on the hub

Hub scripts need **both** `hub.exec` on the calling instance and the hub-local
`accept_remote_scripts` switch. The switch defaults off and cannot be changed
through remote settings or any dashboard/CLI RPC. The host enables it with
`tclaude-hub serve --accept-remote-scripts`, or a private regular `hub-config.json`
next to the hub database (mode `0600`) containing:

```json
{"accept_remote_scripts": true}
```

An explicitly supplied flag wins over the local file, including
`--accept-remote-scripts=false`. Without the flag, the file is re-read while jobs
run; disabling it cancels their process groups. Malformed or unsafe config fails
closed. Scripts are refused when the hub service runs as root.

**Threat:** a script has the non-root hub service user's full host authority and
can read that user's secrets, alter the hub database, or disrupt connectivity for
the whole fleet. Pinned instance keys still prevent the hub from reading or
forging end-to-end node content. Enable this switch only for administrators you
trust with the hub host itself.

Seed the first elevated grant from the hub host's shell:

```bash
tclaude-hub admin grant-exec inst_ID --db PATH
tclaude federation hub run                         # inspect gates and limits
tclaude federation hub run --file script.sh --timeout 1m
tclaude federation hub run --file script.sh --no-wait
tclaude federation hub run --job JOB_ID
tclaude federation hub run --job JOB_ID --log stdout --offset 0
tclaude federation hub audit list --max-entries 100
```

Jobs are durable, never replayed after a restart, and limited to four concurrent
runs, a 16 KiB script, 1s–24h runtime (default 1h), and 4 MiB per output stream.
A private child guardian owns the timeout and watches a hub-lifetime pipe, so
a hub crash also kills the script process group. Terminal outcome audits are
reconciled idempotently on restart. The waiting CLI shows incremental output,
exit status, and duration. Revoking
`hub.exec` or the caller's admission cancels an active run within the authority
watch interval. A restart records an interrupted job and its outcome.

The local human-only dashboard and CLI APIs share these tails beneath
`/api/federation/hub/` and `/v1/federation/hub/`:

| Tail | Method | Result |
| --- | --- | --- |
| `run` | GET | `{accept_remote_scripts,switch_source,can_exec,service_user,limits,warning}` |
| `run` | POST | `{script,timeout_seconds?}` → durable job |
| `run/jobs/{job_id}` | GET | Job metadata; `stdout_tail`, `stderr_tail`, `duration_ms` only with `hub.exec` |
| `run/jobs/{job_id}/logs?stream=stdout&offset=0` | GET | `{data,next_offset,eof}`; `data` is base64, at most 64 KiB decoded; needs `hub.exec` |
| `audit?cursor=…&max_entries=100` | GET | `{entries,next_cursor}`; at most 200 entries and 256 KiB; needs `hub.logs.read` |

An empty log chunk while running has `eof:false`; advance by `next_offset` and
poll again. Job states are `running`, `completed`, `failed`, `timed_out`,
`cancelled`, or `lost` (restart). The hub audits the full script before execution
and the outcome afterward. Audit readers without `hub.exec` see the actor,
script hash/length and result, with `redacted:true` and no script text. The
ordinary hub log ring never includes scripts or their output. These APIs are
local-only; peer-view routes cannot reach them, regardless of node trust.

## Supervised hub updates

Hub self-update requires the explicit `hub.update` capability. Bootstrap admins
can grant it through the admin editor; it does not grant `hub.exec`. The dashboard
and `tclaude federation hub update` use the same signed hub-admin connection.
Use `update check`, `update apply [--version vX.Y.Z]`, or `update rollback`.
`--no-wait` returns the durable job; `--job ID` reads it later. Explicit and
resolved release downgrades are refused. Rollback restores only the verified
previous binary in the journal, never an arbitrary release or path.

A host operator must first enable guardian mode in the service definition.
Plain `serve` allows status/check but refuses apply/rollback with
`not_supervised`. For systemd, run the hub directly as the service MainPID:

```ini
[Service]
User=tclaude-hub
ExecStart=/usr/local/bin/tclaude-hub serve --supervised --db /var/lib/tclaude-hub/hub.sqlite
Restart=on-failure
```

The service user needs write access to the installed hub binary's directory and
its private database/update directory. Do not wrap ExecStart in a shell. The
hub verifies its own MainPID and an `always` or `on-failure` restart policy;
unverifiable supervision refuses guardian startup. A user systemd unit works
with the equivalent permissions and restart policy.

On macOS, the LaunchAgent/LaunchDaemon must directly run
`tclaude-hub serve --supervised --supervisor-label YOUR.LABEL`, with `Label`
matching that argument and unconditional boolean `KeepAlive=true`. Conditional
KeepAlive policies are refused. The hub verifies the loaded job's PID and its
host-owned plist. Restart/reload the unit once after enabling guardian mode.

The guardian remains the supervisor's main process and starts a serving child.
Only verified official release artifacts with matching version and federation
protocol are staged; source/dev builds with an unknown release version cannot
apply an update. Readiness uses a private per-child channel created with 0700
and 0600 permissions, followed by a probe of that child's actual bound listener
and expected hub ID/version. The health deadline is 60 seconds. If the candidate
exits, reports wrong health, or misses the deadline, the guardian verifies the
backup and installed hashes, restores the prior binary, restarts it, and records
`rolled_back`. A failed manual rollback likewise restores the pre-rollback
working release. The journal survives guardian restarts; a serving worker never
infers successful health from an executable hash alone. Federation connections
reconnect during each serving-child restart. Update starts, progress, and final
outcomes are recorded in the hub audit.

The local, human-only routes are `GET/POST /api/federation/hub/update` and
`GET /api/federation/hub/update/jobs/{job_id}`, with identical `/v1` tails.
Status adds `supervisor`, a `blocked` reason when unsupervised, and a durable
`job` containing the phase/deadline/outcome. Poll the selected job while it is
`running` or `restarting`; temporary read failures during restart do not imply
failure. Never retry an apply submission automatically.
