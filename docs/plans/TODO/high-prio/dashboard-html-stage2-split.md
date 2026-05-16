# Dashboard refactor — Stage 2: split `dashboard.js`

**Status:** planned, not started. Execution begins after human review (parked
Stage-2 agent holds for a resume nudge). This file is the plan; it changes no
served bytes.

## 1. What Stage 2 is

`pkg/claude/agentd/dashboard.html` — the agentd browser dashboard — had grown
into a single file of **10,303 lines** (verified at commit `31dfeaa`, its last
single-file state before Stage 1), ~79% of it inline JavaScript. A staged refactor is
making it maintainable under one hard rule: **every mechanical stage produces
byte-identical served output.**

**Stage 1 shipped** (PR #152, commit `2b20159`). The inline `<style>` and
`<script>` bodies were extracted from `dashboard.html` into sibling files
`dashboard.css` and `dashboard.js`. All three are `//go:embed`'d;
`assembleDashboardHTML()` splices the CSS and JS back into the markup shell's
empty `<style></style>` / `<script></script>` placeholders at package init:

```go
func assembleDashboardHTML() string {
	h := strings.Replace(dashboardShellHTML, "<style></style>", "<style>"+dashboardCSS+"</style>", 1)
	h = strings.Replace(h, "<script></script>", "<script>"+dashboardJS+"</script>", 1)
	return h
}
```

A SHA-256 pin test (`TestDashboardHTML_SplitIsByteIdentical`,
`dashboard_split_test.go`) asserts the assembled bytes equal the pre-split
`dashboard.html` exactly. No bundler, no ES modules, no `html/template` — the
build stays build-step-free.

**What remains:** `dashboard.js` is still one file — **8,130 lines**, ~295
top-level declarations, all wrapped in a single IIFE. `dashboard.css` (1,167
lines) and the markup shell `dashboard.html` (1,006 lines) are comparatively
fine and are **out of Stage 2 scope**. Stage 2 is exclusively: break
`dashboard.js` into navigable pieces, mechanically, byte-identically.

Current shape of `dashboard.js`, by natural section (line ranges approximate):

| Lines        | Section                                                              |
|--------------|----------------------------------------------------------------------|
| 1–436        | shared helpers — `$`/`esc`, status dots & pills, context meter, cells, `relTime`, offline toggles |
| 437–617      | sorting + column/accessor tables (`MEMBER_COLS`, `AGENT_COLS`, …)     |
| 619–722      | virtual groups (Ungrouped / Conversations / Retired)                  |
| 724–1276     | rendering — `memberRowHTML`, `renderGroups`, `renderAgents`, messages, usage |
| 1278–1673    | snapshot state, tab filters, `render*Tab`, cron/sudo/links tables     |
| 1674–4574    | modals — sudo, cron + target picker, message, perm, group create, templates, group import/context, link, worktree picker, spawn/clone/reincarnate/rename |
| 4575–6227    | `refresh()`, tab/copy/sort bindings, confirm/window/emergency/cleanup modals, toast |
| 6228–7021    | `bindRowActions()` — one ~800-line delegated click router            |
| 7022–7599    | drag-and-drop (`bindDnd` + `runDnd*`)                                 |
| 7600–8100    | Config tab                                                            |
| 8101–8130    | boot — the `bind*()` / `refresh()` / `setInterval` calls, IIFE close  |

## 2. The (a) vs (b) analysis

The human floated two directions. They are **not symmetric**, and the
asymmetry is the whole decision — so spell it out before weighing them.

Stage 1 was unambiguously mechanical because relocating an inline `<script>`
body into a sibling file is byte-trivial: you cut a contiguous text run and the
splice puts it back. Stage 2 inherits that property **for JavaScript** but not
automatically for everything a "component" drags along.

### Option (a) — fragment `dashboard.js` by feature/tab; concatenate

Split the one JS file into ~12 files along the section boundaries above;
`assembleDashboardJS()` concatenates them, in an explicit order, into the
`dashboardJS` string that `assembleDashboardHTML()` already splices.

- **Mechanical?** Fully. A `<script>` element does not care about file
  boundaries — its content is a byte stream. If the fragments are contiguous
  line-ranges of the original and the concatenation joins them with the empty
  string, the result is the original `dashboard.js`, byte-for-byte. Nothing is
  reordered, rewritten, or rescoped.
- **Cost:** every fragment is a *slice of one IIFE body*, not a standalone
  program. The wrapper `(function() {` lives in the first fragment and `})();`
  in the last; middle fragments are runs of complete top-level declarations
  with no wrapper of their own. See §6 for what that does to editor tooling
  (short version: navigation mostly survives, per-file lint gets noisier).

### Option (b) — break out whole components (HTML + CSS + JS together)

Pull one feature at a time — say the cron modal — into a triplet: its markup,
its styles, its script, co-located, and have assembly splice each third back
into the right slot.

This sounds tidier but **cannot be done mechanically**, and the blocker is CSS:

- **CSS is cascade-ordered.** `dashboard.css` is a flat global stylesheet —
  687 top-level rules, no scoping. Which rule wins on a given element depends
  on source order among equal-specificity rules. You *can* cut CSS into
  contiguous fragments in original order (that is byte-safe, exactly like the
  JS). But option (b)'s premise is *regrouping* rules **by component** — and
  regrouping reorders the cascade. That is not a byte change you can pin away;
  it is a potential **rendering** change. Reproducing the original cascade
  while physically reordering rules is not a mechanical operation.
- **A component's JS is not self-contained.** Every modal leans on shared
  helpers — `$`, `esc`, `confirmModal`, `toast`, `showStatus`, the API
  helpers. Under the no-modules constraint (§4) those can only be shared via
  one common scope. So the "JS third" of every component is *still* a fragment
  of the shared IIFE — exactly what option (a) produces — just relocated into
  a component folder. Option (b) buys nothing for the JS that (a) doesn't.
- **HTML is the one cooperative third.** Each modal (`<div class="modal-overlay"
  id="…">`) and each tab (`<section id="tab-…">`) *is* a contiguous subtree, so
  HTML could be fragmented contiguously. But splitting markup alone has little
  value, and it does not rescue the CSS problem.

So option (b) is **strictly worse**: it takes on the CSS-cascade risk that
(a) never incurs, and its JS outcome is identical to (a)'s. Its only unique
deliverable — true per-component encapsulation — is unreachable anyway while
ES modules are forbidden (§4, §9).

### A third option, named honestly

There is a real third path, and it should be on the record rather than
discovered later: **native ES modules** (`import`/`export`, served as
`<script type="module">`). Browsers run those with **no bundler and no build
step**, so "build-step-free" alone does not forbid them — only the explicit
"no ES modules" constraint does. Native ESM is the *only* approach that yields
genuinely independent, individually-valid, individually-lintable component
files with working cross-file go-to-definition.

But it is a **non-mechanical rewrite**: it changes the served bytes (the page
gains `import` statements and multiple script fetches or an embedded module
graph), so it breaks byte-identity outright and cannot be a "mechanical split"
PR. It is a possible *Stage 3*, gated on the human lifting the no-modules
constraint. **Stage 2 as planned does not need it and does not touch it** — it
is raised in §9 as an open question, not proposed here.

## 3. Byte-identical preservation

**Option (a) stays purely mechanical.** The plan:

1. Each fragment file is a verbatim contiguous line-range of today's
   `dashboard.js`. Splits happen only at `\n` boundaries.
2. `assembleDashboardJS()` concatenates the fragments **in an explicit fixed
   order** with the empty string as the joiner (every line already carries its
   own `\n`; do **not** join with `"\n"` — that would inject bytes).
3. `dashboardJS` becomes `assembleDashboardJS()` instead of a direct
   `//go:embed`. `assembleDashboardHTML()` is **unchanged** — it still splices
   one `dashboardJS` string into one `<script>`.

Because the concatenation reproduces the original `dashboard.js` byte-for-byte,
the assembled page is unchanged, so:

- `TestDashboardHTML_SplitIsByteIdentical` — the existing whole-page SHA-256
  pin — **keeps passing untouched**. Stage 2 changes no served byte.
- `TestDashboardHTML_AssemblySpliceIsClean` also keeps passing: `dashboardJS`
  is still a single spliced string; the length and `Contains` checks hold.

**Add one new guard:** `TestDashboardJS_ConcatIsByteIdentical`, a SHA-256 pin
on the assembled `dashboardJS` alone (seed it from
`git show 2b20159:pkg/claude/agentd/dashboard.js | sha256sum`). The whole-page
pin already catches a broken JS concat, but it blames the whole page; a
JS-specific pin fails with a JS-specific message and localises the regression.

**The wrapper stays in the files.** `(function() {` in the first fragment,
`})();` in the last — as bytes, version-controlled, not synthesised in Go.
Moving the wrapper into a Go string literal would not make middle fragments
any more standalone (they would still be body slices) and only adds a
fragile hand-typed-bytes hazard.

**The realistic break risk is editor hygiene, not logic.** "Insert final
newline", "trim trailing whitespace", and auto-formatters will *silently*
mutate a fragment and break byte-identity. Mitigations: the new JS SHA pin
catches it in CI; add an `.editorconfig` stanza for the fragment directory
disabling final-newline insertion and trailing-whitespace trimming; the carve
PRs must be reviewed as pure line-moves.

**Anything that changes served bytes is not Stage 2.** A genuine content edit
— including the CSS-tidy in §8 — is a separate, explicitly-reviewed PR that
deliberately recomputes the relevant SHA constant. It must never ride inside a
"mechanical split" PR.

## 4. Constraints (unchanged from Stage 1)

- **Build-step-free** — no compile/transpile/concat step in the build or CI.
  Assembly happens in Go at package init, from `//go:embed`'d files.
- **No bundler** — no webpack/esbuild/rollup/etc.
- **No ES modules** — no `import`/`export`, no `<script type="module">`.
  (This is the constraint — not "build-step-free" — that actually forces the
  concatenation approach; see §2's third option and §9.)
- **No `html/template`** — assembly is plain `strings.Replace` splicing.

Option (a) honours all four. Within them, concatenation of fragments is the
*only* way to multi-file the JS at all — which is itself a strong reason it is
the answer.

## 5. Incremental PR breakdown

Splitting 8k lines big-bang is reviewable in principle (the SHA pin proves
safety) but unreviewable in practice — a human cannot eyeball that a
2,900-line modal block moved untouched. So: **isolate the mechanism, then
carve one section at a time.** PR size here is purely review ergonomics; the
SHA pin makes every step equally *safe*.

**PR 1 — mechanism only, zero content cut.**
Create `pkg/claude/agentd/dashboard-js/`, move `dashboard.js` into it verbatim
as a single fragment (e.g. `_rest.js`), add `assembleDashboardJS()` that
concatenates an explicit ordered list of `//go:embed` vars (one entry, for
now), point `dashboardJS` at it, add `TestDashboardJS_ConcatIsByteIdentical`,
add the `.editorconfig` stanza. Diff: one file move + a small Go change. No
line-range is cut, so there is no content risk to review — only the embed/
assembly wiring. Both existing pins stay green.

**PRs 2–N — one carve per PR.** Each PR lifts one contiguous line-range out of
the shrinking `_rest.js` into a named fragment and inserts its embed var at the
correct position in the ordered list. The diff is ~all rename/move; the
reviewer checks two things — "lines only moved" and "embed order correct" —
and the SHA pin does the rest. Suggested fragments (carve order = file order):

| #  | Fragment file              | Carved from lines |
|----|----------------------------|-------------------|
| 1  | `00-helpers.js`            | 1–436 (incl. `(function() {`) |
| 2  | `10-sorting.js`            | 437–617           |
| 3  | `20-virtual-groups.js`     | 619–722           |
| 4  | `30-render.js`             | 724–1276          |
| 5  | `40-tabs.js`               | 1278–1673         |
| 6  | `50-modal-cron.js`         | 1674–2411         |
| 7  | `51-modal-message-perm.js` | 2412–2934         |
| 8  | `52-modal-templates.js`    | 2935–3689         |
| 9  | `53-modal-link-worktree.js`| 3690–3975         |
| 10 | `54-modal-spawn.js`        | 3976–4574         |
| 11 | `60-refresh-binds.js`      | 4575–6227         |
| 12 | `70-row-actions.js`        | 6228–7021         |
| 13 | `80-dnd.js`                | 7022–7599         |
| 14 | `90-config.js`             | 7600–8100         |
| 15 | `99-boot.js`               | 8101–8130 (incl. `})();`) — `_rest.js` renamed once empty |

That is ~15 PRs at one carve each. They may be **batched** — 2–3 carves per PR
cuts it to ~6 PRs — at the reviewer's discretion; the carves are independent
and the pin guards any size. Default to small; batch only adjacent sections.

**Ordering mechanism — recommendation.** Use an **explicit ordered list of
`//go:embed` vars** in `dashboard.go`, concatenated in source order. The order
is then under code review, and adding a fragment is a visible Go diff. The
alternative the brief floated — `//go:embed dashboard-js/*` into an `embed.FS`
and concatenate `ReadDir` output — also works (entries sort by filename, hence
the `NN-` numeric prefixes above), but it has one footgun: renaming a file
silently reorders the JS. JS is order-sensitive for the boot tail and every
top-level `const`/`let`, so a silent reorder is a real hazard. Explicit vars
avoid it. (Either way the SHA pin would catch an actual mistake — this is about
which mechanism makes mistakes *hard*.) Minor; the human may veto in §9.

## 6. Editor-tooling angle

Stage 1's tooling win was unambiguous: a `.js` file gets a JS language service
instead of being an opaque string inside HTML. Stage 2's win is **more mixed,
and honesty here matters**:

- **Gained:** small files. ~300–800 lines each instead of one 8k file — faster
  to open, less scrolling, `git blame`/history localised per feature, and
  "which file" becomes a navigation aid (open `54-modal-spawn.js`, not line
  3,976 of a monolith).
- **Roughly preserved:** cross-file go-to-definition. VS Code's JavaScript
  service treats all non-module `.js` files in a folder as one shared global
  scope. Because each middle fragment loses the IIFE wrapper, its functions
  *look* top-level/global to the service — so it still links a call in one
  fragment to a definition in another. Navigation is not a clean win, but it
  largely survives. (An optional `jsconfig.json` in the fragment dir — not a
  served file, no byte impact — can make this explicit; worth adding in PR 1.)
- **Degraded:** per-file linting. Each fragment is an IIFE-body slice: the
  first has an unclosed `(function() {`, the last a dangling `})();`, and all
  middle fragments sit at the wrapper's 2-space base indent. A linter run on
  one fragment in isolation will flag the brace imbalance and the indentation.
  This is noise, not breakage — the assembled whole is valid — but it is real.

Net: option (a) clearly helps **file size and locality**, roughly **preserves
navigation**, and mildly **hurts per-file lint cleanliness**. Option (b)'s JS
half lands in exactly the same place (same fragments, same IIFE-slice nature),
so it offers **no tooling advantage over (a)** — while adding the CSS risk.
Native ESM (§2, §9) is the only option that would make each file independently
valid and lintable, and it is out of Stage 2 scope.

## 7. Recommendation

**Do option (a): mechanically fragment `dashboard.js` into ~15 concatenated
files, explicit ordered `//go:embed` vars, guarded by a new JS SHA-256 pin,
delivered as ~6–15 small carve PRs after a mechanism-only PR 1.**

Why, decisively:

1. It is the **only** option that stays purely mechanical and byte-identical —
   the same safety property that made Stage 1 land cleanly. Option (b) cannot
   make that promise because of CSS cascade order.
2. The constraints (§4) — specifically no ES modules — mean any multi-file JS
   is concatenated fragments sharing one scope. Option (b)'s "encapsulated
   component" is therefore unattainable for the JS regardless; (b)'s JS
   outcome *is* (a)'s outcome, reached via a riskier route.
3. It delivers the concrete, real win — file size and locality — at near-zero
   risk, in small independently-mergeable PRs.

The honest caveat, on the record: this does **not** give per-file-valid,
independently-lintable modules. Nothing within the current constraints can.
If that gap is later judged to matter, it is a deliberate Stage 3 (native
ESM), not a defect in this plan — see §9.

## 8. Coordination / sequencing

Two other items may touch these files. Neither blocks Stage 2 and Stage 2
blocks neither — but sequencing avoids needless rebase pain.

### Notification-setting follow-up (not yet in flight)

An opt-in desktop notification for new human messages, likely a Config-tab
toggle. It would touch `dashboard.html` (a new control in `<section
id="tab-config">`), possibly `dashboard.css`, and the Config-tab JS — which
Stage 2 carves into `90-config.js`. It is a **feature** (changes served bytes,
recomputes the whole-page SHA pin); Stage 2 carves are **mechanical** (move
bytes, change no SHA). They do not hard-conflict — different concerns,
different SHA effects — but both edit the Config-tab JS region.

*Sequencing:* land the **`90-config.js` carve early** in the Stage 2 sequence
so the notification feature has a stable, final-shaped target file to edit. If
the feature lands first instead, the later carve simply moves a slightly
larger region — also fine. Just do not run the config carve and the feature in
the same PR: one is mechanical, one is not.

### CSS-tidy: deprecated `word-break: break-word`

CodeRabbit flagged on #152 that `dashboard.css` uses `word-break: break-word`
(deprecated; the modern spelling is `overflow-wrap: anywhere`), 4×. It was
correctly left alone on #152 to preserve byte-identity.

This is **not part of Stage 2** — Stage 2 is JS-only and `dashboard.css` is out
of scope. It is a **separate, tiny, content-changing PR**: editing those 4
declarations changes the served bytes, so that PR must deliberately recompute
`preSplitDashboardSHA256` (the whole-page pin). It is fully orthogonal to the
Stage 2 carves — they touch `dashboard.js`, it touches `dashboard.css`, no file
conflict — so it can land before, during, or after Stage 2 with no
coordination beyond "it owns the SHA-pin bump." Recommend: land it as its own
PR whenever convenient; do not fold it into a Stage 2 PR.

## 9. Open questions for the human

1. **Native ES modules — permanently off the table, or a possible Stage 3?**
   This is the one decision that matters. Native ESM needs no bundler and no
   build step, and is the *only* path to genuinely independent, per-file-valid,
   per-file-lintable component files (and the closest thing to option (b)'s
   "encapsulated components"). It is a non-mechanical rewrite that changes the
   served bytes. Stage 2 as planned does not need it. Question: keep the
   no-modules constraint permanent, or earmark "evaluate native ESM" as a
   future Stage 3 once Stage 2 lands?
2. **Fragment granularity** — the plan proposes ~15 fragments at ~300–800 lines.
   Acceptable, or prefer coarser (~6–8 larger files)? Recommendation: ~15;
   one screenful of features per file beats one screenful of lines.
3. **Ordering mechanism** — explicit ordered `//go:embed` vars (recommended,
   §5) vs `embed.FS` directory + numeric-prefix sort. Minor; flagged for
   awareness.
4. **PR batching** — ~15 one-carve PRs vs ~6 batched (2–3 carves each).
   Recommendation: default small, batch only adjacent sections, reviewer's
   call. No architectural impact either way.

## Relevant source files

- `pkg/claude/agentd/dashboard.js` — 8,130 lines, the Stage 2 subject.
- `pkg/claude/agentd/dashboard.go` — `//go:embed` directives,
  `assembleDashboardHTML()`; Stage 2 adds `assembleDashboardJS()` + the
  ordered embed-var list.
- `pkg/claude/agentd/dashboard_split_test.go` — `TestDashboardHTML_SplitIsByteIdentical`
  (whole-page SHA pin), `TestDashboardHTML_AssemblySpliceIsClean`; Stage 2 adds
  `TestDashboardJS_ConcatIsByteIdentical`.
- `pkg/claude/agentd/dashboard.html`, `dashboard.css` — siblings, out of Stage 2
  scope (referenced only by §8 coordination).
