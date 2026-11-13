// The Morph dialog's History carry-over section (morph-carry.js): the
// operator states the target window, carries everything, keeps a share of
// the tool output, and the estimate and warnings come from the dry run. The
// sandbox's seeded agents have no working directory or history to translate,
// so the real dialog is mounted under the dashboard's own stylesheets with
// an API that answers like the daemon's dry run.
//
//   scripts/sandbox/ui.sh smoke morph-carry
import { test, expect } from '@playwright/test';

test('the carry-over section previews overrides and warns without blocking', async ({ page }) => {
  const errors = [];
  page.on('pageerror', (e) => errors.push(String(e)));
  await page.goto('/groups');
  await page.evaluate(async () => {
    const { h, render } = await import('preact');
    const { LaunchTargetDialog } = await import('/static/js/launch-target-dialog.js');
    const hist = (b) => {
      const all = b.history_budget === 'all';
      const keep = b.history_keep?.tool_output;
      const out = keep != null ? Math.round(104000 * keep / 100) : 22000;
      const carried = all ? 96000 + 19000 + out : 76498;
      return {
        source: 'copilot', target: 'claude', source_entries: 267, carried_entries: all ? 267 : 59,
        context_window_tokens: b.target_window_tokens || 200000,
        context_window_source: b.target_window_tokens ? 'stated by the operator' : 'Claude standard context; fable without [1m], so auto-compact 500k has no effect',
        window_stated: !!b.target_window_tokens, detected_window_tokens: b.target_window_tokens ? 200000 : 0,
        detected_window_source: 'Claude standard context; fable without [1m], so auto-compact 500k has no effect',
        budget_tokens: all ? 0 : 120000, budget_source: all ? 'all' : 'auto', carry_all: all,
        source_estimated_tokens: 245215, carried_estimated_tokens: carried,
        over_window: carried > (b.target_window_tokens || 200000), over_headroom: all && carried > 0.6 * (b.target_window_tokens || 200000),
        needs_summary: !all && b.history !== 'truncate', budget_warning: !all, kept_percent: all ? 100 : 22, dropped_percent: all ? 0 : 78,
        selection_reason: all ? '' : 'large history cut: carry a source-written summary with the original task and latest instructions',
        categories: [
          { category: 'chat', source_tokens: 96000, carried_tokens: all ? 96000 : 41000 },
          { category: 'tool_calls', source_tokens: 38000, carried_tokens: 19000, shortened: 211 },
          { category: 'tool_output', source_tokens: 104000, carried_tokens: out, shortened: keep != null ? Math.round(512 * (100 - keep) / 100) : 498, keep_percent: keep },
        ],
        items: [{ kind: 'reasoning_dropped', count: 40 }, { kind: 'tool_records_stubbed', count: 709 }],
      };
    };
    const api = {
      status: async () => ({}),
      profiles: async () => [{ name: 'fable[500k]-med', harness: 'claude', model: 'fable' }],
      morphStatus: async () => ({}),
      morph: async (id, b) => ({
        result: 'dry_run', before: { harness: 'copilot', model: 'gpt-6-astra', approval: 'on-request' },
        after: { harness: 'claude', model: 'fable', effort: 'medium', approval: 'auto', profile: 'fable[500k]-med' },
        history: hist(b), sandbox: { implementation: 'tclaude-layer', tclaude: 'on, unchanged', harness: 'claude auto, stood down inside the tclaude sandbox' },
      }),
    };
    const host = document.createElement('div');
    host.id = 'morph-probe';
    document.body.appendChild(host);
    const self = { id: 'inst_a', name: 'node-a', local: true };
    render(h(LaunchTargetDialog, {
      agent: { id: 'agt_x', conv: 'conv_x', name: 'astra-worker', node: 'inst_a', group: 'tclaude' },
      self, fleet: { self, peers: [] }, initialMode: 'morph', api, actions: {}, onClose: () => {}, toast: () => {},
    }), host);
  });

  const dialog = page.locator('#launch-target');
  await dialog.locator('#lt-morph-profile').click();
  await page.getByRole('option', { name: /fable\[500k\]-med/ }).click();
  const section = dialog.locator('[data-lt-carry-over]');
  await expect(section).toBeVisible();
  await expect(section).toContainText('fable without [1m], so auto-compact 500k has no effect');
  await expect(dialog.locator('#lt-ok')).toHaveText('Request summary and morph');
  await dialog.screenshot({ path: test.info().outputPath('morph-carry-auto.png') });

  await dialog.locator('#lt-carry-window').fill('150k');
  await section.locator('[data-tab-key="all"]').click();
  await section.locator('.lt-carry-adv > summary').click();
  await section.locator('[data-lt-keep="tool_output"]').fill('25');
  await dialog.locator('#lt-carry-method').selectOption('truncate');
  await expect(section.locator('.lt-carry-warn')).toContainText('Not blocked');
  await expect(section.locator('[data-lt-carry-estimate]')).toContainText('window 150k');
  await expect(dialog.locator('[data-lt-carries]')).toContainText('stated; detected 200k');
  await expect(dialog.locator('#lt-ok')).toBeEnabled();
  await dialog.locator('#lt-morph-backup').check();
  await expect(dialog.locator('[data-lt-carries]')).toContainText('Backup');
  await dialog.screenshot({ path: test.info().outputPath('morph-carry-overridden.png') });
  expect(errors).toEqual([]);
});
