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
    '[docs](https://example.invalid/docs "Docs") and [mail](mailto:ops@example.invalid)');
  const [docs, mail] = findAll(tree, 'a');
  assert.equal(docs.attrs.href, 'https://example.invalid/docs');
  assert.equal(docs.attrs.title, 'Docs');
  assert.equal(docs.attrs.target, '_blank');
  assert.equal(docs.attrs.rel, 'noopener noreferrer');
  assert.equal(mail.attrs.href, 'mailto:ops@example.invalid');
});

// A relative target means the author's own repository, and the only thing it
// can resolve against in the viewer is the dashboard's own origin. Rather than
// render a link that quietly retargets at the operator's daemon, keep the words
// and drop the anchor.
test('targets the viewer cannot resolve keep their text and lose the link', async (t) => {
  const { markdownToTree, parser } = await loadModel(t);
  for (const source of [
    '[notes](./notes.md)',
    '[api](/api/human-messages/1/attachment)',
    '[section](#heading)',
    '[protocol relative](//example.invalid/y)',
    '[ftp](ftp://host.invalid/x "Tip")',
  ]) {
    const tree = markdownToTree(parser, source);
    assert.equal(findAll(tree, 'a').length, 0, `${source} builds no anchor`);
    assert.match(tree.map(text).join(''), /notes|api|section|protocol relative|ftp/,
      `${source} keeps its link text`);
  }
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
  const { markdownToTree, parser, REMOTE_IMAGE } = await loadModel(t);
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
    // A refused target must not reappear as something the operator can choose
    // to load: the placeholder is for images the viewer WOULD show, not a
    // second chance for one it rejected.
    assert.equal(findAll(tree, REMOTE_IMAGE).length, 0, `${source} offers nothing to load`);
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

  const inline = find(markdownToTree(parser, '![dot](data:image/png;base64,iVBORw0KGgo= "Dot")'), 'img');
  assert.equal(inline.attrs.src, 'data:image/png;base64,iVBORw0KGgo=');
  assert.equal(inline.attrs.alt, 'dot');
  assert.equal(inline.attrs.title, 'Dot');
  assert.equal(inline.attrs.loading, 'lazy');
});

// An <img> is the only thing a document fetches with no click, and the author
// may be an agent confined by this project's own egress boundary. A remote src
// would borrow the operator's unfiltered browser to make the request the agent
// could not, so a remote image becomes a node the operator has to act on and
// never an element the browser will fetch on its own.
test('a document cannot make the browser fetch a remote image on its own', async (t) => {
  const { markdownToTree, parser, REMOTE_IMAGE } = await loadModel(t);
  for (const source of [
    '![beacon](https://example.invalid/a.png)',
    '![beacon](http://example.invalid/a.png)',
  ]) {
    const tree = markdownToTree(parser, source);
    assert.equal(findAll(tree, 'img').length, 0, `${source} builds no image element`);
    const held = find(tree, REMOTE_IMAGE);
    assert.ok(held, `${source} is held back for the operator to decide on`);
    assert.equal(held.attrs.alt, 'beacon', 'the placeholder can describe what it stands for');
    assert.match(held.attrs.src, /example\.invalid/, 'the operator can see where it would go');
  }
});

// A scheme with no authority reads one way to the model and another to the
// browser: `new URL('https:./x.png')` is https with a host of ".", but the
// browser resolves that same string against the DOCUMENT's base and lands on
// the dashboard's own origin. Holding it back as "remote" would name the
// operator a host that is never contacted and would let a document address any
// same-origin route behind one click — so it is not an image target at all.
test('a scheme without an authority is not a remote image', async (t) => {
  const { markdownToTree, parser, REMOTE_IMAGE } = await loadModel(t);
  const attachments = [
    { id: 1, filename: 'chart.png', url: '/api/human-messages/9/attachments/1', previewable: true },
  ];
  for (const source of [
    '![beacon](https:./rel.png)',
    '![beacon](https:chart.png)',
    '![beacon](https:../api/human-messages/9/attachments/7)',
    '![beacon](http:example.invalid/a.png)',
  ]) {
    const tree = markdownToTree(parser, source, { attachments });
    assert.equal(findAll(tree, REMOTE_IMAGE).length, 0, `${source} is not offered as remote`);
    assert.equal(findAll(tree, 'img').length, 0, `${source} builds no image`);
    assert.match(tree.map(text).join(''), /beacon/, `${source} keeps its alt text`);
  }
  // The same names spelled with an authority are the real thing.
  const held = find(markdownToTree(parser, '![b](https://example.invalid/a.png)'), REMOTE_IMAGE);
  assert.equal(held.attrs.src, 'https://example.invalid/a.png');
});

// A target that is neither inline, nor remote, nor a file published with the
// document resolves against nothing the author meant, so it keeps its words
// and loses the image — the same answer the anchor rules give.
test('a target that resolves against nothing degrades to alt text', async (t) => {
  const { markdownToTree, parser, REMOTE_IMAGE } = await loadModel(t);
  for (const source of [
    '![beacon](//example.invalid/a.png)',
    '![beacon](./local.png)',
    '![beacon](missing.png)',
    '![beacon](/api/human-messages/1/attachment)',
  ]) {
    const tree = markdownToTree(parser, source, { attachments: [] });
    assert.equal(findAll(tree, 'img').length, 0, `${source} builds no image element`);
    assert.equal(findAll(tree, REMOTE_IMAGE).length, 0, `${source} offers nothing to load`);
    assert.match(tree.map(text).join(''), /beacon/, `${source} keeps its alt text`);
  }
});

// A report that arrives with its own illustrations is the case this exists for:
// `tclaude agent notify-human --attach report.md --attach chart.png`, where the
// document says `![chart](chart.png)`. Those bytes are already on the
// operator's daemon, so the reference resolves to its authenticated route and
// the image renders with no click.
test('an image published with the document renders from its own route', async (t) => {
  const { markdownToTree, parser } = await loadModel(t);
  const attachments = [
    { id: 1, filename: 'report.md', url: '/api/human-messages/9/attachments/1', markdown: true },
    { id: 2, filename: 'chart.png', url: '/api/human-messages/9/attachments/2', previewable: true },
    { id: 3, filename: 'Photo Two.jpeg', url: '/api/human-messages/9/attachments/3', previewable: true },
  ];
  for (const [source, want] of [
    ['![chart](chart.png)', '/api/human-messages/9/attachments/2'],
    ['![chart](./chart.png)', '/api/human-messages/9/attachments/2'],
    // The attachment card shows "chart.png"; a document may spell the name in
    // any case, the way a filesystem-insensitive author would.
    ['![chart](Chart.PNG)', '/api/human-messages/9/attachments/2'],
    // A name with a space reaches the model percent-encoded, whichever of the
    // two CommonMark spellings the author used, so the comparison has to
    // decode before it can match the filename the operator sees on the card.
    ['![photo](<Photo Two.jpeg>)', '/api/human-messages/9/attachments/3'],
    ['![photo](Photo%20Two.jpeg)', '/api/human-messages/9/attachments/3'],
  ]) {
    const node = find(markdownToTree(parser, source, { attachments }), 'img');
    assert.ok(node, `${source} resolves to the published file`);
    assert.equal(node.attrs.src, want);
    assert.equal(node.attrs.loading, 'lazy');
  }

  const titled = find(markdownToTree(parser, '![c](chart.png "The chart")', { attachments }), 'img');
  assert.equal(titled.attrs.title, 'The chart');
  assert.equal(titled.attrs.alt, 'c');
});

// Which picture the operator is shown must never be decided by the order the
// files were published in. A name that two published files both answer to
// identifies neither, so it shows the author's words instead of an arbitrary
// one of the two.
test('a name two published files answer to shows no image, in either order', async (t) => {
  const { markdownToTree, parser } = await loadModel(t);
  const file = (filename, id) => ({
    id, filename, url: `/api/human-messages/9/attachments/${id}`, previewable: true,
  });
  const imageSrc = (attachments, source) => {
    const node = find(markdownToTree(parser, source, { attachments }), 'img');
    return node ? node.attrs.src : null;
  };

  for (const [label, attachments, source] of [
    ['published twice under one name', [file('chart.png', 1), file('chart.png', 2)], '![c](chart.png)'],
    ['names differing only in case', [file('Chart.png', 1), file('chart.png', 2)], '![c](chart.png)'],
  ]) {
    assert.equal(imageSrc(attachments, source), null, `${label}: no image`);
    assert.equal(imageSrc([...attachments].reverse(), source), null,
      `${label}: still no image when the list arrives the other way round`);
    assert.match(markdownToTree(parser, source, { attachments }).map(text).join(''), /c/,
      `${label}: keeps its alt text`);
  }

  // A percent-encoded name and its decoded twin. markdown-it normalizes both
  // document spellings to `a%20b.png`, so the reference names the file called
  // exactly that — the decoded key is never what a document asks for first —
  // and the answer must not move when the two files swap places.
  const encoded = [file('a%20b.png', 1), file('a b.png', 2)];
  const want = '/api/human-messages/9/attachments/1';
  for (const source of ['![c](<a b.png>)', '![c](a%20b.png)']) {
    assert.equal(imageSrc(encoded, source), want, `${source} names the file spelled that way`);
    assert.equal(imageSrc([...encoded].reverse(), source), want,
      `${source} is unmoved by the publishing order`);
  }
  // Published alone, the decoded twin is still reachable through that spelling:
  // marking the shared key ambiguous must not cost a file its only name.
  assert.equal(imageSrc([file('a b.png', 2)], '![c](a%20b.png)'),
    '/api/human-messages/9/attachments/2');
});

// A cache-buster or an anchor is something an author appends without thinking,
// and neither can be part of a name that already failed to match — so the
// picture appears rather than silently becoming alt text.
test('a reference carrying a query or fragment still finds its file', async (t) => {
  const { markdownToTree, parser } = await loadModel(t);
  const attachments = [
    { id: 1, filename: 'chart.png', url: '/api/human-messages/9/attachments/1', previewable: true },
    // A file whose name really does contain a `?`. The exact spelling is tried
    // first, so it still matches itself rather than being truncated onto the
    // one above.
    { id: 2, filename: 'odd?name.png', url: '/api/human-messages/9/attachments/2', previewable: true },
  ];
  for (const [source, want] of [
    ['![c](chart.png?v=2)', '/api/human-messages/9/attachments/1'],
    ['![c](chart.png#figure-1)', '/api/human-messages/9/attachments/1'],
    ['![c](./chart.png?v=2)', '/api/human-messages/9/attachments/1'],
    ['![c](<odd?name.png>)', '/api/human-messages/9/attachments/2'],
  ]) {
    const node = find(markdownToTree(parser, source, { attachments }), 'img');
    assert.ok(node, `${source} resolves to the published file`);
    assert.equal(node.attrs.src, want, source);
  }
});

// `previewable` is the daemon's own content-sniffed verdict that a file really
// is a raster image. Referencing anything else — the document itself, an SVG,
// a payload whose bytes do not match its declared type — must not build an
// <img>, because the reference is the only claim that it is one.
test('only a daemon-confirmed raster attachment can be shown as an image', async (t) => {
  const { markdownToTree, parser, REMOTE_IMAGE } = await loadModel(t);
  const attachments = [
    { id: 1, filename: 'notes.md', url: '/api/human-messages/9/attachments/1', markdown: true },
    { id: 2, filename: 'logo.svg', url: '/api/human-messages/9/attachments/2' },
    { id: 3, filename: 'claim.png', url: '/api/human-messages/9/attachments/3', previewable: false },
    // A published file the daemon gave no route to is not addressable either,
    // and neither is one whose route is not the same-origin path the daemon
    // mints — the viewer checks the shape rather than trusting the field.
    { id: 4, filename: 'orphan.png', url: '', previewable: true },
    { id: 5, filename: 'offsite.png', url: 'https://elsewhere.invalid/x.png', previewable: true },
    { id: 6, filename: 'schemeless.png', url: '//elsewhere.invalid/x.png', previewable: true },
    // `new URL('/\\host/x', origin)` is `https://host/x`: for a special scheme
    // the parser reads a backslash as a slash, so a path-looking route can name
    // another origin.
    { id: 7, filename: 'backslash.png', url: '/\\elsewhere.invalid/x.png', previewable: true },
    // The parser DELETES tab, CR and LF before parsing, so these read as
    // `//elsewhere.invalid/x.png` however path-shaped they look.
    { id: 8, filename: 'tabbed.png', url: '/\t/elsewhere.invalid/x.png', previewable: true },
    { id: 9, filename: 'newlined.png', url: '/\n/elsewhere.invalid/x.png', previewable: true },
    { id: 10, filename: 'returned.png', url: '/\r/elsewhere.invalid/x.png', previewable: true },
  ];
  for (const source of [
    '![doc](notes.md)', '![logo](logo.svg)', '![claim](claim.png)', '![orphan](orphan.png)',
    '![offsite](offsite.png)', '![schemeless](schemeless.png)', '![backslash](backslash.png)',
    '![tabbed](tabbed.png)', '![newlined](newlined.png)', '![returned](returned.png)',
  ]) {
    const tree = markdownToTree(parser, source, { attachments });
    assert.equal(findAll(tree, 'img').length, 0, `${source} builds no image`);
    assert.equal(findAll(tree, REMOTE_IMAGE).length, 0, `${source} offers nothing to load`);
    assert.match(tree.map(text).join(''),
      /doc|logo|claim|orphan|offsite|schemeless|backslash|tabbed|newlined|returned/,
      `${source} keeps its alt text`);
  }
});

// The viewer counts what it is holding back so it can say so once, rather than
// leaving the operator to find every placeholder in a long report. The count is
// per PLACEHOLDER, not per distinct URL: the operator can see three of them, so
// a line above the document claiming two would be describing something else.
test('every remote image a document holds back is counted, in order', async (t) => {
  const { markdownToTree, remoteImageSources, parser } = await loadModel(t);
  const tree = markdownToTree(parser, [
    '![one](https://a.invalid/1.png)',
    '> ![two](https://b.invalid/2.png)',
    '- ![one again](https://a.invalid/1.png)',
    '![inline](data:image/png;base64,iVBORw0KGgo=)',
  ].join('\n\n'));
  assert.deepEqual(remoteImageSources(tree),
    ['https://a.invalid/1.png', 'https://b.invalid/2.png', 'https://a.invalid/1.png']);
  assert.deepEqual(remoteImageSources([]), []);
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

test('a byte-order mark does not swallow the first block', async (t) => {
  const { markdownToTree, parser } = await loadModel(t);
  const tree = markdownToTree(parser, '﻿# Title\n\nbody\n');
  assert.equal(text(find(tree, 'h1')), 'Title', 'a BOM-prefixed heading is still a heading');
});

// A single block can hold more inline nodes than an argument list takes, and
// the daemon deliberately admits documents up to 1 MiB. Losing the whole
// document to a stack overflow inside the walk is not an acceptable answer.
test('a document dense with inline nodes still renders', async (t) => {
  const { markdownToTree, parser } = await loadModel(t);
  const tree = markdownToTree(parser, `${'*a* '.repeat(200000)}\n`);
  assert.equal(findAll(tree, 'em').length, 200000);
});

// The property the whole design rests on, asserted over a corpus rather than
// over a handful of chosen inputs: whatever a document contains, every element
// built from it is one the viewer allows and every attribute is one that
// element may carry. This is the test that has to survive a parser upgrade.
test('no document can produce a tag or attribute outside the allowlists', async (t) => {
  const { markdownToTree, parser } = await loadModel(t);
  const ALLOWED_TAGS = new Set([
    'p', 'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'blockquote', 'hr', 'br',
    'ul', 'ol', 'li', 'pre', 'code', 'em', 'strong', 's', 'a', 'img',
    'table', 'thead', 'tbody', 'tr', 'th', 'td',
    // Not an element: the placeholder node a remote image becomes, which
    // markdown-document.js renders as a component. Listed here so the corpus
    // still fails on any OTHER tag the walk might learn to emit.
    'remote-image',
  ]);
  const ALLOWED_ATTRS = new Set(['title', 'href', 'target', 'rel', 'src', 'loading',
    'start', 'style', 'class', 'alt']);

  // Deterministic pseudo-randomness: a fixed seed keeps a failure reproducible
  // and keeps this suite from flaking in CI.
  let seed = 0x2f6e2b1;
  const rand = (n) => {
    seed = (seed * 1103515245 + 12345) & 0x7fffffff;
    return seed % n;
  };
  const fragments = [
    '<img src=x onerror="alert(1)">', '<script>alert(1)</script>', '<iframe src=//x>',
    '<style>*{}</style>', '<a href="javascript:alert(1)">x</a>', '<svg onload=alert(1)>',
    '[x](javascript:alert(1))', '[x](data:text/html;base64,PHN2Zz4=)', '[x](vbscript:m)',
    '![x](data:image/svg+xml;base64,PHN2Zz4=)', '![x](https://h.invalid/a.png)',
    // An inline raster, so the corpus actually BUILDS an <img> and the src
    // assertion below is live rather than vacuous, and the scheme-relative
    // forms, whose parse-vs-resolve disagreement is the reason a remote target
    // has to name its authority.
    '![x](data:image/png;base64,iVBORw0KGgo=)', '![x](https:./rel.png)', '![x](http:h.invalid/a)',
    '```js" onload="alert(1)\nx\n```', '```\nplain\n```', '# h', '## h2', '> q', '---',
    '- a\n- b', '1. a\n2. b', '| a | b |\n| --- | ---: |\n| 1 | 2 |', '**b** *i* ~~s~~ `c`',
    'https://bare.invalid/link', '[t](https://ok.invalid "ti")', '﻿bom', ' ',
    '    indented code', '<!-- comment -->', '</p>', '<p onclick=x>', '&lt;&gt;&amp;',
    'text\n\n', '  ', '\t\ttab', 'a'.repeat(200),
  ];

  // A value assertion that never runs is not a guard. The corpus has to be
  // shown to reach both image kinds, or a later edit to the fragment list can
  // quietly retire the checks below without failing anything.
  let seenImg = false;
  let seenRemote = false;

  for (let doc = 0; doc < 3000; doc += 1) {
    const parts = [];
    for (let i = 0, n = 1 + rand(9); i < n; i += 1) parts.push(fragments[rand(fragments.length)]);
    const source = parts.join('\n\n');
    const stack = [...markdownToTree(parser, source)];
    while (stack.length) {
      const node = stack.pop();
      if (typeof node === 'string') continue;
      assert.ok(ALLOWED_TAGS.has(node.tag), `tag ${node.tag} escaped the allowlist via:\n${source}`);
      for (const name of Object.keys(node.attrs || {})) {
        assert.ok(ALLOWED_ATTRS.has(name), `attribute ${name} escaped the allowlist via:\n${source}`);
      }
      // The src is the one attribute VALUE the corpus has to check, because it
      // is the only one that makes the browser fetch something. The tag name no
      // longer settles it: an image can now legitimately be built from an
      // attachment route or held back as `remote-image`, so a regression that
      // turned a remote target into a plain <img> — or that widened what counts
      // as remote — would produce only allowlisted names.
      //
      // These documents publish no attachments, so a data: raster is the sole
      // src an <img> may carry, and a placeholder may only stand for an
      // http(s) URL naming its authority.
      if (node.tag === 'img') {
        seenImg = true;
        assert.match(node.attrs.src ?? '', /^data:image\/(?:gif|png|jpeg|webp);base64,/,
          `img src ${node.attrs.src} escaped the inline-only rule via:\n${source}`);
      }
      // Parsed rather than pattern-matched. What has to be true of a held-back
      // image is that it addresses a real remote host — which is what the
      // placeholder promises to name and what the operator's click will fetch —
      // and a spelling starting with `https://` only implies that for as long as
      // the model keeps storing the target verbatim. `https://:1/x` matches the
      // pattern and addresses nothing.
      if (node.tag === 'remote-image') {
        seenRemote = true;
        const src = node.attrs.src ?? '';
        let target = null;
        try {
          target = new URL(src);
        } catch {
          // Left null, which the assertion below reports.
        }
        const parsed = target
          ? `protocol ${target.protocol}, host ${JSON.stringify(target.host)}`
          : 'does not parse as an absolute URL';
        assert.ok(
          target !== null
            && (target.protocol === 'http:' || target.protocol === 'https:')
            && target.host !== '',
          `remote-image src ${src} (${parsed}) is not an http(s) URL naming a host, via:\n${source}`,
        );
      }
      stack.push(...(node.children || []));
    }
  }
  assert.ok(seenImg, 'the corpus never built an <img>, so its src rule went unchecked');
  assert.ok(seenRemote, 'the corpus never held an image back, so its src rule went unchecked');
});
