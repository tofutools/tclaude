---
name: demo-recording
description: >-
  Record a narrated demo video of a feature you built end to end: browser
  clips with captions and click highlights, CLI clips from scripted terminal
  sessions, title cards, and a single reel. Then send it to the operator
  with `tclaude agent notify-human -a`. Use after finishing a user-visible
  feature, or when the operator asks for a demo, recording, video or "show
  me". Covers an isolated, credential-free environment, rehearsing,
  recording, reviewing your own footage, and delivering it.
---

# Demo recording

Show, don't tell. After you build a feature end to end, a short video of it
working is the fastest way for the operator to judge it. Recording also
surfaces UI and UX bugs that tests miss. File those as findings: fixing them
is part of the job.

The loop is: build → isolated environment → storyboard → rehearse → record →
review your own footage → deliver → fix what you found → re-record.

## 1. Environment: isolated and disposable

Never record against the operator's real instance, their real `HOME`, or
real credentials.

- **In the tclaude repo**, use the existing tooling, documented in
  `docs/e2e-manual-verification.md`:

  ```bash
  E=scripts/e2e/e2e.sh
  export E2E_BASE=/tmp/<you>/e2e   # keep it SHORT: unix sockets max out around 108 bytes
  $E build && $E up && $E browser start
  $E pair                          # skip this when the pairing itself is the demo
  ```

  Real Claude Code agents talk to `scripts/e2e/mockapi.py`, so no tokens or
  credentials are used. `scripts/e2e/demo/record.sh all` is a complete,
  working example: copy its helpers when you script a new demo.
- **Elsewhere**, give the app its own `HOME`, ports and data directory, and
  start it from an allowlisted environment (`env -i PATH=... HOME=...`). An
  agent session leaks variables such as `TMUX`, `TCLAUDE_*` and
  `CLAUDE_CONFIG_DIR`, which can point a child process at the operator's
  real daemon or credentials.
- **Headless Chrome under a sandbox:** Chrome's crashpad ignores
  `--user-data-dir` and aborts when `~/.config` is read-only. Point
  `XDG_CONFIG_HOME`, `XDG_CACHE_HOME` and `XDG_DATA_HOME` at a disposable
  directory. Launch it with
  `--headless=new --no-sandbox --remote-debugging-port=19222 --window-size=1280,800 --screen-info={1280x800}`.

## 2. Drive the browser one step at a time

`drive` is a small CDP client (go-rod) that acts on one long-lived Chrome.
Each call does one thing and exits, so you can inspect the result between
steps. Build it with `go build ./scripts/e2e/drive` in the tclaude repo, or
`go install github.com/tofutools/tclaude/scripts/e2e/drive@latest`.

```bash
D=drive                               # uses DRIVE_PORT, default 19222
$D open http://127.0.0.1:8080/        # navigate the current tab
$D shot /tmp/s.png                    # screenshot; view it before the next step
$D clicktext 'Save'                   # smallest visible element containing the text
$D click 'button[data-x=open]'        # CSS selector
$D type '#name' 'hello'
$D key Enter                          # Enter, Escape or Tab
$D eval 'document.title'
$D caption 'What the viewer should notice'   # banner; no args clears it
```

**Rehearse every step with screenshots before recording.** Most broken
takes come from the UI being in a different state than you assumed. Check
these in particular:

- `clicktext` can hit a container or a table cell. If it does, find the
  element with `eval` and use `click` on a selector.
- Toggles: a just-created item may already be expanded, so clicking its row
  collapses it.
- `<select>`: set `.value`, then dispatch both `input` and `change` events.
  Otherwise the app's state may not follow, and the confirm dialog will name
  the old choice.
- Text such as tokens: read it from the leaf element (`childElementCount===0`),
  not with a regex over `innerText`. `innerText` runs adjacent link text
  ("copy", "cancel") into the token.
- Single-use actions (claim tokens, invites, moves): rehearse with a
  throwaway object, or reset the state afterwards. Don't burn the real one.
- Confirm dialogs: Enter confirms and Escape cancels. Use them in the demo:
  they're part of the UX.

## 3. Record

```bash
export DRIVE_SHOW=1                         # outline each click/type target first
$D record /tmp/rec/03 /tmp/rec/03.mp4 & R=$!
$D caption 'Open the map'; sleep 2.2
$D clicktext 'Map'; sleep 3
...
$D caption; sleep 1.5; kill -INT $R; wait $R
```

- `drive record` takes steady screenshots (`DRIVE_FPS`, default 10) and
  follows the tab across navigations, including other origins. Do **not**
  use Chrome's screencast API: it delivers frames at changing viewport
  heights, and the video jumps.
- Give each clip one user story, about 20–90 s. Write a caption *before*
  each action, saying what is about to happen and why it matters. Hold it
  about 2 s. Pause after results so the viewer can read them.
- Show the guard rails as well as the happy path: refusals, confirm dialogs,
  "nothing changes until you import".
- Navigating removes the caption, so set it again after each navigation.

## 4. CLI clips

Script the terminal session, record it with asciinema 2.x, and play the cast
back in asciinema-player inside the same Chrome while `drive record` runs.
The result looks the same as the browser clips. `scripts/e2e/demo/`
(`cli-common.sh`, `cli-pair.sh`, `player.html`) is the template:

- `say "..."` prints a coloured comment; `show`/`on` echo the command
  before running it.
- When a value you capture is shown on screen, send stderr to `/dev/null`,
  and stop when a captured value is empty.
- Wrap expected refusals in `|| true`: they are part of the demo.
- Set `ASCIINEMA_CONFIG_HOME` to a writable directory when `~/.config` is
  read-only. Serve the player over HTTP (`python3 -m http.server`), because
  `file://` cannot fetch the cast.

## 5. Assemble and review

- Title cards: screenshot a simple HTML page (`scripts/e2e/demo/title.html`),
  then turn it into a 3 s clip with `ffmpeg -loop 1 -t 3`. Use the same
  pixel format as the clips, so `ffmpeg -f concat -c copy` can join them.
- Check that it decodes: `ffmpeg -v error -i reel.mp4 -f null -`.
- **Watch your own footage** before you send it. Extract frames
  (`ffmpeg -ss <t> -i clip.mp4 -frames:v 1`) and tile them with
  `hstack`/`vstack` into a contact sheet, then look at it. Check that each
  caption matches what is on screen and that no step silently failed.
  Confirm the end state from the CLI too, for example that the imported role
  exists.
- Take a bad take again. Don't explain it away.

## 6. Deliver

```bash
tclaude agent notify-human --subject "Demo: <feature> (reel attached)" \
  --file body.md -a /tmp/rec/reel.mp4
```

The body should say:

- what you set up;
- a numbered list of the clips;
- where the separate clips are;
- the bugs the recording surfaced, with their tickets and PRs;
- whether anything is needed from the operator.

`notify-human` needs `human.notify`. Without it, send the reel through your
coordinating agent, or fall back to `--ask-human`.

## 7. Afterwards

- Make the demo reproducible: script it, and commit the script where the
  repo keeps such tooling. In tclaude that is `scripts/e2e/demo/`.
- Turn the operator's feedback on the video into tickets. Re-record once
  the fixes land.
- Tear down: `e2e.sh down` (or stop your own processes by their PIDs).
  Never use `pkill -f <pattern>`: the pattern also matches your own shell's
  command line and kills it.
