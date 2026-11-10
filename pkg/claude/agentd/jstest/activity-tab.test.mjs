import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function installActivity(harness) {
  harness.document.body.innerHTML = `<section id="tab-diagnostics">
    <a class="activity-subtab active" data-subtab="audit" href="/diagnostics/audit" aria-selected="true">Audit</a>
    <a class="activity-subtab" data-subtab="logs" href="/diagnostics/logs" aria-selected="false">Logs</a>
    <a class="activity-subtab" data-subtab="performance" href="/diagnostics/performance" aria-selected="false">Performance</a>
    <div class="activity-panel active" id="activity-audit"></div>
    <div class="activity-panel" id="activity-logs"></div>
    <div class="activity-panel" id="activity-performance"></div>
  </section>`;
}

test('Activity subnav mirrors its signal onto the panels and reports navigation', async (t) => {
  const harness = await createPreactHarness(t);
  installActivity(harness);
  const [{ bindActivityTab }, { activityViewActive, setActivitySubtab }] = await Promise.all([
    harness.importDashboardModule('js/activity-tab.js'),
    harness.importDashboardModule('js/activity-state.js'),
  ]);
  const subtab = harness.signals.signal('audit');
  const navigated = [];
  harness.document.addEventListener('tclaude:navigated', (event) => navigated.push(event.detail.location));
  const unbind = bindActivityTab({ doc: harness.document, subtab });
  const link = (name) => harness.document.querySelector(`[data-subtab="${name}"]`);
  const shown = () => [...harness.document.querySelectorAll('.activity-panel.active')].map((panel) => panel.id);

  const click = harness.fireEvent(link('logs'), 'click', { button: 0 });
  assert.equal(click.defaultPrevented, true, 'a plain click switches in place');
  assert.equal(subtab.value, 'logs');
  assert.deepEqual(shown(), ['activity-logs']);
  assert.equal(link('logs').getAttribute('aria-selected'), 'true');
  assert.equal(link('audit').classList.contains('active'), false);
  assert.deepEqual(navigated, [{ tab: 'diagnostics', subtab: 'logs' }]);

  const space = harness.fireEvent(link('performance'), 'keydown', { key: ' ' });
  assert.equal(space.defaultPrevented, true);
  assert.deepEqual(shown(), ['activity-performance'], 'Space activates like the Access subnav');

  assert.equal(setActivitySubtab('bogus', subtab), false, 'an unknown view is refused');
  assert.equal(setActivitySubtab('performance', subtab), false, 'a repeat selection is a no-op');
  const activeTab = harness.signals.signal('diagnostics');
  assert.equal(activityViewActive(activeTab, subtab, 'performance'), true);
  activeTab.value = 'groups';
  assert.equal(activityViewActive(activeTab, subtab, 'performance'), false, 'a view is active only on the Activity tab');

  unbind();
  harness.fireEvent(link('audit'), 'click', { button: 0 });
  assert.equal(subtab.value, 'performance', 'cleanup removes the click handler');
});
