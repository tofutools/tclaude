import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// markdown-model.js is deliberately free of Preact and of the DOM, so these
// tests exercise the real parse-and-allowlist path directly. The harness is
// used only for its materialized module tree, which resolves the vendored
// markdown-it the way the browser's import map does.
async function loadModel(t) {
  const harness = await createPreactHarness(t);
  const model = await harness.importDashboardModule('js/markdown-model.js');
  const parser = await model.loadMarkdownParser();
  return { ...model, parser };
}

// find walks the document tree for the first node with the given tag.
function find(nodes, tag) {
  for (const node of nodes) {
    if (typeof node === 'string') continue;
    if (node.tag === tag) return node;
    const nested = find(node.children || [], tag);
    if (nested) return nested;
  }
  return null;
}

function findAll(nodes, tag, out = []) {
  for (const node of nodes) {
    if (typeof node === 'string') continue;
    if (node.tag === tag) out.push(node);
    findAll(node.children || [], tag, out);
  }
  return out;
}

// text flattens a subtree to the characters that would reach the DOM.
function text(node) {
  if (typeof node === 'string') return node;
  return (node.children || []).map(text).join('');
}

test('ordinary Markdown becomes an element tree', async (t) => {
  const { markdownToTree, parser } = await loadModel(t);
  const tree = markdownToTree(parser, [
    '# Release notes',
    '',
    'Shipped **today**, with `--tui` support.',
    '',
    '- first',
    '- second',
    '',
    '> quoted',
    '',
    '```js',
    'const x = 1;',
    '```',
    '',
    '| col | val |',
    '| --- | ---:|',
    '| a   | 1   |',
  ].join('\n'));

  assert.equal(find(tree, 'h1').tag, 'h1');
  assert.equal(text(find(tree, 'h1')), 'Release notes');
  assert.equal(text(find(tree, 'strong')), 'today');
  assert.equal(text(find(tree, 'code')), '--tui');
  assert.equal(findAll(tree, 'li').length, 2);
  assert.equal(text(find(tree, 'blockquote')).trim(), 'quoted');

  const pre = find(tree, 'pre');
  assert.ok(pre, 'a fence becomes a pre');
  assert.equal(pre.children[0].attrs.class, 'language-js');
  assert.equal(text(pre), 'const x = 1;\n');

  assert.ok(find(tree, 'table'), 'tables are rendered');
  const aligned = findAll(tree, 'td').find((cell) => cell.attrs.style);
  assert.equal(aligned.attrs.style, 'text-align:right');
});

test('an empty or blank document produces no nodes', async (t) => {
  const { markdownToTree, parser } = await loadModel(t);
  assert.deepEqual(markdownToTree(parser, ''), []);
  assert.deepEqual(markdownToTree(parser, '   \n\n'), []);
  assert.deepEqual(markdownToTree(parser, null), []);
});

test('links are allowlisted, opened away from the dashboard, and titled', async (t) => {
  const { markdownToTree, parser } = await loadModel(t);
  const tree = markdownToTree(parser,
    '[docs](https://example.invalid/docs "Docs") and [mail](mailto:ops@example.invalid) and [rel](./notes.md)');
  const [docs, mail, rel] = findAll(tree, 'a');
  assert.equal(docs.attrs.href, 'https://example.invalid/docs');
  assert.equal(docs.attrs.title, 'Docs');
  assert.equal(docs.attrs.target, '_blank');
  assert.equal(docs.attrs.rel, 'noopener noreferrer');
  assert.equal(mail.attrs.href, 'mailto:ops@example.invalid');
  assert.equal(rel.attrs.href, './notes.md', 'a relative target stays relative');
});

test('raw HTML in an agent document is shown, never honoured', async (t) => {
  const { markdownToTree, parser } = await loadModel(t);
  const tree = markdownToTree(parser,
    '<img src=x onerror="alert(1)"> <b>bold?</b>\n\n<script>alert(2)</script>');
  assert.equal(find(tree, 'img'), null, 'no element is built from raw HTML');
  assert.equal(find(tree, 'b'), null);
  const shown = tree.map(text).join('');
  assert.match(shown, /onerror/, 'the markup is displayed as text');
  assert.match(shown, /<script>alert\(2\)<\/script>/);
});

test('dangerous link and image targets are dropped, safe ones kept', async (t) => {
  const { markdownToTree, parser } = await loadModel(t);
  // Some of these markdown-it's own validateLink refuses to turn into a link
  // at all, and the rest the allowlist strips. Either way what matters is the
  // same: no element reaches the DOM carrying the target.
  const hostile = [
    '[x](javascript:alert(1))',
    '[x](java\tscript:alert(1))',
    '[x](JaVaScRiPt:alert(1))',
    '[x](vbscript:msgbox)',
    '[x](data:text/html;base64,PHNjcmlwdD4=)',
    '[x](file:///etc/passwd)',
    '![x](javascript:alert(1))',
    '![x](data:image/svg+xml;base64,PHN2Zz4=)',
    '![x](data:text/html;base64,PHNjcmlwdD4=)',
  ];
  for (const source of hostile) {
    const tree = markdownToTree(parser, source);
    assert.equal(findAll(tree, 'img').length, 0, `${source} builds no image`);
    for (const node of findAll(tree, 'a')) {
      assert.equal(node.attrs.href, undefined, `${source} carries no href`);
    }
  }
  // ftp: is a scheme markdown-it's own validateLink permits and the allowlist
  // here does not, so it exercises this module's rejection path rather than
  // the parser's.
  const refused = markdownToTree(parser, '![the shot](ftp://host.invalid/a.png)');
  assert.equal(findAll(refused, 'img').length, 0);
  assert.equal(text(refused[0]), 'the shot', 'a refused image degrades to its alt text');

  const remote = find(markdownToTree(parser, '![shot](https://example.invalid/a.png "Shot")'), 'img');
  assert.equal(remote.attrs.src, 'https://example.invalid/a.png');
  assert.equal(remote.attrs.alt, 'shot');
  assert.equal(remote.attrs.title, 'Shot');
  assert.equal(remote.attrs.loading, 'lazy');

  const inline = find(markdownToTree(parser, '![dot](data:image/png;base64,iVBORw0KGgo=)'), 'img');
  assert.equal(inline.attrs.src, 'data:image/png;base64,iVBORw0KGgo=');
});

test('only the attributes each element is allowed to carry survive', async (t) => {
  const { markdownToTree, parser } = await loadModel(t);
  // An ordered list starting elsewhere is markdown-it's own `start` attribute.
  const list = find(markdownToTree(parser, '7. seven\n8. eight'), 'ol');
  assert.equal(list.attrs.start, 7);
  assert.equal(find(markdownToTree(parser, '1. one'), 'ol').attrs.start, undefined);

  // A fence's info string can be arbitrary text; only a plain language name
  // reaches the class attribute.
  const bad = find(markdownToTree(parser, '```js" onload="alert(1)\nx\n```'), 'code');
  assert.equal(bad.attrs.class, undefined);
  const good = find(markdownToTree(parser, '```bash\nls\n```'), 'code');
  assert.equal(good.attrs.class, 'language-bash');
});

test('the parser is loaded once and shared', async (t) => {
  const { loadMarkdownParser, parser } = await loadModel(t);
  assert.equal(await loadMarkdownParser(), parser);
});
