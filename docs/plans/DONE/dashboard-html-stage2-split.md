# Dashboard refactor — Stage 2: native ES modules for `dashboard.js` (SHIPPED)

Stage 2 converted the agentd browser dashboard's monolithic
`dashboard.js` — one ~8,130-line IIFE — into a set of **native ES
modules**: real `import`/`export`, one module per feature, served to the
browser as `<script type="module">`. No bundler, no build step — the
browser resolves the import graph itself.

## What shipped

The three flat Stage-1 sibling files (`dashboard.html` / `dashboard.css`
/ `dashboard.js`) became a `pkg/claude/agentd/dashboard/` directory:

```
pkg/claude/agentd/dashboard/
├── dashboard.html         served at "/" verbatim (no assembly)
├── dashboard.css          served at /static/dashboard.css via <link>
└── js/                    served under /static/js/, all as <script type="module">
    ├── dashboard.js        entrypoint — the import block + boot sequence
    ├── helpers.js          $, esc, status dots/pills, context meter, cells, relTime
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
    └── config.js           Config tab — visual editor for ~/.tclaude/config.json
```

## PR sequence

Planned in PR #153. Delivered as one rewrite split into reviewable PRs:

- **#157** — the cutover: moved the three files into `dashboard/`,
  stripped the IIFE wrapper (leaving `dashboard.js` as one entrypoint
  module), switched `dashboard.html` to `<link rel="stylesheet">` +
  `<script type="module">`, embedded the directory in `dashboard.go`,
  added the auth-gated `/static/` route, deleted `assembleDashboardHTML()`
  and the `<style>`/`<script>` splice.
- **#158** — `helpers.js`.
- **#159** — `sort.js` + `virtual-groups.js`.
- **#163** — `render.js`.
- **#164** — `tabs.js`.
- **#166** — the five `modal-*.js`.
- **#168** — `refresh.js` + `row-actions.js` + `dnd.js`. Got a real
  CodeRabbit review; four error-handling fixes folded into the merge
  (rejected-`fetch()` try/catch in `addOne`/`resumeAgentReq`/
  `stopAgentReq`, a stale-response `active`-flag guard in the
  worktree-delete modal, a deferred `refresh()` in the cleanup modal),
  one false positive skipped (optional chaining short-circuits the whole
  chain — no TypeError on a missing clipboard).
- **#170** — `config.js`.
- **the final de-indent PR** — through every extraction PR the leftover
  body of `dashboard.js` kept the old IIFE-body 2-space indent on
  purpose, so each intermediate diff stayed a clean extraction diff.
  This PR de-indented that residual + boot to column 0; `dashboard.js`
  is now a flat entrypoint = the import block + the §6 shared-state
  residual + the boot sequence.

## Shared mutable state (plan §6)

ES-module imported bindings are read-only in the importer, so a `let`
must live in the module that *assigns* it. Most module-scope `let`s were
private to one feature and moved with it. The cross-cutting exceptions:

- `lastSnapshot` — two writers (`refresh()` in refresh.js, the
  rename-rollback in row-actions.js). It stays declared in
  `dashboard.js`, exported alongside a `setLastSnapshot()` setter that
  both writers route through.
- `sudoByConv` / `sudoGrantBlocklist` → refresh.js, `renameEditing` →
  row-actions.js, `dndDragActive` → dnd.js — each lives with its
  assigner; other modules import it read-only.

A deliberate, benign `import` cycle exists: a mid-graph module that needs
a not-yet-extracted symbol imports it back from `./dashboard.js`. Safe
because the shared bindings (hoisted functions, read-only live `let`s)
are touched only at call-time, never at module top level.

## Constraints

Stage 2 lifted exactly one constraint — "no ES modules" — by the human's
2026-05-17 decision. No-bundler, build-step-free, and no-`html/template`
all still hold. Byte-identical served output was explicitly *not* a
goal; the Stage-1 SHA-256 pin (`TestDashboardHTML_SplitIsByteIdentical`,
`TestDashboardHTML_AssemblySpliceIsClean`) was retired with the splice.

## Guards that replaced the pin

- `dashboard_assets_test.go` — `TestDashboardEmbed_HasExpectedFiles`
  asserts the embed.FS carries the expected files and a non-empty
  `js/*.js` set; `TestDashboardHTML_ReferencesStaticAssets` pins that
  `dashboard.html` loads the stylesheet and the entrypoint module by
  absolute `/static/` path and that the Stage-1 inline splice points are
  gone.
- Per-extraction verification (the pattern proven over PRs 2–8):
  forced-module-goal `node --check --input-type=module` on every
  `dashboard/js/*.js` (plain `node --check` misses ESM syntax errors); a
  Node module-graph `import()` resolution check (reaching
  `document is not defined` from boot = the graph links); a
  byte-faithfulness `diff` of each extracted body vs the de-indented
  original; a dependency cross-ref (zero missing / zero unused imports);
  `go build ./...` + `go test ./...` + `golangci-lint`.
- Editor / language-service diagnostics — the real payoff: every module
  is independently valid and lintable, with genuine cross-file
  go-to-definition, find-references, and missing-/unused-export
  detection. The IIFE never offered this.

## Relevant source files

- `pkg/claude/agentd/dashboard/` — the `dashboard.html` + `dashboard.css`
  + `js/` module set.
- `pkg/claude/agentd/dashboard.go` — `//go:embed dashboard`, the
  auth-gated `/static/` route, `handleDashboardRoot`.
- `pkg/claude/agentd/dashboard_assets_test.go` — the served-shape Go
  guards (replaced the retired `dashboard_split_test.go` SHA pin).
