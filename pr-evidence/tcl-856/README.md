# TCL-856 Chrome evidence

Production tip: `07745a8544b43ad841f7b98d4bb0c92f2209b2c4`

These screenshots were rendered from the real dashboard handler with host
Google Chrome at 1280×900 and 720×900, in regular and wizard skins. The fixture
opens the production sandbox-profile editor with:

- one filesystem row in each Read, Write, and Deny state;
- Path-selected and Glob-selected Unix-socket rows;
- two Environment name/value rows; and
- one resolved include.

The green badge in every screenshot is inserted only after the browser-side
gate passes all of these hard assertions:

- relevant viewport, modal, and section horizontal overflow: `0px`;
- clipped segmented/button/select labels: `0`;
- filesystem, socket, and environment row-column alignment delta: `0.0px`;
- filesystem selected states have three distinct computed colors;
- Path and Glob selected states have identical neutral computed paint;
- Includes occupies less than 70% of its section width and retains its value;
- Environment name columns are exactly `14rem` (`224px`) and aligned; and
- Environment name/value inputs compute to a monospace font family.

Files:

- `tcl-856-regular-1280.png`
- `tcl-856-regular-720.png`
- `tcl-856-wizard-1280.png`
- `tcl-856-wizard-720.png`
