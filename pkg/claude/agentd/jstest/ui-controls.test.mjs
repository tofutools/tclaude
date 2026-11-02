// New buttons and selects use the standard ui-* controls (dashboard/ui.css,
// tcl-32qwip), so they are styled by default instead of by whichever container
// they land in. A source scan of the page shells and every dashboard module:
// a <button> or <select> tag counts as standard when its class attribute names
// a base control (ui-btn, ui-menu-item or ui-select) or it is marked
// data-ui-exempt, the opt-out for deliberate one-offs such as window controls.
// The class must be written at the tag, as a literal or a string inside its
// ${…}: a class held in a variable (class=${cls}) cannot be read and counts as
// non-standard. A createElement('button'|'select') counts the same way unless
// a className naming a base control follows within the next few lines.
//
// Existing markup is a ratchet: ui-controls-baseline.json records each file's
// count of non-standard tags. A file may not exceed it, and once an area is
// migrated the baseline must come down with it so the count cannot creep back.
// Regenerate after a migration with
//   UPDATE_UI_CONTROLS_BASELINE=1 node --test jstest/ui-controls.test.mjs

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync, writeFileSync } from 'node:fs';

const DASHBOARD = new URL('../dashboard/', import.meta.url);
const BASELINE = new URL('./ui-controls-baseline.json', import.meta.url);

// tagEnd returns the index of the `>` closing the tag that opens at `start`,
// skipping `>` inside quoted attribute values and htm ${…} expressions.
function tagEnd(source, start) {
  let depth = 0;
  let quote = '';
  for (let i = start; i < source.length; i++) {
    const c = source[i];
    if (depth === 0 && quote) {
      if (c === quote) quote = '';
    } else if (c === '$' && source[i + 1] === '{') {
      depth++;
      i++;
    } else if (depth > 0) {
      if (c === '{') depth++;
      else if (c === '}') depth--;
    } else if (c === '"' || c === "'") {
      quote = c;
    } else if (c === '>') {
      return i;
    }
  }
  return source.length;
}

// classAttr returns the value of a tag's class (or className) attribute: the
// quoted string, the ${…} expression, or the bare token; '' when it has none.
function classAttr(tag) {
  const at = tag.match(/\sclass(?:Name)?=/);
  if (!at) return '';
  const start = at.index + at[0].length;
  const open = tag[start];
  if (open === '"' || open === "'") return tag.slice(start + 1, tag.indexOf(open, start + 1));
  if (open === '$' && tag[start + 1] === '{') {
    let depth = 0;
    for (let i = start + 1; i < tag.length; i++) {
      if (tag[i] === '{') depth++;
      else if (tag[i] === '}' && --depth === 0) return tag.slice(start, i + 1);
    }
    return tag.slice(start);
  }
  return tag.slice(start).match(/^[^\s>]*/)[0];
}

const BASE_CONTROL = /(^|[\s"'`])ui-(btn|menu-item|select)(?![\w-])/;

// inComment reports whether the text before a match on its line opens a comment.
function inComment(source, index) {
  const before = source.slice(source.lastIndexOf('\n', index) + 1, index);
  return /^\s*(\/\/|\/\*|\*)/.test(before) || /(^|[\s;])\/\/\s/.test(before);
}

const lineOf = (source, index) => source.slice(0, index).split('\n').length;

// nonStandardTags lists the line numbers of <button>/<select> tags and
// createElement calls in source that name no base ui-* control and are not
// marked data-ui-exempt.
export function nonStandardTags(source) {
  const lines = [];
  for (const match of source.matchAll(/<(button|select)(?=[\s>/])/g)) {
    if (inComment(source, match.index)) continue;
    const tag = source.slice(match.index, tagEnd(source, match.index) + 1);
    if (BASE_CONTROL.test(classAttr(tag)) || /\sdata-ui-exempt\b/.test(tag)) continue;
    lines.push(lineOf(source, match.index));
  }
  for (const match of source.matchAll(/(?:(\w+)\s*=\s*)?[\w.]*createElement\(\s*['"](button|select)['"]\s*\)/g)) {
    if (inComment(source, match.index)) continue;
    const following = source.slice(match.index, match.index + 400).split('\n').slice(0, 4).join('\n');
    const owner = match[1] ? `\\b${match[1]}\\.` : '';
    const named = [...following.matchAll(new RegExp(`${owner}className\\s*=\\s*([^;\\n]*)`, 'g'))].some((m) => BASE_CONTROL.test(m[1]));
    if (!named) lines.push(lineOf(source, match.index));
  }
  return lines.sort((a, b) => a - b);
}

function scan() {
  const files = ['dashboard.html', 'terminals.html', ...readdirSync(new URL('js/', DASHBOARD)).filter((name) => name.endsWith('.js')).sort().map((name) => `js/${name}`)];
  const found = {};
  for (const file of files) {
    const lines = nonStandardTags(readFileSync(new URL(file, DASHBOARD), 'utf8'));
    if (lines.length) found[file] = lines;
  }
  return found;
}

test('the scanner tells standard, exempt and bare controls apart', () => {
  const source = [
    '<button class="ui-btn ui-btn-primary">Save</button>',
    '<button class=${`ui-btn ${busy ? "is-busy" : ""}`} onClick=${() => a > b && go()}>Go</button>',
    '<select class="ui-select ui-sm"></select>',
    '<button data-ui-exempt class="overlay-close">×</button>',
    '<button class="tool">Old</button>',
    '<button onClick=${() => x > 1}>Bare</button>',
    '<select id="pick"></select>',
    '<buttonish>not a tag</buttonish>',
    '// a click does not focus a <button> on every platform',
    ' * repopulates a descendant <select>, whereas',
    'rebuild(); // the <select> is rebuilt',
    '<button class=ui-btn>Unquoted</button>',
    '<button style="font-family: ui-monospace">Font, not a class</button>',
    '<button class="ui-sm tool">Size only</button>',
    '<button class=${cls}>Class in a variable</button>',
    "const b = document.createElement('button');",
    "const s = document.createElement('select');",
    "s.className = 'ui-select ui-sm';",
  ].join('\n');
  assert.deepEqual(nonStandardTags(source), [5, 6, 7, 13, 14, 15, 16]);
});

test('no file gains a button or select without a standard ui-* class', () => {
  const found = scan();
  if (process.env.UPDATE_UI_CONTROLS_BASELINE) {
    const counts = Object.fromEntries(Object.entries(found).map(([file, lines]) => [file, lines.length]));
    writeFileSync(BASELINE, `${JSON.stringify(counts, null, 2)}\n`);
  }
  const baseline = JSON.parse(readFileSync(BASELINE, 'utf8'));
  const over = [];
  const stale = [];
  for (const [file, lines] of Object.entries(found)) {
    const allowed = baseline[file] ?? 0;
    if (lines.length > allowed) {
      over.push(`${file}: ${lines.length} non-standard (baseline ${allowed}), lines ${lines.join(', ')}`);
    }
  }
  for (const [file, allowed] of Object.entries(baseline)) {
    const count = found[file]?.length ?? 0;
    if (count < allowed) stale.push(`${file}: ${count} (baseline ${allowed})`);
  }
  if (over.length) {
    assert.fail([
      'New <button>/<select> markup without a standard control class (dashboard/ui.css, tcl-32qwip):',
      ...over.map((line) => `  ${line}`),
      '',
      'Fix each one with one of:',
      '  - a class at the tag: class="ui-btn" plus one of ui-btn-primary | -success | -danger | -ghost | -link',
      '    (or -ghost / -link with -danger for the quiet red-text form); ui-sm for row/toolbar size;',
      '    class="ui-select" on a select; class="ui-menu-item" on a menu row.',
      '    In a ${…} class expression, write the class as a string there: class=${`ui-btn ${extra}`}.',
      '  - in Preact code, where js/ui-components.js exists: <${Button} variant="primary">, <${Select}>, <${MenuItem}>.',
      '  - a deliberate one-off that must not look standard (window controls, canvas tools): add data-ui-exempt.',
      '',
      'Only when the count is right but the baseline is behind (e.g. your branch predates a baseline change),',
      'regenerate it from pkg/claude/agentd:',
      '  UPDATE_UI_CONTROLS_BASELINE=1 node --test jstest/ui-controls.test.mjs',
    ].join('\n'));
  }
  if (stale.length) {
    assert.fail([
      'Fewer non-standard controls than ui-controls-baseline.json records:',
      ...stale.map((line) => `  ${line}`),
      '',
      'Good: lower the baseline so the count cannot creep back. From pkg/claude/agentd:',
      '  UPDATE_UI_CONTROLS_BASELINE=1 node --test jstest/ui-controls.test.mjs',
    ].join('\n'));
  }
});
