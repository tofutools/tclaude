package agentd_test

import (
	"github.com/tofutools/tclaude/pkg/claude/agentd/dashsnap"
)

// savedPromptFixtureJS stubs the read endpoints the saved-prompt, worktree
// signal and spend-limit surfaces load, so their populated states render
// without seeding daemon tables. Writes still reach the real daemon.
const savedPromptFixtureJS = `(function(){
  var realFetch = window.fetch.bind(window);
  function json(body) {
    return Promise.resolve(new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } }));
  }
  window.fetch = function(input, init) {
    var url = new URL(typeof input === 'string' ? input : input.url, location.href);
    var method = ((init && init.method) || 'GET').toUpperCase();
    if (method === 'GET' && url.pathname === '/api/saved-prompts') {
      return json([
        { name: 'cold-review', body: 'Spawn a fresh reviewer and give it the diff plus the PR Background / Purpose.' },
        { name: 'status-report', body: 'Reply with your branch, PR link, CI state and blockers.' },
      ]);
    }
    if (method === 'GET' && url.pathname === '/api/worktrees/cleanup') {
      var day = 86400, now = Math.floor(Date.now() / 1000);
      return json({
        repo_roots: ['/home/dev/git/app'],
        worktrees: [
          { path: '/home/dev/git/app-merged', name: 'app-merged', branch: 'fix-login', repo_root: '/home/dev/git/app', category: 'orphan', checked: true, reason: 'no agent bound; safe to remove', agents: [], base_ref: 'refs/remotes/origin/main', merged: true, unpushed_commits: 0, last_commit: now - 3 * day, last_activity: now - 2 * day, potentially_dangling: true, dangling_reason: 'merged into refs/remotes/origin/main' },
          { path: '/home/dev/git/app-stale', name: 'app-stale', branch: 'spike-cache', repo_root: '/home/dev/git/app', category: 'orphan', checked: false, reason: 'no agent bound; review before deleting; commits absent from upstream/base', agents: [], base_ref: 'refs/remotes/origin/main', merged: false, unpushed_commits: 4, upstream_gone: true, stale: true, last_commit: now - 45 * day, last_activity: now - 40 * day, potentially_dangling: true, dangling_reason: 'no recent activity (30 days)' },
          { path: '/home/dev/git/app-live', name: 'app-live', branch: 'feature-x', repo_root: '/home/dev/git/app', category: 'live', checked: false, reason: 'a live agent works here', agents: [{ agent_id: 'agt_1', conv_id: 'c1', title: 'fe-dev', online: true }], base_ref: 'refs/remotes/origin/main', merged: false, unpushed_commits: 2, last_commit: now - day, last_activity: now - 600, potentially_dangling: false },
          { path: '/home/dev/git/app-detached', name: 'app-detached', branch: '', repo_root: '/home/dev/git/app', category: 'orphan', checked: false, reason: 'detached checkout', agents: [], signal_error: 'Git signals unavailable (missing base or commit)', potentially_dangling: false },
        ],
        prunable_repos: [],
        prune_errors: [],
      });
    }
    if (method === 'GET' && url.pathname === '/api/snapshot') {
      return realFetch(input, init).then(function(response) {
        if (!response.ok) return response;
        return response.clone().json().then(function(body) {
          body.usage = body.usage || {};
          body.usage.spend_limits = [
            { period: 'daily', period_start: '2026-10-15', spend_usd: 42.5, limit_usd: 40, threshold: 100 },
            { period: 'monthly', period_start: '2026-10', spend_usd: 410, limit_usd: 500, threshold: 80 },
          ];
          return new Response(JSON.stringify(body), { status: response.status, headers: response.headers });
        });
      });
    }
    return realFetch(input, init);
  };
})();`

// savedPromptRowCheckJS fails the shot when the picker's select and Manage
// button wrap onto two lines or fall back to the browser's white button chrome.
const savedPromptRowCheckJS = `
  var row = scope.querySelector('.saved-prompts-controls');
  if (!row) throw new Error('saved prompts picker missing');
  var select = row.querySelector('select'), button = row.querySelector('button');
  var sb = select.getBoundingClientRect(), bb = button.getBoundingClientRect();
  if (Math.abs(sb.top - bb.top) > 4) throw new Error('saved prompts picker wraps onto two rows');
  [select, button].forEach(function(el){
    var bg = getComputedStyle(el).backgroundColor;
    if (bg === 'rgb(239, 239, 239)' || bg === 'rgb(255, 255, 255)') throw new Error('saved prompts ' + el.tagName + ' has browser-default chrome ' + bg);
  });
  row.scrollIntoView({ block: 'center' });
`

func waitForJS(selector string) string {
	return `var deadline = Date.now() + 4000;
  while (!document.querySelector('` + selector + `') && Date.now() < deadline) {
    await new Promise(function(resolve){ setTimeout(resolve, 40); });
  }
  if (!document.querySelector('` + selector + `')) throw new Error('missing ` + selector + `');
`
}

func savedPromptAndSpendStates(showGroups, expandGroups string) []dashsnap.State {
	openSpawn := showGroups + expandGroups + `return (async function(){
  var spawn = document.querySelector('[data-act="spawn-agent"]');
  if (!spawn) throw new Error('no group row offers a spawn button');
  spawn.click();
  ` + waitForJS(`#agent-spawn-modal .saved-prompts-controls select option[value="cold-review"]`) + `
  var scope = document.querySelector('#agent-spawn-modal');
  ` + savedPromptRowCheckJS
	return []dashsnap.State{
		{
			Key:      "quickwin-saved-prompts-spawn",
			Title:    "Spawn dialog — saved prompts picker",
			Caption:  "The saved-prompt select and its Manage prompts… button share one dark row under Initial msg.",
			InitJS:   savedPromptFixtureJS,
			JS:       openSpawn + `})();`,
			SettleMS: 500,
		},
		{
			Key:     "quickwin-saved-prompts-manager",
			Title:   "Saved prompts — manager",
			Caption: "The manager opened from the spawn dialog, editing an existing prompt, in the active skin.",
			InitJS:  savedPromptFixtureJS,
			JS: openSpawn + `
  scope.querySelector('.saved-prompts-controls button').click();
  ` + waitForJS(`#saved-prompts-manage-modal select`) + `
  var edit = document.querySelector('#saved-prompts-manage-modal select');
  edit.value = 'cold-review';
  edit.dispatchEvent(new Event('change', { bubbles: true }));
})();`,
			SettleMS: 500,
		},
		{
			Key:     "quickwin-saved-prompts-operator-message",
			Title:   "Operator message — saved prompts picker",
			Caption: "The terminal composer's saved-prompt picker sits on one row and follows the composer's skin.",
			InitJS:  savedPromptFixtureJS,
			JS: `return (async function(){
  var dialogs = await import('/static/js/message-access-dialog-controller.js');
  dialogs.openOperatorMessageDialog({ agent: 'fe-dev-forms', label: 'fe-dev-forms' });
  ` + waitForJS(`#operator-message-modal .saved-prompts-controls select option[value="cold-review"]`) + `
  var scope = document.querySelector('#operator-message-modal');
  ` + savedPromptRowCheckJS + `
})();`,
			SettleMS: 400,
		},
		{
			Key:     "quickwin-worktree-cleanup-dangling",
			Title:   "Worktree cleanup — dangling signals",
			Caption: "Merged, unpublished, upstream-gone, stale and unavailable-signal badges, the dangling reason notes, and the pressed Potentially dangling filter.",
			InitJS:  savedPromptFixtureJS,
			JS: `return (async function(){
  var cleanup = await import('/static/js/worktree-cleanup-controller.js');
  cleanup.openWorktreeCleanup('');
  ` + waitForJS(`#worktree-cleanup-modal .cleanup-badge.merged`) + `
  var filter = document.querySelector('#worktree-cleanup-modal .cleanup-dangling-filter button');
  filter.click();
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  if (filter.getAttribute('aria-pressed') !== 'true') throw new Error('dangling filter did not toggle');
})();`,
			SettleMS: 400,
		},
		{
			Key:      "quickwin-spend-limit-banner",
			Title:    "Spend limits — banner",
			Caption:  "A crossed daily limit and a monthly 80% warning in the shell banner above the tabs.",
			InitJS:   savedPromptFixtureJS,
			JS:       `return (async function(){ ` + waitForJS(`.spend-limit-banner`) + ` })();`,
			SettleMS: 300,
		},
		{
			Key:      "quickwin-config-spend-limits",
			Title:    "Config — Costs panel spend limits",
			Caption:  "The Daily/Monthly limit rows use the shared Config setting chrome.",
			JS:       boundedTabJS("config", "#cfg-save", configPanelJS("costs", "#cfg-cost-daily-limit")),
			SettleMS: 500,
		},
	}
}
