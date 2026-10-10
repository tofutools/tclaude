# Manual end-to-end verification

This runbook sets up a small skynet on one host: a `tclaude-hub` plus two
isolated tclaude nodes, `a` and `b`, each running real Claude Code agents.
You then drive both dashboards in a headless browser and both CLIs, and
check that the cross-node features work the way an operator would use them.

It is a manual, on-request procedure. It is not part of CI. Run it when the
operator asks for an end-to-end check, typically before a release or after a
large federation or dashboard change. Do not start it on your own.

The tooling lives in `scripts/e2e/`:

| File | Purpose |
|---|---|
| `e2e.sh` | builds the binaries; starts and stops the hub, nodes, mock APIs and browser; runs a node's CLI |
| `mockapi.py` | a stdlib mock of the Anthropic Messages API, so agents run without credentials or token spend |
| `drive/` | a small go-rod CLI that drives one long-lived headless Chrome a step at a time |

## Requirements

- Linux (or WSL), with `tmux`, `python3`, `go` and `google-chrome` on `PATH`.
- Claude Code installed as `claude`. It is only ever pointed at the mock API.
- Free local ports 18470 (hub), 18481/18482 (dashboards), 18491/18492 (mock
  APIs) and 19222 (Chrome DevTools).

Everything runs on 127.0.0.1 under one base directory (default
`$TMPDIR/tce2e`, override with `E2E_BASE`). Nothing touches your real
`~/.tclaude`, `~/.claude` or tmux server. Every process gets its own `HOME`,
`TMUX_TMPDIR` and XDG directories.

### Running from inside an agent sandbox

The whole procedure works inside the Claude Code sandbox; no unsandboxed
access was needed. Watch for these points:

- **Keep the base directory short.** Unix socket paths are limited to about
  108 bytes, and the agentd and tmux sockets live under each node's `HOME`.
  A scratchpad path is usually too long, so use something like
  `/tmp/claude-1000/e2e`.
- **Leaked agent environment.** `e2e.sh` unsets `TMUX`, `TMUX_PANE` and the
  `TCLAUDE_*` session variables that the calling agent leaks. Without that,
  a node's agentd tries to reuse the caller's tmux server and runtime unit.
- **Chrome and crashpad.** Chrome ignores `--user-data-dir` for crashpad and
  aborts when `~/.config` is unwritable. `e2e.sh browser start` points every
  XDG directory at a throwaway one.
- **Go build cache.** If `go build` cannot write `~/.cache/go-build`, set
  `GOCACHE` (and `GOMODCACHE`/`GOPATH` if needed) to a writable directory.

## Setup

```bash
E=scripts/e2e/e2e.sh
export E2E_BASE=/tmp/claude-1000/e2e      # short path, see above
$E build                                  # tclaude, tclaude-agentd, tclaude-hub, drive -> $E2E_BASE/bin
$E up                                     # hub + node a + node b + mock APIs; prints the hub admin claim token
$E pair                                   # both nodes connect with hub invites and trust each other (unrestricted)
$E browser start
D=$E2E_BASE/bin/drive
$D open "$($E dash a)"                    # log the browser into node a's dashboard
```

Each node's `agentd` runs with `--persist-operator-token`, so
`$E run <node> ...` acts as that node's operator. Logs are in
`$E2E_BASE/logs/`.

`pair` uses unrestricted trust to keep the run short. To test the trust
levels themselves, pair by hand instead: `$E hub invite`, then
`$E run a federation connect ws://127.0.0.1:18470 --invite <token> --name node-a`,
then `federation peers` and `federation trust` on each side.

Create some agents to work with. Model-backed agents get replies from the
mock, `[mock model @ node-b] Received: <your text>`:

```bash
$E run a agent groups create builders
$E run a agent groups set-default-dir builders $E2E_BASE/a/proj
$E run a agent spawn builders --name builder-1 --initial-message 'hello from a'
$E run b agent groups create reviewers
$E run b agent groups set-default-dir reviewers $E2E_BASE/b/proj
$E run b agent spawn reviewers --name reviewer-1 -C $E2E_BASE/b/proj --initial-message 'hello from b'
$E run b agent spawn reviewers --name reviewer-home -C $E2E_BASE/b --initial-message 'rooted in HOME'
echo 'shared notes' > $E2E_BASE/b/proj/notes.txt
```

## Driving the browser

`drive` connects to the Chrome started by `e2e.sh browser start`, acts on
the current tab and exits. You can take a step, look at a screenshot, and
decide the next step.

```bash
$D shot /tmp/shot.png             # then view the PNG
$D clicktext 'Fleet'              # clicks the smallest visible element containing the text
$D click 'button[data-board=open]'
$D type '#fleet-board-token' "$TOKEN"
$D key Enter                      # confirm dialogs accept Enter; Escape cancels
$D wait 'Joined' 10
$D eval 'location.href'
$D tab                            # list tabs; `drive tab 1` switches
```

Some tips:

- `clicktext` can match a container, such as a table cell, instead of the
  button inside it. If nothing happens, use `eval` to find the element and
  read its markup, then `click` a CSS selector.
- The fleet views (map, merged groups, Fleet admin) are opened with the map
  button at the top right of the dashboard, or by opening `/fleet-admin`
  directly on a logged-in tab.
- To switch nodes, open the other node's `dash` URL. Each node has its own
  dashboard session.

## Scenario checklist

Work through the sections that match the change under test. Take a
screenshot at each step marked 📸; those screenshots are the evidence for
your report.

### 1. Fleet basics

- [ ] Map shows both nodes as online, linked through the hub 📸
- [ ] *Groups · all nodes* shows groups and agents from both nodes
- [ ] Fleet → Peers lists the other node as trusted, with level and fingerprint
- [ ] `$E run a federation status` and `federation peers` agree with the UI

### 2. Remote terminal

- [ ] From node a's map, open a node-b agent's terminal in the browser 📸
- [ ] Type a line. The node-b agent receives it, and the mock reply
      `[mock model @ node-b]` appears in the same terminal 📸

### 3. Remote files

- [ ] `$E run a federation sessions b` lists b's agents as `agt_…@b`
      handles. `$E run a federation file get <reviewer-1 handle> notes.txt --output /tmp/notes.txt`
      downloads the file
- [ ] Each of these is refused with a clear error: `../.claude.json`
      (parent traversal), `.env` (secret path), and any file from
      `reviewer-home`, whose root is the node's whole `HOME`
- [ ] The dashboard file browser lists and previews the same file 📸

### 4. Hub administration

- [ ] Fleet → Hub: claim the hub with the token printed by `e2e.sh up`.
      The claim file under `hub/data/` is consumed 📸
- [ ] Hub settings, admissions and health render. A setting change survives
      a reload
- [ ] Infra-sensitive actions show a confirmation dialog. Enter confirms and
      Escape cancels

### 5. Move an agent between nodes

- [ ] Move `builder-1` from a to b. The offer shows up on b
- [ ] The landing picker explains each candidate directory (explicit, repo match,
      same path, group default, landing policy) 📸
- [ ] After accepting, the agent runs on b in the chosen directory, and the
      source on a is retired
- [ ] `$E run a federation moves ls` shows the completed move

### 6. Boards (shared config without peer access)

- [ ] On b: Fleet → Boards → create `team-presets`, then *Create invite…*
      (can read, 1 hour). The invite is shown once 📸
- [ ] On a: paste the invite and Join. The board shows a as *can read* and
      b as *owner* 📸
- [ ] On b: *Post config…* with `roles`, confirm with Enter. The item appears
      under Shared config 📸
- [ ] On a: *Open…* lists `bundle.json` and `sections/roles.json`, and
      clicking an entry previews it. Nothing is imported yet 📸
- [ ] On a: *Import…* shows a per-item diff. If every item is unchanged,
      Import stays disabled
- [ ] On b: add a role
      (`$E run b agent roles create --file role.json`), then *New version…*.
      On a, Import now shows that role as `new`. Import it, then
      `$E run a agent roles ls` lists it 📸

### 7. Optional

- Run scripts on a peer node (Fleet → Run scripts)
- Human inbox and answers across nodes
- Peer mail: `federation export` / `import`, then `agent message <member>@b`

## Recording demos

Browser clips come from Chrome's screencast. Start `drive record` in the
background, drive the page, then stop the recorder with SIGINT. It encodes
the frames with ffmpeg, holding each frame on screen for as long as it was
actually visible:

```bash
export DRIVE_SHOW=1                            # outline each click/type target first
$D record /tmp/rec/frames /tmp/rec/demo.mp4 & R=$!
$D caption 'Node a: opening the skynet map'    # banner at the bottom; no args clears it
$D clicktext 'Fleet'
...
kill -INT $R; wait $R
```

Navigating to another page removes the caption, so set it again after each
navigation.

For CLI clips, record a scripted session with asciinema. Set
`ASCIINEMA_CONFIG_HOME` to a writable directory when `~/.config` is
read-only. To turn the cast into video, play it in asciinema-player inside
the same Chrome and record that playback. The page needs to be served over
HTTP, for example with `python3 -m http.server` in the cast's directory,
because `file://` cannot fetch the cast:

```bash
ASCIINEMA_CONFIG_HOME=/tmp/asciinema asciinema rec -q --overwrite --cols 140 --rows 24 -c ./cli-demo.sh /tmp/rec/cli.cast
```

The player page loads
`https://cdn.jsdelivr.net/npm/asciinema-player@3.8.0/dist/bundle/asciinema-player.min.js`
(and its CSS), creates the player with `autoPlay: false`, and exposes it as
`window.player`. Open it, start `drive record`, run
`$D eval 'window.player.play()'`, wait for the cast's length, then stop the
recorder.

## Single-host caveats

Both nodes share one filesystem, so some results differ from a real
two-machine setup:

- **Same-path landing always matches.** A path on a exists on b too, so a
  move offers `same_path` where two machines usually would not. On a real
  pair, expect `group_default` or the landing policy instead.
- **Shared tools.** Both nodes use the same `claude`, `git` and `go`
  binaries, so harness-version differences between nodes are not covered.
- **No real network.** The hub is plain `ws://` on loopback. TLS
  (`--ca-file`), NAT and reconnects after network loss need real machines.

## Reporting

Summarize what you set up, which checklist items passed, and what failed,
with the screenshots. Send the summary to the operator with
`tclaude agent notify-human -a shot.png ...`. For each defect, file a
ticket or hand it to the owning agent, and say in the summary that you did.

## Teardown

```bash
$E down                 # hub, nodes, mock APIs, both tmux servers and Chrome
rm -rf "$E2E_BASE"      # only after checking that it is the e2e directory
```
