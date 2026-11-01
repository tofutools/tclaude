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
  canType = true, secure = true, phrase, agents = [], solo = false, silence = 0, mark, pauses = {}, harnesses = [], fetchImpl = async () => ({ ok: true, json: async () => ({}) }),
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
  const snapshot = signal({ collab_enabled: collab, agents, harnesses, ...(phrase === undefined && !silence && mark === undefined && !Object.keys(pauses).length ? {} : { voice: { phrases: phrase === undefined ? [] : [{ action: 'send', phrase }], silence_send_seconds: silence, mark_dictated: mark, ...pauses } }) });
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
  assert.match(s.posted[0].body.body, /^\[dictated\] fix the bug\n/, 'marked, with the phrase stripped');
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
  assert.match(s.posted[0].body.body, /^\[dictated\] hello send now partial/, 'the words heard so far all go');
});

const typeDraft = (s, text) => s.harness.act(() => {
  s.$('.companion-draft').value = text;
  s.harness.fireEvent(s.$('.companion-draft'), 'input');
});

test('a draft with dictated words stays marked through an edit, until it is emptied', async (t) => {
  const s = await setup(t);
  await s.mic();
  await s.harness.act(() => s.speech.last().hear('fix the bug'));
  await typeDraft(s, 'fix the login bug');
  await s.submit();
  assert.match(s.posted[0].body.body, /^\[dictated\] fix the login bug\n/);
  await s.mic();
  await s.harness.act(() => s.speech.last().hear('scrap this'));
  await typeDraft(s, '');
  await s.mic();
  await typeDraft(s, 'typed instead');
  await s.submit();
  assert.match(s.posted[1].body.body, /^typed instead\n/, 'emptying the draft ends the mark');
});

test('a typed draft is not marked, even with the mic opened on the way', async (t) => {
  const s = await setup(t);
  await s.mic();
  await typeDraft(s, 'typed by hand');
  await s.submit();
  assert.match(s.posted[0].body.body, /^typed by hand\n/);
});

test('the marker can be turned off', async (t) => {
  const s = await setup(t, { mark: false });
  await s.mic();
  await s.harness.act(async () => { s.speech.last().hear('plain words send now'); await tick(); });
  assert.match(s.posted[0].body.body, /^plain words\n/);
});

test('a failed send keeps the mark for the retry, and the marker out of the draft', async (t) => {
  let refuse = true;
  const s = await setup(t, { fetchImpl: async () => (refuse ? { ok: false, status: 500, statusText: 'no', json: async () => ({}) } : { ok: true, json: async () => ({}) }) });
  await s.mic();
  await s.harness.act(async () => { s.speech.last().hear('keep me send now'); await tick(); });
  assert.equal(s.$('.companion-draft').value, 'keep me', 'the marker stays out of the draft');
  refuse = false;
  await s.submit();
  await s.harness.act(tick);
  assert.match(s.posted[1].body.body, /^\[dictated\] keep me\n/);
});

test('a plain shell never gets the marker', async (t) => {
  const s = await setup(t, SHELL);
  await s.mic();
  await s.harness.act(() => s.speech.last().hear('ls -la'));
  await s.submit();
  assert.deepEqual(s.pasted, ['ls -la']);
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
  assert.match(s.$('.companion-mic').title, /plain HTTP pages/);
  assert.ok(!s.$('.companion-mic-note'), 'the tooltip says it, with no text beside the tools');
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
  assert.match(s.posted[0].body.body, /^\[dictated\] quiet now/);
  assert.equal(s.speech.last().aborted, true);
});

test('speaking again restarts the countdown, and Esc ends it for good', async (t) => {
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
  assert.equal(s.speech.last().aborted, true, 'a late result can\'t start it again');
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

test('a one-breath wake starts the silence countdown', async (t) => {
  const s = await setup(t, { silence: 1 });
  const { pickWakeTarget } = await s.harness.importDashboardModule('js/voice-wake.js');
  const pane = s.harness.document.body.appendChild(s.harness.document.createElement('div'));
  pane.id = 'pane-p';
  await s.harness.act(() => pickWakeTarget(s.harness.document).start('fix the bug'));
  assert.equal(s.$('.companion-draft').value, 'fix the bug');
  await s.harness.act(() => wait(1200));
  assert.match(s.posted[0].body.body, /^\[dictated\] fix the bug/);
});

test('a one-breath wake leaves the send phrase out of the draft', async (t) => {
  const s = await setup(t);
  const { pickWakeTarget } = await s.harness.importDashboardModule('js/voice-wake.js');
  const pane = s.harness.document.body.appendChild(s.harness.document.createElement('div'));
  pane.id = 'pane-p';
  await s.harness.act(() => pickWakeTarget(s.harness.document).start('fix the bug send now'));
  assert.equal(s.$('.companion-draft').value, 'fix the bug');
});

test('pauses never punctuate in a plain shell', async (t) => {
  const s = await setup(t, { ...SHELL, pauses: { comma_pause_seconds: 0.01 } });
  await s.mic();
  await s.harness.act(() => s.speech.last().hear('ls'));
  await s.harness.act(() => wait(40));
  await s.harness.act(() => s.speech.last().hear('build'));
  assert.equal(s.$('.companion-draft').value, 'ls build');
});

test('pauses punctuate a draft for an agent', async (t) => {
  const s = await setup(t, { pauses: { comma_pause_seconds: 0.01 } });
  await s.mic();
  await s.harness.act(() => s.speech.last().hear('first'));
  await s.harness.act(() => wait(40));
  await s.harness.act(() => s.speech.last().hear('second'));
  assert.equal(s.$('.companion-draft').value, 'first, second');
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

// ---- continuous mode ----

const AGENT = 'agt_0123456789abcdef0123456789abcdef';
const CATALOG = [{ name: 'claude', can_mail_mid_turn: true }, { name: 'gemini', can_mail_mid_turn: false }];
const continuous = (t, { status = 'working', harness = 'claude', seed = { agent: AGENT, harness }, voice = {}, ...rest } = {}) => setup(t, {
  seed, harnesses: CATALOG, agents: [{ agent_id: AGENT, state: { harness, status } }],
  pauses: { continuous_enabled: true, continuous_idle_seconds: 120, ...voice }, ...rest,
});
const says = (s, words, final = true) => s.harness.act(async () => { s.speech.last().hear(words, final); await tick(); });
const reopened = (s) => s.harness.act(() => wait(260));
const bodies = (s) => s.posted.map((item) => item.body.body.split('\n')[0]);

test('after a voice send the mic reopens, and the next sentence sends on silence', async (t) => {
  const s = await continuous(t);
  await s.mic();
  await says(s, 'first message send now');
  assert.deepEqual(bodies(s), ['[dictated] first message']);
  await reopened(s);
  assert.equal(s.speech.made.length, 2, 'a second recognizer listens with no click');
  assert.match(s.$('.companion-continuous').textContent, /^⏺ Continuous mode · say “stop listening” to stop/, 'no countdown over an empty draft');
  assert.ok(s.$('.companion-mic').classList.contains('steady'), 'no pulse for a mode that runs for minutes');
  await says(s, 'second message');
  await s.harness.act(() => wait(3300));
  assert.deepEqual(bodies(s), ['[dictated] first message', '[dictated] second message']);
  assert.equal(s.posted[1].body.to, AGENT);
  await reopened(s);
  assert.equal(s.speech.made.length, 3);
  assert.ok(s.$('.companion-continuous'), 'it is still on');
});

test('with no voice send yet nothing loops', async (t) => {
  const s = await continuous(t);
  await s.mic();
  await s.harness.act(() => s.$('.companion-mic').click());
  await reopened(s);
  assert.equal(s.speech.made.length, 1);
  assert.ok(!s.$('.companion-continuous'));
});

test('a recognizer ending by itself is replaced, and the draft survives it', async (t) => {
  const s = await continuous(t);
  await s.mic();
  await says(s, 'one send now');
  await reopened(s);
  await says(s, 'half a thought');
  await s.harness.act(() => s.speech.last().onend());
  await reopened(s);
  assert.equal(s.speech.made.length, 3);
  await says(s, 'and the rest');
  assert.equal(s.$('.companion-draft').value, 'half a thought and the rest');
  assert.ok(s.$('.companion-mic').classList.contains('on'), 'the mic reads as on throughout');
});

test('a recognizer that keeps ending at once stops it and says so', async (t) => {
  const s = await continuous(t);
  await s.mic();
  await says(s, 'one send now');
  await reopened(s);
  for (let i = 0; i < 5; i += 1) {
    await s.harness.act(() => s.speech.last().onend());
    await reopened(s);
  }
  assert.ok(!s.$('.companion-continuous'));
  assert.match(s.notices.at(-1), /keeps ending/);
});

test('a blocked mic stops it and says so', async (t) => {
  const s = await continuous(t);
  await s.mic();
  await says(s, 'one send now');
  await reopened(s);
  await s.harness.act(() => s.speech.last().onerror({ error: 'not-allowed' }));
  await reopened(s);
  assert.ok(!s.$('.companion-continuous'));
  assert.equal(s.speech.made.length, 2);
  assert.match(s.notices.at(-1), /blocked the microphone/);
});

test('the stop phrase ends it, alone and never sent', async (t) => {
  const s = await continuous(t);
  await s.mic();
  await says(s, 'one send now');
  await reopened(s);
  await says(s, 'Stop listening.');
  assert.deepEqual(bodies(s), ['[dictated] one']);
  assert.ok(!s.$('.companion-continuous'));
  await reopened(s);
  assert.equal(s.speech.made.length, 2);
});

test('the stop phrase keeps the words before it in the draft', async (t) => {
  const s = await continuous(t);
  await s.mic();
  await says(s, 'one send now');
  await reopened(s);
  await says(s, 'keep this');
  await says(s, 'stop listening');
  assert.equal(s.$('.companion-draft').value, 'keep this');
  assert.deepEqual(bodies(s), ['[dictated] one']);
});

test('inside a sentence the stop phrase is plain text', async (t) => {
  const s = await continuous(t);
  await s.mic();
  await says(s, 'one send now');
  await reopened(s);
  await says(s, 'please stop listening to the socket');
  assert.equal(s.$('.companion-draft').value, 'please stop listening to the socket');
  assert.ok(s.$('.companion-continuous'));
});

test('an unfinished stop phrase never goes out on silence', async (t) => {
  const s = await continuous(t);
  await s.mic();
  await says(s, 'one send now');
  await reopened(s);
  await says(s, 'keep this');
  await says(s, 'stop listening', false);
  await s.harness.act(() => wait(3300));
  assert.deepEqual(bodies(s), ['[dictated] one']);
  assert.equal(s.$('.companion-draft').value, 'keep this');
  assert.ok(!s.$('.companion-continuous'));
});

test('an unfinished stop phrase with the send phrase after it never goes out either', async (t) => {
  const s = await continuous(t);
  await s.mic();
  await says(s, 'one send now');
  await reopened(s);
  await says(s, 'stop listening send now', false);
  await s.harness.act(() => wait(3300));
  assert.deepEqual(bodies(s), ['[dictated] one']);
  assert.ok(!s.$('.companion-continuous'));
});

test('off, the stop phrase is words', async (t) => {
  const s = await setup(t);
  await s.mic();
  await says(s, 'stop listening');
  assert.equal(s.$('.companion-draft').value, 'stop listening');
});

test('Esc stops it from anywhere, and doesn\'t reach the terminal', async (t) => {
  const s = await continuous(t);
  await s.mic();
  await says(s, 'one send now');
  await reopened(s);
  let key;
  await s.harness.act(() => { key = s.harness.fireEvent(s.harness.document.body, 'keydown', { key: 'Escape' }); });
  assert.ok(!s.$('.companion-continuous'));
  assert.equal(key.defaultPrevented, true);
  await reopened(s);
  assert.equal(s.speech.made.length, 2);
});

test('Stop ends it in one click', async (t) => {
  const s = await continuous(t);
  await s.mic();
  await says(s, 'one send now');
  await reopened(s);
  await s.harness.act(() => s.$('.companion-silence-cancel').click());
  assert.ok(!s.$('.companion-continuous'));
});

test('typing in the draft ends it, and the text stays', async (t) => {
  const s = await continuous(t);
  await s.mic();
  await says(s, 'one send now');
  await reopened(s);
  await says(s, 'almost right');
  await typeDraft(s, 'quite right');
  assert.ok(!s.$('.companion-continuous'));
  await reopened(s);
  assert.equal(s.$('.companion-draft').value, 'quite right');
  assert.equal(s.speech.made.length, 2);
});

test('leaving the pane ends it, so speech never follows the focus', async (t) => {
  const s = await continuous(t);
  await s.mic();
  await says(s, 'one send now');
  await reopened(s);
  await s.rerender(false);
  await reopened(s);
  assert.equal(s.speech.made.length, 2);
  assert.equal(s.speech.last().aborted, true);
  await s.rerender(true);
  await reopened(s);
  assert.equal(s.speech.made.length, 2, 'coming back doesn\'t restart it');
});

test('hiding the tab ends it', async (t) => {
  const s = await continuous(t);
  await s.mic();
  await says(s, 'one send now');
  await reopened(s);
  Object.defineProperty(s.harness.document, 'hidden', { configurable: true, value: true });
  t.after(() => { delete s.harness.document.hidden; });
  await s.harness.act(() => s.harness.document.dispatchEvent(new s.win.Event('visibilitychange')));
  assert.ok(!s.$('.companion-continuous'));
  assert.match(s.notices.at(-1), /tab was hidden/);
});

test('long silence ends it', async (t) => {
  const s = await continuous(t, { voice: { continuous_idle_seconds: 0.4 } });
  await s.mic();
  await says(s, 'one send now');
  await reopened(s);
  assert.ok(s.$('.companion-continuous'));
  await s.harness.act(() => wait(300));
  assert.ok(!s.$('.companion-continuous'));
  assert.match(s.notices.at(-1), /long silence/);
});

test('it stops rather than sends while the agent waits on the operator', async (t) => {
  const s = await continuous(t, { status: 'awaiting_permission' });
  await s.mic();
  await says(s, 'yes send now');
  assert.deepEqual(s.posted, [], 'a spoken word can never answer a permission question');
  assert.equal(s.$('.companion-draft').value, 'yes');
  assert.match(s.notices.at(-1), /waiting on you/);
  assert.ok(!s.$('.companion-continuous'));
  await reopened(s);
  assert.equal(s.speech.made.length, 1);
});

test('only what blocks the agent holds it, with the session still working', async (t) => {
  const { paneContinuous } = await (await createPreactHarness(t)).importDashboardModule('js/terminal-dictation.js');
  const base = { voice: { continuous_enabled: true }, harnesses: CATALOG, agents: [{ agent_id: AGENT, conv_id: 'c1', state: { harness: 'claude', status: 'working' } }] };
  const held = (extra) => paneContinuous({ seed: { harness: 'claude' } }, { agent: AGENT }, { ...base, ...extra }, 'alpha').held();
  assert.equal(held({}), '');
  assert.match(held({ access_requests: [{ agent_id: AGENT, status: 'pending', perm: 'x' }] }), /waiting on you/);
  assert.match(held({ access_requests: [{ agent_id: AGENT, status: 'pending', perm: 'models.proxy' }] }), /waiting on you/, 'a request no card can answer counts');
  assert.equal(held({ access_requests: [{ agent_id: AGENT, status: 'approved' }] }), '');
  assert.equal(held({ access_requests: [{ agent_id: 'agt_other', status: 'pending' }] }), '');
  assert.equal(held({ messages: [{ id: 1, from_agent: AGENT, options: ['yes', 'no'] }] }), '', 'an open decision blocks nobody');
});

test('with silence send off the first message waits for the send phrase', async (t) => {
  const s = await continuous(t);
  await s.mic();
  await says(s, 'thinking out loud');
  await s.harness.act(() => wait(3300));
  assert.deepEqual(s.posted, []);
  assert.ok(!s.$('.companion-continuous'));
  await says(s, 'and done send now');
  assert.equal(s.posted.length, 1);
});

test('a loop never starts in a hidden tab', async (t) => {
  const s = await continuous(t);
  await s.mic();
  Object.defineProperty(s.harness.document, 'hidden', { configurable: true, value: true });
  t.after(() => { delete s.harness.document.hidden; });
  await says(s, 'one send now');
  assert.deepEqual(bodies(s), ['[dictated] one'], 'the message said out loud still goes');
  await reopened(s);
  assert.equal(s.speech.made.length, 1);
  assert.ok(!s.$('.companion-continuous'));
});

test('a tab hidden in the gap between recognizers stops it', async (t) => {
  const s = await continuous(t);
  await s.mic();
  await says(s, 'one send now');
  Object.defineProperty(s.harness.document, 'hidden', { configurable: true, value: true });
  t.after(() => { delete s.harness.document.hidden; });
  await reopened(s);
  assert.equal(s.speech.made.length, 1);
  assert.ok(!s.$('.companion-continuous'));
});

test('ending it in the gap between recognizers stops a countdown too', async (t) => {
  const s = await continuous(t);
  await s.mic();
  await says(s, 'one send now');
  await reopened(s);
  await says(s, 'half a thought');
  await s.harness.act(() => s.speech.last().onend());
  await typeDraft(s, 'half a thought, edited');
  await s.harness.act(() => wait(3300));
  assert.deepEqual(bodies(s), ['[dictated] one']);
  assert.equal(s.$('.companion-draft').value, 'half a thought, edited');
});

test('the stop phrase ending in the send phrase is never sent', async (t) => {
  const s = await continuous(t);
  await s.mic();
  await says(s, 'stop listening send now');
  assert.deepEqual(s.posted, []);
  await reopened(s);
  assert.equal(s.speech.made.length, 1);
});

test('a failed send ends it and keeps the words', async (t) => {
  const s = await continuous(t, { fetchImpl: async () => ({ ok: false, status: 500, statusText: 'no', json: async () => ({}) }) });
  await s.mic();
  await says(s, 'keep me send now');
  assert.equal(s.$('.companion-draft').value, 'keep me');
  assert.ok(!s.$('.companion-continuous'));
  await reopened(s);
  assert.equal(s.speech.made.length, 1);
});

test('without a harness that takes mail mid-turn, a voice send ends the mic as before', async (t) => {
  const s = await continuous(t, { harness: 'gemini' });
  await s.mic();
  await says(s, 'one send now');
  assert.deepEqual(bodies(s), ['[dictated] one']);
  await reopened(s);
  assert.equal(s.speech.made.length, 1);
  assert.ok(!s.$('.companion-continuous'));
});

test('continuous mode never runs on a plain shell or a peer', async (t) => {
  const s = await continuous(t, { harness: 'shell', seed: { agent: AGENT, harness: 'shell' } });
  await s.mic();
  await says(s, 'whoami send now');
  await s.harness.act(() => wait(3300));
  assert.deepEqual(s.posted, []);
  assert.deepEqual(s.pasted, []);
  assert.equal(s.$('.companion-draft').value, 'whoami send now', 'the stop and send phrases mean nothing here');
  const { paneContinuous } = await s.harness.importDashboardModule('js/terminal-dictation.js');
  const snapshot = { voice: { continuous_enabled: true }, harnesses: CATALOG, agents: [{ agent_id: AGENT, state: { harness: 'claude' } }] };
  assert.ok(paneContinuous({ seed: { harness: 'claude' } }, { agent: AGENT }, snapshot, 'a'));
  assert.equal(paneContinuous({ seed: { harness: 'claude', remote: true } }, { agent: AGENT }, snapshot, 'a'), null);
  assert.equal(paneContinuous({ seed: { harness: 'claude' } }, { agent: 'agt_other' }, snapshot, 'a'), null, 'not in this node\'s roster');
  assert.equal(paneContinuous({ seed: { harness: 'claude' } }, { agent: AGENT }, { ...snapshot, voice: {} }, 'a'), null, 'off by default');
});

// ---- the Steward phrase in continuous mode ----

const STEWARD_PHRASES = [{ action: 'send', phrase: 'send now' }, { action: 'steward', phrase: 'hey stewie' }, { action: 'stop', phrase: 'stop listening' }];
const STEWARD_VOICE = { wake_enabled: true };
async function setupStewardLoop(t, { refusal = '', voice = STEWARD_VOICE } = {}) {
  const s = await continuous(t, { voice: { ...voice, phrases: STEWARD_PHRASES } });
  const { registerStewardDoor } = await s.harness.importDashboardModule('js/voice-wake.js');
  s.opened = [];
  t.after(registerStewardDoor({ refusal: () => refusal, reserve() {}, open: (...args) => s.opened.push(args) }));
  await s.mic();
  await says(s, 'one send now');
  await reopened(s);
  return s;
}

test('the Steward phrase ends continuous here and carries it there', async (t) => {
  const s = await setupStewardLoop(t);
  await says(s, 'half a thought');
  await says(s, 'hey stewie check the logs');
  assert.deepEqual(s.opened, [['check the logs', true]]);
  assert.deepEqual(bodies(s), ['[dictated] one'], 'nothing more goes to the old agent');
  assert.equal(s.$('.companion-draft').value, 'half a thought', 'the draft stays here, unsent');
  assert.ok(!s.$('.companion-continuous'));
  await reopened(s);
  await s.harness.act(() => wait(3300));
  assert.deepEqual(bodies(s), ['[dictated] one']);
  assert.equal(s.speech.made.length, 2, 'this pane stopped listening');
});

test('an unfinished Steward phrase never goes to the old agent on silence', async (t) => {
  const s = await setupStewardLoop(t);
  await says(s, 'hey stewie look at this', false);
  await s.harness.act(() => wait(3300));
  assert.deepEqual(bodies(s), ['[dictated] one']);
  assert.deepEqual(s.opened, [['look at this', true]]);
});

test('with no Steward running the phrase says so and continuous stays where it is', async (t) => {
  const s = await setupStewardLoop(t, { refusal: 'The Steward isn’t running.' });
  await says(s, 'hey stewie check the logs');
  assert.deepEqual(s.opened, []);
  assert.equal(s.notices.at(-1), 'The Steward isn’t running.');
  assert.ok(s.$('.companion-continuous'), 'nothing moved');
  assert.equal(s.$('.companion-draft').value, '', 'the phrase stays out of the draft');
  assert.deepEqual(bodies(s), ['[dictated] one']);
});

test('a refused Steward phrase on silence stays out of what is sent next', async (t) => {
  const s = await setupStewardLoop(t, { refusal: 'The Steward isn’t running.' });
  await says(s, 'hey stewie check the logs', false);
  await s.harness.act(() => wait(3300));
  await reopened(s);
  assert.equal(s.notices.at(-1), 'The Steward isn’t running.');
  await says(s, 'next sentence');
  assert.equal(s.$('.companion-draft').value, 'next sentence');
  await s.harness.act(() => wait(3300));
  assert.deepEqual(bodies(s), ['[dictated] one', '[dictated] next sentence']);
});

test('inside a sentence the Steward phrase is plain text', async (t) => {
  const s = await setupStewardLoop(t);
  await says(s, 'ask hey stewie about it');
  assert.deepEqual(s.opened, []);
  assert.equal(s.$('.companion-draft').value, 'ask hey stewie about it');
});

test('with the wake listener off the Steward phrase is plain text', async (t) => {
  const s = await setupStewardLoop(t, { voice: {} });
  await says(s, 'hey stewie check the logs');
  assert.deepEqual(s.opened, []);
  assert.equal(s.$('.companion-draft').value, 'hey stewie check the logs');
});

test('before a loop runs, a dictation takes the Steward phrase as words', async (t) => {
  const s = await continuous(t, { voice: { ...STEWARD_VOICE, phrases: STEWARD_PHRASES } });
  await s.mic();
  await says(s, 'hey stewie is a name');
  assert.equal(s.$('.companion-draft').value, 'hey stewie is a name');
});
