// js/phone-compose-sheet.js: the phone's message sheet, driven through what
// the operator taps and types: it sends the draft, inserts a saved prompt at
// the caret, and asks before a draft is lost to Escape, the scrim, Cancel or
// a swipe down on the grab handle.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const AGENT = 'agt_00000000000000000000000000000001';

async function setup(t, { send = null, prompts = [] } = {}) {
  const harness = await createPreactHarness(t);
  const realFetch = globalThis.fetch;
  const fetched = [];
  globalThis.fetch = async (url) => {
    fetched.push(String(url));
    return new Response(JSON.stringify(prompts), { status: 200, headers: { 'Content-Type': 'application/json' } });
  };
  t.after(() => { globalThis.fetch = realFetch; });
  const [compose, sheet] = await Promise.all([
    harness.importDashboardModule('js/phone-compose-sheet.js'),
    harness.importDashboardModule('js/phone-sheet.js'),
  ]);
  t.after(() => sheet.closePhoneSheet());
  const sent = [];
  const mounted = await harness.mount(harness.html`<${sheet.PhoneSheetHost} />`);
  const open = () => harness.act(() => compose.openPhoneCompose({ agent: AGENT, label: 'checkout-ui' }, {
    send: send || (async (draft) => { sent.push(draft); }),
  }));
  const dialog = () => mounted.container.querySelector('[role="dialog"]');
  const field = () => dialog().querySelector('textarea');
  const button = (name) => harness.getByRole(dialog(), 'button', { name });
  const tap = (element) => harness.act(() => element.click());
  const escape = () => harness.act(() => {
    const event = new harness.window.Event('keydown', { bubbles: true });
    Object.defineProperty(event, 'key', { value: 'Escape' });
    harness.document.dispatchEvent(event);
  });
  return { harness, compose, sheet, mounted, open, dialog, field, button, tap, escape, sent, fetched };
}

test('the sheet titles its agent, focuses the text, and sends the draft through the mailbox', async (t) => {
  const { harness, open, dialog, field, button, tap, sent } = await setup(t);
  await open();
  assert.equal(dialog().getAttribute('aria-label'), 'Message checkout-ui');
  assert.equal(dialog().querySelector('.phone-compose-title').textContent, 'Message checkout-ui');
  assert.equal(harness.document.activeElement, field(), 'the text field takes focus, not 📎');
  assert.match(dialog().querySelector('.phone-compose-note').textContent, /queued if it's busy/);

  await tap(button('Send ➤'));
  assert.equal(sent.length, 0, 'nothing to send');
  assert.match(dialog().querySelector('[role="alert"]').textContent, /Write a message/);

  await harness.input(field(), 'Yes, migrate checkout');
  await tap(button('Send ➤'));
  await harness.act(async () => {});
  assert.deepEqual(sent.map((d) => ({ to: d.to, body: d.body, files: d.files.length })), [{ to: AGENT, body: 'Yes, migrate checkout', files: 0 }]);
  assert.equal(dialog(), null, 'a sent message closes the sheet');
});

test('a failed send keeps the draft and says why', async (t) => {
  const { harness, open, dialog, field, button, tap } = await setup(t, { send: async () => { throw new Error('agent mailbox full'); } });
  await open();
  await harness.input(field(), 'hello');
  await tap(button('Send ➤'));
  await harness.act(async () => {});
  assert.ok(dialog(), 'the sheet stays');
  assert.equal(field().value, 'hello');
  assert.match(dialog().querySelector('[role="alert"]').textContent, /agent mailbox full/);
});

test('an empty sheet closes on Escape; a draft asks first, and Keep keeps it', async (t) => {
  const { harness, open, dialog, field, button, tap, escape } = await setup(t);
  await open();
  await escape();
  assert.equal(dialog(), null, 'nothing written: Escape just closes');

  await open();
  await harness.input(field(), 'half a thought');
  await escape();
  assert.ok(dialog(), 'a draft is not dropped');
  assert.equal(dialog().querySelector('.phone-compose-ask').textContent, 'Discard this message?');
  await tap(button('Keep'));
  assert.equal(field().value, 'half a thought');
  assert.equal(dialog().querySelector('.phone-compose-ask'), null);

  await tap(button('Cancel'));
  assert.ok(dialog().querySelector('.phone-compose-ask'), 'Cancel asks too');
  await tap(button('Discard'));
  assert.equal(dialog(), null);
});

test('the scrim and a swipe down on the grab handle ask before dropping a draft', async (t) => {
  const { harness, mounted, open, dialog, field, button, tap } = await setup(t);
  await open();
  await harness.input(field(), 'draft');
  await tap(mounted.container.querySelector('.phone-sheet-scrim'));
  assert.ok(dialog().querySelector('.phone-compose-ask'), 'the scrim asks');
  await tap(button('Keep'));

  const grab = dialog().querySelector('.phone-sheet-grab');
  const pointer = (type, clientY) => {
    const event = new harness.window.Event(type, { bubbles: true });
    Object.defineProperty(event, 'clientY', { value: clientY });
    grab.dispatchEvent(event);
  };
  await harness.act(() => { pointer('pointerdown', 100); pointer('pointerup', 120); });
  assert.equal(dialog().querySelector('.phone-compose-ask'), null, 'a short drag is not a swipe');
  await harness.act(() => { pointer('pointerdown', 100); pointer('pointerup', 200); });
  assert.ok(dialog().querySelector('.phone-compose-ask'), 'a swipe down asks');
  await tap(button('Discard'));
  assert.equal(dialog(), null);
});

test('Prompts lists the saved prompts and inserts one at the caret', async (t) => {
  const { harness, open, dialog, field, button, tap, fetched } = await setup(t, {
    prompts: [{ name: 'lgtm', body: 'Looks good, merge it.' }, { name: 'tests', body: 'Add tests first.' }],
  });
  await open();
  assert.equal(fetched.length, 0, 'prompts load only when asked for');
  await harness.input(field(), 'Thanks.  Bye');
  field().selectionStart = 8;
  field().selectionEnd = 8;
  await tap(button('Prompts ▾'));
  // The list loads: wait for it, a bounded number of turns.
  for (let turn = 0; turn < 50 && !dialog().querySelector('.phone-compose-prompt'); turn += 1) {
    await harness.act(() => new Promise((resolve) => setImmediate(resolve)));
  }
  assert.deepEqual(fetched, ['/api/saved-prompts']);
  const items = [...dialog().querySelectorAll('.phone-compose-prompt')].map((b) => b.textContent);
  assert.deepEqual(items, ['lgtm', 'tests']);
  await tap(button('lgtm'));
  assert.equal(field().value, 'Thanks. Looks good, merge it. Bye');
  assert.equal(dialog().querySelector('.phone-compose-prompt'), null, 'picking one folds the list');
});

test('insertAt refuses a prompt that would go over the message limit', async (t) => {
  const { compose } = await setup(t);
  assert.deepEqual(compose.insertAt('ab', 'X', 1), { value: 'aXb', caret: 2 });
  assert.deepEqual(compose.insertAt('abcd', 'X', 1, 3), { value: 'aXd', caret: 2 });
  assert.equal(compose.insertAt('a'.repeat(compose.MESSAGE_MAX_BYTES), 'b'), null);
});

test('another node\'s agent takes text only: no 📎', async (t) => {
  const { harness, compose, dialog } = await setup(t);
  await harness.act(() => compose.openPhoneCompose({ agent: AGENT, label: 'remote', node: 'node_x' }, { send: async () => {} }));
  assert.equal(dialog().querySelector('[aria-label="Attach files"]'), null);
  assert.match(dialog().querySelector('.phone-compose-note').textContent, /text only/);
});

test('the discard question takes focus, and Escape while it is up means Keep', async (t) => {
  const { harness, open, dialog, field, button, tap, escape } = await setup(t);
  await open();
  await harness.input(field(), 'draft');
  await tap(button('Cancel'));
  assert.equal(harness.document.activeElement, button('Keep'), 'focus moves into the question, not to the page');
  await escape();
  assert.equal(dialog().querySelector('.phone-compose-ask'), null, 'Escape again keeps the draft');
  assert.equal(field().value, 'draft');
  assert.equal(harness.document.activeElement, field(), 'and goes back to the text');
});

test('Back is a close too: a draft holds it and takes its entry back; an empty sheet goes', async (t) => {
  const { harness, open, dialog, field } = await setup(t);
  const [history, routes] = await Promise.all([
    harness.importDashboardModule('js/phone-history.js'),
    harness.importDashboardModule('js/overlay-routes.js'),
  ]);
  const announced = [];
  harness.document.addEventListener(routes.OVERLAY_EVENT, (event) => announced.push(event.detail));
  const uninstall = history.installPhoneHistory({ view: harness.signals.signal(null), doc: harness.document });
  t.after(uninstall);
  const turn = () => harness.act(() => new Promise((resolve) => setTimeout(resolve, 5)));
  await turn();

  await open();
  assert.equal(routes.overlayParams().phone, '1');
  await harness.input(field(), 'a long reply');
  announced.length = 0;
  await harness.act(() => routes.restoreOverlays({}));
  assert.ok(dialog(), 'Back does not drop the draft');
  assert.ok(dialog().querySelector('.phone-compose-ask'), 'it asks');
  await turn();
  assert.deepEqual(announced, [{ closed: false, replace: false }], 'the sheet takes its history entry back');

  await harness.act(() => routes.restoreOverlays({}));
  assert.equal(dialog().querySelector('.phone-compose-ask'), null, 'Back again while asked means Keep');
  // Each Back is a task of its own: the kept sheet has taken its entry back
  // before the next one.
  await turn();
  await harness.input(field(), '');
  await harness.act(() => routes.restoreOverlays({}));
  assert.equal(dialog(), null, 'an empty sheet just closes');
});

test('Write\'s sheet starts with no one: To finds an agent, and sending needs one', async (t) => {
  const { harness, compose, dialog, button, tap, sent } = await setup(t);
  const recipients = () => [
    { agent: 'agt_a', label: 'lead', group: 'webshop', online: true },
    { agent: 'agt_b', label: 'checkout-ui', group: 'webshop', online: false },
    { agent: 'agt_c', label: 'indexer', group: 'search', online: true },
  ];
  await harness.act(() => compose.openPhoneCompose({}, { recipients, send: async (draft) => { sent.push(draft); } }));
  assert.equal(dialog().getAttribute('aria-label'), 'New message');
  await harness.input(dialog().querySelector('textarea'), 'hello');
  await tap(button('Send ➤'));
  assert.match(dialog().querySelector('[role="alert"]').textContent, /Choose who/);
  assert.equal(sent.length, 0);
  await harness.input(dialog().querySelector('.phone-compose-to-input'), 'webshop');
  assert.deepEqual([...dialog().querySelectorAll('.phone-compose-recipient .phone-compose-file')].map((e) => e.textContent), ['lead', 'checkout-ui'],
    'by group too, live ones first');
  await tap(dialog().querySelector('.phone-compose-recipient'));
  await tap(button('Send ➤'));
  await harness.act(async () => {});
  assert.deepEqual(sent.map((d) => [d.to, d.body]), [['agt_a', 'hello']]);
});

test('matchRecipients: by name or group, live first, at most RECIPIENTS_SHOWN', async (t) => {
  const { compose } = await setup(t);
  const many = Array.from({ length: 9 }, (_, i) => ({ agent: `a${i}`, label: `agent${i}`, online: i % 2 === 0 }));
  const hits = compose.matchRecipients(many, '');
  assert.equal(hits.length, compose.RECIPIENTS_SHOWN);
  assert.ok(hits.slice(0, 5).every((r) => r.online));
  assert.deepEqual(compose.matchRecipients(many, 'agent3').map((r) => r.agent), ['a3']);
});

test('a reply titles its sheet, sends through its own route, and an offline sender takes nothing', async (t) => {
  const { harness, compose, dialog, button, tap, field } = await setup(t);
  const replies = [];
  await harness.act(() => compose.openPhoneCompose({ agent: AGENT, label: 'checkout-ui' }, {
    title: 'Reply to checkout-ui', send: async (draft) => { replies.push(draft.body); },
  }));
  assert.equal(dialog().querySelector('.phone-compose-title').textContent, 'Reply to checkout-ui');
  await harness.input(field(), 'yes');
  await tap(button('Send ➤'));
  await harness.act(async () => {});
  assert.deepEqual(replies, ['yes']);

  await harness.act(() => compose.openPhoneCompose({ agent: AGENT, label: 'checkout-ui' }, {
    title: 'Reply to checkout-ui', offline: 'checkout-ui is offline', send: async () => { replies.push('sent'); },
  }));
  assert.equal(button('Send ➤').disabled, true);
  assert.match(dialog().querySelector('.phone-compose-note').textContent, /offline/);
});
