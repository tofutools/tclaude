// Tests for the right panel library's shared operations (dock-library.js):
// search matching, pins, config-bundle export with the credential gate,
// import format detection, and the process rename / clone round trips.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function load(t) {
  const harness = await createPreactHarness(t);
  return harness.importDashboardModule('js/dock-library.js');
}

function memoryPrefs() {
  const values = new Map();
  return {
    getItem: (key) => (values.has(key) ? values.get(key) : null),
    setItem: (key, value) => values.set(key, String(value)),
    removeItem: (key) => values.delete(key),
    values,
  };
}

function jsonResponse(status, body) {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => body,
    text: async () => JSON.stringify(body),
  };
}

test('search matches every term against names and values, or keeps a section named by it', async (t) => {
  const { filterSection, leafTexts, searchTerms } = await load(t);
  const items = [
    { name: 'reviewer', model: 'opus', prompt_refs: ['kickoff'] },
    { name: 'coder', model: 'sonnet', sandbox: { name: 'base' } },
  ];
  const run = (query) => filterSection({
    terms: searchTerms(query), titles: ['Agent profiles', 'profiles'], items, textsOf: (item) => leafTexts(item),
  });
  assert.deepEqual(run('OPUS').items.map((item) => item.name), ['reviewer'], 'case-insensitive, on a value');
  assert.deepEqual(run('son base').items.map((item) => item.name), ['coder'], 'every term, nested values too');
  assert.deepEqual(run('opus base').items, [], 'terms AND together');
  assert.deepEqual(run('model').items, [], 'field names do not match');
  const whole = run('agent prof');
  assert.equal(whole.whole, true);
  assert.equal(whole.items.length, 2, 'a term naming the section keeps all of it');
  assert.equal(run('').items.length, 2);
});

test('pins lead their section in pin order, follow a rename and drop on delete', async (t) => {
  const { dropPin, orderPinned, pinnedNames, renamePin, togglePin, PINS_PREF } = await load(t);
  const prefs = memoryPrefs();
  const names = (list) => list.map((item) => item.name);
  const items = [{ name: 'a' }, { name: 'b' }, { name: 'c' }];
  assert.equal(togglePin(prefs, 'roles', 'c'), true);
  assert.equal(togglePin(prefs, 'roles', 'b'), true);
  assert.deepEqual(names(orderPinned(items, (item) => item.name, pinnedNames(prefs, 'roles'))), ['c', 'b', 'a']);
  renamePin(prefs, 'roles', 'c', 'z');
  assert.deepEqual(pinnedNames(prefs, 'roles'), ['z', 'b']);
  dropPin(prefs, 'roles', 'z');
  assert.equal(togglePin(prefs, 'roles', 'b'), false, 'a second toggle unpins');
  assert.equal(prefs.getItem(`${PINS_PREF}roles`), null, 'no pins leaves no pref behind');
  prefs.setItem(`${PINS_PREF}roles`, 'not json');
  assert.deepEqual(pinnedNames(prefs, 'roles'), [], 'a corrupt pref reads as no pins');
});

test('exportBundle downloads one item, and asks before exporting flagged credentials', async (t) => {
  const { exportBundle } = await load(t);
  const urls = [];
  const bundle = { format: 'tclaude-config-bundle' };
  const fetchImpl = async (url) => {
    urls.push(url);
    if (!url.includes('allow_flagged')) {
      return jsonResponse(422, { code: 'flagged_credentials', flags: [{ item: 'sandbox-profiles/base', field: 'environment.TOKEN', hint: 'looks like a token' }] });
    }
    return jsonResponse(200, bundle);
  };
  const downloads = [];
  const asked = [];
  const declined = await exportBundle({
    section: 'sandbox_profiles', name: 'base', fetchImpl,
    confirm: async (options) => { asked.push(options.body); return false; },
    download: (...args) => downloads.push(args),
  });
  assert.equal(declined, false);
  assert.deepEqual(downloads, []);
  assert.match(asked[0], /sandbox-profiles\/base environment\.TOKEN: looks like a token/);
  assert.equal(urls[0], '/api/config-bundle/export?only=sandbox-profiles%2Fbase');

  const done = await exportBundle({
    section: 'sandbox_profiles', name: 'base', fetchImpl, confirm: async () => true, download: (...args) => downloads.push(args),
  });
  assert.equal(done, true);
  assert.equal(urls.at(-1), '/api/config-bundle/export?only=sandbox-profiles%2Fbase&allow_flagged=true');
  assert.deepEqual(downloads, [['sandbox-profiles-base.tclaude.json', bundle]]);

  await exportBundle({ section: 'saved_prompts', fetchImpl: async (url) => { urls.push(url); return jsonResponse(200, bundle); }, confirm: async () => true, download: (...args) => downloads.push(args) });
  assert.equal(urls.at(-1), '/api/config-bundle/export?only=saved-prompts', 'no name exports the whole section');
  assert.equal(downloads.at(-1)[0], 'saved-prompts.tclaude.json');
});

test('detectImport tells a config bundle from each kind\'s own export', async (t) => {
  const { detectImport } = await load(t);
  assert.equal(detectImport('{"format":"tclaude-config-bundle"}').kind, 'bundle');
  assert.equal(detectImport('{"format":"tclaude-spawn-profiles"}').kind, 'profiles');
  assert.equal(detectImport('{"format":"tclaude-sandbox-profiles"}').kind, 'sandbox_profiles');
  assert.equal(detectImport('{"format":"tclaude-task-force"}').kind, 'templates');
  assert.deepEqual(detectImport('{"format":"other"}'), { kind: 'unknown', reason: 'unknown format other' });
  assert.equal(detectImport('nope').reason, 'not JSON');
});

test('renameItem goes through the daemon; a process renames as its next version and clones as a new template', async (t) => {
  const { cloneProcess, freeName, renameItem } = await load(t);
  const calls = [];
  const view = { template: { id: 'tpl-1', name: 'Release', nodes: {} }, layout: { x: 1 }, sourceHash: 'h1' };
  const fetchImpl = async (url, options = {}) => {
    calls.push([options.method || 'GET', url, options.body ? JSON.parse(options.body) : null]);
    return jsonResponse(200, url.startsWith('/v1/process/templates/tpl-1?') ? view : { ok: true });
  };
  await renameItem({ section: 'roles', name: 'one', to: 'two', fetchImpl });
  assert.deepEqual(calls[0], ['POST', '/api/library/rename', { kind: 'roles', name: 'one', to: 'two' }]);

  await renameItem({ section: 'processes', name: 'tpl-1', to: 'Ship', fetchImpl });
  assert.deepEqual(calls[2], ['POST', '/v1/process/templates/tpl-1', {
    template: { id: 'tpl-1', name: 'Ship', nodes: {} }, sourceHash: 'h1', layout: { x: 1 },
  }]);

  await cloneProcess({ id: 'tpl-1', to: 'Release copy', fetchImpl });
  assert.deepEqual(calls[4], ['POST', '/v1/process/templates', {
    template: { name: 'Release copy', nodes: {} }, layout: { x: 1 },
  }], 'a clone drops the id so the daemon mints a new one');

  const refused = async () => jsonResponse(409, { error: 'name taken', code: 'exists' });
  await assert.rejects(renameItem({ section: 'roles', name: 'one', to: 'two', fetchImpl: refused }), { message: 'name taken', code: 'exists', status: 409 });

  assert.equal(freeName('a', ['a', 'a-copy']), 'a-copy-2');
});
