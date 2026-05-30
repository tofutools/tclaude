# workflows: dashboard "Workflows" tab (frontend) — SHIPPED

Part of the **Workflows** feature (see `docs/plans/workflows.md`). Step 5 — the
monitoring surface. Front-end only; consumes the Step 3/4 backend (the
`/api/workflows*` routes + the `workflows` / `workflow_templates` fields on the
`/api/snapshot` payload). Shipped in **PR #<TBD>**.

## What shipped

A new **Workflows** tab in the agentd browser dashboard, built with the existing
vanilla-ESM tab/modal/filter idiom (no build step):

- **Nav + section** — `data-tab="workflows"` button + `<section id="tab-workflows">`
  in `dashboard.html`. Tab show/hide rides the existing CSS toggle in `bindTabs()`.
- **Two-pane layout** — a **Templates** column (each discoverable
  `workflow_templates` row, with an ⎘ instantiate button; broken templates show
  their load `err` and a disabled button) and an **Instances** column (each
  `workflows` row: title, status pill, bound-group tag, and an N/M progress bar).
  Both honor `bindFilter('workflows')`.
- **Instance detail** — an inline master/detail pane below the two columns (not a
  modal — the chart needs width and a modal would suspend the 2 s refresh).
  Selecting an instance fetches `GET /api/workflows/{id}` and renders the header
  (status + cancel/delete), the graph, a side panel, and a collapsible event
  timeline (`detail.events`).
- **Mermaid graph + per-node status coloring** — the snapshotted `mermaid` chart
  is rendered by **vendored mermaid v9.4.3 (UMD)**, with `classDef` + per-node
  `class` lines injected to color each node by status (pending / ready / running
  / awaiting_verify / done / failed / skipped). The **running** node pulses (CSS
  keyframes; respects `prefers-reduced-motion`). Clicking a node selects it
  (outline + side-panel detail). Full re-render only when the *built* definition
  changes (chart or any node status); steady-state polls restyle in place, so no
  flicker.
- **Live agent vitals overlay** — on `ai` nodes the side panel matches the node's
  runtime `assignee` (and the intended-agent hint `node.agent` once Step 4
  surfaces it) against the bound group's members in `snapshot.groups[]`, showing
  that member's `statePill` (online / idle / working / awaiting) + current
  subject. Degrades gracefully: no group, no assignee, or no matching member →
  "— no agent bound" / "assignee … not found". **Never blocks on Step 4** — the
  group/member data is already in the main snapshot.
- **Manual drive controls** — per selected node: mark-running, a settle button
  per `allowed_outcome` (the human-gate "approve" path), and fail — all via
  `PATCH /api/workflows/{id}/nodes/{nodeId}`. Instance-level **cancel**
  (`POST …/cancel`) and **delete** (`DELETE …/{id}`), each behind a confirm
  modal. An ai-node **🤖 start agent** hook POSTs `…/nodes/{nodeId}/start`;
  while Step 4 is unmerged that 501s and is surfaced as a friendly "lands in
  Step 4" toast (feature-detected, not an error).
- **Instantiate modal** — pick a template, optional title, `key=value`-per-line
  params; `POST /api/workflows`; the server's missing-required-param 400 is
  surfaced inline. On success it selects the new instance.
- **Template warnings banner** — feature-detected: renders `detail.warnings` (or
  the template row's `warnings`) when present. Lights up once Step 4 puts
  `Template.Warnings` on the wire; until then the template `err` covers hard
  load failures.

## Offline mermaid — why v9.4.3

The dashboard must work with no network, so mermaid is vendored as one
self-contained file (no CDN, no ES-module chunks). **v11/v10 UMD lazy-load
diagram chunks via dynamic `import()`**, which 404 when self-hosted as a single
asset. **v9.4.3 UMD has zero dynamic imports** (verified) and renders flowcharts
offline via `window.mermaid`. It's loaded by a classic `<script>` placed before
the deferred `dashboard.js` module, so `window.mermaid` is defined when the tab
first renders. `TestDashboardEmbed_VendoredMermaid` pins the no-`import()`
property so a future bump to a chunked build fails the test instead of the
browser.

## Source files

- `pkg/claude/agentd/dashboard/dashboard.html` — nav button, tab section,
  instantiate modal, mermaid `<script>`.
- `pkg/claude/agentd/dashboard/dashboard.css` — `.wf-*` styles, status legend
  chips, running-node pulse.
- `pkg/claude/agentd/dashboard/js/workflows.js` — NEW; the whole tab
  (`renderWorkflowsTab` + `bindWorkflowsUI`, graph render, vitals overlay,
  drive controls, instantiate).
- `pkg/claude/agentd/dashboard/js/refresh.js` — `renderWorkflowsTab()` wired
  into the poll + filter rerender.
- `pkg/claude/agentd/dashboard/js/dashboard.js` — `bindWorkflowsUI()` +
  `bindFilter('workflows')` in the bootstrap.
- `pkg/claude/agentd/dashboard/vendor/mermaid.min.js` — NEW; vendored UMD
  bundle (kept out of `js/` so it isn't swept into the ESM concatenation).
- `pkg/claude/agentd/dashboard_workflows_tab_test.go` — NEW; asset-served +
  self-contained-mermaid + HTML/JS wiring tests.

## Consumed contract (READ-ONLY; owned by Step 3/4 in `dashboard_workflows.go`)

- `GET /api/snapshot` → `.workflows[]` (id, title, template_ref, template_name,
  status, group_id, group_name, total/done/failed/running, timestamps),
  `.workflow_templates[]` (ref, name, description, node_count, source, err
  [+ `warnings` once Step 4 adds it]).
- `GET /api/workflows/{id}` → `{instance, mermaid, params, vars, nodes[],
  events[]}`; `nodes[]` = workflowNodeJSON (node_id, label, executor_kind,
  status, outcome, assignee, visits, output, started_at, finished_at,
  allowed_outcomes [+ `agent` once Step 4 adds it]).
- Mutations: `POST /api/workflows`, `PATCH …/nodes/{nodeId}`, `POST …/cancel`,
  `DELETE …/{id}`. `…/nodes/{nodeId}/start|attach` are Step 4 (501 until merged).

## Deferred polish (not in the Step 5 re-brief scope)

The original TODO floated richer visuals; the controlling re-brief scoped Step 5
to coloring + vitals + drive. Left as future enhancements:

- Marching-ant animation on the in-flight edge (mermaid edge ids are less stable
  than node ids — needs care).
- Absolutely-positioned status-badge overlays on node bounding boxes (bbox math +
  re-placement on every re-render; brittle vs. the classDef approach shipped).
- Per-node right-click context menu and a per-node `…/nodes/{nodeId}/audit`
  drill-down (the instance-level event timeline covers the common case today).
- Param-schema-driven instantiate form (needs the template's declared `params`
  on the wire; today's `key=value` textarea + server-side validation is the MVP).
