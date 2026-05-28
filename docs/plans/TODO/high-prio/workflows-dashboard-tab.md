# workflows: dashboard "Workflows" tab (frontend)

Part of the **Workflows** feature — see `docs/plans/workflows.md`. The
monitoring surface — the headline deliverable.

## Open / to build

Vanilla ES modules, no build step. Follow the existing tab/modal/filter patterns.

1. **Nav + section** in `dashboard.html`: add `<button data-tab="workflows">
   Workflows</button>` (slot it prominently, e.g. right after Groups) and a
   matching `<section id="tab-workflows">`. Tab switching is pure CSS toggle via
   `bindTabs()` — no per-tab JS wiring needed for show/hide.
2. **Vendor `mermaid.min.js`** into `dashboard/js/vendor/` and load it (first
   diagram lib in the dashboard; local, no CDN — dashboard must work offline).
3. New `dashboard/js/workflows.js`:
   - `renderWorkflowsTab()` (called from the refresh path like `renderCronTab()`)
     reads `lastSnapshot.workflows` + `lastSnapshot.workflow_templates`, renders
     the **Templates** list (with "Instantiate") and the **Instances** list
     (status + progress N/M done), honoring `bindFilter('workflows')`.
   - Instance detail view (panel or modal): fetch `GET /api/workflows/{id}`, then
     **render the snapshotted mermaid** and apply live status.
4. **Mermaid status visualization** — the human wants colors **and overlays and
   animations**:
   - Inject `classDef <status> …` + per-node `class <id> <status>` into the
     mermaid source before `mermaid.render()`, mapping node status → class.
   - Colors: done=green, running=blue, failed=red, awaiting_verify=amber,
     skipped=grey, pending=dim. Defined in `dashboard.css`.
   - **Animations** via CSS keyframes targeting the status classes on the
     rendered SVG: `running` node pulses; the in-flight edge gets a marching
     `stroke-dasharray` animation. (Mermaid itself only supports static
     classDef styling, so animation lives in our CSS.)
   - **Overlays**: small status-badge / icons positioned absolutely over each
     node's bounding box (mermaid emits `g.node` elements with ids derived from
     node ids — read their bbox to place badges). Re-place on re-render.
   - Re-style on each 2s snapshot; only full re-render when topology/state set
     changes (cheap diff) to avoid flicker.
5. **Per-node I/O summary** — clicking a node (or its row in a node list beneath
   the chart) shows inputs (interpolated params/captures) + captured `output`,
   expandable.
6. **Context menu** per node (right-click or ⋯): *Open audit data* → fetch
   `…/nodes/{nodeId}/audit`, show the event timeline; for AI nodes *Start* / 
   *Attach* → POST the start/attach endpoints.
7. **Instantiate modal** — new `dashboard/js/modal-workflows.js`: pick template,
   enter title + params (driven by the template's declared `params`), POST
   `/api/workflows`, then `refresh()`. Wire `bindBackdropDiscard` for Escape /
   backdrop-discard with dirty-check, like `modal-cron.js`. Modal z-index 100
   (picker 150) per existing convention.
8. **Bootstrap** — import + init in `dashboard.js`; add the `renderWorkflowsTab()`
   branch in the refresh path and `bindFilter('workflows')`.
9. **Tests** — there are HTML-assertion tests for tabs
   (`dashboard_*_html_test.go`); add one asserting the Workflows tab/section and
   that instances render with status classes.

## Shipped context

- Tab registration: `dashboard.html` nav (~line 30-38) + `bindTabs()` in
  `js/refresh.js:269`.
- Poll: `/api/snapshot` every 2s (`js/dashboard.js:93`); data in `snapshotPayload`.
  No SSE in this branch.
- Tab renderer exemplar: `renderCronTab()` in `js/tabs.js:186`.
- Filter bar: `bindFilter(tab)` in `js/refresh.js:67` (add a `workflows` branch).
- Modal exemplars: `js/modal-cron.js`; `bindBackdropDiscard` in `js/refresh.js:405`;
  Escape capture-phase handling `js/refresh.js:353`. z-index in `dashboard.css:466`.
- No mermaid present anywhere yet (confirmed) — we add it.

## Relevant source files

- `pkg/claude/agentd/dashboard/dashboard.html` — nav + section + script tags
- `pkg/claude/agentd/dashboard/dashboard.css` — status classes + animations
- NEW: `pkg/claude/agentd/dashboard/js/workflows.js`, `js/modal-workflows.js`
- NEW: `pkg/claude/agentd/dashboard/js/vendor/mermaid.min.js`
- `pkg/claude/agentd/dashboard/js/{dashboard.js,refresh.js,tabs.js,modal-cron.js}`
- Asset embedding: `dashboard_assets_test.go` checks served assets — keep green.

## Open questions

- Render full mermaid SVG each poll vs cache + restyle. Lean: cache by
  (instanceId, topology hash); restyle classes on status change; full re-render
  only when node set changes.
- Instance detail as a modal vs an inline sub-view within the tab. Lean: inline
  master/detail within the tab (charts are large; modals feel cramped).
- mermaid.min.js size (~few hundred KB) — acceptable as a vendored local asset.
