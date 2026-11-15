// js/phone-doctor.js: Diagnostics › Doctor on a phone, over the real doctor
// state and recorded actions, driven through what the operator taps.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const LONG_PATH = '/home/someone/.cache/tclaude/agent-dirs/spwn-4bbeee/GOTMPDIR/TestSandboxServe275511985/001/.codex/hooks.json';

const CHECKS = [
  { id: 'hooks-claude', category: 'setup', severity: 'warning', title: 'Claude Code hooks are not installed',
    detail: 'Without them, status, context and idle detection are degraded.', fix: 'tclaude setup' },
  { id: 'sandbox-default', category: 'decisions', severity: 'decision', title: 'Choose the default sandbox for new agents',
    detail: 'New installs confine agents in tclaude\'s own sandbox.', action: 'sandbox-default' },
  { id: 'hooks-codex', category: 'setup', severity: 'warning', title: 'Codex CLI hooks are not installed',
    detail: `missing: all (open ${LONG_PATH}: no such file or directory)`, fix: 'tclaude setup' },
  { id: 'tmux', category: 'tools', severity: 'ok', title: 'tmux 3.4' },
  { id: 'git', category: 'tools', severity: 'ok', title: 'git 2.45' },
];

async function setup(t) {
  const harness = await createPreactHarness(t);
  const [{ PhoneDoctor, groupChecks }, { createDoctorState }, { phonePage }] = await Promise.all([
    harness.importDashboardModule('js/phone-doctor.js'),
    harness.importDashboardModule('js/doctor-state.js'),
    harness.importDashboardModule('js/phone-page.js'),
  ]);
  const { signal } = harness.signals;
  const activeTab = signal('diagnostics');
  const subtab = signal('doctor');
  const state = createDoctorState({ activeTab, subtab });
  state.checks.value = CHECKS;
  state.phase.value = 'ready';
  const calls = [];
  const actions = {
    load: (opts) => { calls.push(['load', opts]); },
    setSandboxDefault: (value) => { calls.push(['setSandboxDefault', value]); },
  };
  const mounted = await harness.mount(harness.html`<${PhoneDoctor} state=${state} actions=${actions} notify=${() => {}} />`);
  const root = mounted.container;
  const tap = (element) => harness.act(() => element.click());
  const sections = () => [...root.querySelectorAll('[role="group"]')].map((g) => g.getAttribute('aria-label'));
  return { harness, root, state, calls, tap, sections, phonePage, groupChecks, subtab };
}

test('one list grouped by severity, two-line rows, the passing checks folded', async (t) => {
  const { root, calls, tap, sections } = await setup(t);
  assert.deepEqual(calls[0], ['load', { refresh: true }], 'entering the Doctor re-checks');
  assert.equal(root.querySelector('.doctor-side'), null, 'no second row of category tabs (B12)');
  assert.deepEqual(sections(), ['Decisions', 'Warnings', 'All good']);
  const warnings = root.querySelector('[aria-label="Warnings"]');
  assert.match(warnings.querySelector('.phone-section-head').textContent, /Warnings · 2/);
  const row = warnings.querySelector('[data-check="hooks-codex"]');
  assert.equal(row.querySelector('.phone-doctor-title').textContent, 'Codex CLI hooks are not installed');
  assert.match(row.querySelector('.phone-doctor-sub').textContent, /^Hooks & setup · missing: all/);
  assert.match(root.querySelector('.phone-doctor-lede').textContent, /3 need you/);

  const fold = root.querySelector('.phone-doctor-fold');
  assert.match(fold.textContent, /Show the 2 passing checks/);
  assert.equal(root.querySelector('[data-check="tmux"]'), null);
  await tap(fold);
  assert.ok(root.querySelector('[data-check="tmux"]'), 'opened, the passing checks are listed');

  await tap([...root.querySelectorAll('button')].find((b) => /Re-check/.test(b.textContent)));
  assert.deepEqual(calls.at(-1), ['load', { refresh: true }]);
});

test('a finding opens as its own page: all of it, its fix, and Back returns to the list', async (t) => {
  const { root, tap, phonePage, harness } = await setup(t);
  await tap(root.querySelector('[data-check="hooks-codex"]'));
  assert.equal(phonePage.value.title, 'Doctor');
  assert.equal(phonePage.value.hideNav, true);
  const page = root.querySelector('.phone-doctor-page');
  assert.equal(page.querySelector('.phone-doctor-heading').textContent, 'Codex CLI hooks are not installed');
  assert.ok(page.querySelector('.phone-doctor-detail').textContent.includes(LONG_PATH), 'the whole detail, long path and all');
  assert.equal(page.querySelector('.phone-doctor-fix code').textContent, 'tclaude setup');
  assert.ok([...page.querySelectorAll('button')].some((b) => b.textContent === 'Copy'));

  await harness.act(() => phonePage.value.onBack());
  assert.equal(root.querySelector('.phone-doctor-page'), null);
  assert.ok(root.querySelector('[data-check="hooks-codex"]'), 'back on the list');
  assert.equal(phonePage.value, null, 'the page is taken down');
});

test('the sandbox decision\'s page offers its choice; a check that clears says so', async (t) => {
  const { root, tap, calls, state, harness } = await setup(t);
  await tap(root.querySelector('[data-check="sandbox-default"]'));
  const use = [...root.querySelectorAll('.phone-doctor-page button')].find((b) => /Use tclaude sandbox/.test(b.textContent));
  await tap(use);
  assert.deepEqual(calls.at(-1), ['setSandboxDefault', 'tclaude-layer']);
  await harness.act(() => { state.checks.value = CHECKS.filter((check) => check.id !== 'sandbox-default'); });
  assert.match(root.querySelector('.phone-doctor-page').textContent, /This check has cleared/);
});

test('leaving the Doctor closes an open finding', async (t) => {
  const { root, tap, subtab, harness, phonePage } = await setup(t);
  await tap(root.querySelector('[data-check="hooks-claude"]'));
  await harness.act(() => { subtab.value = 'audit'; });
  assert.equal(root.querySelector('.phone-doctor-page'), null);
  assert.equal(phonePage.value, null);
});

test('groupChecks: asking severities in order, the rest passing', async (t) => {
  const { groupChecks } = await setup(t);
  const { groups, passing } = groupChecks([...CHECKS, { id: 'x', severity: 'blocking', title: 'x' }, { id: 'i', severity: 'info', title: 'i' }]);
  assert.deepEqual(groups.map((g) => [g.severity, g.checks.map((c) => c.id)]),
    [['decision', ['sandbox-default']], ['blocking', ['x']], ['warning', ['hooks-claude', 'hooks-codex']]]);
  assert.deepEqual(passing.map((c) => c.id), ['tmux', 'git', 'i']);
});
