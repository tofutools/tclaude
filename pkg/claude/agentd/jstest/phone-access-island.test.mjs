// js/phone-access-island.js: the Access tab on a phone, over the real access
// state and recorded actions, driven through what the operator taps.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function prefs() {
  const values = new Map();
  return {
    getItem: (key) => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, String(value)),
    removeItem: (key) => values.delete(key),
  };
}

const NOW = Date.parse('2026-11-14T12:00:00Z');

function snapshotWith({ requests = [], slugs = 3, uses = null, deniedDefault = '' } = {}) {
  return {
    generated_at: new Date(NOW).toISOString(),
    agents: [
      { agent_id: 'agt_lead', conv_id: 'c-lead', title: 'lead', online: true, groups: ['webshop'] },
      { agent_id: 'agt_old', conv_id: 'c-old', title: 'old-bot', online: false, groups: ['webshop'] },
    ],
    // deniedDefault: a slug every agent holds by default, which lead is denied.
    permissions: deniedDefault
      ? { defaults: ['self.rename', 'self.task', deniedDefault], overrides: { 'c-lead': { 'agent.spawn': 'grant', 'git.push': 'deny', [deniedDefault]: 'deny' } } }
      : { defaults: ['self.rename', 'self.task'], overrides: { 'c-lead': { 'agent.spawn': 'grant', 'git.push': 'deny' } } },
    slugs: Array.from({ length: slugs }, (_, i) => ({ slug: `slug.${String(i).padStart(3, '0')}`, description: `does ${i}`, countable: i !== 2, risky: i === 1 })),
    sudo: [{
      id: 7, agent_id: 'agt_lead', conv_id: 'c-lead', conv_title: 'lead', slug: 'human.notify', granted_by: 'operator',
      granted_at: new Date(NOW - 600000).toISOString(), expires_at: new Date(NOW + 3000000).toISOString(), reason: 'UI review',
      ...(uses ? { uses_limit: uses[1], uses_left: uses[0] } : {}),
    }],
    access_requests: requests,
  };
}

async function setup(t, options = {}) {
  const harness = await createPreactHarness(t);
  const [island, { createAccessState }] = await Promise.all([
    harness.importDashboardModule('js/phone-access-island.js'),
    harness.importDashboardModule('js/access-state.js'),
  ]);
  const { signal } = harness.signals;
  const snapshot = signal(options.loaded === false ? null : snapshotWith(options));
  const state = createAccessState({ snapshot, prefs: prefs(), now: () => NOW });
  state.initialize();
  // grantFor: an agent's menu asked for its Grant sudo… before Access mounted.
  if (options.grantFor) (await harness.importDashboardModule('js/phone-grant.js')).grantFor.value = options.grantFor;
  const calls = [];
  const actions = {
    revoke: async (grant) => { calls.push(['revoke', grant.id]); return true; },
    openGrant: () => { calls.push(['openGrant']); },
  };
  const posts = [];
  const fetchImpl = async (url, init) => { posts.push([String(url), JSON.parse(init.body)]); return { ok: true, status: 200 }; };
  const grants = [];
  const grantActions = {
    sudoPolicy: async () => options.policy || null,
    grantSudo: async (body) => { grants.push(body); if (options.grantFails) throw new Error(options.grantFails); return {}; },
  };
  const navigations = [];
  harness.document.addEventListener('tclaude:navigated', (event) => navigations.push(event.detail.location));
  const mounted = await harness.mount(harness.html`<${island.PhoneAccessApp} state=${state} actions=${actions} snapshot=${snapshot}
    fetchImpl=${fetchImpl} openInMail=${(id) => calls.push(['openInMail', id])} grantActions=${grantActions} />`);
  const root = mounted.container;
  const tab = (name) => [...root.querySelectorAll('[role="tab"]')].find((element) => element.textContent.startsWith(name));
  const tap = (element) => harness.act(() => element.click());
  const settle = async () => {
    for (let i = 0; i < 4; i++) await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
  };
  return { harness, island, state, snapshot, calls, posts, grants, navigations, root, tab, tap, settle };
}

test('a waiting request leads, answered with the same card as Now', async (t) => {
  const requests = [
    { id: 'r1', perm: 'git.push', agent_id: 'agt_lead', conv_title: 'lead', status: 'pending' },
    { id: 'r2', perm: 'y', agent_id: 'agt_lead', status: 'pending', origin_peer: 'node-2' },
  ];
  const { harness, root, tab, posts, calls, tap, settle } = await setup(t, { requests });
  assert.equal(tab('Requests').getAttribute('aria-selected'), 'true');
  assert.equal(tab('Requests').querySelector('.ui-badge').textContent, '2');
  const waiting = root.querySelector('[role="group"][aria-label="Waiting on you"]');
  const cards = waiting.querySelectorAll('.phone-box-key');
  assert.equal(cards.length, 1, 'a peer\'s request is not approved blind');
  assert.deepEqual([...cards[0].querySelectorAll('.phone-btns button')].map((b) => b.textContent.trim()), ['Deny', '+5 min', 'Approve'],
    'the phone card, as on Now');
  await harness.act(() => harness.getByRole(cards[0], 'button', { name: /Approve/ }).click());
  await settle();
  assert.deepEqual(posts.at(-1)[1], { decision: 'approve' });
  await tap([...root.querySelectorAll('.ui-btn')].find((button) => /1 more access request in Mail/.test(button.textContent)));
  assert.deepEqual(calls.at(-1), ['openInMail', undefined]);
  const sudo = root.querySelector('[role="group"][aria-label="Sudo grants"]');
  assert.match(sudo.querySelector('.phone-section-head').textContent, /Sudo grants · 1/, 'the grants running, under the requests');
  await tap(sudo.querySelector('[aria-label="Revoke human.notify from lead"]'));
  assert.deepEqual(calls.at(-1), ['revoke', 7]);
});

test('+ Grant sits in the top bar, whichever view is open, and opens the phone grant page', async (t) => {
  const { harness, root, tab, tap } = await setup(t);
  const { phoneBar, phonePage } = await harness.importDashboardModule('js/phone-page.js');
  const grant = () => phoneBar.value?.actions?.find((act) => act.key === 'grant');
  assert.equal(phoneBar.value.tab, 'access');
  assert.equal(grant().text, '+ Grant');
  await tap(tab('Slugs'));
  await harness.act(() => grant().onClick());
  assert.equal(phonePage.value.title, 'Grant sudo', 'step 1, a page of its own');
  assert.equal(phonePage.value.hideNav, true);
  assert.equal(root.querySelector('.add-member-modal'), null, 'not the desktop picker');
  await harness.act(() => phonePage.value.onBack());
  assert.ok(tab('Slugs'), 'Back leaves it');
  assert.equal(phonePage.value, null);
});

// openGrant opens + Grant's page and returns helpers over it.
async function openGrant(t, options) {
  const env = await setup(t, options);
  const { harness, root, tap } = env;
  const { phoneBar, phonePage } = await harness.importDashboardModule('js/phone-page.js');
  await harness.act(() => phoneBar.value.actions.find((act) => act.key === 'grant').onClick());
  const submit = () => root.querySelector('.phone-grant-submit');
  const box = (slug) => root.querySelector(`.phone-grant-row[data-slug="${slug}"] input`);
  const pressSeg = (label, text) => tap([...root.querySelector(`.phone-grant-seg[aria-label="${label}"]`).querySelectorAll('button')]
    .find((b) => b.textContent.trim() === text));
  const type = (element, value) => harness.act(() => { element.value = value; element.dispatchEvent(new harness.window.Event('input', { bubbles: true })); });
  const check = (slug) => harness.act(() => { box(slug).click(); });
  return { ...env, phonePage, submit, box, pressSeg, type, check };
}

test('Grant, step 1: the agents by group, online first; offline ones on request', async (t) => {
  const { root, tap, settle, phonePage } = await openGrant(t);
  assert.deepEqual([...root.querySelectorAll('.phone-grant-agent')].map((b) => b.dataset.agent), ['agt_lead']);
  assert.match(root.querySelector('[role="group"][aria-label="webshop"]').textContent, /lead/);
  const offline = [...root.querySelectorAll('.ui-btn')].find((b) => /Show 1 offline agent/.test(b.textContent));
  await tap(offline);
  assert.deepEqual([...root.querySelectorAll('.phone-grant-agent')].map((b) => b.dataset.agent), ['agt_lead', 'agt_old']);
  await tap(root.querySelector('[data-agent="agt_lead"]'));
  await settle();
  assert.equal(phonePage.value.title, 'Grant to lead');
  await tap({ click: () => phonePage.value.onBack() });
  assert.equal(phonePage.value.title, 'Grant sudo', 'Back from step 2 is step 1');
});

test('an agent\'s Grant sudo… opens Access on that agent\'s step 2; Back is step 1', async (t) => {
  const { harness, root, settle } = await setup(t, { grantFor: 'agt_lead' });
  await settle();
  const [{ phonePage }, { grantFor }] = await Promise.all([
    harness.importDashboardModule('js/phone-page.js'),
    harness.importDashboardModule('js/phone-grant.js'),
  ]);
  assert.equal(phonePage.value.title, 'Grant to lead');
  assert.ok(root.querySelector('.phone-grant-submit'), 'step 2, the permissions');
  assert.equal(grantFor.value, '', 'taken, so coming back to Access later is Access');
  await harness.act(() => phonePage.value.onBack());
  await settle();
  assert.equal(phonePage.value.title, 'Grant sudo');
});

test('Grant, step 2: the warning, nothing sent until a permission is picked, the limits in the label', async (t) => {
  const { root, tap, settle, submit, check, pressSeg, type, grants, tab, box } = await openGrant(t, { slugs: 4 });
  await tap(root.querySelector('[data-agent="agt_lead"]'));
  await settle();
  assert.match(root.querySelector('.phone-grant-warn').textContent, /Temporary.*Sudo wins over an explicit Deny/);
  assert.equal(submit().disabled, true, 'off until a permission is picked');
  assert.match(root.querySelector('.phone-grant-why').textContent, /Pick at least one permission/);
  await check('slug.001');
  assert.equal(submit().disabled, false);
  assert.equal(submit().textContent.trim(), 'Grant 1 · 5 min', 'the policy\'s default time');
  assert.match(root.querySelector('.phone-grant-picked').textContent, /slug\.001/);
  await pressSeg('Time limit', '15 min');
  await pressSeg('Max uses', '3');
  assert.equal(submit().textContent.trim(), 'Grant 1 · 15 min · 3 uses');

  // "…" opens a value of one's own.
  await pressSeg('Max uses', '…');
  const custom = root.querySelector('[aria-label="Custom use limit"].phone-grant-custom');
  assert.ok(custom, 'a field for a custom use limit');
  await type(custom, '2000');
  assert.equal(submit().disabled, true);
  assert.match(root.querySelector('.phone-grant-why').textContent, /whole number from 1 to 1000/);
  await type(custom, '7');
  await check('slug.002');
  assert.match(root.querySelector('.phone-grant-note').textContent, /1 picked takes no use limit/);
  await type(root.querySelector('.phone-grant-reason'), ' release check ');
  await tap(submit());
  await settle();
  assert.deepEqual(grants, [{ agentID: 'agt_lead', slugs: ['slug.001', 'slug.002'], duration: '15m', reason: 'release check', uses: { 'slug.001': 7 } }],
    'a slug with nothing to count goes without a use limit');
  assert.equal(root.querySelector('.phone-grant'), null, 'the page closes');
  assert.equal(tab('Sudo').getAttribute('aria-selected'), 'true', 'on Sudo, where the grant now runs');
});

test('Grant, step 2: the policy caps the time, refuses a slug, and a failed grant stays open', async (t) => {
  const policy = { max_seconds: 900, default_seconds: 600, refused: { 'slug.000': 'Refused: it hands out permissions itself.' } };
  const { root, tap, settle, submit, check, pressSeg, type, box, grants } = await openGrant(t, { slugs: 4, policy, grantFails: 'agent is gone', deniedDefault: 'slug.003' });
  await tap(root.querySelector('[data-agent="agt_lead"]'));
  await settle();
  assert.deepEqual([...root.querySelector('.phone-grant-seg[aria-label="Time limit"]').querySelectorAll('button')].map((b) => b.textContent.trim()),
    ['5 min', '15 min', '…'], 'no preset over the maximum');
  assert.equal(box('slug.000').disabled, true);
  assert.match(root.querySelector('[data-slug="slug.000"]').textContent, /Refused/);
  await check('slug.003');
  assert.equal(submit().textContent.trim(), 'Grant 1 · 10 min', 'the policy\'s default');
  assert.equal(root.querySelector('.phone-grant-seg[aria-label="Time limit"] [aria-pressed="true"]').textContent, '…',
    'a default no preset shows is the custom value');
  assert.equal(root.querySelector('[aria-label="Custom time limit"].phone-grant-custom').value, '10m');
  await pressSeg('Max uses', '1');
  await tap(submit());
  await settle();
  assert.deepEqual(grants.at(-1).uses, { 'slug.003': 1 }, 'a default the agent is denied is not held: it takes the use limit');
  await pressSeg('Time limit', '…');
  await type(root.querySelector('[aria-label="Custom time limit"].phone-grant-custom'), '1h');
  assert.equal(submit().disabled, true);
  assert.match(root.querySelector('.phone-grant-why').textContent, /longest this agent may have is 15 min/);
  await pressSeg('Time limit', '5 min');
  await tap(submit());
  await settle();
  assert.match(root.querySelector('.phone-grant [role="alert"]').textContent, /agent is gone/);
  assert.equal(submit().disabled, false, 'free to try again');
});

test('grantAgents and grantPlan', async (t) => {
  const { harness } = await setup(t);
  const { grantAgents, grantPlan, usesValid } = await harness.importDashboardModule('js/phone-grant.js');
  const snapshot = { agents: [
    { agent_id: 'b', title: 'beta', online: true, groups: ['web'] }, { agent_id: 'a', title: 'alpha', online: false, groups: ['web'] },
    { agent_id: 'c', title: 'gamma', online: true }, { conv_id: 'no-agent', title: 'conv only', online: true },
  ] };
  assert.deepEqual(grantAgents(snapshot, { offline: true }).map((g) => [g.group, g.agents.map((a) => a.agent_id)]), [['web', ['b', 'a']], ['No group', ['c']]]);
  assert.deepEqual(grantAgents(snapshot, { query: 'web' }).map((g) => g.group), ['web'], 'a search matches the group too');
  assert.deepEqual(['', '1', '1000', '0', '1001', '2.5', 'x'].map(usesValid), [true, true, true, false, false, false, false]);
  const rows = [{ slug: 'a', countable: true }, { slug: 'b', countable: true, granted: true }];
  const plan = grantPlan({ agentID: 'agt', picked: rows, duration: '1h30m', uses: '2', reason: '' });
  assert.equal(plan.ready, false, 'over the default maximum of an hour');
  const ok = grantPlan({ agentID: 'agt', picked: rows, duration: '30m', uses: '1', reason: 'r' });
  assert.equal(ok.label, 'Grant 2 · 30 min · 1 use');
  assert.deepEqual(ok.body, { agentID: 'agt', slugs: ['a', 'b'], duration: '30m', reason: 'r', uses: { a: 1 } }, 'what it already holds takes no count');
});

test('a sudo grant with a use limit shows the uses left beside the time left', async (t) => {
  const { root, tab, tap } = await setup(t, { uses: [2, 3] });
  await tap(tab('Sudo'));
  const row = root.querySelector('.phone-access-row[data-key="sudo-7"]');
  assert.match(row.querySelector('.phone-access-left').textContent, /^50m left · 2 of 3 uses left · "UI review"$/);
});

test('with nothing waiting it opens on the routed subtab; Sudo revokes and grants', async (t) => {
  const { root, tab, calls, navigations, tap } = await setup(t);
  assert.equal(tab('Agents').getAttribute('aria-selected'), 'true', 'the access state\'s default subtab');
  await tap(tab('Sudo'));
  assert.deepEqual(navigations.at(-1), { tab: 'access', subtab: 'sudo' });
  const row = root.querySelector('.phone-access-row[data-key="sudo-7"]');
  assert.match(row.textContent, /human\.notify/);
  assert.match(row.querySelector('.phone-access-left').textContent, /^50m left · "UI review"$/);
  const revoke = row.querySelector('[aria-label="Revoke human.notify from lead"]');
  assert.ok(revoke.matches('.phone-btn.phone-btn-no'), 'a bordered red button, not a text link');
  await tap(revoke);
  assert.deepEqual(calls.at(-1), ['revoke', 7]);
  await tap(tab('Requests'));
  assert.match(root.textContent, /No access request is waiting/);
  assert.deepEqual(navigations.at(-1), { tab: 'access' }, 'Requests is the bare /access');
});

test('a route that names a subtab wins over the Requests default', async (t) => {
  const requests = [{ id: 'r1', perm: 'git.push', agent_id: 'agt_lead', status: 'pending' }];
  const { harness, state, tab } = await setup(t, { requests });
  assert.equal(tab('Requests').getAttribute('aria-selected'), 'true');
  // nav-history.js restores /access/slugs by clicking the subtab.
  await harness.act(() => tab('Slugs').click());
  assert.equal(state.subtab.value, 'slugs');
  assert.equal(tab('Slugs').getAttribute('aria-selected'), 'true');
  assert.ok(tab('Slugs').matches('.access-subtab[data-subtab="slugs"]'));
  await harness.act(() => state.setSubtab('sudo'));
  assert.equal(tab('Sudo').getAttribute('aria-selected'), 'true');
});

test('Agents lists the overrides read-only and points editing at the desktop', async (t) => {
  const { root, tab, tap } = await setup(t);
  await tap(tab('Agents'));
  const defaults = root.querySelector('.phone-access-defaults');
  assert.match(defaults.textContent, /Everyone: 2 default slugs/);
  assert.equal(root.querySelector('.phone-access-default-tags'), null, 'the defaults are one line until opened');
  await tap(defaults);
  assert.equal(defaults.getAttribute('aria-expanded'), 'true');
  assert.deepEqual([...root.querySelectorAll('.phone-access-default-tags .phone-access-slug')].map((tag) => tag.textContent), ['self.rename', 'self.task']);
  const row = root.querySelector('.phone-access-row[data-key="c-lead"]');
  assert.deepEqual([...row.querySelectorAll('.phone-access-slug')].map((tag) => tag.textContent), ['+ agent.spawn', '− git.push']);
  assert.equal(row.querySelector('button'), null);
  assert.ok(row.querySelector('.phone-access-slug.deny'), 'a denial is the red chip');
  assert.match(root.querySelector('.phone-desktop-only').textContent, /desktop only/);
});

test('Slugs shows the first page until searched or asked for all', async (t) => {
  const { harness, island, root, tab, tap } = await setup(t, { slugs: 55 });
  await tap(tab('Slugs'));
  assert.equal(root.querySelectorAll('.phone-access-row').length, island.SLUGS_SHOWN);
  const search = root.querySelector('.phone-access-search');
  await harness.act(() => { search.value = 'slug.05'; harness.fireEvent(search, 'input'); });
  assert.deepEqual([...root.querySelectorAll('.phone-access-row')].map((row) => row.dataset.key), [
    'slug.050', 'slug.051', 'slug.052', 'slug.053', 'slug.054',
  ]);
  await harness.act(() => { search.value = ''; harness.fireEvent(search, 'input'); });
  await tap([...root.querySelectorAll('.ui-btn')].find((button) => /Show all 55 slugs/.test(button.textContent)));
  assert.equal(root.querySelectorAll('.phone-access-row').length, 55);
});

test('before the first snapshot it says it is loading, then picks Requests if any wait', async (t) => {
  const { harness, root, snapshot, tab } = await setup(t, { loaded: false });
  assert.match(root.textContent, /Loading/);
  const { phoneBar } = await harness.importDashboardModule('js/phone-page.js');
  assert.equal(phoneBar.value?.actions?.length ?? 0, 0, 'no + Grant before there is anyone to grant to');
  await harness.act(() => { snapshot.value = snapshotWith({ requests: [{ id: 'r1', perm: 'x', agent_id: 'agt_lead', status: 'pending' }] }); });
  assert.equal(tab('Requests').getAttribute('aria-selected'), 'true');
  assert.equal(phoneBar.value.actions[0].text, '+ Grant');
});

test('a subtab picked before the first snapshot is not overridden by a waiting request', async (t) => {
  const { harness, snapshot, tab } = await setup(t, { loaded: false });
  // nav-history restores /access/sudo by clicking Sudo, which can land
  // before the effect that would pick Requests has run.
  await harness.act(() => {
    tab('Sudo').click();
    snapshot.value = snapshotWith({ requests: [{ id: 'r1', perm: 'x', agent_id: 'agt_lead', status: 'pending' }] });
  });
  assert.equal(tab('Sudo').getAttribute('aria-selected'), 'true');
});
