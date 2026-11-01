// Tests for the snapshot behind a capture (tcl-lw1uqb): what the one filter pass
// takes out, and what the text covers. Geometry comes from a small fake, since
// linkedom does no layout.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';
import { CAPTURE_MASK, diffSecrets, filterSnapshot, openForm, readPage, snapshotPage, snapshotText } from '../dashboard/js/capture-snapshot.js';
import { parseHTML } from './third_party/linkedom.mjs';

const SECRET = 'sk-live-s3cret-value';
const SOURCE_DIR = new URL('../dashboard/js/', import.meta.url);

// page builds a window. A style attribute's display and position are honoured,
// and data-rect="left top right bottom" is an element's box.
function page(body, { width = 800, height = 600 } = {}) {
  const { window } = parseHTML(`<!doctype html><html><body>${body}</body></html>`);
  window.innerWidth = width;
  window.innerHeight = height;
  window.getComputedStyle = (el) => {
    const style = el.getAttribute('style') || '';
    const read = (name, fallback) => style.match(new RegExp(`(?:^|;)\\s*${name}:\\s*([^;]+)`))?.[1]?.trim() || fallback;
    return {
      display: el.hasAttribute('hidden') ? 'none' : read('display', 'block'), position: read('position', 'static'),
      visibility: read('visibility', 'visible'), overflow: read('overflow', 'visible'), zIndex: read('z-index', 'auto'),
    };
  };
  for (const el of window.document.querySelectorAll('[data-rect]')) {
    const [left, top, right, bottom] = el.getAttribute('data-rect').split(' ').map(Number);
    el.getBoundingClientRect = () => ({ left, top, right, bottom, width: right - left, height: bottom - top });
  }
  // linkedom has no input.labels.
  for (const el of window.document.querySelectorAll('input, select, textarea')) {
    const labels = [...window.document.querySelectorAll('label')].filter((label) => label.getAttribute('for') === el.id || label.contains(el));
    Object.defineProperty(el, 'labels', { value: labels });
  }
  return window;
}

const textOf = (win) => snapshotText(snapshotPage(win));

test('no secret survives the filter, in any field or nested element', () => {
  const win = page(`<div aria-modal="true">
    <label for="a">Key</label><input id="a" data-capture-mask title="${SECRET}" data-raw="${SECRET}">
    <input id="b" type="password">
    <input id="c" autocomplete="new-password">
    <textarea id="d" data-capture-mask>${SECRET}</textarea>
    <select id="e" data-capture-mask><option selected>${SECRET}</option></select>
    <label><code data-capture-mask>${SECRET}</code></label><input id="f" value="plain">
    <div data-capture-mask><span>${SECRET}</span><input value="${SECRET}"></div>
    <div data-capture-skip>${SECRET}</div>
  </div>`);
  const { document } = win;
  for (const id of ['a', 'b', 'c', 'd']) document.getElementById(id).value = SECRET;
  const snapshot = snapshotPage(win);
  assert.ok(!JSON.stringify(snapshot.root).includes(SECRET), 'nothing a renderer could reach holds it');
  const text = snapshotText(snapshot);
  assert.ok(!text.includes(SECRET), text);
  assert.match(text, /Key \[text, #a\]: \[masked\]/);
  assert.match(text, /\[password, #b\]: \[masked\]/);
  assert.match(text, /\[text, #f\]: "plain"/);
});

test('a password stays masked after a show button makes it plain text', () => {
  const win = page('<div aria-modal="true"><input id="p" data-capture-mask type="password"></div>');
  const field = win.document.getElementById('p');
  field.value = SECRET;
  field.type = 'text';
  const text = textOf(win);
  assert.ok(!text.includes(SECRET), text);
  assert.match(text, /\[masked\]/);
});

test('a label cannot carry out what a masked element holds', () => {
  const win = page(`<div aria-modal="true"><label for="x"><code data-capture-mask>${SECRET}</code> token</label><input id="x" value="v"></div>`);
  const text = textOf(win);
  assert.ok(!text.includes(SECRET), text);
  assert.ok(text.includes(CAPTURE_MASK), 'it says something was there');
});

test('nothing that points into a masked branch can read it', () => {
  const win = page(`<div aria-modal="true" style="z-index:1">
    <div data-capture-mask><span id="held">${SECRET}</span><label for="b">${SECRET}</label></div>
    <div data-capture-skip><span id="cue">${SECRET}</span></div>
    <input id="a" aria-labelledby="held" value="v">
    <input id="b" value="v">
    <input id="c" aria-labelledby="cue" value="v">
    <select id="s" aria-label="Pick"><option data-capture-mask>${SECRET}</option><option selected>fine</option></select>
    <input id="l" aria-label="Suggest" list="d" value="v"><datalist id="d"><option data-capture-mask value="${SECRET}"></option></datalist>
  </div>`);
  const text = textOf(win);
  assert.ok(!text.includes(SECRET), text);
  assert.match(text, /options: ••••••••, fine/);
});

test('a dialog inside something masked is not read as the open form', () => {
  const win = page(`<div data-capture-mask data-rect="0 0 800 600"><div aria-modal="true" data-rect="0 0 800 600"><input id="p" value="${SECRET}"></div></div>`);
  const text = textOf(win);
  assert.ok(!text.includes(SECRET), text);
  assert.match(text, /^Page: /);
});

const masked = (lines) => diffSecrets(lines).map(([, secret]) => secret).filter(Boolean);

test('a diff dialog masks secret names in any case, and shows the rest', () => {
  const lines = JSON.stringify({
    name: 'p', max_tokens: 1, Api_Key: 's1', apiKey: 's2', PRIVATE_KEY: 's3', Password: 's4', passwd: 's5', client_secret: 's6',
    GitHubToken: 's7', credentials: 's8', authorization: 's9', invite: 's10', cookie: 's11', environment: [{ name: 'API', value: 's12' }],
    filesystem: ['/work'], enabled: true,
  }, null, 2).split('\n');
  const secret = masked(lines).join('\n');
  for (let i = 1; i <= 12; i++) assert.ok(secret.includes(`"s${i}"`), `s${i} is masked`);
  const parts = diffSecrets(lines);
  const plain = parts.map(([text]) => text).join('\n');
  assert.ok(!/"s\d+"/.test(plain), plain);
  for (const shown of ['"name": "p"', '"name": "API"', '"/work"', '"enabled": true']) assert.ok(plain.includes(shown), shown);
  assert.ok(plain.includes('"Api_Key": '), 'the key still reads, so the operator sees what changed');
});

test('everything inside a secret-named key is masked, and it ends where the block does', () => {
  const lines = JSON.stringify({ tokens: ['t1', { deep: 't2' }], auth: { user: 't3' }, after: 'shown' }, null, 2).split('\n');
  const plain = diffSecrets(lines).map(([text]) => text).join('\n');
  assert.ok(!/"t\d"/.test(plain), plain);
  assert.ok(plain.includes('"after": "shown"'), plain);
});

test('a diff line that cannot be told apart from a secret fails closed', () => {
  for (const line of [
    `{"federation":{"invite":"${SECRET}"}}`,
    `  "federation": {"invite": "${SECRET}"},`,
    `  "to\\u006ben": "${SECRET}",`,
    `invite=${SECRET}`,
    `  "unterminated: "${SECRET}`,
  ]) {
    assert.ok(!diffSecrets([line])[0][0].includes(SECRET), line);
  }
});

test('a line marked by a diff dialog never reaches the text', () => {
  const [[plain, secret]] = diffSecrets([`"value": "${SECRET}"`]);
  const text = textOf(page(`<div aria-modal="true"><span class="dl">+ ${plain}<span data-capture-mask>${secret}</span></span></div>`));
  assert.ok(!text.includes(SECRET), text);
  assert.ok(text.includes('"value":'), 'the line itself still reads');
});

test('every diff body marks its secrets through DiffLines', () => {
  const bodies = readdirSync(SOURCE_DIR).filter((name) => name.endsWith('.js')).flatMap((file) => {
    const source = readFileSync(new URL(file, SOURCE_DIR), 'utf8');
    return [...source.matchAll(/class="config-diff">(.{0,40})/g)].map((match) => `${file}: ${match[1]}`);
  });
  assert.ok(bodies.length >= 2, 'the check still finds them');
  assert.deepEqual(bodies.filter((body) => !body.includes('<${DiffLines}')), []);
});

test('a control reads as selected when the page says so', () => {
  const win = page(`<div aria-modal="true"><button aria-pressed="false">Existing</button><button aria-pressed="true">Clone</button>
    <div role="tab" aria-selected="true">General</div></div>`);
  const text = textOf(win);
  assert.match(text, /\[Existing\] +\[Clone, selected\]/);
  assert.match(text, /General \[selected\]/);
});

test('a field line keeps the exact spacing of its value', () => {
  const win = page('<div aria-modal="true"><input id="v" aria-label="Args" value="a  b"></div>');
  assert.match(textOf(win), /Args \[text, #v\]: "a  b"/);
});

test('the text reaches past the viewport inside the open form, and not outside it', () => {
  const win = page(`<div data-rect="0 0 800 600"><p>behind the dialog</p><input value="outside"></div>
    <div aria-modal="true" aria-label="Spawn" data-rect="0 0 800 600">
      <input id="far" aria-label="Far" value="way down" data-rect="0 5000 100 5020">
      <select id="pick" aria-label="Pick"><option>one</option><option selected>two</option></select>
    </div>`);
  const text = textOf(win);
  assert.match(text, /^Form: Spawn/);
  assert.match(text, /Far \[text, #far\]: "way down"/);
  assert.match(text, /Pick \[select, #pick\]: "two" \(options: one, two\)/);
  assert.ok(!text.includes('behind the dialog') && !text.includes('outside'), text);
});

test('collapsed parts of the form are included and labelled, and hidden parts outside it are not', () => {
  const win = page(`<input hidden value="elsewhere">
    <div class="modal-overlay show">
      <section hidden aria-label="Advanced"><input id="adv" aria-label="Budget" value="9"></section>
      <details><summary>More</summary><input id="more" aria-label="Note" value="hi"></details>
      <span hidden>Summon a new hero</span><span>Spawn a new agent</span>
      <button aria-expanded="false">Options</button>
    </div>`);
  const text = textOf(win);
  assert.match(text, /\[collapsed: Advanced\]/);
  assert.match(text, /Budget \[text, #adv, collapsed\]: "9"/);
  assert.match(text, /Note \[text, #more, collapsed\]: "hi"/);
  assert.match(text, /\[Options, collapsed\]/, 'a section not built yet still shows as closed');
  assert.ok(!text.includes('Summon'), 'a hidden theme copy is not a collapsed section');
  assert.ok(!text.includes('elsewhere'), text);
});

test('a field is named once, without the buttons beside it in its label', () => {
  const win = page(`<div aria-modal="true">
    <label for="n">Name <button>Clear</button></label><input id="n" value="sim">
    <label><input id="o" type="checkbox"> owner</label>
    <label hidden><select id="s" aria-label="Fast mode"></select></label>
    <button data-capture-text="none" aria-expanded="false">?</button>
  </div>`);
  const lines = textOf(win).split('\n');
  assert.deepEqual(lines.slice(1), [
    '[Clear]', 'Name [text, #n]: "sim"', 'owner [checkbox, #o]: off', 'Fast mode [select, #s, collapsed]: (none) (options: )',
  ]);
});

test('with no form open the text covers what is on screen', () => {
  const win = page(`<div data-rect="0 0 800 600">
      <p data-rect="0 0 800 20">in view</p>
      <p data-rect="0 900 800 920">below the fold</p>
      <div data-rect="0 100 800 200" style="overflow:hidden">
        <p data-rect="0 300 800 320">clipped by its panel</p>
        <p data-rect="0 120 800 140">inside its panel</p>
      </div>
      <input hidden value="hidden value">
    </div>`);
  const text = textOf(win);
  assert.match(text, /^Page: /);
  assert.ok(text.includes('in view') && text.includes('inside its panel'), text);
  for (const absent of ['below the fold', 'clipped by its panel', 'hidden value']) assert.ok(!text.includes(absent), text);
});

test('a list says how much of itself it shows, even with its pager off screen', () => {
  const win = page(`<table data-rect="0 0 800 100"><tbody>
      <tr style="display:table-row" data-rect="0 0 800 20"><td style="display:table-cell">alpha</td><td style="display:table-cell">idle</td></tr>
    </tbody></table>
    <div data-rect="0 2000 800 2020" data-capture-note="showing 30 of 200 rows"></div>`);
  const text = textOf(win);
  assert.match(text, /alpha \| idle/);
  assert.match(text, /\(showing 30 of 200 rows\)/);
});

test('terminals are named, not read', () => {
  const win = page(`<div class="xterm" data-rect="0 0 800 600"><div class="xterm-rows">${SECRET}</div></div>`);
  const text = textOf(win);
  assert.match(text, /\[terminal\]/);
  assert.ok(!text.includes(SECRET), text);
  assert.ok(JSON.stringify(snapshotPage(win).root).includes(SECRET), 'the image still shows it, as the operator sees it');
});

test('the topmost open dialog is the form, and a non-modal window is not one', () => {
  const win = page(`<div id="window" role="dialog" aria-modal="false"></div>
    <div id="low" aria-modal="true" style="z-index:10"></div>
    <div id="high" aria-modal="true" style="z-index:20"></div>
    <div id="later" aria-modal="true" style="z-index:5"></div>
    <div id="gone" aria-modal="true" style="z-index:99;display:none"></div>
    <div id="suspended" aria-modal="true" inert style="z-index:99"></div>
    <div id="cue" aria-modal="true" data-capture-skip style="z-index:99"></div>`);
  assert.equal(openForm(win).id, 'high');
});

test('a dialog inside a closed overlay is not open', () => {
  const win = page(`<div style="display:none"><div id="palette" aria-modal="true" style="z-index:99"></div></div>
    <div id="spawn" aria-modal="true"></div>`);
  assert.equal(openForm(win).id, 'spawn');
});

test('a button reads as one control, by its text or its name', () => {
  const win = page(`<div aria-modal="true"><button><span>🎨</span><span hidden>wizard</span><span>Appearance</span></button>
    <button aria-label="Close"></button><button aria-expanded="false">Options</button></div>`);
  const text = textOf(win);
  assert.match(text, /\[🎨 Appearance\]/);
  assert.match(text, /\[Close\]/);
  assert.match(text, /\[Options, collapsed\]/);
});

test('a page marked as a form counts when no dialog is open', () => {
  const win = page(`<div data-capture-form="Hidden tab" hidden></div>
    <div data-capture-form="Config" data-rect="0 0 800 600"><input aria-label="Theme" value="dark" hidden></div>`);
  assert.equal(openForm(win).getAttribute('data-capture-form'), 'Config');
  const text = textOf(win);
  assert.match(text, /^Form: Config/);
  assert.match(text, /Theme .*collapsed.*"dark"/);
});

test('long text says it was cut', () => {
  const win = page(`<div aria-modal="true">${'<p>line of text</p>'.repeat(50)}</div>`);
  const text = snapshotText(snapshotPage(win), 100);
  assert.ok(text.length < 200, text);
  assert.match(text, /text cut at \d+ characters/);
});

test('a field far longer than a line is cut, with its length', () => {
  const win = page(`<div aria-modal="true"><textarea id="t" aria-label="Brief">${'x'.repeat(5000)}</textarea></div>`);
  win.document.getElementById('t').value = 'x'.repeat(5000);
  assert.match(textOf(win), /\(cut, 5000 characters\)/);
});

test('filtering works on the plain tree alone', () => {
  const win = page(`<input id="k" type="password">`);
  win.document.getElementById('k').value = SECRET;
  const snapshot = readPage(win);
  assert.ok(JSON.stringify(snapshot.root).includes(SECRET), 'the read itself decides nothing');
  assert.ok(!JSON.stringify(filterSnapshot(snapshot).root).includes(SECRET));
});

// A new secret field has to be marked to be masked, and nothing at runtime can
// tell it holds one. This looks at how every field is written. A field below is
// allowed because it holds no secret, and says what it holds instead.
const NOT_SECRET = new Map([
  ['fleet-mg-header', 'the header name a key goes in'],
  ['fleet-mg-prefix', 'the text before a key'],
]);
const SECRET_NAME = /\b(?:token|secret|api[-_ ]?key|password|passphrase|bearer|credential)\b|-key\b/i;

// fieldTags finds the fields in a source file. An arrow function in a handler is
// not the end of its tag.
const fieldTags = (source) => [...source.matchAll(/<(?:input|textarea)\b(?:=>|[^>])*>/g)].map(([tag]) => tag);

function unmarkedSecret(tag) {
  const naming = [...tag.matchAll(/\b(?:id|name|placeholder|aria-label)=(?:"([^"]*)"|'([^']*)'|\$\{`?([^}`]*)`?\})/g)].map((m) => m[1] ?? m[2] ?? m[3]).join(' ');
  if (!SECRET_NAME.test(naming)) return false;
  return !tag.includes('data-capture-mask') && !/\btype=(?:"(?:password|checkbox|radio|number|hidden)"|\$\{[^}]*password)/.test(tag);
}

test('the secret field check finds a field written after a handler', () => {
  const [tag] = fieldTags('html`<input onInput=${(e) => set(e)} id="new-api-token" value=${v} />`');
  assert.ok(unmarkedSecret(tag), tag);
  assert.ok(!unmarkedSecret(tag.replace('<input', '<input data-capture-mask')));
  assert.ok(!unmarkedSecret('<input id="new-api-token" type="password">'));
});

test('every field that looks like it holds a secret is masked', () => {
  const unmarked = [];
  const allowed = new Set();
  for (const file of readdirSync(SOURCE_DIR).filter((name) => name.endsWith('.js'))) {
    for (const tag of fieldTags(readFileSync(new URL(file, SOURCE_DIR), 'utf8'))) {
      if (!unmarkedSecret(tag)) continue;
      const id = tag.match(/\bid="([^"]+)"/)?.[1];
      if (NOT_SECRET.has(id)) allowed.add(id);
      else unmarked.push(`${file}: ${id || tag.slice(0, 120)}`);
    }
  }
  assert.deepEqual(unmarked, [], 'add data-capture-mask, or list the field in NOT_SECRET with what it holds');
  assert.deepEqual([...NOT_SECRET.keys()].filter((id) => !allowed.has(id)), [], 'NOT_SECRET lists a field that no longer needs it');
});
