import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const ago = (minutes) => new Date(Date.now() - minutes * 60_000).toISOString();
const row = (id, title, extra = {}) => ({ id, group: 'crew', title, stage: 'next', status: 'open', depends_on: [], created_at: ago(5), ...extra });
const builder = { agent_id: 'agt_builder', name: 'builder' };

function fakeApi() {
  const calls = [];
  const record = (verb) => async (...args) => { calls.push([verb, ...args]); return {}; };
  return { calls, capture: record('capture'), importRefs: record('importRefs'), edit: record('edit'), move: record('move'),
    stage: record('stage'), status: record('status'), assign: record('assign'), unassign: record('unassign'), create: record('create') };
}

// Frames run on a timer for this file: an overlay's frame loop (a blip's,
// which ends on its own) may outlive the test that started it.
globalThis.requestAnimationFrame ??= (fn) => setTimeout(() => fn(Date.now()), 16);
globalThis.cancelAnimationFrame ??= (id) => clearTimeout(id);

// layout gives the node DOM what the cross-links measure and do: every
// element has a box and scrollIntoView is recorded.
function layout(t, harness) {
  const proto = harness.window.HTMLElement.prototype;
  const saved = ['getClientRects', 'getBoundingClientRect', 'scrollIntoView'].map((name) => [name, Object.getOwnPropertyDescriptor(proto, name)]);
  const scrolled = [];
  proto.getClientRects = function getClientRects() { return [{}]; };
  proto.getBoundingClientRect = function getBoundingClientRect() { return { top: 100, bottom: 120, left: 10, right: 110, width: 100, height: 20 }; };
  proto.scrollIntoView = function scrollIntoView(options) { scrolled.push([this.dataset?.taskId || this.dataset?.dndAgent || this.className, options]); };
  t.after(() => {
    for (const [name, desc] of saved) { if (desc) Object.defineProperty(proto, name, desc); else delete proto[name]; }
  });
  return scrolled;
}

const until = async (harness, check, what) => {
  for (let i = 0; i < 100; i += 1) {
    if (check()) return;
    await harness.act(() => new Promise((r) => setTimeout(r, 20)));
  }
  assert.fail(`timed out waiting for ${what}`);
};

async function mount(t, name, { by = 'agent', tasks = [], view = 'lanes' } = {}) {
  const harness = await createPreactHarness(t);
  const { TaskboardSection } = await harness.importDashboardModule('js/groups-taskboard.js');
  const { AgentTaskIndicator } = await harness.importDashboardModule('js/taskboard-agent-indicator.js');
  const { dashPrefs } = await harness.importDashboardModule('js/prefs.js');
  dashPrefs.setItem(`tclaude.dash.groups.taskboard.view.${name}`, view);
  dashPrefs.setItem(`tclaude.dash.groups.taskboard.lanes-by.${name}`, by);
  dashPrefs.setItem(`tclaude.dash.groups.fused.taskboard.${name}`, '1');
  const entry = { group: name, group_id: 1, counts: { live: tasks.length }, tasks, closed: [] };
  const source = { context: harness.signals.signal({ groups: [entry] }), status: harness.signals.signal('ready'), attach: () => () => {}, refresh: async () => {} };
  const group = { name, members: [{ agent_id: 'agt_builder', title: 'builder', role: 'dev', online: true }] };
  // The agent's row, as the Groups view draws it, with its task indicator.
  const mounted = await harness.mount(harness.html`<div>
    <details data-group-key=${name} open><summary>${name}</summary>
      <table><tbody><tr data-dnd-agent="agt_builder"><td>builder
        <${AgentTaskIndicator} group=${group} member=${group.members[0]} source=${source} />
      </td></tr></tbody></table>
      <${TaskboardSection} group=${group} snapshot=${{ groups: [group] }} source=${source} api=${fakeApi()} />
    </details>
  </div>`);
  return { harness, c: mounted.container, dashPrefs };
}

test('a lane head names its agent with a link', async (t) => {
  const { c } = await mount(t, 'xl-links');
  const head = c.querySelector('[data-lane-agent="agt_builder"] .tb-lane-name');
  assert.equal(head.getAttribute('role'), 'link');
  assert.equal(head.getAttribute('title'), 'builder', 'a long name keeps its full name as the tooltip');
  assert.equal(c.querySelector('[data-agent-tasks] .tb-agent-task-title'), null, 'no task, no indicator');
});

test('in triage a card\'s avatar links to its agent, and the agent\'s row links to its task', async (t) => {
  const tasks = [row('tk_00000001', 'building', { assignee: builder })];
  const tri = await mount(t, 'xl-links-triage', { by: 'triage', tasks });
  const avatar = tri.c.querySelector('.tb-card[data-task-id="tk_00000001"] .tb-card-marker');
  assert.equal(avatar.getAttribute('role'), 'link');
  assert.equal(avatar.getAttribute('tabindex'), '0');
  const indicator = tri.c.querySelector('[data-agent-tasks="agt_builder"] .tb-agent-task-title');
  assert.equal(indicator.getAttribute('role'), 'link', 'the agent\'s row links to its task');
  assert.match(indicator.getAttribute('title'), /Show “building” on the task board/);
  // A click on it finds the card: the indicator names the board rightly.
  const scrolled = layout(t, tri.harness);
  await tri.harness.act(async () => { tri.harness.fireEvent(indicator, 'click'); });
  await until(tri.harness, () => scrolled.some(([id]) => id === 'tk_00000001'), 'the scroll to the card');
});

test('a jump to a task scrolls to the card, opening the cell that holds it past its cap', async (t) => {
  // Unassigned Next holds four: the fourth, the target, is past the cap of three.
  const tasks = [...['a', 'b', 'c'].map((n) => row(`tk_0000000${n}`, `queued ${n}`)), row('tk_0000000e', 'target')];
  const { harness, c } = await mount(t, 'xl-jump', { tasks });
  const scrolled = layout(t, harness);
  const cell = c.querySelector('.tb-cell[data-lane="unassigned"][data-column="next"]');
  assert.equal(cell.querySelector('[data-task-id="tk_0000000e"]'), null, 'the target is past the cap');
  assert.match(cell.querySelector('.tb-cell-more').getAttribute('data-hidden-tasks'), /tk_0000000e/, '"+ n more" names what it hides');

  const { jumpToTask } = await harness.importDashboardModule('js/cross-links.js');
  await harness.act(async () => { jumpToTask({ shown: 'xl-jump', group: 'xl-jump', node: '', taskId: 'tk_0000000e' }); });
  await until(harness, () => scrolled.some(([id]) => id === 'tk_0000000e'), 'the scroll to the card');
  assert.equal(cell.querySelector('.tb-cell-more').getAttribute('aria-expanded'), 'true', 'the cell opened for it');
  const [, options] = scrolled.find(([id]) => id === 'tk_0000000e');
  assert.equal(options.behavior, 'smooth');
  assert.equal(options.block, 'center');
  assert.ok(harness.document.querySelector('#drop-landing-layer .drop-landing-blip'), 'and it blips');
});

test('a jump to a task on a folded board unfolds the section on the click, not at the next poll', async (t) => {
  const tasks = [row('tk_0000000f', 'folded target', { assignee: builder })];
  const { harness, c, dashPrefs } = await mount(t, 'xl-fold', { tasks });
  const scrolled = layout(t, harness);
  const toggle = () => c.querySelector('[data-fused-section="taskboard"] .gf-section-toggle');
  await harness.act(() => harness.fireEvent(toggle(), 'click'));
  assert.equal(dashPrefs.getItem('tclaude.dash.groups.fused.taskboard.xl-fold'), '0');
  assert.equal(toggle().getAttribute('aria-expanded'), 'false', 'the board starts folded');
  assert.equal(c.querySelector('[data-task-id="tk_0000000f"]'), null);

  // The agent's row link, clicked: one render flush and no timers later the
  // section is open and the card drawn.
  const link = c.querySelector('[data-agent-tasks="agt_builder"] .tb-agent-task-title');
  assert.ok(link, 'the agent row links to its task');
  const start = Date.now();
  await harness.act(() => harness.fireEvent(link, 'click'));
  assert.equal(toggle().getAttribute('aria-expanded'), 'true', 'the section opened with the click');
  assert.ok(c.querySelector('[data-task-id="tk_0000000f"]'), 'the card is drawn');
  await until(harness, () => scrolled.some(([id]) => id === 'tk_0000000f'), 'the scroll to the card');
  assert.ok(Date.now() - start < 1000, 'the scroll comes well before the jump deadline');
});

test('a click on a lane head jumps to the agent\'s row; hovering outlines it, a folded group\'s header instead', async (t) => {
  const { harness, c, dashPrefs } = await mount(t, 'xl-agent');
  const scrolled = layout(t, harness);
  const head = c.querySelector('[data-lane-agent="agt_builder"] .tb-lane-name');

  await harness.act(async () => { harness.fireEvent(head, 'mouseenter'); });
  await until(harness, () => harness.document.querySelector('#drop-landing-layer .xlink-hover'), 'the hover outline');
  const outline = harness.document.querySelector('#drop-landing-layer .xlink-hover');
  await until(harness, () => !outline.hidden && outline.style.width === '100px', 'the outline over the row');
  assert.equal(outline.classList.contains('xlink-folded'), false, 'the row itself is outlined');
  assert.deepEqual(scrolled, [], 'hovering never scrolls');
  await harness.act(async () => { harness.fireEvent(head, 'mouseleave'); });
  assert.equal(harness.document.querySelector('#drop-landing-layer .xlink-hover'), null, 'leaving drops it');

  // The agent's row gone (its section folded): the group's header stands in.
  const tr = c.querySelector('tr[data-dnd-agent="agt_builder"]');
  tr.removeAttribute('data-dnd-agent');
  await harness.act(async () => { harness.fireEvent(head, 'mouseenter'); });
  await until(harness, () => harness.document.querySelector('#drop-landing-layer .xlink-hover.xlink-folded'), 'the folded header outline');
  assert.equal(dashPrefs.getItem('tclaude.dash.groups.fused.agents.xl-agent'), null, 'hovering never unfolds');
  await harness.act(async () => { harness.fireEvent(head, 'mouseleave'); });
  tr.setAttribute('data-dnd-agent', 'agt_builder');

  await harness.act(async () => { harness.fireEvent(head, 'click'); });
  await until(harness, () => scrolled.some(([id]) => id === 'agt_builder'), 'the scroll to the row');
  assert.equal(dashPrefs.getItem('tclaude.dash.groups.fused.agents.xl-agent'), '1', 'a click unfolds the agents section');
  assert.ok(harness.document.querySelector('#drop-landing-layer .drop-landing-blip'));
});

test('in the List, Who links to a member agent; the operator and a former member are plain', async (t) => {
  const tasks = [row('tk_00000001', 'building', { assignee: builder }),
    row('tk_00000002', 'asked', { assignee: { agent_id: 'operator', name: 'operator', operator: true } }),
    row('tk_00000003', 'left behind', { assignee: { agent_id: 'agt_gone', name: 'gone' } })];
  const { c } = await mount(t, 'xl-who', { view: 'list', tasks });
  const who = (id) => c.querySelector(`tr[data-task-id="${id}"] .tb-who-name`);
  assert.equal(who('tk_00000001').getAttribute('role'), 'link');
  assert.equal(who('tk_00000002').getAttribute('role'), null);
  assert.equal(who('tk_00000003').getAttribute('role'), null);
  assert.match(c.querySelector('tr[data-task-id="tk_00000001"] .tb-who').getAttribute('title'), /^builder/, 'the tooltip still says who assigned it');
});

test('an outline goes when its link re-renders or unmounts while hovered', async (t) => {
  const harness = await createPreactHarness(t);
  const { agentLink } = await harness.importDashboardModule('js/cross-links.js');
  const n = harness.signals.signal(0);
  const shown = harness.signals.signal(true);
  function Link() {
    return shown.value ? harness.html`<span class="x" ...${agentLink({ group: 'g', id: 'a', name: `n${n.value}` })}>hi</span>` : null;
  }
  const { container } = await harness.mount(harness.html`<${Link} />`);
  const outlines = () => harness.document.querySelectorAll('.xlink-hover').length;
  await harness.act(async () => { harness.fireEvent(container.querySelector('.x'), 'mouseenter'); });
  assert.equal(outlines(), 1);
  await harness.act(async () => { n.value = 1; });
  await harness.act(async () => { harness.fireEvent(container.querySelector('.x'), 'mouseleave'); });
  assert.equal(outlines(), 0, 'the re-rendered link\'s leave takes it');

  await harness.act(async () => { harness.fireEvent(container.querySelector('.x'), 'mouseenter'); });
  assert.equal(outlines(), 1);
  await harness.act(async () => { shown.value = false; });
  await until(harness, () => outlines() === 0, 'the outline of the unmounted link to go');
});

test('a double-click on a card\'s avatar does not open the card', async (t) => {
  const tasks = [row('tk_00000001', 'building', { assignee: builder })];
  const { harness, c } = await mount(t, 'xl-dbl', { by: 'triage', tasks });
  let bubbled = 0;
  c.addEventListener('dblclick', () => { bubbled += 1; });
  await harness.act(async () => { harness.fireEvent(c.querySelector('.tb-card[data-task-id="tk_00000001"] .tb-card-marker'), 'dblclick'); });
  assert.equal(bubbled, 0);
});

test('a click on an offline agent\'s task unfolds the offline lanes; hovering outlines their fold', async (t) => {
  const tasks = [row('tk_00000001', 'parked', { assignee: { agent_id: 'agt_sleeper', name: 'sleeper' } })];
  const harness = await createPreactHarness(t);
  const { TaskboardSection } = await harness.importDashboardModule('js/groups-taskboard.js');
  const { AgentTaskIndicator } = await harness.importDashboardModule('js/taskboard-agent-indicator.js');
  const { dashPrefs } = await harness.importDashboardModule('js/prefs.js');
  const name = 'xl-offline';
  dashPrefs.setItem(`tclaude.dash.groups.taskboard.view.${name}`, 'lanes');
  dashPrefs.setItem(`tclaude.dash.groups.taskboard.lanes-by.${name}`, 'agent');
  dashPrefs.setItem(`tclaude.dash.groups.fused.taskboard.${name}`, '1');
  const entry = { group: name, group_id: 1, counts: { live: 1 }, tasks, closed: [] };
  const source = { context: harness.signals.signal({ groups: [entry] }), status: harness.signals.signal('ready'), attach: () => () => {}, refresh: async () => {} };
  const group = { name, members: [{ agent_id: 'agt_builder', title: 'builder', online: true }, { agent_id: 'agt_sleeper', title: 'sleeper', online: false }] };
  const { container: c } = await harness.mount(harness.html`<details data-group-key=${name} open><summary>${name}</summary>
    <table><tbody><tr data-dnd-agent="agt_sleeper"><td>sleeper
      <${AgentTaskIndicator} group=${group} member=${group.members[1]} source=${source} /></td></tr></tbody></table>
    <${TaskboardSection} group=${group} snapshot=${{ groups: [group] }} source=${source} api=${fakeApi()} />
  </details>`);
  const scrolled = layout(t, harness);
  const fold = c.querySelector('.tb-lane-fold');
  assert.equal(fold.getAttribute('aria-expanded'), 'false', 'the offline lanes start folded');
  assert.equal(c.querySelector('[data-task-id="tk_00000001"]'), null);
  const link = c.querySelector('[data-agent-tasks="agt_sleeper"] .tb-agent-task-title');

  await harness.act(async () => { harness.fireEvent(link, 'mouseenter'); });
  await until(harness, () => harness.document.querySelector('.xlink-hover.xlink-folded'), 'the fold outlined');
  assert.equal(fold.getAttribute('aria-expanded'), 'false', 'hovering never unfolds');
  await harness.act(async () => { harness.fireEvent(link, 'mouseleave'); });

  await harness.act(async () => { harness.fireEvent(link, 'click'); });
  await until(harness, () => scrolled.some(([id]) => id === 'tk_00000001'), 'the scroll to the card');
  assert.equal(c.querySelector('.tb-lane-fold').getAttribute('aria-expanded'), 'true');
});
