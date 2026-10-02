# Vendored markdown-it parser

The dashboard renders Markdown files an agent published with a human
notification. The parser is committed as a browser-native ES module so the
dashboard stays an offline, single-binary Go application, exactly like the
Preact runtime next door. Application code imports the bare specifier
`markdown-it` through the import map in `dashboard.html`.

This is the `dist/browser/` bundle, which already contains markdown-it's own
dependencies (`entities`, `linkify-it`, `mdurl`, `punycode.js`, `uc.micro`), so
one file is the whole parser and no transitive vendoring is needed.

| Package | Version | Runtime file SHA-256 | Source map SHA-256 |
| --- | --- | --- | --- |
| `markdown-it` | 15.0.0 | `markdown-it.esm.min.mjs`: `eb0a6cb2beb08326ea4d3e0e3b25ac72c1e6f119a619d9bbe061e72000ffa118` | `a1fccb4bda2e184b3f5e25b8dd7d020bedc30975e0e8bfec89d03811aee3312a` |

markdown-it is MIT-licensed; its license text is committed here.

The dashboard never asks markdown-it for an HTML string. `js/markdown-model.js`
walks the token stream into a plain tree and `js/markdown-document.js` turns
that tree into Preact vnodes, so agent-published text reaches the DOM only as
text nodes and allowlisted elements. Upgrading the parser must not change that:
do not introduce `md.render()`, and keep `html: false` set on the instance.

To upgrade, create a temporary directory and download the exact tarball with
`npm pack --ignore-scripts markdown-it@<version>`:

| Tarball spec | Copy from its `package/` extraction | Destination |
| --- | --- | --- |
| `markdown-it@15.0.0` | `dist/browser/markdown-it.esm.min.mjs{,.map}` | `markdown-it.esm.min.mjs{,.map}` |
| `markdown-it@15.0.0` | `LICENSE` | `LICENSE-markdown-it.txt` |

Then update the version/hash table above and
`dashboard_markdown_assets_test.go`, verify the import map, and run:

```bash
go test ./pkg/claude/agentd -run 'TestDashboardMarkdown' -count=1
go test ./...
```

No npm metadata, package cache, or generated dependency tree belongs in the
repository.
