# Content boards

Share skills and configuration with people you don't grant peer access to.

## Content board membership

Content boards use a separate hub connection at `/v1/boards/connect`. Joining
with a board invitation binds membership to the joining instance's key; it does
not admit that instance to the fleet, assign peer-visible spaces, pair it with
other members, or grant access to any member's machine. Board connections accept
only signed board RPCs. Hub administration, envelopes, directories and relay
streams remain unavailable, even when the hub uses open fleet admission.

Owners manage membership and invitations; publishers may publish and readers
may browse and pull. Removing or demoting the last owner is refused. Invitations
are single-use, expire after at most seven days, and may grant only reader or
publisher membership. Keep the invitation private: its token includes the
secret needed to unwrap the board key. Neither bearer tokens nor plaintext
board keys are stored by the hub. A consumed invitation can recover an
interrupted join only for the same instance while its membership still exists.

The hub stores encrypted key envelopes and ciphertext. A key rotation wraps a
new epoch key separately to each current member's instance key, after verifying
that member's proof of the current key, and invalidates
unused invitations from the previous epoch. Removed members immediately lose
hub access; rotation protects future publications. Previously obtained keys or
plaintext cannot be revoked. Remaining members keep access to retained older
key epochs. Lost owner keys require another owner or a new board; hub admins
cannot recover content keys. A dishonest hub operator could deny service or
keep a removed member in the loop for new content; it cannot read content or
forge publisher-signed items, so pick a hub you trust.

Use `tclaude federation boards ls` (with `list` retained as an alias), `create --name NAME`, `join --token TOKEN`,
`show --board ID`, `members --board ID`, `invite --board ID --role reader --ttl 1h`,
`revoke-invite --board ID --token-id HASH`, `set-member --board ID --instance ID
--role publisher`, `remove-member --board ID --instance ID`, `rotate-key --board
ID`, and `leave --board ID`. Configure the invitation's hub URL first; joining
never enables a peer connection. Dashboard and CLI routes under
`/api/federation/boards` and `/v1/federation/boards` require local human authority
and are never forwarded through peer view.

Hub admins with `hub.boards.manage` can list, freeze, limit or delete boards using
`tclaude federation hub boards list|set|delete --board ID`. This capability does
not reveal keys or plaintext. Default limits are 100 boards per hub, 100 members
per board, 100 outstanding invitations, 256 MiB of ciphertext plus encrypted
key/invitation metadata, and 1,000 blobs per board. Ciphertext objects are
bounded at 16 MiB plus encryption overhead. Pending uploads consume quota;
failed uploads release their reservation. Ready, unpinned blobs expire after
30 days, and abandoned upload reservations after one hour; startup cleanup
reaps expired objects and old orphan files. A quota reduction never silently
ejects members or removes pinned objects. Publication/import surfaces follow
in the item slice: membership alone never installs content or runs commands.

Board items are immutable, publisher-signed configuration versions. The hub
stores only encrypted payloads and encrypted descriptive metadata; re-publishing
an unchanged version preserves its original publisher signature. Phase 1 shares
portable configuration sections such as roles, profiles and process templates,
not broad local config or default permissions. Credential scanning runs before
publication and again after decryption on the receiver. Suspected credentials
must be excluded before publication.

Skills are a separate signed item kind. Publish one directory with
`tclaude federation boards publish --board ID --name NAME --only skills/NAME`.
The source must contain `SKILL.md` under a known user skill root:
`~/.claude/skills`, `~/.agents/skills`, or `$CODEX_HOME/skills` (default
`~/.codex/skills`). If several roots contain that name, select
`--skill-root claude|agents|codex`. Symlinks and special files are refused;
there are at most 512 files and the encoded bundle must fit the existing
16 MiB Board item limit. Credential scanning covers every file.

Skill preview/import use `--skill-target claude,agents,codex` (select the roots
you want) instead of config selectors. Preview lists files and script text,
flagging shebangs, script extensions and executable bits as security-relevant.
Executable bits are stripped unless `--preserve-executable` is explicitly set.
Existing skills require `--replace`. Import requires the preview token with
exactly the same roots and confirmations; target changes invalidate it.
Group targets use `--skill-target group:NAME` and install project skills into
the group's current default directory at `.claude/skills/NAME`. The preview
shows the resolved destination. A custom skill root uses `--skill-target custom
--custom-dir /absolute/existing/directory`; it must exist, with no `..` or
symlink in any path component. Custom targets are human-only. Target paths and
root identities are bound to the single-use preview token; a changed group
default directory or custom root requires another preview.

After fetching, GET `/api/federation/boards/BOARD/items/ITEM/versions/VERSION/archive`
(or `/v1/federation/boards/.../archive`) streams a verified tar.gz download.
Skills contain `NAME/SKILL.md` and supporting files; config items contain
`bundle.json`. Executable bits are stripped unless `?preserve_executable=1`
is explicitly requested. The existing `/download` still returns the JSON
transfer bundle.

Catalog rows expose `updated_at` (UTC) and `seq` as hub-reported sorting data,
outside signed receipts. An optional description from skill frontmatter is
independently publisher-signed inside the existing encrypted metadata, bound
to the item, version and payload hash. Older daemons ignore this extension and
continue verifying the original manifest and receipt unchanged. Missing or
invalid descriptions are omitted without hiding valid content. New re-posts
preserve the description; older re-posters may omit it. Older hubs omit the
sorting fields, so newer clients display them as unknown.

Installing a skill does not execute any of its files. Agents may later follow
its instructions or run scripts, including through an interpreter without an
executable bit, so inspect them before confirming.

Agent snapshots are a third item kind, `agent-snapshot`: a snapshot PAF
encrypted to named members only, with a hard expiry (7 days by default, at
most 30) and up to four blobs. Use `tclaude agent snapshot share` and
`tclaude agent snapshot board ls|fetch|preview|import|rm`; see
[PAF: Sharing a snapshot on a Board](paf.md#sharing-a-snapshot-on-a-board).

Use `federation boards publish --board ID --name NAME --only roles/NAME`,
`items --board ID`, and `versions --board ID --item ITEM`. Updates use
`publish --item ITEM --parent VERSION`; a stale parent is refused. `pin --board
ID --item ITEM --version VERSION` keeps a selected version through retention;
item listings show `latest_version`, `pinned_version` and `update_available`.
`boards updates` (GET `/api/federation/boards/updates`, `/v1` alike) lists
every kept item, across this node's boards, whose latest version differs from
its pin: `{updates:[{board,board_name,item,name,pinned_version,latest_version}],
notify,checked_at,error}`. It reuses a check up to five minutes old; `--refresh`
(`?refresh=1`) checks again, at most every 30 seconds. `boards updates --notify
on|off` (PUT `{notify}`, stored as `federation.board_update_notify`, default off)
makes the daemon check every 15 minutes and leave one Messages note per newly
seen version. The very first check on a node only records what it finds;
after that every new version gets one note, including versions posted while
the daemon was down (announced versions are kept in the data directory for 180
days). A failed check is retried after 30 seconds rather than cached, one
unreadable board does not hide the others, and keeping a version, leaving or
deleting a board refreshes the list. The check reads catalogs and pins only:
nothing is fetched or imported. These routes
are human-only and never served through peer view.
Re-publication uses `publish --board DEST --from-board SOURCE --from-item ITEM
--from-version VERSION` and retains exact original bytes and provenance.

`fetch`, `contents`, `download`, `preview` and `import` take `--board ID --item
ITEM --version VERSION`. Fetch verifies and scans into a private spool; it never
imports. Contents accepts `--path`, `--offset` and `--max-bytes` for bounded
inspection. Download requires `--file PATH` and never overwrites an existing
file. Preview accepts the existing `--only`, `--skip`, `--set NAME=VALUE` and
`--replace` choices. Import requires the preview's `--preview-token` with the
same choices. Tokens expire after ten minutes and are single-use; changed local
configuration requires a fresh preview. There is no automatic apply.

The matching human-only `/api/federation/boards/{board}/items` and `/v1` routes
provide list/publish, `/{item}/versions`, `/{item}/pin`, and
`/{item}/versions/{version}/{fetch,contents,download,preview,import}`. Downloads
support HEAD and use attachment disposition, nosniff and a sandbox CSP. Board
blob transfer uses the separate `/v1/boards/stream` connection with the existing
authenticated stream framing, shared hub bandwidth limits, bounded sizes and a
five-minute deadline. Board-only connections remain refused at every fleet and
hub-admin entry point, including on this stream endpoint.


`federation boards --help` lists action subcommands. Each action's help shows
only its applicable flags and marks the required ones. `boards invites --board
ID` lists owner-visible invitation IDs, roles, epochs, expiry times and redeemed
instances; it never returns bearer tokens or sealed key packages. The matching
human-only local GET `/api/federation/boards/{board}/invites` and `/v1` routes
return `{invites:[...],next_cursor}` with at most 50 rows per page.

`federation moves ls` prints a table; use `--json` for the full response.
Remote file downloads require the stable `agt_…@peer` address shown by
`federation sessions`, rather than a session display name.

## Board cleanup and project file browsing

Owners can permanently delete a board with `tclaude federation boards delete --board ID` (dashboard API: `DELETE /api/federation/boards/ID`). Deletion removes hub membership, invitations, keys and item metadata; it cannot erase copies members already pulled. The dashboard should confirm deletion before calling it.

`tclaude federation file ls agt_…@peer [dir]` lists one project directory level through a temporary pinned terminal viewer, with the same watch/attach and `sessions.files.read` checks as `file get`. It examines at most 100 entries, omits secret paths, symlinks, special files and files above the download cap, and reports when the listing is capped. Use a narrower directory to see more. No recursive scan is performed. A browser with a live viewer uses `GET /api/federation/terminal-file?terminal=…&viewer=…&path=DIR&list=true`; the JSON response contains `entries` (`path`, `kind`, `size`) and `truncated`.

`federation boards ls`, `items`, `invites`, `federation file ls` and `federation hub status` show compact tables by default; pass `--json` for the unchanged structured response. Short-lived board RPC connections are deliberately isolated from fleet pairing and appear only in hub Debug logs.
