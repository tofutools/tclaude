import test from 'node:test'; import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const TICKET = `${'a'.repeat(32)}.${'b'.repeat(32)}@inst_forge7`;
const peers = [{ id: 'inst_forge7', label: 'forge', online: true }, { id: 'inst_lab2', label: 'lab', online: false }];

test('peer operator mail is recognised, and a forwarded cover request yields its ticket', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/peer-mail.js');
  const msg = { group: 'federation:inst_forge7', from_conv: '', subject: 'cover request', body: `Away access request from x\nOne-shot answer: tclaude federation answer ${TICKET} --decision approve\n` };
  assert.equal(m.peerOfMessage(msg), 'inst_forge7');
  assert.equal(m.awayTicket(msg), TICKET);
  assert.equal(m.peerOfMessage({ ...msg, from_conv: 'conv-1' }), '', 'agent mail is not peer operator mail');
  assert.equal(m.awayTicket({ ...msg, body: 'tclaude federation answer x; rm -rf / --decision' }), '');
  assert.equal(m.replySubject('Release'), 'Re: Release');
  assert.equal(m.replySubject('RE: Release'), 'RE: Release');
});

test('the mail dialog notifies a peer operator or sends to agents, and shows the outbox', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/peer-mail.js');
  const calls = []; const toasts = [];
  const actions = {
    peers: async () => peers,
    notify: async (o) => { calls.push(['notify', o]); return { to: 'operator@forge', state: 'queued', hub_connected: true }; },
    send: async (o) => { calls.push(['send', o]); return { to: 'ops@forge', state: 'queued' }; },
    outbox: async () => [{ envelope_id: 'e1', to: 'operator@lab', subject: 'hi', state: 'pending', attempts: 3, last_error: 'peer offline', updated_at: '2026-10-10T09:00:00Z' }],
  };
  const mounted = await harness.mount(harness.html`<${m.PeerMailDialog} initial=${{ peer: 'inst_forge7', subject: 'Re: Release' }} actions=${actions} toast=${(x, e) => toasts.push([x, e])} onClose=${() => {}} />`);
  const doc = harness.document; const q = (s) => doc.querySelector(s);
  const settle = () => harness.act(() => new Promise((r) => setTimeout(r, 25)));
  await settle();
  assert.match(q('#peer-mail-outbox').textContent, /operator@lab.*pending · 3 tries/s);
  const body = q('#peer-mail-body'); body.value = 'Freeze at 18:00';
  await harness.act(() => harness.fireEvent(body, 'input'));
  await harness.act(() => q('#peer-mail-send').click());
  await settle();
  assert.deepEqual(calls[0], ['notify', { peer: 'inst_forge7', subject: 'Re: Release', body: 'Freeze at 18:00' }]);
  const agents = [...doc.querySelectorAll('input[name=peer-mail-kind]')].find((r) => r.value === 'agents');
  agents.checked = true;
  await harness.act(() => harness.fireEvent(agents, 'change'));
  const to = q('#peer-mail-to'); to.value = 'group:ops@forge';
  await harness.act(() => harness.fireEvent(to, 'input'));
  const role = q('#peer-mail-role'); role.value = 'reviewer';
  await harness.act(() => harness.fireEvent(role, 'input'));
  body.value = 'Please review';
  await harness.act(() => harness.fireEvent(q('#peer-mail-body'), 'input'));
  await harness.act(() => q('#peer-mail-send').click());
  await settle();
  assert.deepEqual(calls[1], ['send', { to: 'group:ops@forge', role: 'reviewer', subject: 'Re: Release', body: 'Please review' }]);
  assert.equal(toasts.length, 2);
  await mounted.unmount();
});

test('away: choose a cover with the consequence spelled out, return, and answer a forwarded request once', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/peer-mail.js');
  let away = null; const calls = []; const confirms = []; const toasts = [];
  const actions = {
    peers: async () => peers,
    away: async () => ({ away }),
    setAway: async (o) => { calls.push(['away', o]); away = { cover: o.cover, since: '2026-10-10T09:00:00Z' }; return { away, warnings: ['covering peer is offline'] }; },
    returnHome: async () => { calls.push(['return']); away = null; return { away: null }; },
    answer: async (tk, d) => { calls.push(['answer', tk, d]); return { envelope_id: 'e', state: 'queued' }; },
  };
  const confirm = async (o) => { confirms.push(o); return o.action ? o.action() : true; };
  const settle = () => harness.act(() => new Promise((r) => setTimeout(r, 25)));
  const mounted = await harness.mount(harness.html`<div><${m.AwayControl} actions=${actions} confirm=${confirm} toast=${(x, e) => toasts.push([x, e])} /><${m.AwayAnswer} ticket=${TICKET} peerLabel="forge" actions=${actions} confirm=${confirm} /></div>`);
  const doc = harness.document; const q = (s) => doc.querySelector(s);
  await settle();
  assert.match(q('#fleet-away').textContent, /^away…$/);
  await harness.act(() => q('#fleet-away-open').click());
  for (const o of q('#fleet-away-cover').querySelectorAll('option')) { if (o.value === 'inst_lab2') o.setAttribute('selected', ''); else o.removeAttribute('selected'); }
  await harness.act(() => harness.fireEvent(q('#fleet-away-cover'), 'change'));
  await harness.act(() => q('#fleet-away-go').click());
  await settle();
  assert.match(confirms[0].body, /forwarded to lab's operator, who may approve or deny each one once.*Peer operators' access requests are never forwarded/);
  assert.deepEqual(calls[0], ['away', { cover: 'inst_lab2', until: '' }]);
  assert.deepEqual(toasts[0], ['Away: lab covers — covering peer is offline', true]);
  await settle();
  assert.match(q('#fleet-away').textContent, /away: lab return…/);
  await harness.act(() => q('#fleet-away-return').click());
  await settle();
  assert.deepEqual(calls[1], ['return']);
  await settle();
  assert.ok(q('#fleet-away-open'));
  await harness.act(() => q('[data-act=away-approve]').click());
  await settle();
  assert.match(confirms.at(-1).body, /answer once, on forge's operator's behalf.*grants nothing lasting/);
  assert.deepEqual(calls.at(-1), ['answer', TICKET, 'approve']);
  await mounted.unmount();
});
