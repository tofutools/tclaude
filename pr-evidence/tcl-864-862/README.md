# TCL-864 / TCL-862 evidence

Captures produced by the production dashboard handler on branch
`tcl-864-862-ui-nits` at `b00f6fbf`, using the repo's own visual-smoke harness
(`dashsnap`) against the real profile editor and the real prediction endpoint.

## TCL-864 — deny preview copy

`TCLAUDE_DASHSNAP=1 TCLAUDE_DASHSNAP_FILTER=deny-preview-unsupported go test
./pkg/claude/agentd/ -run TestDashSnap` on the OpenCode / Linux / tclaude-layer
target, which is the state that shows both defects at once.

- `tcl-864-before-regular.png`, `tcl-864-before-wizard.png` — `origin/main`
  behaviour: rows read `Deny network: network 192.0.2.0/28 · port 443`, and the
  Partially-supported limitation reads `… remains available Deny limitation:
  This deny rule is saved, but …`.
- `tcl-864-after-regular.png`, `tcl-864-after-wizard.png` — same state on this
  branch: `Deny network: CIDR 192.0.2.0/28 · port 443`, and `… remains
  available. Deny limitation: This deny rule is saved, but …`.

Every clause survives verbatim, including the fail-open consequence sentence;
only the CIDR selector word and the sentence separator changed.

## TCL-862 — 720px profile-editor overflow

`TCLAUDE_DASHSNAP=1 go test ./pkg/claude/agentd/ -run
TestDashboardSandboxPreviewOverflowChrome`, the new browser smoke added by this
PR. It opens the real editor on TCL-859's evidence seed (11 network rules plus
the control-socket row), selects a Linux tclaude-layer target, expands every
populated preview bucket, and hard-fails unless:

- the modal overlay, the modal card, the effective-policy section and every
  expanded bucket all measure 0px horizontal overflow; and
- no rule label, bucket summary or bucket reason is clipped.

All eight captures (Claude/OpenCode × regular/wizard × 1280/720) pass:
`tcl-862-<target>-<skin>-<width>.png`.

### What the reported 259px / 362px actually was

The ticket's A/B numbers are page-level (`documentElement.scrollWidth −
clientWidth`), and they reproduce **before the editor is even opened**: measured
on this branch, the page-level delta at the moment the dashboard finishes
loading is 259px regular / 612px wizard at 720px, and 0px / 52px at 1280px —
identical to the value measured after expanding a bucket. The source is the
dashboard shell, not the modal: `header`, `nav` (and in the wizard skin
`#slop-marquee`) widen to their content width by design, which is JOH-313's
horizontal-scroll behaviour, so the tab strip never shrinks below its natural
width.

The modal's own boxes are clean at 720px: the card is `min(900px, 100vw − 32px)`
= 688px wide with its content wrapping inside the buckets.

The new test's sensitivity was verified by temporarily changing
`.sbx-rule-row` to `grid-template-columns: max-content 18px`, which makes it
fail with `modal-card +53px` and matching bucket deltas — so it is a real guard
against the wrapping regressing, not a vacuous assertion.
