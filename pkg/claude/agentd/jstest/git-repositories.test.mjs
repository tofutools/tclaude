import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('100-repository dialog retains hidden selections and freezes the submitted options', async (t) => {
  const harness = await createPreactHarness(t);
  const { GitRepositoriesDialog } = await harness.importDashboardModule('js/git-repositories-island.js');
  const repos = Array.from({ length: 100 }, (_, i) => ({
    name: `repo-${i}`, path: `/repo-${i}`, groups: ['team'], branch: 'feature', default_branch: 'main', dirty: false,
  }));
  let submitted;
  let finish;
  const pending = new Promise((resolve) => { finish = resolve; });
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  await harness.mount(harness.html`<${GitRepositoriesDialog} current=${{ mode: 'pull', group: 'team' }} state=${{ close() {} }} actions=${{
    scan: async () => ({ repos, issues: [] }),
    run: async (requests) => { submitted = requests; await pending; },
  }} />`, host);
  await harness.act(async () => { await Promise.resolve(); });
  assert.equal(host.querySelectorAll('.git-repo-row').length, 100);
  const options = host.querySelectorAll('.git-repos-options input');
  assert.equal(options[0].checked ?? options[0].hasAttribute('checked'), true);
  assert.equal(options[1].checked ?? options[1].hasAttribute('checked'), false);
  await harness.input(host.querySelector('input[type=search]'), 'repo-99');
  assert.equal(host.querySelectorAll('.git-repo-row').length, 1);
  await harness.act(() => harness.fireEvent(host.querySelector('.git-repos-toolbar button'), 'click')); 
  assert.match(host.querySelector('.git-repos-footer').textContent, /99 of 100 selected/);
  await harness.act(() => harness.fireEvent(host.querySelector('.git-repos-footer .primary'), 'click')); 
  assert.equal(submitted.length, 99);
  assert.equal(submitted.some((r) => r.path === '/repo-99'), false);
  assert.equal(submitted.every((r) => r.switch_default === true && r.discard === false && r.group === 'team'), true);
  assert.equal(host.querySelector('.git-repos-footer .primary').disabled, true);
  assert.ok(host.querySelector('.theme-copy-wizard').textContent.includes('Summon'));
  await harness.act(async () => { finish(); await pending; });
});

test('batch Git updates bound concurrency and continue after individual failures', async (t) => {
  const harness = await createPreactHarness(t);
  const { createGitRepositoriesActions } = await harness.importDashboardModule('js/git-repositories-actions.js');
  let active = 0;
  let maxActive = 0;
  const gates = [];
  const calls = [];
  const results = [];
  const actions = createGitRepositoriesActions({ fetchImpl: async (_url, init) => {
    const req = JSON.parse(init.body); calls.push(req); active++; maxActive = Math.max(active, maxActive);
    await new Promise((resolve) => gates.push(resolve)); active--;
    return req.path === '/2' ? new Response('network failure', { status: 502 }) : new Response(JSON.stringify({ path: req.path, status: 'updated' }));
  } });
  const requests = Array.from({ length: 10 }, (_, i) => ({ path: `/${i}`, mode: 'sync', group: '', switch_default: true, discard: false }));
  const run = actions.run(requests, (result) => results.push(result));
  while (results.filter((r) => r.status !== 'running').length < 10) {
    gates.splice(0).forEach((resolve) => resolve());
    await new Promise((resolve) => setImmediate(resolve));
  }
  await run;
  assert.equal(maxActive, 4);
  assert.equal(calls.length, 10);
  assert.equal(results.filter((r) => r.status === 'updated').length, 9);
  assert.equal(results.filter((r) => r.status === 'failed').length, 1);
});

test('palette exposes all/group Git commands in plain and wizard modes', async (t) => {
  const harness = await createPreactHarness(t);
  await harness.replaceDashboardModule('js/dashboard.js', `export let lastSnapshot = null; export function setLastSnapshot(v) { lastSnapshot=v; } export function webTerminalDefault() { return false; }`);
  const { buildCommands } = await harness.importDashboardModule('js/palette.js');
  const { registerGitRepositoriesController } = await harness.importDashboardModule('js/git-repositories-controller.js');
  const calls = [];
  t.after(registerGitRepositoriesController({ open: (...args) => calls.push(args) }));
  for (const wizard of [false, true]) {
    harness.document.body.classList.toggle('wizard', wizard);
    const commands = buildCommands({ groups: [{ name: 'tclaude', agents: [] }], agents: [] });
    const git = commands.filter((c) => c.keywords?.startsWith('git pull ') || c.keywords?.startsWith('git sync '));
    assert.equal(git.length, 4);
    git.forEach((command) => command.run());
  }
  assert.deepEqual(calls, [
    ['pull', ''], ['sync', ''], ['pull', 'tclaude'], ['sync', 'tclaude'],
    ['pull', ''], ['sync', ''], ['pull', 'tclaude'], ['sync', 'tclaude'],
  ]);
});
