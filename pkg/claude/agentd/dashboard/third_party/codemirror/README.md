# Vendored CodeMirror 6 (Markdown editor)

The Task board's brief and comment editor (`js/markdown-editor.js`) is
CodeMirror 6 with its Markdown language. The library is committed as one
self-contained browser ES module, so the dashboard stays an offline,
single-binary Go application, like the Preact runtime and markdown-it next
door. Application code imports the bare specifier `codemirror` through the
import map in `dashboard.html`, and only with a dynamic `import()` when an
editor first mounts, so a dashboard load never pays for it. Until it loads,
or if it fails to, the editor is a plain textarea.

| File | SHA-256 |
| --- | --- |
| `codemirror.esm.min.mjs` (~326 KB, ~107 KB gzipped) | `8a904ef1d88f557696d16d89826f5fa39b8373a13f6cf0774f1622ea0be1e016` |

It is an esbuild bundle (esbuild 0.28.1, `--bundle --format=esm
--platform=browser --minify`) of these packages, all MIT-licensed. Their
license texts are in `LICENSE-codemirror.txt`.

| Package | Version |
| --- | --- |
| `@codemirror/autocomplete` | 6.20.4 |
| `@codemirror/commands` | 6.11.0 |
| `@codemirror/lang-markdown` | 6.5.2 |
| `@codemirror/language` | 6.12.4 |
| `@codemirror/state` | 6.7.3 |
| `@codemirror/view` | 6.43.11 |
| `@lezer/common` | 1.5.5 |
| `@lezer/highlight` | 1.2.3 |
| `@lezer/markdown` | 1.7.2 |
| `@marijn/find-cluster-break` | 1.0.4 |
| `crelt` | 1.0.7 |
| `style-mod` | 4.1.4 |
| `w3c-keyname` | 2.2.8 |

`@codemirror/lang-markdown` depends on `@codemirror/lang-html`, which pulls in
the HTML, CSS and JavaScript grammars (~170 KB) for highlighting raw HTML
inside Markdown. The dashboard renders Markdown with `html: false`, so raw HTML
is never markup, and the bundle replaces `lang-html` with a stub (below).

## Rebuilding

In a temporary directory:

```bash
npm install --ignore-scripts esbuild@0.28.1 @codemirror/commands@6.11.0 \
  @codemirror/lang-markdown@6.5.2 @codemirror/language@6.12.4 \
  @codemirror/state@6.7.3 @codemirror/view@6.43.11 @lezer/highlight@1.2.3 \
  @lezer/markdown@1.7.2
```

`entry.mjs`, the bundle's whole export surface. Keep it minimal, and keep it
in step with what `js/markdown-codemirror.js` destructures from the import:

```js
export { EditorView, keymap, placeholder } from "@codemirror/view";
export { EditorState, EditorSelection, Prec, Compartment } from "@codemirror/state";
export { defaultKeymap, history, historyKeymap, indentMore, indentLess } from "@codemirror/commands";
export { syntaxHighlighting } from "@codemirror/language";
export { classHighlighter, tagHighlighter, tags } from "@lezer/highlight";
export { markdown } from "@codemirror/lang-markdown";
export { GFM } from "@lezer/markdown";
```

`stub-html.mjs`:

```js
export function html() { return { support: [], language: { parser: undefined } }; }
export function htmlCompletionSource() { return null; }
```

Then:

```bash
node_modules/.bin/esbuild entry.mjs --bundle --format=esm --platform=browser \
  --minify --alias:@codemirror/lang-html=./stub-html.mjs \
  --outfile=codemirror.esm.min.mjs
```

Copy the output here, regenerate `LICENSE-codemirror.txt` from the bundled
packages' own license files, update the hash and version tables above and
`dashboard_codemirror_assets_test.go`, and run:

```bash
go test ./pkg/claude/agentd -run 'TestDashboardCodeMirror|TestDashboardModuleGraph' -count=1
```

No npm metadata, package cache, or generated dependency tree belongs in the
repository.
