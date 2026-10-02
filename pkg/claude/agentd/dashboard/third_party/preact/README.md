# Vendored Preact runtime

These browser-native ES modules are committed so the dashboard remains an
offline, single-binary Go application. Application code imports normal package
names through the import map in `dashboard.html`; HTM supplies component
templates without JSX compilation.

| Package | Version | Runtime file SHA-256 | Source map SHA-256 |
| --- | --- | --- | --- |
| `preact` | 10.29.7 | `preact.module.js`: `850dcba8ed3535b0a3611495c405551b9887724885d3b8482207a03de365d64e` | `a24b8606d61210775bbd11a742054c25b74f82c33bcde21efa1883253ce65630` |
| `preact/hooks` | 10.29.7 | `hooks.module.js`: `a6ee626f2d01570592dd569a792e3f050154aa02890eead8c223fa3ed5aa3d5a` | `a13936803e904e19f2f154e953541c20dbbd0667c881f446e7aefafcfe487a97` |
| `@preact/signals` | 2.9.3 | `signals.module.js`: `0439faa8ed059c955df6ab43bf02e67b886daf73adb795f6252ca9e783d68190` | `a34390151735a6c7cce8342dce89a1b27efd571baa42fbae94440995b4beaadb` |
| `@preact/signals-core` | 1.14.4 | `signals-core.module.js`: `bfbb64b74f7f06a4f7c6f6bb854cccb40d03f1e96305d43c41876cba581ea112` | `14f651f12e13f5f51b29fd1108c01a6408c9d87c0f49f0d96ffb19b0e1fc75a3` |
| `htm` | 3.1.1 | `htm.module.js`: `ab33dd3f38059b9be4d5f5350128eefb2356639c4e0bbe9d9e8b3ba75847e9e4` | Not published |

The files come from exact npm tarballs. `preact/hooks` is a subpath of the
`preact` package, not a separate npm package. Upstream source maps are committed
beside the modules where provided. Preact is MIT-licensed, Signals and Signals
Core share the Preact team's MIT license, and HTM is Apache-2.0; the
corresponding license texts are committed here.

To upgrade, create a temporary directory and download these exact tarballs with
`npm pack --ignore-scripts <spec>`:

| Tarball spec | Copy from its `package/` extraction | Destination |
| --- | --- | --- |
| `preact@10.29.7` | `dist/preact.module.js{,.map}` | `preact.module.js{,.map}` |
| `preact@10.29.7` | `hooks/dist/hooks.module.js{,.map}` | `hooks.module.js{,.map}` |
| `preact@10.29.7` | `LICENSE` | `LICENSE-preact.txt` |
| `@preact/signals@2.9.3` | `dist/signals.module.js{,.map}` and `LICENSE` | `signals.module.js{,.map}` and `LICENSE-signals.txt` |
| `@preact/signals-core@1.14.4` | `dist/signals-core.module.js{,.map}` | `signals-core.module.js{,.map}` |
| `htm@3.1.1` | `dist/htm.module.js` and `LICENSE` | `htm.module.js` and `LICENSE-htm.txt` |

Extract each tarball separately because npm always names its root `package/`.
Then update the version/hash table above and
`dashboard_preact_assets_test.go`, verify the import map, and run:

```bash
go test ./pkg/claude/agentd -run 'TestDashboardPreact(RuntimeAssets|ImportMap)$' -count=1
go test ./...
```

No npm metadata, package cache, or generated dependency tree belongs in the
repository.
