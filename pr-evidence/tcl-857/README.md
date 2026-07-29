# TCL-857 Chrome evidence

Final production tip: `8b8624166b2ffa25aaa3d5ee6ee2623c23617299`

Capture source tip: `1d197d9cd2206b6b4f7b0e9f66831b0be0502948`.
The final production commit is a base-only rebase; all six changed production
and test files are byte-for-byte identical to the capture source tip.

These screenshots were rendered from the real dashboard handler with host
Google Chrome at 1280×900 and 720×900, in regular and wizard skins, with the
production worktree pinned to the capture source tip above. The fixture opens
the production sandbox-profile editor with both:

- a resolvable `dev-caches` include; and
- an authored, unresolvable `base-caches` include.

The green badge in every screenshot is inserted only after the browser-side
gate passes all of these hard assertions:

- exactly one resolvable and one missing include row are present;
- the resolvable row selects `dev-caches` without a warning;
- the missing row retains the authored DOM value `base-caches` while selecting
  the visible `— missing —` sentinel;
- `⚠ "base-caches" not found in registry` is visible in the existing warning
  color and is wired to the invalid select with `role=alert`,
  `aria-invalid=true`, and `aria-describedby`;
- the missing row retains a visible delete button;
- neither include row nor its section has horizontal overflow at either width;
- the missing select remains intrinsic-width (less than 70% of the section);
  and
- the effective-policy endpoint returns and renders a composition warning that
  identifies `base-caches` and says its unresolved rules are absent from the
  preview.

Files:

- `tcl-857-regular-1280.png`
- `tcl-857-regular-720.png`
- `tcl-857-wizard-1280.png`
- `tcl-857-wizard-720.png`
