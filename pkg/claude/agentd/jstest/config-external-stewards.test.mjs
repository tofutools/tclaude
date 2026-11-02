import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const CREDENTIAL = 'tcls_abc_secretsecretsecret';

function daemon(initial = []) {
  const calls = [];
  const stewards = [...initial];
  const fetchImpl = async (path, options = {}) => {
    calls.push({ path, body: options.body ? JSON.parse(options.body) : null });
    const reply = (status, body) => ({ ok: status < 400, status, json: async () => body });
    if (!options.method) return reply(200, { external_stewards: stewards });
    if (path === '/api/external-stewards') {
      const { name } = JSON.parse(options.body);
      if (name === 'taken') return reply(409, { error: 'that name is taken' });
      stewards.push({ agent_id: `agt_${name}`, name, credentials: [{ token_id: 't1', live: true, expires_at: '2099-01-01T00:00:00Z' }] });
      return reply(200, { name, credential: CREDENTIAL });
    }
    if (path.endsWith('/renew')) return reply(200, { credential: `${CREDENTIAL}2` });
    return reply(200, {});
  };
  return { calls, fetchImpl };
}

async function setup(t, { stewards, enabled = true, confirm = async () => true } = {}) {
  const harness = await createPreactHarness(t);
  const { ExternalStewardManager } = await harness.importDashboardModule('js/config-external-stewards.js');
  const fake = daemon(stewards);
  const mounted = await harness.mount(harness.html`<${ExternalStewardManager} enabled=${enabled}
    fetchImpl=${fake.fetchImpl} confirm=${confirm} copy=${async () => true} />`);
  await harness.act(async () => { await Promise.resolve(); });
  const button = (label) => [...mounted.container.querySelectorAll('button')].find(b => b.textContent === label);
  const settle = async (fn) => {
    await harness.act(async () => { await fn(); });
    await harness.act(async () => { await new Promise(resolve => setTimeout(resolve)); });
  };
  const add = async (name) => {
    const input = mounted.container.querySelector('input[type="text"]');
    await settle(() => {
      input.value = name;
      harness.fireEvent(input, 'input');
    });
    await settle(() => harness.fireEvent(mounted.container.querySelector('form'), 'submit', { cancelable: true }));
  };
  return { ...fake, harness, mounted, button, settle, add };
}

const row = { agent_id: 'agt_a', name: 'assistant', credentials: [{ token_id: 't1', live: true, expires_at: '2099-01-01T00:00:00Z' }] };

test('a new credential is shown once, inside the capture mask, until Done', async (t) => {
  const { harness, mounted, add, button, settle } = await setup(t);
  await add('assistant');
  const secret = mounted.container.querySelector('[data-capture-mask]');
  assert.equal(secret?.textContent, CREDENTIAL);
  assert.match(mounted.container.textContent, /can't be shown again/);
  assert.equal(mounted.container.textContent.split(CREDENTIAL).length, 2, 'it appears once and only in the mask');
  const { CAPTURE_MASK, snapshotPage, snapshotText } = await harness.importDashboardModule('js/capture-snapshot.js');
  const captured = snapshotText(snapshotPage(mounted.container.ownerDocument.defaultView));
  assert.ok(!captured.includes('tcls_'), 'a Steward capturing this page does not get it');
  assert.ok(captured.includes(CAPTURE_MASK));
  await settle(() => button('Done').click());
  assert.ok(!mounted.container.textContent.includes(CREDENTIAL));
  assert.match(mounted.container.textContent, /assistant/, 'the list refreshed');
  await mounted.unmount();
});

test('revoke asks first, and a no sends nothing', async (t) => {
  let answer = false;
  const { calls, mounted, button, settle } = await setup(t, { stewards: [row], confirm: async () => answer });
  await settle(() => button('Revoke').click());
  assert.equal(calls.filter(call => call.path.endsWith('/revoke')).length, 0);
  answer = true;
  await settle(() => button('Revoke').click());
  assert.equal(calls.filter(call => call.path === '/api/external-stewards/assistant/revoke').length, 1);
  await mounted.unmount();
});

test('renew shows the new credential once', async (t) => {
  const { mounted, button, settle } = await setup(t, { stewards: [row] });
  await settle(() => button('Renew').click());
  assert.equal(mounted.container.querySelector('[data-capture-mask]')?.textContent, `${CREDENTIAL}2`);
  await mounted.unmount();
});

test('a credential on screen cannot be replaced by another', async (t) => {
  const { calls, mounted, add, button, settle } = await setup(t, { stewards: [row] });
  await settle(() => button('Renew').click());
  assert.equal(button('Renew').disabled, true);
  assert.equal(button('Add').disabled, true);
  await add('second');
  assert.equal(calls.filter(call => call.path === '/api/external-stewards' && call.body).length, 0, 'Enter in the form sends nothing');
  assert.equal(mounted.container.querySelector('[data-capture-mask]')?.textContent, `${CREDENTIAL}2`);
  await settle(() => button('Done').click());
  assert.equal(button('Renew').disabled, false);
  await mounted.unmount();
});

test('Copied does not carry over to the next credential', async (t) => {
  const { mounted, button, settle } = await setup(t, { stewards: [row] });
  await settle(() => button('Renew').click());
  await settle(() => button('Copy').click());
  assert.ok(button('Copied'));
  await settle(() => button('Done').click());
  await settle(() => button('Renew').click());
  assert.ok(button('Copy'));
  await mounted.unmount();
});

test('a refusal shows, and the name stays to fix', async (t) => {
  const { mounted, add } = await setup(t);
  await add('taken');
  assert.equal(mounted.container.querySelector('[role="alert"]')?.textContent, 'that name is taken');
  assert.equal(mounted.container.querySelector('input[type="text"]').value, 'taken');
  await mounted.unmount();
});

test('while off, nothing can be added and revoke still works', async (t) => {
  const { mounted, button } = await setup(t, { stewards: [row], enabled: false });
  assert.equal(mounted.container.querySelector('form'), null);
  assert.equal(button('Revoke').disabled, false);
  assert.match(mounted.container.textContent, /still revoke/);
  assert.equal(button('Renew').disabled, true, 'the daemon refuses a renewal while off');
  await mounted.unmount();
});

test('the roster row has no resume and says what it is', async (t) => {
  const harness = await createPreactHarness(t);
  await harness.replaceDashboardModule('js/dashboard.js', `
    export const lastSnapshot = { groups: [], ungrouped: [] };
    export function setLastSnapshot() {}
  `);
  const { HarnessLine } = await harness.importDashboardModule('js/groups-member-table.js');
  const mounted = await harness.mount(harness.html`<${HarnessLine} member=${{ conv_id: 'ext-1', external_steward: true }} />`);
  assert.match(mounted.container.textContent, /External steward/);
  await mounted.unmount();
});
