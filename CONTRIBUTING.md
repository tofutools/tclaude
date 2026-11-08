# Contributing

## Quick start

```bash
git clone https://github.com/tofutools/tclaude
cd tclaude
go install . ./cmd/...
```

The repo builds three binaries: the `tclaude` CLI at the module root, and
under `cmd/` the standalone `tclaude-agentd` daemon and the `tclaude-hub`
federation relay (see `docs/federation.md`). A bare `go install .` builds
only the root package, so name both patterns — `./cmd/...` matches the
daemon and the hub. Do not widen it to `go install ./...`: that would also drop the
non-production tools under `scripts/` into your `GOBIN`.

The daemon is built into the CLI as `tclaude agentd serve`, so the second
binary matters mainly when you are working on it or running a daemon-only host.
Installing the pair keeps them from drifting apart across rebuilds.

## Running tests

```bash
go build ./...
go test ./...
golangci-lint run ./...
```

Platform sandbox assumptions are separate hardware-backed hard gates; the
contract lives in `pkg/claude/sandboxassumptions`. On a compatible Linux
host:

```bash
TCLAUDE_SANDBOX_ASSUMPTIONS=1 \
  go test ./pkg/claude/sandboxassumptions \
    -run '^TestBubblewrapAssumptions$' -count=1 -v -timeout=180s
```

### What PR CI actually runs

`go test ./...` locally runs everything. PR CI does not: each shard is graded
against the pull request's base commit by `scripts/ci/affected`, which walks
the real import graph — including test-only imports — and runs only the
packages a change can reach. The step log always names the verdict and why it
was reached.

The selector is deliberately conservative. A changed file it cannot attribute
to a Go package — `go.mod`, a workflow, a shell script — falls back to running
the whole shard, as does a change to the selector itself or setting
`TCLAUDE_CI_FULL_TESTS=1`. Documentation (`docs/`, `mkdocs.yml`, root-level
Markdown) is the exception: it feeds no build, so a changed doc selects only
the packages whose tests declare reading it with a `//ci:reads docs/x.md`
comment line (or a `docs/dir/` prefix) — in a Go test file or a `.test.mjs`
suite, which counts for the Go package that runs it — and nothing at all
otherwise. A PR
that changes only docs skips the Go jobs entirely. Release and
manual-release always run every shard in full, so the merge commit is measured
without selection before anything ships.

Before any of that, the `Changes` job decides which whole jobs run at all
(`scripts/ci/classify-changes.py`, unit-tested by `test_classify_changes.py`),
so a job the change cannot need never claims a runner:

- **docs**: only docs changed — only the mkdocs and Learn-commands checks run.
- **ui**: only dashboard/jstest assets, agentd's `dashboard_*_test.go` and
  docs changed — the Linux agentd, jstest and rest shards run (plus lint when
  a Go test changed); every smoke is skipped.
- **code**: everything else runs every test shard. Sandbox, proxy, session
  and harness-launch code also runs the filtered-proxy and proxy-posture
  smokes. The Copilot, Gemini and pi smokes run when `scripts/ci/affected`
  says the change reaches their fixture package. Lint also type-checks every
  package and its tests as `GOOS=darwin` (`scripts/ci/darwin-vet.sh`), so a
  darwin compile break fails on Linux.
- **full**: `.github/`, `scripts/ci/`, `scripts/test.sh`, `go.mod`, `go.sum`,
  `mise.toml`, an unknown path or an empty diff — everything runs.

PR CI runs on Linux only unless the PR opts into macOS; see [macOS CI](#macos-ci). `CI complete` checks every
job against that verdict, and release.yml runs every job, including the macOS
one, on every merge to main.

### macOS CI

macOS on PRs is opt-in by design, not a gap to close. GitHub's hosted macOS
capacity is too small for this repository to run macOS on every PR. The
self-hosted runner described below is an experiment whose reliability is not
yet known. Until it has a track record, darwin coverage relies on the macOS
job that runs on every merge to main.

Hosted macOS runners are concurrency-capped, and PR CI and Release share them.
When none is free, a job fails with "The job was not acquired by Runner of type
hosted even after multiple attempts" (in the jobs API: `conclusion: cancelled`,
`runner_id: 0`, no steps), and that failed `CI complete` and Release without
anything having been tested. So:

- **PRs run no macOS jobs unless they opt in** (below). The macOS parts of
  `copilot-lab.yml`, `group-route-feasibility.yml` and `macos-pf-probe.yml`
  run on macOS only when started by hand (`workflow_dispatch`). Lint's
  `GOOS=darwin` vet still catches darwin compile breaks on every PR.
- **Every merge to main runs one macOS job**: `macos` in `release.yml` (and
  in `manual-release.yml`). It runs the darwin goreleaser build, the `agentd`
  and `rest-all` test shards, and the Sandbox v2 Seatbelt smoke
  (`.github/actions/seatbelt-smoke`) in sequence, in about 25 minutes. macOS
  skips the `jstest` shard, which runs browser JS under Node and LinkeDOM and
  does not vary by OS. A darwin-only break therefore shows up as a red Release
  after the merge, and blocks publishing until it is fixed on main.

#### Opting a PR into macOS

A PR gets the macOS jobs (the `agentd` and `rest-all` test shards and the
Sandbox v2 Seatbelt smoke) when either of these holds:

- **Per commit:** the PR's head commit has the trailer `CI-macOS: true` in the
  last paragraph of its message, next to `Co-Authored-By:`. The opt-in lasts
  for that commit only: the next push without the trailer runs Linux only, and
  a trailer on any other commit of the PR counts for nothing. Git only parses
  the last paragraph for trailers, and each `-m` starts a new paragraph, so
  put `CI-macOS: true` in the same `-m` (or heredoc paragraph) as the other
  trailers: `-m "CI-macOS: true" -m "Co-Authored-By: ..."` silently opts
  out. Check with `git log -1 --format='%(trailers:key=CI-macOS)'`. To opt in
  without changing code, push an empty commit:
  `git commit --allow-empty -m "ci: run macOS" -m "CI-macOS: true"`.
- **Per PR:** the PR has the `ci:macos` label (create it once with
  `gh label create ci:macos` if the repository lacks it). Adding a label
  starts no CI run, so after adding it, push, or rerun the PR's CI run in
  full (`gh run rerun <run-id>`, not `--failed`). The `Changes` job reads the
  labels when it runs, so a rerun picks up the change, and removing the label
  works the same way.

`CI complete` then requires the macOS jobs to pass, exactly as it requires the
Linux ones. A starved macOS job blocks the PR again, so opt in only when the
coverage is worth it. Do opt in before merging changes that mostly matter on
darwin and that Linux CI cannot exercise: `_darwin.go` / `_unix.go` files or
`//go:build darwin` code, cgo, Seatbelt profiles and the macOS sandbox
launcher, macOS paths (`/var/folders` vs `/private/var/folders`, `~/Library`),
and the `scripts/seatbelt-smoke` driver. Everything else gets darwin coverage
on main after the merge. Fork PRs can use the trailer too (a label needs
triage rights), but their macOS jobs run on hosted runners; see below.

#### Running macOS on every PR again

To make macOS the default rather than an opt-in, have the classifier treat
every PR as opted in (`macos=True` in its `main`), or bring back the
platform-file rules that decided per change which PRs needed macOS. Find them
with `git log --oneline -S needs_macos -- scripts/ci/classify-changes.py`; the
newest hit removed them, and its parent has them together with their tests.
The macOS jobs and their gate wiring already exist, so no ci.yml change is
needed. `CI complete` compares each job with the classifier's `expected`
verdict, and its full-run check exempts only `OPT_IN_JOBS`; empty that tuple
(and the matching `opt_in` set in ci.yml, which a test pins to it) if the
Seatbelt smoke becomes unconditional. In the other workflows, remove
`if: github.event_name != 'pull_request'` from `group-route-feasibility.yml`'s
`darwin` job, restore the `pull_request` paths in `macos-pf-probe.yml`, and
make `copilot-lab.yml`'s `platform` list include `macos` for every event.

#### Restoring the per-shard macOS split on main

Find the commit with
`git log --oneline -S 'os: [ubuntu-latest, macos-latest]' -- .github/workflows/release.yml`
(the newest hit) and restore `release.yml`, `manual-release.yml` and
`.github/workflows/reusable-sandbox-v2-seatbelt-smoke.yml` from its parent
(`git show <sha>^:<path>`). That parent has `macos-latest` in the test matrix
`os` list (every shard, `jstest` included) and in the `build` matrix, plus the
Seatbelt smoke as its own reusable workflow. If you do this, put the `release`
job's `needs` and the `homebrew` job's `if` back on those jobs. Also set
`SAVERS` in `scripts/ci/build-cache.py` to the number of release jobs that
save a build cache (the `agentd` and `sandbox` shards on each OS).

#### Self-hosted macOS runners

PR CI's opted-in macOS jobs run on the repository's self-hosted macOS runner
(`mini`, in the runner group `tclaude-mac`, which only this repository can
use) when the PR's head branch is in this repository, and on hosted `macos-latest`
when it comes from a fork. Main keeps hosted macOS: `release.yml` and
`manual-release.yml` are unchanged.

The runner is an experiment. It is one Mac mini, and how it holds up under
real PR load (uptime, job isolation, how persistent caches age, macOS updates)
is still being measured. That is why main, which gates releases, stays on
hosted macOS. A failure on `mini` that has nothing to do with the change (job
setup, a cache or toolchain error, a timeout) may be the runner, so rerun the
job. If it persists, report it, and if you need to unblock the PR, opt out by
removing the label or pushing without the trailer. A failure in darwin-specific
code is still most likely real: catching those is why a PR opts in.

- **Routing.** The `Changes` job passes `--self-hosted-macos` to the
  classifier only when `github.event.pull_request.head.repo.full_name` equals
  `github.repository` and that is `tofutools/tclaude` (a fork's own CI has no
  such runner). The classifier then gives the macOS test entries and the
  Seatbelt smoke (through its `macos_runner` output) the runner's default
  labels, `[self-hosted, macOS, ARM64]`, instead of `macos-latest`.
  `test_classify_changes.py` pins this.
- **Caches.** The runner keeps `~/go/pkg/mod` and `GOCACHE` between jobs,
  and those local caches replace setup-go's: both composite actions pass
  `cache: ${{ runner.environment != 'self-hosted' }}`, because restoring the
  module cache over Go's read-only module files fails with "Can't unlink
  already-existing object". Hosted runners still use setup-go's cache.
- **Security.** This repository is public. A fork's pull request can run
  arbitrary code on any runner it reaches, so never let fork PRs reach
  self-hosted runners. The `CI-macOS: true` trailer is fork-controlled, which
  is why the head repository decides the route. That check stops accidental
  routing only: a fork PR's workflow is read from its own merge commit, so a
  fork can edit `ci.yml` itself. The real safeguard is in the repository
  settings: require approval for all outside contributors' workflow runs
  (Settings › Actions › General), read a fork's changes to `.github/` and
  `scripts/ci/` before approving, and keep the runner in its group
  restricted to this repository.
- **Ephemeral runners.** Prefer ephemeral runners (`config.sh --ephemeral`,
  one job per machine). The sandbox tests change their environment, and the
  Seatbelt smoke runs `npm install --global`, so a persistent runner carries
  state from one job into the next.
- **Concurrency.** The pool size is how many macOS jobs run at once. With
  macOS back on PRs, each code PR asks for up to three (the agentd and rest-all
  shards plus the Seatbelt smoke), and each merge asks for one. Jobs queue
  rather than fail on self-hosted runners, but a small pool still makes PRs
  wait, so size it for the PRs you expect at once.

A test that reads a file outside its own package directory without importing
that package is invisible to the graph. Keep such reads inside the package, or
reach the file through an import, as `pkg/claude/agentd/jstest` does. When a
test genuinely has to scan the whole module — the structural guards that parse
every production file, for instance — no import graph can predict it, so put a
`//ci:whole-tree` comment in the file. That opts its package out of selection
entirely: every change selects it. `scripts/ci/affected` has a guard test that
fails when a module-root scanner is missing the marker, so this is enforced
rather than remembered. A second guard fails when a test climbs out of its
package and names the docs tree without a `//ci:reads` directive.

Two more things worth knowing. A green PR check means "everything this change
can reach passed", not "the whole suite ran". And running the selector by hand
against your working tree under-reports untracked files, since it grades a
`git diff`; commit first, or pass `-head`.

### Filesystem paths in tests

A temporary directory's spelling is not its identity. macOS commonly returns
`/var/folders/...` while Git and filesystem canonicalization return
`/private/var/folders/...`. `filepath.Clean` and `filepath.Abs` do not resolve
symlinks.

Use `testutil.CanonicalTempDir(t)` from `pkg/testutil` for fixtures that need a
canonical path: Git identity comparisons, resolved sandbox grants, and
no-follow filesystem operations. The helper retains `t.TempDir` cleanup and
has no application dependencies, so low-level packages can import it too.
Migrate existing fixtures when touching them; ordinary temporary files do not
need a repository-wide rewrite.

When testing a public API that accepts paths, explicitly include a symlinked
input and check the intended contract: preserve the supplied spelling, resolve
it, or reject it. Do not canonicalize those inputs merely to make assertions
pass, and do not weaken production no-follow checks to accommodate a fixture.

Run focused tests through the shared wrapper:

```bash
scripts/test.sh ./pkg/claude/agentd -run '^TestMaterializeAgentDirectories' -count=1
```

It forwards all arguments to `go test` (defaults to `./...`), supplies a fresh
symlinked `TMPDIR`, and removes its scratch directory after the command exits.
Tests and subprocesses therefore see the alias even on Linux. The shared CI
action uses this wrapper for Linux shards; macOS retains its native temp
layout. A fresh `TMPDIR` can invalidate cached test results for tests that read
it. The wrapper does not simulate other macOS behavior or replace macOS CI.

### Deterministic timing in tests

Make correctness conditions causal. Use a channel, barrier, callback, wait
group, or observable state to prove that work reached the relevant point before
allowing it to continue. Do not sleep to “give a goroutine time,” and do not
prove that an action was suppressed by observing nothing for a few
milliseconds. For example, a queued-writer test should wait until the writer is
actually contending on the lock through an explicit hook or channel barrier,
then assert state before releasing it.

Never rely on a single fixed sleep. When a test checks a timeout, a delay or
any other async behaviour, poll until the condition holds, up to a time
limit, and fail with a clear message when the limit is hit: `waitForCondition`
in the jstest suites, `require.Eventually` / `assert.Eventually` (or the local
equivalent) in Go. An empirically tuned sleep or a widened margin is not a
fix. A fixed wait is acceptable only to show that something does *not* happen
within a window, and even then prefer driving a fake or injected clock.

Use the narrowest timing mechanism that matches the boundary:

1. Prefer causal handshakes or observable state when ordering is the contract.
2. Use Go's `testing/synctest` when all relevant goroutines, channels, timers,
   tickers, and contexts can live and durably quiesce inside one bubble.
3. Inject a clock, timer, or sleeper when production behavior is genuinely
   time-driven and the system cannot fit in a synctest bubble.
4. At real filesystem, subprocess, tmux, network, or other OS boundaries, poll
   production-observable state with a generous failure deadline and useful
   diagnostics, or add a controlled helper-process handshake.
5. Assert real elapsed time only when wall-clock performance is itself the
   contract.

`testing/synctest` does not virtualize external I/O, and `synctest.Wait` does not
treat mutex contention or syscalls as durable blocking. Use explicit hooks or
channel barriers for mutex/OS-lock contention. Real subprocesses, fsnotify
watcher descriptors, and database or daemon goroutines created outside the
bubble usually need a handshake, injected-time seam, or observable-state
polling instead. Pure debounce or state-machine logic fed by bubbled channels
can still be a good synctest candidate. Do not distort production boundaries
merely to force them into a bubble.

A generous deadline that only prevents a hung test is a liveness guard, not a
correctness assertion, and is acceptable. Keep it well outside normal
operation, make timeout failures diagnostic, and never replace a flake by
broadly increasing sleeps or retrying the whole test. A deadline created inside
a synctest bubble advances in fake time and tests controlled behavior; it is not
a wall-clock hang guard. Rely on the outer `go test -timeout` watchdog for a
stuck bubble. Preserve shared test harnesses and parallelism; deterministic
conversions should keep total suite time flat or make it faster.

## Dashboard frontend

### See your change in the sandbox

Before you open a PR that changes the dashboard, look at the change in a real
browser. `scripts/sandbox/ui.sh` serves the real dashboard and daemon API from
your working tree, over seeded data that fills every page and tab. It runs
under a throwaway HOME with tmux simulated, so it never touches `~/.tclaude`,
your daemon or your tmux. See `scripts/sandbox/README.md` for what the data
covers and what it cannot show (live terminals, a live federation peer).

```bash
scripts/sandbox/ui.sh up            # build, seed, print the URL (about 5s warm)
scripts/sandbox/ui.sh tour          # every route: no errors, no empty pages
scripts/sandbox/ui.sh smoke [NAME]  # the Playwright smoke tests
scripts/sandbox/ui.sh down          # stop it and delete its data
```

**Drive it with Playwright.** `scripts/sandbox/smoke` pins `@playwright/test`
and uses the system Chrome, so nothing is downloaded. To look at a page,
`playwright-cli` or a short Playwright script against the printed URL is
enough. A check worth keeping goes into `smoke/tests/`. If Playwright is not
available, fall back to headless Chrome:

- `google-chrome --headless=new --virtual-time-budget=10000 --window-size=1440,900
  --screenshot=out.png URL`. The budget lets the dashboard finish booting;
  without it the shot shows the loading curtain.
- The step-wise driver `scripts/e2e/drive`.

**Under an agent sandbox**, point `XDG_CONFIG_HOME`, `XDG_CACHE_HOME` and
`XDG_DATA_HOME` at a writable temp dir before you start Chrome yourself.
Chrome's crashpad aborts when `~/.config` is unwritable. `ui.sh smoke` and
`ui.sh tour` already do this.

**Show it to the operator** with `tclaude agent show :PORT`, which needs
`self.page`. On a host-network agent, the stage shows only a server inside
your own process tree. A plain `ui.sh up` detaches its server, so serve it in
the foreground instead, kept alive by your harness's own background run
(Claude Code's `run_in_background`, for example):

```bash
scripts/sandbox/ui.sh up --foreground --port 7790 > sandbox-out/fg.log 2>&1   # as a background run
tclaude agent show :7790 --name sandbox
```

A bare `&` works only in a shell that stays alive. A tool call's shell exits
when the command returns, and its orphaned jobs leave your process tree.

The operator then sees the seeded dashboard, live, in their own. This works
for the `full` fixture and the other docshots fixtures, but not `page`. `tclaude
agent page snapshot` renders the page only 300ms after it loads, before the
dashboard has painted. To see it yourself, use Playwright on the URL.

**Say so in the PR.** Say what you looked at and how: for example, "checked
the Task board edit form in the sandbox with Playwright; `ui.sh tour`
passes".

**A new page or tab** is toured automatically, since the tour reads the
router's route list. The tour fails until the `full` fixture
(`pkg/claude/agentd/sandbox_serve_test.go`) seeds it, or until the tour's
`ALLOW` list says why it stays empty.

### Docs drift

`.claude/skills/docs-drift` compares what `docs/` says with what the dashboard
shows and files what differs as tickets for review. `scripts/docs/drift.sh` records
each page in `scripts/docs/tour.json` from a seeded throwaway server, and
`scripts/docs/drift-file.sh` files each finding once. Neither touches the docs, the
UI or a running daemon. To run it on a schedule, opt in with the `cron` command in
the skill.

### Documentation screenshots

Every dashboard image in `docs/assets/` comes from one command:

```bash
scripts/docs/screenshots.sh                  # capture and install changed images
scripts/docs/screenshots.sh --dry-run        # capture and report only
scripts/docs/screenshots.sh --only 'learn-'  # capture matching shots
```

The manifest `scripts/docs/shots.json` lists each shot's name, fixture, page,
tab, target and setup steps. To add an image, add a shot; to add data, extend
the seeds in `pkg/claude/agentd/docshots_test.go`. `TestDocShotsManifest` fails
in CI if a docs image is missing from the manifest. Terminal images
(`term-*.png`) are drawn by hand.

- Data is invented. The server runs from the flow-test harness under an isolated
  `HOME`, and a shot fails if the page shows the host's user, hostname or home.
- Time is frozen. A build overlay stops the server's wall clock at the manifest's
  `frozenTime` and gives agents a fixed id sequence, and the browser clock stops
  at the same instant. No shipped code changes.
- A missing page, tab or element fails the run, and nothing is installed.
- An image is replaced only when over 0.05% of its pixels changed, so rendering
  noise causes no diff. Review `docshots-out/contact-sheet.png` before committing.
- It needs Chrome or Chromium, plus the Noto Sans, DejaVu Sans Mono and Noto Color
  Emoji fonts. The first run takes a few minutes because the overlay rebuilds
  everything above package `time`.

### Vendored dependencies

The dashboard uses browser-native ES modules. Preact islands use HTM for
component templates, so editing or running the dashboard requires no Node
install, compiler, or frontend build step: the normal `go install` workflow
embeds everything it needs. Runtime modules are pinned and committed under
`pkg/claude/agentd/dashboard/third_party/preact/`; the dashboard never loads them
from a CDN.

Dependency upgrades are deliberately rare and reviewed as vendored-code
changes. Update the exact versions, hashes, source maps, and license metadata
in that directory's `README.md` together, then run the ordinary Go test suite.

### Standard controls

Buttons, selects, text fields, checkboxes and menus use the `ui-*` classes in
`pkg/claude/agentd/dashboard/ui.css`: `ui-btn` plus at most one of
`ui-btn-primary` (the main action), `-success` (approve, run, start),
`-danger`, `-ghost` or `-link`, with `ui-sm` for row and toolbar sizes;
`ui-input`, `ui-select`, `ui-textarea`; `ui-check` and `ui-switch`; and
`ui-menu` with `ui-menu-item`, `ui-menu-sep` and `ui-menu-heading`. State goes in
attributes (`disabled`, `aria-pressed`, `aria-busy`, `aria-invalid`), and
colours are `--ui-*` variables that the wizard and slop skins override, so a
new control needs no CSS of its own.

Preact code uses the components in `dashboard/js/ui-components.js` rather than
the raw classes: `Button` (`variant`, `size="sm"`, `icon`, `busy`, `pressed`),
`Select`, `Input`, `Textarea`, `Check`, `Switch`, `Seg`, and `Menu` with
`MenuItem`, `MenuSep` and `MenuHeading`. They pass other props through, take
extra classes as `class`, and hand their element to `domRef`, because a Preact 10
function component cannot take `ref`. `Button` defaults to `type="button"`. The
classes stay for innerHTML and `createElement` code, where `uiButtonClass()`
builds a button's class list.

An optional setting, one that is either left unset (inherit a profile, the
group or the harness default) or set, uses `OptionalSelect`. With no
`options` it is inherit / on / off on the values `''` / `'1'` / `'0'`
(`triValue` and `readTri` in `management-model.js` convert those to and from a
nullable boolean). `unset`, `on` and `off` relabel it, `options` makes it
inherit / value, and `unsetValue` keeps a wire value such as `'inherit'`.
`onChange` gets the value, not the event. `jstest/ui-optional-select.test.mjs`
fails when a file hand-writes those options again.

`jstest/ui-controls.test.mjs` fails when a file gains a `<button>` or `<select>`
without a `ui-*` class. Mark a deliberate one-off, such as an overlay window
control, with `data-ui-exempt`. Older markup is held by a per-file baseline;
after moving an area onto the standard classes, lower it with
`UPDATE_UI_CONTROLS_BASELINE=1 node --test jstest/ui-controls.test.mjs`.

### Table columns

For a new dashboard table, the default is bounded auto-width: every column
is as wide as its content, between a minimum and a maximum, and text wraps
past the maximum (no ellipsis on content a reader needs). The columns pack to
the left, and the spare width of a wide screen goes to one trailing column or
a filler, never to a content column such as Name. No table sizes its columns
from another table's. The task list and Resources use it.

This is a default, not a mandate: do not migrate existing tables to it. A
table may deliberately do otherwise, and these are valid choices:

- breaking a name mid-word when the name has no spaces to wrap at;
- a fixed-width or right-aligned value column, such as the costs on the
  Usage tab;
- columns that hide when space is tight.

`ui.css` carries the convention. Give the table `ui-table-auto`, each content
column `<col class="ui-col-fit">`, and the trailing one `ui-col-rest`. Bound a
cell that can grow, such as a name or a title, by wrapping its content in
`.ui-fit` (a block box, whatever the element), and set `--ui-fit-min` / `--ui-fit-max` on it (the task list's
Name: 12em to 30em). Use a fixed track, `<col class="ui-col-track">` with
`--ui-track`, only where tables must line up with each other, such as one list
drawn as two tables (Resources). Check a table at about 1280px and 2000px wide
with short and long content.

### Layers

Every `z-index` is a layer from `ui.css`. From bottom to top:

1. `--z-content`
2. `--z-window`
3. `--z-chrome`
4. `--z-panel`
5. `--z-dialog`
6. `--z-popover`
7. `--z-confirm`
8. `--z-toast`

`--z-system` sits above the scale, for the boot bar, the disconnect screen and
skin effects. To order things within a layer, use `calc()` of the layer plus a
small step, for example an editor that opens over another dialog. A raw 0–9
orders the parts of a single component. `jstest/ui-layers.test.mjs` fails on any
other number.

Anything opened from the panel layer up paints over the overlay windows and
never moves or minimizes them.

### Component tests

Component tests live in `pkg/claude/agentd/jstest/*.test.mjs` and use Node's
built-in `node:test` runner plus `createPreactHarness` from
`jstest/preact-harness.mjs`. The harness loads the real dashboard import map,
exact vendored Preact modules, HTM, Signals, Preact test utilities, and a
committed LinkeDOM runtime. It provides `mount`, `act`, `input`, `fireEvent`,
`getByRole`, `getByLabelText`, and `importDashboardModule` helpers.

Prefer behavioural assertions through accessible roles and labels. Dispatch
real DOM events, use stable keys, and unmount every tested component so effect
cleanup is observable. Run a focused file directly with:

```bash
node --test pkg/claude/agentd/jstest/your-component.test.mjs
```

The ordinary `go test ./...` command also runs every `*.test.mjs` file. CI has
Node installed and fails loudly if Node or a committed test-runtime dependency
is missing. No npm install, browser, CDN, or network access is needed.

### State and DOM ownership

`snapshot-poll.js` owns the dashboard's single 2-second snapshot schedule;
`refresh.js` owns the request/render/publish transaction. Accepted server state
flows through `snapshot-store.js` and bounded feature-state adapters. Feature
components read their state there and route refreshes, retries, and mutations
through explicit action modules. Islands must not import `refresh.js`, start
competing poll timers, or infer application state from DOM nodes.

Every island owns reconciliation of the hosts declared by its loader.
Non-component modules must not independently replace a claimed host or treat
its DOM as durable application state. Narrow adapters are allowed only when the
island explicitly exposes the boundary: delegated actions may read an event
target's `data-*`; transient editors may hide a managed node and insert a
sibling; and the native `SlopMachine` component mounts its reel tree into an
explicit opaque child host that `slop-fx.js` may temporarily replace. Keep those
constraints covered behaviorally and do not widen them. Code outside claimed
hosts may remain imperative where it has a clear owner.

The global Signals boundary is for server-backed snapshot data, connection and
poll metadata, the active tab, and computed feature views. Ephemeral UI state
such as focus, hover, an open disclosure, draft form text, or a local selection
belongs in the owning component. Persisted preferences remain server-backed:
write them through an explicit action and let the authoritative snapshot
confirm the resulting state. Follow the established `*-state.js`,
`*-island.js`, and `*-actions.js` split when a feature has distinct reactive
state, rendering, and mutation responsibilities; do not create layers that
have no separate ownership job.

### Preact island lifecycle

Every production island uses `mountFeatureIsland` from
`dashboard/js/island-lifecycle.js`. Give it a stable feature name, every host
subtree the feature owns, and a loader callback that dynamically imports the
optional Preact feature graph. The callback returns the feature state (when the
authoritative poll needs it) and a mount function. That function receives
`registerCleanup`; after each render, listener, timer, or subscription is
created, register its idempotent cleanup immediately before starting the next
side effect. The lifecycle can then roll back a partial mount in reverse order.
It claims hosts, registers state, requires cleanup, and keeps load failures
inside the primary host. Do not write another feature-local registry or loader
lifecycle.

Host ownership is exclusive. Non-component code may fetch and publish feature
data but must not independently reconcile or replace a claimed host; the
bounded opaque and transient adapters above are explicit exceptions. Island
cleanup must release effects, listeners, timers, subscriptions, and registry
entries; component tests should call cleanup twice to prove it is idempotent.
Every registered cleanup is attempted even if another throws; host ownership is
released only after they all succeed, so a partial unmount cannot overlap a
later mount. Keep feature modules behind the dynamic loader so a missing
optional asset cannot prevent the static dashboard module graph from linking.

Use `AsyncLoadState` only for the shared accessible loading/error/retry notice.
The feature still owns stale-content layout, request generations, paging,
dialogs, focus policy, and mutations. Extract broader controls only when
multiple features demonstrate the same behavioral contract.

Signals hold durable or derived state. Use effects only to synchronize with an
external system, keep server effects in explicit action modules, use stable
domain IDs as component keys, and reserve imperative refs for widgets or focus
operations that cannot be expressed declaratively.

### Maintained imperative boundaries

Some browser integrations deliberately remain outside Preact reconciliation:

- xterm owns the terminal canvas, hidden textarea, addons, WebSocket stream,
  and terminal-specific input listeners below its stable host;
- `process-graph.js` owns its SVG viewport and pointer mechanics while the
  Processes island owns the surrounding lists, dialogs, and selected data;
- `costs-chart.js` owns its imperative DOM bar chart below the Costs component's host;
- `vegas.js`, `slop-audio.js`, `slop-fx.js`, and `wizard-fx.js` own media,
  audio contexts, transient animation nodes, and their one-way event buses.

Treat these as opaque children: components may mount a stable host and publish
inputs, but must not reconcile the integration's descendants. Lifetimes are
explicit: xterm pane close and graph/chart disposal clean up feature instances,
while the Vegas/slop/wizard binders are installed once for the document lifetime
and do not participate in island cleanup. Moving one behind a component requires
an explicit lifecycle adapter and behavioural coverage for cleanup, focus/input,
and reconnect or animation state.

### Imperative boundary markers

Preact is the default owner for operator-facing markup, drafts, validation,
busy/error state, dialogs, lists, and forms. Static dashboard HTML may provide
an empty stable host, but it must not contain a second dialog implementation.
Snapshot polling and reconciliation must not inspect UI draft state or pause
because an editor is open. Cross-feature `data-act` routing snapshots an
immutable plain descriptor before starting an operation; DOM attributes are
not request or application state.

Imperative code remains only at the following explicit boundaries. A module
that directly creates or injects DOM carries a
`dashboard-imperative-boundary` marker naming a category; the architecture test
discovers markers rather than maintaining a filename allowlist. It accepts seven:
`browser-io`, `config-adapter`, `cost-chart`, `media-effects`, `platform-layout`,
`preact-compat` and `process-graph`. The xterm core and drag and drop rows below
are surfaces with the same contract, not markers.

| Marker / surface | Ownership | Lifetime and disposal | Behavioral test expectation |
|---|---|---|---|
| xterm terminal core | Preact owns terminal tabs, shells, and stable hosts; xterm owns only the opaque descendants handed to `terminals-core.js`. | Terminal close/unmount disposes the terminal, addons, socket, resize observer, and listeners. Reconciliation may retain or retire a host but never rebuild xterm children. | Mount/close/pop-out and roster reconciliation tests must prove opaque-node identity and teardown. |
| `process-graph` | Preact owns editor/dialog state; `process-graph.js` and `process-graph-adapter.js` exclusively own their SVG/canvas-like host. The connector-drop `process-node-chooser.js` owns its anchored combobox/listbox subtree. | The adapter removes pointer/key listeners, connection bands, and graph instances on keyed replacement/unmount. Closing or disposing the chooser removes its document listener and subtree; cancellation restores focus, while unmount disposal leaves focus to the next owner. | Graph interaction tests cover selection, drag/connect, rerender identity, and disposal; chooser tests cover selection, cancellation, focus, click-away, and idempotent disposal. |
| `cost-chart` | Preact owns filters, data derivation, and the chart host; `costs-chart.js` owns the chart's drawing nodes only. | Each effect clears/replaces the chart host and returns cleanup before the next draw or unmount. | Costs tests cover filtered redraw, empty/error states, and chart cleanup without asserting incidental node layout. |
| drag and drop | Preact emits live keyed producers and semantic `data-*` descriptors; `dnd.js`, `group-reorder.js`, `dock-dnd.js`, and `dock-save-dnd.js` adapt native `DataTransfer` events. | Every binder is idempotent, returns cleanup, resets gesture state on `dragend`/unmount, and rejects detached producers. | DnD tests cover copy/move/delete intent, cancellation, cleanup, and live-source guards. |
| `media-effects` | The Vegas audio player and slop/wizard cosmetic modules are boot-time, page-lifetime effects that own media elements, particles, and the explicitly opaque reel host. `drop-landing.js` owns the fixed `#drop-landing-layer` and the blip / in-transit overlays it draws over a drop's landing place. They do not own operational state. | Their delegated document listeners intentionally live until navigation and are not `pageCleanups`. Within that page lifetime, transient nodes self-remove, Vegas stops audio and polling timers when inactive, and identity tokens prevent stale timers from overwriting a newer Preact host. | Reduced-motion, stale-timer, inactive-audio, and opaque reel hand-back tests are required. |
| `platform-layout` | Scoped browser effects own focus traps, resize, horizontal scroll, navigation history, overlay stacking, and stable shell/dock re-homing. The dashboard profile controls are Preact-owned inside named stable hosts; the hosts move between the toolbar and dock while each chip can turn into an inline picker. `island-lifecycle.js` owns only its claimed host's load-failure alert. `peer-view-limits.js` owns one generated `<style>` in the head that greys the controls a peer view refuses. `process-editor-place.js` owns the process editor's mount, which it re-homes between the Processes tab's slot and an overlay tab's slot with the live editor in it, and the open-failure alert in that mount. | Effects attach to refs/stable shell nodes and return cleanup. Focus returns to the remounted chip after keyboard cancellation. Island failure rendering replaces only the claimed host; successful cleanup releases host ownership. The peer-view style is recomputed with the snapshot and removed by the limits' disposer. The process editor's mount is destroyed with its editor when its last place lets it go: the docked tab's unmount, or its overlay tab closing for good. | Binder lifecycle, inline-picker focus, overlay stack, dock identity, refresh-generation, and island rollback/ownership tests cover these contracts; peer-view limit tests cover the generated rule and its removal. Process editor placement tests cover the hand-over, re-adoption without reopening, and teardown on close. |
| `browser-io` | Operation modules may create a short-lived download anchor, clipboard textarea fallback, or standalone Preact host solely to invoke a browser API. `xterm-loader.js` owns the non-visual script node used to fetch the classic xterm runtime on first valid terminal intent. | Temporary operation nodes and object URLs are removed/revoked in the same operation; no draft or request state is stored on them. The xterm script and installed global intentionally live for the page lifetime, while an in-flight promise deduplicates concurrent opens and a failed load is retryable. An auth-aware HEAD preflight preserves the dashboard's expired-session redirect before script injection. | Payload/download/clipboard tests assert the invoked browser contract and cleanup. The xterm loader test proves imports cause no fetch, auth failure causes no injection, concurrent requests append one script, and a ready runtime is reused; facade tests prove canceled/invalid requests do not prepare it. |
| `config-adapter` | `config-form-adapter.js` is a bounded adapter for server-described native controls embedded in Preact-owned config surfaces. | Activation is generation-guarded; option/list replacement is scoped to the supplied control, and the Config island owns activation cleanup. | Config activation and retry tests must prove stale loads cannot publish into a replacement control. |
| `preact-compat` | Preact remains the visual owner. This marker covers trusted legacy readback HTML and standalone process-dialog wrappers used outside the main editor tree, not permission to build new imperative UI. | Injected markup is escaped/trusted at its model boundary; standalone hosts unmount Preact and remove themselves on completion. | Readback escaping and standalone-dialog close/disposal tests are required. New uses need explicit review and documentation here. |

One surface sits just inside that allowance and is worth naming, because it
reads at a glance like an exception: `menu-filter.js`, the type-to-filter core
behind the ⚙ cog menus, decides which items a query keeps by **reading the
rendered menu** rather than from data. It is not new imperative UI — it creates
no nodes, and Preact still owns the box, the query and every item — but it does
mark items on a Preact-rendered subtree. Two properties keep that safe. Its
four `data-menu-*` attributes are declared by no vnode, so Preact never diffs
them and a snapshot publish cannot fight them; and `ActionMenu` re-applies the
filter after every render, so items that appear or disappear with the snapshot
are re-evaluated. Reading the DOM is also the only complete option here: several
items (`NotifyMenuItem`, `RemoteMenuItem`, `RestartMenuItem`,
`SandboxRestartMenuItem`) compute their own label inside the component from live
member state, so no call site knows what they say — and matching the rendered
text means the filter cannot drift from the labels the operator can see.

The guard intentionally allows ordinary ref-based effects (`focus`, measure,
scroll, browser APIs) and rejects undocumented DOM ownership. If a new surface
needs imperative ownership, first prove that Preact cannot own the ordinary UI,
add a documented category/lifecycle/test contract, and then add the source
marker. Do not solve a guard failure with a compatibility re-export, static
dialog markup, or a blanket filename exception.

### Lists that span fleet nodes

A fleet view that pages one list across several nodes (the fused Messages
folders and the fused audit's "all nodes" view are the references) queries each node on demand and never copies a
peer's rows into this node's database, so revoking a peer cuts its rows off
at once. The parts:

- **Server:** the node's list endpoint is bounded and paged in SQL and accepts
  a keyset cursor, `before=<id>`: the page of rows older than that id in the
  node's own newest-first order. It reports `total` (the filtered count),
  `has_more`, and `cursor_paging: true`, so a client can tell it from an older
  build that ignores the parameter. See `mailboxWindow` in
  `pkg/claude/agentd/dashboard_mailbox.go` and `handleDashboardAudit`.
- **Client:** `dashboard/js/fleet-pager.js` turns per-node pages into one
  merged page. A page is a cursor vector (one cursor per node). Page p reads
  one page from each node below its cursor, merges the streams by taking the
  newest head each time, and keeps the first page's worth. The last row taken
  from each node becomes that node's cursor for page p+1. No row is skipped
  or shown twice, a page reads at most nodes × page size rows, and polling
  re-reads the same vector. Counts are the nodes' summed totals.
- **Navigation:** Prev uses the vector kept for the earlier page. There is no
  random access, so a deeper page (Last, or a page restored from the URL) is
  reached by walking forward one read per page (`walkTarget`). Fold each
  merged page into the walk with `recordVector`: it rewrites the next vector
  when the page's boundary moves (new rows on page 1, deletions), and restarts
  the walk when the set of unreachable nodes changes, because a node that was
  down has no cursor in the vectors recorded meanwhile.
- **Mixed versions:** a node without `cursor_paging` is read newest-first to
  the depth the page needs and cut at the cursor client-side
  (`legacyStream`), which stays exact down to the server's largest page.

- **Engine:** `createMergedList` in `fleet-pager.js` does all of the above
  for a list read imperatively (the fused audit view uses it). Give it
  `readPage(node, { before, depth }, ctx)` for one node's page and a refusal
  wording, and it walks, merges, and applies the fleet's liveness rules:
  - A node reported offline is not asked, and it shows as offline, not as
    an error.
  - A node that checks in as online is asked on every refresh. It is only
    given up on when its read fails (a peer read times out after 15s), and
    the message says so.
  - A slow node does not hold up a newly opened page: after
    `SLOW_NODE_MS` the page shows the nodes that answered and names the
    ones still awaited. A refresh keeps the page on screen whole.
  - A walk steps past nodes that just failed, so a silent node costs one
    wait rather than one per page.
- **Pager:** render the footer with `Pager` from `pager.js` (the `pages`
  layout); `stepping` names the page a forward walk is heading for.

Reuse these for any other merged list rather than reading every node down
to the page shown.

## Writing flow tests

Flow tests in `pkg/claude/agentd/*_flow_test.go` are regular Go tests
— they run under bare `go test ./...`. Boundaries (`tmux`, the
`tclaude session new` subprocess) are mocked by assigning fake
implementations to package-wide interface vars (`clcommon.Default`,
`agentd.Spawn`) at test setup, with `t.Cleanup` restoring the
production singletons. No toolchain dependency, no build tag, no
wrapper script.

Flow tests live next to the code they exercise (e.g. `pkg/claude/
agentd/spawn_flow_test.go`) and use the
[`pkg/testharness`](pkg/testharness) DSL. A scenario reads as
Given / When / Then:

```go
package agentd_test

func TestSpawn_RenamesAndResumes(t *testing.T) {
    f := newFlow(t)

    f.HaveGroup("alpha")

    spawn := f.AsHuman().Spawn("alpha", "worker")

    f.AssertSentContains(spawn.TmuxTarget(), "/rename worker", 5*time.Second)
    f.AssertGroupMember("alpha", spawn.ConvID, "worker", 5*time.Second)

    f.MarkOffline(spawn.TmuxSession)
    resume := f.AsHuman().Resume(spawn.ConvID)
    f.AssertResumeSpawned(resume)
}
```

`newFlow(t)` lives in `pkg/claude/agentd/flow_setup_test.go` and
swaps `clcommon.Default` (Tmux interface) and `agentd.Spawn`
(Spawner interface) for simulator-backed fakes. Scenarios that need
to override further can shadow with another assignment after
`newFlow` returns; the original cleanup still runs and restores the
production singletons.

When adding a new scenario:

1. Pick a verb on `Flow` that captures the action — `Spawn`,
   `Resume`, `Reincarnate`, `Clone`, `Delete` are there. New verbs
   go in `pkg/testharness/flow.go`.
2. Pin the bug class in the test name + a comment paragraph at the
   top — what coordination this guards, what the regression would
   look like.
3. Keep the body short. If a scenario needs more than ~20 lines of
   imperative setup, the harness probably wants a new `Have*`
   helper.
4. Assert at real surfaces (e.g. `GET /v1/groups/{name}/members`,
   `conv.ListSessions`) — not at the simulator's `.jsonl`. The
   simulator writes the file so the real production read path has
   something realistic to walk; the test verifies the surface.
5. When a new Claude Code or tmux quirk bites in production, encode
   it in the simulator (`cc.OnInput` for behavior,
   `cc.SetCommandDelay` for timing) so the regression fails the
   relevant flow test. Over time the sims accrete the institutional
   knowledge of "things that have surprised us".

### Shrinking production timings

Flow fixtures shrink production waits so a scenario costs milliseconds instead
of seconds. Shrinking two related timings *proportionally* is the trap: it
preserves their ratio and discards their absolute separation, and scheduler
jitter is absolute.

TCL-1028 is the worked example. A soft stop runs two concurrent actors with
nothing ordering them: the bounded re-injection ladder spends its attempts by
~8 s, and the escalation watchdog's deadline is 10 s — 2 s of separation that no
amount of runner load can close. The fixture scaled both down, to 1 ms retries
against a 20 ms deadline. Same ratio, and now a single 20 ms deschedule reorders
them. Two scenarios in `soft_exit_retry_flow_test.go` failed intermittently on
loaded macOS runners — in *opposite* directions, over-running and under-running
the same attempt count — while passing 40/40 on an idle Linux box. The fixture
had not modelled production faster; it had moved the system into a regime where
two actors production never races can be reordered by nothing more than a busy
runner.

So before shrinking a wait, ask which other actor it is separated from, and
whether the shrunken separation is still large next to a scheduler pause (tens
of milliseconds on a loaded CI runner). When it is not, do not go looking for a
bigger margin — order the two actors, per "Deterministic timing in tests" above:
park one on a hook, wait for observable state proving the other reached the
point under test, then release. A margin that needs tuning is the symptom; the
missing order is the bug.

## Code conventions

- Focused single-topic commits. See `git log` for the style:
  `agent: …`, `db: …`, `test: …`, `docs: …`.
- HEREDOC commit messages.
- Co-Authored-By trailer on AI-assisted commits — but **no session /
  remote-access links** (`Claude-Session:` trailers,
  `https://claude.ai/code/...` URLs) in commit messages or PR
  descriptions, even if the coding harness's default instructions ask
  for them.
- Make sure your changes at least build (and pass any focused tests
  you relied on) before pushing, but don't duplicate the full
  test/lint suite locally — push early and let CI run:
  ```bash
  go test ./...
  golangci-lint run ./...
  ```
- Don't add comments that restate what the code does. Add WHY
  comments only when the reason is non-obvious (a hidden
  constraint, a workaround for a specific bug, or behaviour that
  would surprise a reader).
- Don't `git add -A` — some dotfiles in the repo root are mounted
  as char devices in dev sandboxes and break the add. Stage by
  path.

## Layout

High-level:

- `pkg/claude/` — main packages (session, conv, agent, agentd, etc.).
- `pkg/claude/common/` — shared utilities (config, db, tmux,
  notify, etc.).
- `pkg/testharness/` — flow-test DSL (CCSim + TmuxSim + Given/When/Then).
- `pkg/common/` — generic utilities (dirs, locking, size parsing).

For feature-level architecture, prefer the focused docs under `docs/` and the
package comments near the code. `CLAUDE.md` / `AGENTS.md` is intentionally only
agent startup context, not a complete architecture inventory.
