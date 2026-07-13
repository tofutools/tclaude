# Vendored component-test runtime

These files are test-only and live outside `dashboard/`, so Go does not embed
them in the tclaude binary. Committing the self-contained modules keeps
`go test ./...` offline and avoids adding npm files or a frontend build step.

| Package | Version | File | SHA-256 | License |
| --- | --- | --- | --- | --- |
| `linkedom` | 0.18.12 | `linkedom.mjs` (`worker.js` from the npm package) | `196efeb17c260e001979dbc54a3c30e701a881c6e8a3eedaddc5ad83c99ee5ff` | ISC (`LICENSE-linkedom.txt`) |
| `preact/test-utils` | 10.29.7 | `preact-test-utils.mjs` | `17a3bfef8f2d7d552b3b5e3f4cc9a92ab82338a9f51607a620267dac71fcd8f3` | MIT (`dashboard/vendor/preact/LICENSE-preact.txt`) |
| `preact/test-utils` source map | 10.29.7 | `testUtils.module.js.map` | `84c6d2b7ee7862c3f2e44e483a0266d2f5cabbeb20c9f6d54c41e4c5fedb89fa` | MIT |

To upgrade, use `npm pack --ignore-scripts` in a temporary directory. Extract
each tarball separately because npm always names its root `package/`:

| Tarball spec | Copy from its `package/` extraction | Destination |
| --- | --- | --- |
| `linkedom@0.18.12` | `worker.js` and `LICENSE` | `linkedom.mjs` and `LICENSE-linkedom.txt` |
| `preact@10.29.7` | `test-utils/dist/testUtils.module.js{,.map}` | `preact-test-utils.mjs` and `testUtils.module.js.map` |

`preact/test-utils` is a subpath of the `preact` tarball, not a separate npm
package. Update the table and `preact_test_harness_assets_test.go`, then run:

```bash
go test ./pkg/claude/agentd -run TestPreactTestHarnessVendorAssets -count=1
go test ./...
```

Do not add root npm metadata, a package cache, or a generated dependency tree.
