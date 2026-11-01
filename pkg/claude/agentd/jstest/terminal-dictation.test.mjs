import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function fakeRecognition() {
  const made = [];
  class Recognition {
    constructor() { made.push(this); this.stops = 0; }
    start() {}
    stop() { this.stops += 1; this.onend?.(); }
    abort() { this.aborted = true; this.onend?.(); }
    hear(transcript, isFinal = true) {
      const result = [{ transcript }];
      result.isFinal = isFinal;
      this.onresult({ resultIndex: 0, results: [result] });
    }
  }
  return { Recognition, made, last: () => made[made.length - 1] };
}

async function setup(t, {
  seed = { agent: 'agt_0123456789abcdef0123456789abcdef', harness: 'claude' }, collab = true, status = 'connected',
  canType = true, secure = true, phrase, agents = [], solo = false, silence = 0, fetchImpl = async () => ({ ok: true, json: async () => ({}) }),
} = {}) {
  const harness = await createPreactHarness(t);
  const { h } = harness.preact;
  const { signal } = harness.signals;
  const { PaneDictation } = await harness.importDashboardModule('js/terminal-dictation.js');
  const { dashboardState } = await harness.importDashboardModule('js/snapshot-store.js');
  dashboardState.activeTab.value = 'terminals';
  const speech = fakeRecognition();
  const pasted = [];
  const activated = [];
  const notices = [];
  const posted = [];
  let result = true;
  const widget = { paste: (text) => { pasted.push(text); return result; } };
  const actions = { widgetFor: () => widget, activatePane: (key) => activated.push(key) };
  const snapshot = signal({ collab_enabled: collab, agents, ...(phrase === undefined && !silence ? {} : { voice: { phrases: phrase === undefined ? [] : [{ action: 'send', phrase }], silence_send_seconds: silence } }) });
  const win = harness.window;
  win.SpeechRecognition = speech.Recognition;
  win.isSecureContext = secure;
  const realFetch = globalThis.fetch;
  globalThis.fetch = (url, init) => { posted.push({ url, body: JSON.parse(init.body) }); return fetchImpl(url, init); };
  t.after(() => { globalThis.fetch = realFetch; });
  const pane = { key: 'p', id: 'pane-p', label: 'alpha', seed };
  const view = (active = true, key = 'k') => h(PaneDictation, {
    key, pane, active, solo, status, canType, target: seed.agent ? { agent: seed.agent } : null,
    snapshot, actions, win, notify: (text) => notices.push(text),
  });
  const mounted = await harness.mount(view());
  const rerender = (active) => mounted.rerender(view(active));
  const $ = (selector) => mounted.container.querySelector(selector);
  const mic = async () => harness.act(() => $('.companion-mic').click());
  const submit = async () => harness.act(() => $('.companion-input').dispatchEvent(new win.Event('submit', { bubbles: true, cancelable: true })));
  return {
    harness, rerender, $, mic, submit, speech, pasted, activated, notices, posted, win,
    failPaste: () => { result = false; },
  };
}

const SHELL = { seed: {} };
const paneTalks = async (s, seed, agents) => {
  const { paneTalksToAgent } = await s.harness.importDashboardModule('js/terminal-dictation.js');
  return paneTalksToAgent({ seed }, { agent: seed.agent }, { collab_enabled: true, agents });
};
const tick = () => new Promise((resolve) => setTimeout(resolve, 0));

test('a plain shell pastes one line, no newline, and only on an explicit send', async (t) => {
  const s = await setup(t, SHELL);
  assert.ok(!s.$('.companion-context'), 'no view tick for a shell');
  assert.ok(!s.$('.companion-annotate'), 'no markup for a shell');
  await s.mic();
  await s.harness.act(() => s.speech.last().hear('rm -rf build send now'));
  assert.deepEqual(s.pasted, [], 'the send phrase is only words here');
  assert.equal(s.$('.companion-draft').value, 'rm -rf build send now');
  assert.doesNotMatch(s.$('.companion-send-hint').textContent, /to send/, 'no hint for a phrase that does nothing');
  assert.equal(s.$('.companion-input button').textContent, 'Paste');
  await s.harness.act(() => { const f = s.$('.companion-draft'); f.value = 'ls\n-la\u001b[A'; f.dispatchEvent(new s.win.Event('input', { bubbles: true })); });
  await s.submit();
  assert.deepEqual(s.pasted, ['ls -la [A'], 'control characters never reach the terminal');
  assert.deepEqual(s.activated, ['p'], 'the keyboard goes back to the terminal');
  assert.ok(!s.$('.companion-input'));
});

test('a shell session with an agent id is a plain shell', async (t) => {
  const s = await setup(t, { seed: { agent: 'agt_0123456789abcdef0123456789abcdef', harness: 'shell' } });
  await s.mic();
  await s.harness.act(() => s.speech.last().hear('whoami send now'));
  assert.deepEqual(s.posted, []);
  assert.deepEqual(s.pasted, []);
});

test('a pane opened without a harness takes it from the roster', async (t) => {
  const agent = 'agt_0123456789abcdef0123456789abcdef';
  const s = await setup(t, { seed: { agent }, agents: [{ agent_id: agent, state: { harness: 'shell' } }] });
  assert.ok(!s.$('.companion-context'));
  assert.equal(await paneTalks(s, { agent }, [{ agent_id: agent, state: { harness: 'codex' } }]), true);
  assert.equal(await paneTalks(s, { agent }, []), false, 'unknown stays a terminal');
});

test('with Collab off an agent terminal pastes and never mails', async (t) => {
  const s = await setup(t, { collab: false });
  await s.mic();
  await s.harness.act(() => s.speech.last().hear('hello send now'));
  assert.deepEqual(s.posted, []);
  await s.submit();
  assert.equal(s.pasted.length, 1);
  assert.deepEqual(s.posted, []);
});

test('an agent terminal sends on the spoken phrase, stripped', async (t) => {
  const s = await setup(t);
  await s.mic();
  assert.match(s.$('.companion-send-hint').textContent, /^say “send now” to send · .*speech service$/);
  assert.ok(s.harness.document.activeElement === s.$('.companion-draft'), 'the draft takes the keyboard');
  await s.harness.act(async () => { s.speech.last().hear('fix the bug send now.'); await tick(); });
  assert.equal(s.posted.length, 1);
  assert.equal(s.posted[0].url, '/api/operator-message');
  assert.match(s.posted[0].body.body, /^fix the bug\n/);
  assert.deepEqual(s.pasted, []);
  assert.ok(!s.$('.companion-input'));
  assert.deepEqual(s.activated, ['p']);
});

test('an emptied phrase turns spoken send off and Enter still sends', async (t) => {
  const s = await setup(t, { phrase: '' });
  await s.mic();
  assert.doesNotMatch(s.$('.companion-send-hint').textContent, /to send/);
  await s.harness.act(() => s.speech.last().hear('hello send now'));
  assert.deepEqual(s.posted, []);
  await s.harness.act(() => s.speech.last().hear('partial', false));
  await s.submit();
  assert.equal(s.speech.last().aborted, true, 'sending closes the mic first');
  assert.match(s.posted[0].body.body, /^hello send now partial/, 'the words heard so far all go');
});

test('a failed send leaves the words in the draft', async (t) => {
  const s = await setup(t, { fetchImpl: async () => ({ ok: false, status: 500, statusText: 'no', json: async () => ({}) }) });
  await s.mic();
  await s.harness.act(async () => { s.speech.last().hear('keep me send now'); await tick(); });
  assert.equal(s.$('.companion-draft').value, 'keep me');
  assert.match(s.notices[0], /failed/);
});

test('leaving the pane stops the mic and keeps the draft with it', async (t) => {
  const s = await setup(t);
  await s.mic();
  await s.harness.act(() => s.speech.last().hear('half a thought'));
  await s.rerender(false);
  assert.equal(s.speech.last().aborted, true);
  assert.ok(!s.$('.companion-floating'));
  await s.rerender(true);
  assert.equal(s.$('.companion-draft').value, 'half a thought');
});

test('the mic button stops, keeps the draft and returns the keyboard', async (t) => {
  const s = await setup(t);
  await s.mic();
  await s.harness.act(() => s.speech.last().hear('half a thought'));
  await s.mic();
  assert.equal(s.$('.companion-draft').value, 'half a thought');
  assert.deepEqual(s.activated, ['p']);
  assert.deepEqual(s.posted, [], 'stopping never sends');
});

test('a watch-only peer shell shows no tools', async (t) => {
  const s = await setup(t, { seed: { remote: { peer: 'n' } }, canType: false });
  assert.ok(!s.$('.companion-floating'));
});

test('a closed terminal shows no tools', async (t) => {
  const s = await setup(t, { status: 'disconnected' });
  assert.ok(!s.$('.companion-floating'));
});

test('on a non-secure page the mic is disabled and says why', async (t) => {
  const s = await setup(t, { secure: false });
  assert.equal(s.$('.companion-mic').disabled, true);
  assert.match(s.$('.companion-mic-note').textContent, /HTTPS or localhost/);
});

test('a detached window shows failures on the terminal, and the mic clears them', async (t) => {
  const s = await setup(t, { solo: true, fetchImpl: async () => ({ ok: false, status: 500, statusText: 'no', json: async () => ({}) }) });
  await s.mic();
  await s.harness.act(async () => { s.speech.last().hear('keep me send now'); await tick(); });
  assert.match(s.$('.companion-pane-notice').textContent, /failed/);
  assert.deepEqual(s.notices, [], 'no toast host to reach');
  await s.mic();
  assert.ok(!s.$('.companion-pane-notice'));
});

test('a drawing in progress survives the terminal dropping', async (t) => {
  const s = await setup(t);
  await s.harness.act(() => s.$('.companion-annotate').click());
  assert.ok(s.harness.document.body.classList.contains('companion-annotating'));
  await s.rerender(false);
  assert.ok(!s.$('.companion-floating'));
  assert.ok(s.$('.annotate-send'), 'the overlay and its controls stay');
});

const wait = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

test('silence sends after a countdown that shows, and never when off', async (t) => {
  const s = await setup(t, { silence: 1 });
  await s.mic();
  await s.harness.act(() => s.speech.last().hear('quiet now', false));
  await s.harness.act(() => wait(800));
  assert.match(s.$('.companion-silence').textContent, /Sending in 1s/);
  await s.harness.act(() => wait(500));
  assert.equal(s.posted.length, 1);
  assert.match(s.posted[0].body.body, /^quiet now/);
  assert.equal(s.speech.last().aborted, true);
});

test('speaking again restarts the countdown, and Esc cancels it', async (t) => {
  const s = await setup(t, { silence: 1 });
  await s.mic();
  await s.harness.act(() => s.speech.last().hear('one', false));
  await s.harness.act(() => wait(750));
  await s.harness.act(() => s.speech.last().hear('two', false));
  await s.harness.act(() => wait(750));
  assert.deepEqual(s.posted, [], 'new speech pushed it back');
  await s.harness.act(() => s.$('.companion-draft').dispatchEvent(Object.assign(new s.win.Event('keydown', { bubbles: true, cancelable: true }), { key: 'Escape' })));
  await s.harness.act(() => wait(600));
  assert.deepEqual(s.posted, []);
  assert.ok(!s.$('.companion-silence'));
  assert.equal(s.$('.companion-draft').value, 'two', 'the words stay');
});

test('silence never sends in a plain shell', async (t) => {
  const s = await setup(t, { ...SHELL, silence: 1 });
  await s.mic();
  await s.harness.act(() => s.speech.last().hear('rm -rf build'));
  await s.harness.act(() => wait(1200));
  assert.deepEqual(s.pasted, []);
  assert.ok(!s.$('.companion-silence'));
});

test('silence send is off unless configured', async (t) => {
  const s = await setup(t);
  await s.mic();
  await s.harness.act(() => s.speech.last().hear('hello'));
  await s.harness.act(() => wait(1200));
  assert.deepEqual(s.posted, []);
});

test('a refused paste keeps the draft', async (t) => {
  const s = await setup(t, SHELL);
  s.failPaste();
  await s.mic();
  await s.harness.act(() => s.speech.last().hear('echo hi'));
  await s.submit();
  assert.equal(s.$('.companion-draft').value, 'echo hi');
  assert.match(s.notices[0], /not taking input/);
});
