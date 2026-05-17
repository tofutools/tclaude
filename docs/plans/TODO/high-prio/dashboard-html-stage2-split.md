# Dashboard refactor — Stage 2: native ES modules for `dashboard.js`

## The plan (committed)

**What.** `dashboard.js` is one 8,130-line IIFE — the last big lump of the
agentd dashboard. Stage 2 converts it into **native ES modules**: real
`import`/`export`, one module per feature, served to the browser as
`<script type="module">`.

**Why ES modules.** They are the only option that yields independently valid,
individually lintable, navigable files — real cross-file go-to-definition and
import/export checking. Two alternatives — mechanical line-range fragmentation,
and whole-component HTML+CSS+JS splitting — were considered and rejected (see
the knowledge pool). Browsers run ES modules natively, so this needs **no
bundler and no build step**.

**Decided and committed:**

- Stage 2 = the native-ESM rewrite of `dashboard.js`. It is a deliberate,
  byte-changing rewrite — **byte-identical output is explicitly not a goal**;
  functional correctness is the bar. Stage 1's SHA-256 pin is retired.
- The constraints **no bundler / build-step-free / no `html/template`** still
  hold; the old **no-ES-modules** constraint is lifted — the one removed.
- Target shape: `dashboard.html` + `dashboard.css` + a `js/` module set, served
  by agentd as static assets behind the existing dashboard auth; the
  `assembleDashboardHTML()` splice goes away.

**Sequencing.** Stage 2 is **gated behind the agents-tab removal PR** — it
starts once that lands. Expect to deliver it as **~5–7 reviewable PRs** (a
cutover PR, then grouped module-extraction PRs).

**Status.** In progress. The agents-tab-removal gate (#154) has landed; Stage 2
PR 1 — the ESM cutover — shipped as #157; PR 2 — `helpers.js` extraction —
shipped as #158; PR 3 — `sort.js` + `virtual-groups.js` — is in progress. The
committed plan above still holds; everything below is the original *research
and considerations*, not locked-in detail, and has expected drift from how the
work is actually landing.

## Considerations & research (knowledge pool)

The sections below are the research behind the committed plan above:
background, the option analysis, an architecture sketch, the genuinely hard
part, testing ideas, a delivery breakdown, and things to settle during
implementation. They are **considered and worth knowing — not locked-in
decisions.** Specifics here (file names, exact PR slicing, route paths, test
mechanics) are expected to shift as the work proceeds; treat this as a
well-explored starting point and a record of what was already thought through,
not a spec to follow literally.

### 1. Background — Stage 1, and where `dashboard.js` stands

`pkg/claude/agentd/dashboard.html` — the agentd browser dashboard — had grown
into a single file of **10,303 lines** (verified at commit `31dfeaa`, its last
single-file state), ~79% of it inline JavaScript. A staged refactor is making
it maintainable.

**Stage 1 shipped** (PR #152, commit `2b20159`). The inline `<style>` and
`<script>` bodies were extracted from `dashboard.html` into sibling files
`dashboard.css` and `dashboard.js`. All three are `//go:embed`'d;
`assembleDashboardHTML()` splices the CSS and JS back into the markup shell's
empty `<style></style>` / `<script></script>` placeholders at package init.
That was a *pure mechanical relocation*: a SHA-256 pin test
(`TestDashboardHTML_SplitIsByteIdentical`) asserts the assembled bytes equal
the pre-split `dashboard.html` exactly.

**What remains:** `dashboard.js` is still one file — **8,130 lines**, ~295
top-level declarations, all inside a single IIFE (`(function() { … })()`).
`dashboard.css` (1,167 lines) and the markup shell `dashboard.html` (1,006
lines) are comparatively fine.

Current shape of `dashboard.js`, by natural section — these are the likely
module boundaries (line ranges at commit `2b20159`; **note** the agents-tab
removal lands first and will shift these and drop one cluster, so re-derive the
ranges at execution time):

| Lines        | Section → likely module                                               |
|--------------|-----------------------------------------------------------------------|
| 1–436        | shared helpers — `$`/`esc`, status dots & pills, context meter, cells, `relTime`, offline toggles |
| 437–617      | sorting + column/accessor tables (`MEMBER_COLS`, `AGENT_COLS`, …)      |
| 619–722      | virtual groups (Ungrouped / Conversations / Retired)                  |
| 724–1276     | rendering — `memberRowHTML`, `renderGroups`, `renderAgents`, messages, usage |
| 1278–1673    | snapshot state, tab filters, `render*Tab`, cron/sudo/links tables      |
| 1674–4574    | modals — sudo, cron + target picker, message, perm, group create, templates, group import/context, link, worktree picker, spawn/clone/reincarnate/rename |
| 4575–6227    | `refresh()`, tab/copy/sort bindings, confirm/window/emergency/cleanup modals, toast |
| 6228–7021    | `bindRowActions()` — one ~800-line delegated click router             |
| 7022–7599    | drag-and-drop (`bindDnd` + `runDnd*`)                                  |
| 7600–8100    | Config tab                                                             |
| 8101–8130    | boot — the `bind*()` / `refresh()` / `setInterval` calls, IIFE close   |

### 2. Why ES modules — the option analysis

*This plan originally recommended option (a) below; the human reviewed it and
redirected to (c) on 2026-05-17. The (a)/(b)/(c) record is kept as the
rationale.*

Three directions were on the table:

- **(a) Mechanical fragmentation** — split `dashboard.js` into line-range
  fragment files, `//go:embed` them, concatenate back into one byte-identical
  blob behind a SHA pin. *Safe* (purely mechanical, same discipline as Stage 1)
  but the tooling win is only partial: every fragment is a slice of one IIFE
  body, not a standalone valid module — per-file linting stays broken and
  cross-file navigation is incidental, not real.
- **(b) Whole components (HTML+CSS+JS together)** — *unsound mechanically*:
  `dashboard.css` is a flat 687-rule global stylesheet whose cascade is
  source-order-sensitive, so regrouping rules by component is a potential
  rendering change, not a pinnable byte change. And under the old no-modules
  constraint (b)'s JS half collapsed into exactly (a)'s outcome anyway.
- **(c) Native ES modules** — real `import`/`export`, one module per feature.
  Browsers run ES modules **natively**: no bundler and no build step are
  required (only the project's own past *constraint* forbade them — that
  constraint is now lifted, §4). This is the only option that yields genuinely
  independent, individually-valid, individually-lintable files with real
  cross-file go-to-definition and import/export checking.

(c) was chosen with eyes open about the cost: it is *not* a mechanical
relocation. The page's served bytes change, the Stage-1 byte-identity SHA pin
is retired, and agentd grows a static-asset route. Those costs are accepted
deliberately because (c) is the only option that actually delivers the
maintainability goal the whole refactor exists for. Options (a) and (b) are
dropped.

### 3. Byte-identity, and what changes from Stage 1's safety model

Stage 1's rule was "every *mechanical* stage produces byte-identical served
output," guarded by a SHA-256 pin. Stage 2 is **not a mechanical stage** — it
is the deliberate, openly-reviewed, byte-changing rewrite. That is not a
violation of the discipline; the discipline always allowed byte-changing steps
*as long as they are explicit and separately reviewed* rather than smuggled
into a "mechanical split." Stage 2 is exactly that explicit step.

The human has also confirmed (2026-05-17) that **byte-identical refactor output
is not a goal they value** — functional correctness is the bar. So retiring the
pin gives up nothing the project actually depended on; it was a convenience, not
a requirement.

Concretely:

- `assembleDashboardHTML()` and the `<style></style>`/`<script></script>`
  splice are **removed**. The dashboard is served as ordinary static assets.
- `TestDashboardHTML_SplitIsByteIdentical` and
  `TestDashboardHTML_AssemblySpliceIsClean` (in `dashboard_split_test.go`) are
  **retired** — there is no longer a single assembled blob to pin.
- New guards replace them — see §7.

### 4. Constraints

| Constraint           | Stage 2 status                                                  |
|----------------------|-----------------------------------------------------------------|
| No ES modules        | **LIFTED** — by the human's 2026-05-17 decision. This is the one constraint Stage 2 removes, and it is removed deliberately and on the record. |
| No bundler           | **Holds.** Native ESM needs no webpack/esbuild/rollup — the browser resolves `import` graphs itself. |
| Build-step-free      | **Holds.** No compile/transpile/concat step. Go `//go:embed`s the module files and serves them as-is; the browser loads them natively. |
| No `html/template`   | **Holds** — more so. With assembly gone, `dashboard.html` is served verbatim. |

So Stage 2 lifts exactly one constraint, the one that was blocking the real
fix, and keeps the other three intact.

### 5. Target architecture — a worked sketch

*One worked-out shape, to show the approach hangs together. File names, the
route path, and the exact split are expected to change during implementation.*

#### File layout

A `dashboard/` directory replaces the three flat sibling files:

```
pkg/claude/agentd/dashboard/
├── dashboard.html          served verbatim at "/"
├── dashboard.css           served at "/static/dashboard.css" via <link>
└── js/
    ├── dashboard.js        entrypoint module — imports the rest, runs boot
    ├── helpers.js          $, esc, dots, pills, context meter, cells, relTime
    ├── sort.js             sort state + column/accessor tables
    ├── virtual-groups.js   Ungrouped / Conversations / Retired
    ├── render.js           row/group/agent/message/usage rendering
    ├── tabs.js             snapshot state, filters, render*Tab, cron/sudo/links tables
    ├── modal-cron.js       sudo-grant + cron modal + target picker
    ├── modal-message.js    message + sudo + perm-edit + group-create modals
    ├── modal-templates.js  templates UI + group import + group context
    ├── modal-link-wt.js    link modal + worktree picker
    ├── modal-spawn.js      spawn / clone / reincarnate / rename modals
    ├── refresh.js          refresh(), tab/copy/sort binds, confirm/window/cleanup modals, toast
    ├── row-actions.js      bindRowActions() delegated click router
    ├── dnd.js              drag-and-drop
    ├── config.js           Config tab
    └── state.js            shared mutable state (only if §6 needs it)
```

`dashboard.html` is `//go:embed`-served as a real document:

```html
<link rel="stylesheet" href="static/dashboard.css">
...
<script type="module" src="static/js/dashboard.js"></script>
```

(Relative paths resolve against `/`; a module's own `import './helpers.js'`
resolves against `/static/js/`.)

#### Serving — agentd

`dashboard.go` changes:

- `//go:embed dashboard` → an `embed.FS`; `fs.Sub` to root it at `dashboard/`.
- `handleDashboardRoot` keeps its job — token consumption, the session cookie,
  auth — and now serves `dashboard.html` *read from the embed.FS* (no
  assembly).
- A new static route, e.g. `/static/`, serves the CSS and JS modules from the
  embed.FS via `http.FileServerFS`, **wrapped in `checkDashboardAuth`** (cookie
  check — same gate as `/api/*`). `http.FileServerFS` sets the `Content-Type`
  from the extension; this matters — **browsers refuse to execute a module
  served as `text/plain`**; `.js` must be `text/javascript`. Modern Go's mime
  table does this correctly, but a test should assert it (§7).
- Auth flows fine: the first navigation to `/` is a same-site top-level GET, so
  the `SameSite=Strict` cookie is set; every subsequent module fetch is a
  same-origin subresource request and carries that cookie automatically. A
  module URL hit cold (no cookie) gets rejected — same protection as today.
- **Cache invalidation — decide in PR 1.** `embed.FS` files carry a zero
  modtime, so `http.FileServerFS` emits no `Last-Modified`/`ETag` validators.
  Without them a browser may heuristically cache a module and, after an agentd
  upgrade, run *stale* module JS — worse with a module graph, where versions
  can mismatch across files. Fix: set `Cache-Control: no-store` (or
  `no-cache`) on the static responses — for a localhost tool the cost is nil
  and it is robust. Versioned URLs (`?v=<build id>`) are the alternative if
  caching is ever wanted.
- `assembleDashboardHTML()`, `dashboardShellHTML`/`dashboardCSS`/`dashboardJS`
  string vars, and the splice are deleted.

#### The IIFE → module conversion

An ES module already *has* its own private scope — that is what the IIFE was
faking. So the wrapper `(function() {` / `})();` is simply deleted; the body
becomes the module body. Modules are also implicitly **strict mode**: if the
current sloppy-mode IIFE relies on anything strict mode forbids (an accidental
global, a duplicate parameter name, octal literal, …) it will throw once it is
a module. PR 1 (§8) must verify the dashboard still works after the wrapper
strip — most likely it is already clean, but this is the one place a "just
delete the wrapper" assumption can bite.

### 6. The genuinely hard part — shared mutable state

This is the part that makes Stage 2 genuinely *not* a line-move, and the part
the executor must design rather than mechanically apply.

Inside today's single IIFE, ~30 `let`/`var` module-scope variables are read
*and written* from many functions scattered across the file — e.g. `sortState`,
`lastSnapshot`, `renameEditing`, `sudoGrantBlocklist`, `sudoByConv`,
`cronEditId`, `cronOriginalTarget`, `cronOriginalGroupID`, `targetPickerScopes`,
`messageScopedGroup`, `permEditConv`, `templateEditorEditing`,
`templateEditorAgents`, `giInspectSeq`, `giLastInspection`, `giAsDebounce`,
`groupContextModalGroup`, `linkModalState`, `lastSpawnCwdPrefill`,
`spawnWtRepoEdited`, `configObj`, `configBaseRaw`, `configLoaded`,
`configFileMalformed`, `toastTimer`, `dndDragActive`, `dndSource*`.

ES modules give **live, read-only bindings**: an importing module sees the
current value of an imported variable but **cannot assign to it**. So a `let`
cannot simply be `export`ed and mutated from elsewhere. Each piece of mutable
state needs a deliberate home:

1. **Co-locate state with its mutators.** Most of these `let`s are private to
   one feature (`cronEditId` ↔ the cron modal, `configObj` ↔ the Config tab).
   Put the variable in that feature's module; nothing else touches it. This
   covers the large majority.
2. **For genuinely cross-cutting state**, expose an explicit setter from the
   owning module (`export function setSortState(v){…}`) — or, if several such
   vars exist, gather them in `state.js` with getter/setter pairs. `lastSnapshot`
   and `sortState` are the likely candidates.

Each extraction PR (§8) makes this call for the state it touches. This is
design judgement, not mechanism — and it is why Stage 2 needs real review per
PR, not just a green pin.

A related note: cyclic `import`s between modules are fine for **functions**
(hoisted, live bindings) but can hit a temporal-dead-zone error if module A
*uses an imported `const` at its own top level* before module B has
initialized. Today's top-level `const`s (`CTX_SEGMENTS`, `MEMBER_COLS`, …) are
all consumed *inside* functions called later, so this is not expected to bite —
but extract leaf modules (helpers, constants) first so their bindings
initialize before dependents, and keep top-level code to the entrypoint's boot
sequence.

### 7. Testing & guards — approach

The byte-identity pin is gone (§3). Replacements, in order of value:

1. **Editor / language-service diagnostics — a *new* net ESM gives us.** Real
   `import`/`export` means the JS language service resolves the graph: a typo'd
   import, a missing `export`, an unused export, an undefined symbol are all
   flagged *at edit time*. The IIFE never offered this. A `jsconfig.json` in
   `dashboard/js/` makes it explicit. This is the single biggest correctness
   gain and the main point of the whole exercise.
2. **A CI syntax check.** `node --check` on each module file catches syntax
   errors cheaply. GitHub-hosted runners ship Node, so this adds no install
   step. (Whether to add a Node-touching CI step to an otherwise pure-Go
   pipeline is one of the §11 implementation calls.) `node --check` validates
   syntax only — it does not catch a missing `export` (that is a runtime
   `undefined`); (1) and (3) cover that.
3. **Manual dashboard verification per PR.** Each extraction PR keeps the
   dashboard fully working; the reviewer/author loads it and exercises the
   touched feature. ESM fails *loudly* — a bad import is an immediate console
   error — so regressions surface fast.
4. **A Go test for the served shape.** Assert the embed.FS contains the
   expected files, that `dashboard.html` references the entrypoint module and
   the stylesheet, and that the static handler serves `.js` with a
   JavaScript MIME type and `.css` with `text/css`. This guards the *plumbing*
   (the part Go owns) without trying to validate JS semantics.

There is no pretending this matches the SHA pin's bit-exactness — it cannot,
because Stage 2 deliberately changes the bytes. The honest trade is: lose a
bit-exact pin on a frozen blob, gain real static analysis on living modules.

### 8. Delivery — a PR-breakdown sketch

*Committed: ~5–7 reviewable PRs. The exact slicing below is a sketch — group
modules however keeps each PR reviewable at execution time.* Stage 2 is **one
rewrite**, not a sequence of refactor stages; the breakdown is only how that
single rewrite is split for review. Every PR leaves a working dashboard.

**PR 1 — the cutover.** Move the three files into `dashboard/`. Strip the IIFE
wrapper from `dashboard.js`, leaving it as one entrypoint module (no `import`s
yet). Switch `dashboard.html` to `<link rel="stylesheet">` + `<script
type="module" src="…">`. In `dashboard.go`: embed the directory, add the
auth-gated `/static/` route, serve `dashboard.html` from the embed.FS, delete
`assembleDashboardHTML()` and the splice. Retire the two SHA/splice tests; add
the §7.4 Go test (and, if adopted, the §7.2 CI step). The JS *body* is
untouched apart from the two deleted wrapper lines, so this PR is mostly
HTML + Go + tests. After it, the dashboard runs as one ES module.

**PRs 2–N — module extraction.** Lift the §1 clusters into their own modules,
in dependency order (leaves first): move the code, `export` the public
surface, `import` it where used, and apply the §6 state decision for any
mutable state. With byte-identity no longer a gate, these need not be tiny —
**group them into a handful of coherent PRs**, sized so each stays reviewable,
e.g.:

- helpers + sort + virtual-groups
- render + tabs
- the five `modal-*.js`
- refresh + row-actions + dnd
- config

~4–6 PRs; finer is fine if a reviewer prefers it.

**Final state.** `dashboard.js` becomes the entrypoint: the `import`s plus the
boot sequence (`bindTabs()`, `refresh()`, `setInterval`, …). Optionally rename
it `main.js` (cosmetic; keep the HTML `src` in sync).

So Stage 2 lands in roughly **5–7 PRs total**. They are not mechanical — each
carries export/import design and the §6 state decisions — so each wants genuine
review, not just a green check.

### 9. The editor-tooling payoff

This is the unambiguous win. Stage 1 gave `dashboard.js` JS tooling *as a
file*. Stage 2 (c) gives every feature its own **independently valid,
independently lintable** module: linters run per file with no IIFE-slice
noise; the language service resolves `import`s for real cross-file
go-to-definition, find-references, and missing-/unused-export detection;
files are ~200–800 lines each. Options (a) and (b) could not deliver this —
(a)'s fragments were never valid standalone files, and (b)'s JS collapsed into
(a). ES modules are the only option that makes the tooling goal real, which is
why it is the chosen path.

### 10. Coordination with other in-flight work

**Agents-tab removal — the gate (landed).** Stage 2 was gated on the agents-tab
removal PR; that landed as #154. It removed UI from `dashboard.html` and code
from `dashboard.js`, so the §1 line ranges are pre-removal — re-derive them
against the current `dashboard.js` rather than trusting the numbers here.

Two further items touch these files but do **not** block Stage 2; sequencing
just avoids rebase churn:

**Notification-setting follow-up (not yet in flight)** — an opt-in desktop
notification for new human messages, likely a Config-tab toggle. It touches
`dashboard.html` (a control in `<section id="tab-config">`) and the Config-tab
JS, which Stage 2 extracts into `config.js`. Recommendation: land PR 1 (the
cutover) first so the feature is written against the new static-asset layout,
then extract `config.js` early so the feature has a stable module target — or
let the feature land against the entrypoint module and the extraction moves
slightly more code. Either works; just do not combine the feature with an
extraction PR.

**CSS-tidy: deprecated `word-break: break-word`** — CodeRabbit flagged on #152
that `dashboard.css` uses `word-break: break-word` (deprecated; modern spelling
`overflow-wrap: anywhere`), 4×. It was left alone on #152 to preserve
byte-identity. **Stage 2 removes that obstacle entirely:** once the SHA pin is
retired (§3) and `dashboard.css` is served as a plain static file, editing
those four declarations is a trivial one-line PR with *zero* ceremony — no pin
to recompute. Recommendation: do it as its own tiny PR any time after PR 1
lands; it no longer needs to be fenced off.

### 11. Things to decide pragmatically during implementation

None of these is an up-front blocker, and none needs a human decision before
work starts. Decide each pragmatically while implementing; surface to the human
only if one turns out to actually matter.

1. **CI Node dependency.** §7.2's `node --check` step puts a non-Go tool in an
   otherwise pure-Go pipeline. Default: add it (cheap, runners already have
   Node); drop it if it ever causes friction and rely on editor diagnostics +
   the Go plumbing test + manual verification.
2. **CSS delivery.** Default: serve `dashboard.css` as a static file via
   `<link>` — it falls out naturally once a `/static/` route exists and lets
   the splice be deleted cleanly. The conservative alternative (keep CSS
   spliced, modularise only the JS) leaves a vestigial half-splice.
3. **Static route path.** `/static/` is the working assumption for the CSS + JS
   modules; `/dashboard/`, `/assets/` are equally fine. Executor's call.
4. **Entrypoint rename.** Keep the entrypoint module `dashboard.js`, or rename
   it `main.js` once it is just imports + boot. Cosmetic; decide at the end.

### Relevant source files

- `pkg/claude/agentd/dashboard.js` — 8,130 lines, the Stage 2 subject; becomes
  the `dashboard/js/` module set.
- `pkg/claude/agentd/dashboard.html`, `dashboard.css` — move into `dashboard/`;
  HTML switches to `<link>` + `<script type="module">`, served verbatim.
- `pkg/claude/agentd/dashboard.go` — `//go:embed` directives,
  `assembleDashboardHTML()` (deleted), `handleDashboardRoot`,
  `registerDashboardRoutes`; gains the auth-gated `/static/` route.
- `pkg/claude/agentd/dashboard_split_test.go` — the two SHA/splice tests are
  retired; replaced by the §7.4 served-shape Go test.
