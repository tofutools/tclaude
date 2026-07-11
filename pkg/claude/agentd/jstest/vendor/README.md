# Vendored component-test runtime

These files are test-only and live outside `dashboard/`, so Go does not embed
them in the tclaude binary. Committing the self-contained modules keeps
`go test ./...` offline and avoids adding npm files or a frontend build step.

| Package | Version | File | SHA-256 | License |
| --- | --- | --- | --- | --- |
| `linkedom` | 0.18.12 | `linkedom.mjs` (`worker.js` from the npm package) | `196efeb17c260e001979dbc54a3c30e701a881c6e8a3eedaddc5ad83c99ee5ff` | ISC (`LICENSE-linkedom.txt`) |
| `preact/test-utils` | 10.29.7 | `preact-test-utils.mjs` | `17a3bfef8f2d7d552b3b5e3f4cc9a92ab82338a9f51607a620267dac71fcd8f3` | MIT (`dashboard/vendor/preact/LICENSE-preact.txt`) |
| `preact/test-utils` source map | 10.29.7 | `testUtils.module.js.map` | `84c6d2b7ee7862c3f2e44e483a0266d2f5cabbeb20c9f6d54c41e4c5fedb89fa` | MIT |

To upgrade, use `npm pack <package>@<exact-version>` in a temporary directory,
copy the upstream distribution files and licenses here, and update this table
plus `preact_test_harness_assets_test.go`. Do not add root npm metadata or a
generated dependency tree.
