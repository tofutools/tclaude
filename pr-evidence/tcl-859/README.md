# TCL-859 real-dashboard evidence

These captures were produced by the production dashboard handler at feature
tip `5df2206f3a8e74a0279cdd515328d6215059ef62`. The visual-smoke harness opened
the real profile editor with `module.openSandboxProfileEditor(seed)`, selected
Claude + tclaude-layer + Linux, and expanded the effective-policy preview's
Fully supported bucket.

The seed reproduces the operator's exact 11 network rules. The rendered totals
are honestly `12 / 0 / 0`: the operator's report counted the 11 network rows
that had been in Partially supported, while `Allow Unix socket: tclaude agent
control` was already a separate Fully supported row before this change.

Both 1280×1200 captures hard-failed unless all of these conditions held:

- bucket counts were exactly 12 Fully supported, 0 Partially supported, and
  0 Unsupported;
- the Fully supported bucket contained exactly the expected 11 network labels,
  no more and no fewer, plus exactly one `Allow Unix socket: tclaude agent
  control` row;
- all five approved network-help disclosures were present verbatim, including
  the amended shared-IP sentence and the launch-check fail-open sentence;
- measured horizontal overflow was 0px across the viewport, body, modal,
  effective-policy section, and all three buckets; and
- the visible summaries, rule labels, form labels, and buttons had 0 clipped
  labels.

Captures:

- `tcl-859-regular-1280.png` — regular dashboard
- `tcl-859-wizard-1280.png` — wizard dashboard

The optional 720px probe differed from the 1280px layout and exposed existing
profile-editor modal overflow (259px regular, 362px wizard). Those narrow
captures are not presented as passing evidence because TCL-859 does not change
the modal layout.
