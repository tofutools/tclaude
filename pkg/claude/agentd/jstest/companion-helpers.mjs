// Shared fixtures for the companion suites (companion*.test.mjs). The
// companion's tests are split across files so node --test runs them in
// parallel processes: each test builds a fresh Preact harness, and a file's
// module graphs stay in memory until its process exits.
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

export const A = `agt_${'a'.repeat(32)}`;
export const B = `agt_${'b'.repeat(32)}`;
export const S = `agt_${'5'.repeat(32)}`;

export function snapshotWith({ aStatus = 'idle', steward = false } = {}) {
  const groups = [{
    name: 'team', members: [
      { agent_id: A, conv_id: 'conv-a', title: 'alpha', online: true, state: { harness: 'claude', status: aStatus, status_detail: aStatus === 'awaiting_permission' ? 'Bash(rm -rf build)' : '' } },
      { agent_id: B, conv_id: 'conv-b', title: 'bravo', online: true, state: { status: 'working', harness: 'claude' } },
    ],
  }];
  if (steward) groups.push({ name: 'steward', scribe: true, members: [{ agent_id: S, conv_id: 'conv-s', title: 'steward-k3x', online: true, state: { status: 'idle', harness: 'claude' } }] });
  return { groups, ...(steward ? { steward_roster: [{ agent_id: S, name: 'steward-k3x', seat: 'active', online: true }] } : {}) };
}

// A hand-over's claim is asynchronous and starts as an act's render ends.
export const settleClaim = (harness) => harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });

export function memoryPrefs(initial = {}) {
  const store = new Map(Object.entries(initial));
  return { getItem: (k) => (store.has(k) ? store.get(k) : null), setItem: (k, v) => store.set(k, String(v)), removeItem: (k) => store.delete(k), store };
}

export function fakeWidgetFactory(mounted) {
  return (options) => {
    const widget = {
      options, disposed: false, focused: 0,
      setActive() {}, connect: async () => { options.onStatus('connected'); },
      focus() { widget.focused += 1; }, dispose() { widget.disposed = true; },
    };
    mounted.push(widget);
    return widget;
  };
}

// A dictation fake: the only way text reaches the draft field, which appears
// while there is some.
export function fakeDictation() {
  const fake = { callbacks: null };
  fake.impl = (cb) => { fake.callbacks = cb; return { supported: true, start: () => cb.onState(true), stop: () => cb.onState(false), abort: () => cb.onState(false) }; };
  fake.say = async (harness, view, text) => {
    await harness.act(() => view.container.querySelector('.companion-mic').click());
    await harness.act(() => fake.callbacks.onText(text));
    await harness.act(() => view.container.querySelector('.companion-mic').click());
  };
  return fake;
}

export const SHOWN_WINDOW = '.companion-window:not(.companion-window-warm)';

export async function setup(t, { reloading = null, snapshot = snapshotWith(), prefs = memoryPrefs(), sessionPrefs = memoryPrefs(), onComposeMessage = null, openMorph = null, openPermissions = null, openSudo = null, detachWindow = null, openTerminalsPane = null, openStewardHandover = null, fetchImpl, speakerImpl = null, dictationImpl = null, notify = () => {}, locks = null } = {}) {
  // A reload is the same harness, as two would tear down in the wrong order.
  if (reloading) await reloading.view.unmount();
  const harness = reloading?.harness || await createPreactHarness(t);
  const { CompanionApp } = await harness.importDashboardModule('js/companion-island.js');
  const overlays = await harness.importDashboardModule('js/overlay-instances.js');
  overlays.overlayWindows.value = [];
  overlays.minimizedOverlayTabs.value = [];
  const snap = harness.signals.signal(snapshot);
  const activeTab = harness.signals.signal('processes');
  const mounted = [];
  const requests = [];
  const closed = [];
  let controller = null;
  const fetchFn = fetchImpl || (async (url, init = {}) => {
    requests.push({ url: String(url), body: init.body ? JSON.parse(init.body) : null });
    if (String(url) === '/api/scribe') return { ok: true, status: 200, json: async () => ({ agent_id: S, conv_id: 'conv-s', name: 'steward-k3x', reused: false }) };
    if (String(url).endsWith('/peek')) return { ok: true, status: 200, json: async () => ({ lines: ['● hello', 'Allow this command?'] }) };
    return { ok: true, status: 202, json: async () => ({ id: 1 }) };
  });
  const view = await harness.mount(harness.html`<${CompanionApp}
    snapshot=${snap} activeTab=${activeTab} prefs=${prefs} sessionPrefs=${sessionPrefs} fetchImpl=${fetchFn} notify=${notify}
    locks=${locks} onComposeMessage=${onComposeMessage} openMorph=${openMorph} openPermissions=${openPermissions} openSudo=${openSudo} detachWindow=${detachWindow} openTerminalsPane=${openTerminalsPane} openStewardHandover=${openStewardHandover} widgetFactory=${fakeWidgetFactory(mounted)} speakerImpl=${speakerImpl} dictationImpl=${dictationImpl} closeTerminalPanes=${(selectors) => closed.push(selectors)}
    registerController=${(c) => { controller = c; return () => { controller = null; }; }} />`);
  return { harness, view, snap, mounted, requests, closed, prefs, controller: () => controller };
}

export const FLOATING = { x: 300, y: 100, w: 500, h: 400, snap: '' };

export async function openLook(t, look, { prefs, snapshot = snapshotWith(), reloading = null } = {}) {
  const store = prefs || memoryPrefs({ 'tclaude.dash.companion.tabs': JSON.stringify([A, B]), 'tclaude.dash.companion.active': A });
  const ctx = await setup(t, { reloading, prefs: store, snapshot: { ...snapshot, window_controls: look } });
  await ctx.harness.act(() => ctx.controller().open(A));
  await ctx.harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  return ctx;
}
export const buttonNames = (root) => [...root.querySelectorAll('button')].map((button) => button.getAttribute('aria-label'));

export async function openWithMic(t, { supported, secure }) {
  const dictationImpl = () => ({ supported, start() {}, stop() {}, abort() {} });
  const { view, harness, controller } = await setup(t, { prefs: memoryPrefs(), dictationImpl });
  harness.window.isSecureContext = secure;
  await harness.act(() => controller().open(A));
  await harness.act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  return { mic: view.container.querySelector('.companion-mic'), note: view.container.querySelector('.companion-mic-note'), container: view.container };
}

// ---- the Steward phrase ----

export function micCounting() {
  const fake = fakeDictation();
  const impl = fake.impl;
  fake.starts = [];
  fake.impl = (cb) => {
    const made = impl(cb);
    fake.reservations ||= [];
    fake.instances = (fake.instances || 0) + 1;
    const door = fake.instances === 1;
    return { ...made, keep: (on) => { if (door) fake.reservations.push(on); }, start: (options) => { fake.starts.push(options); made.start(); } };
  };
  return fake;
}

export const CONTINUOUS_SNAPSHOT = () => ({
  ...snapshotWith({ steward: true }),
  agents: snapshotWith({ steward: true }).groups.flatMap((group) => group.members),
  harnesses: [{ name: 'claude', can_mail_mid_turn: true }],
  voice: { wake_enabled: true, continuous_enabled: true, phrases: [{ action: 'send', phrase: 'send now' }, { action: 'steward', phrase: 'hey stewie' }, { action: 'stop', phrase: 'stop listening' }] },
});

export const offlineSteward = () => {
  const snapshot = snapshotWith({ steward: true });
  const member = snapshot.groups.flatMap((group) => group.members).find((candidate) => candidate.agent_id === S);
  return { ...snapshot, steward_roster: [{ agent_id: S, name: 'steward-k3x', seat: 'active', online: false, resumable: true }], groups: snapshot.groups.map((group) => ({ ...group, members: group.members.map((one) => (one === member ? { ...one, online: false } : one)) })) };
};
export const resumeOk = async () => ({ ok: true, json: async () => ({ action: 'resumed' }), text: async () => '' });
export const flush = async () => { await Promise.resolve(); await Promise.resolve(); await new Promise((r) => setTimeout(r, 0)); };

// Everything a companion tab does, a chat tab in a window of its own does too
// (tcl-e4i44z): the companion becomes a window of these.
export const chatOf = (harness, view) => view.container.querySelector('.chat-window');
export async function floated(t, options = {}) {
  const prefs = options.prefs || memoryPrefs();
  const opened = await setup(t, { ...options, prefs });
  const { harness, view, controller } = opened;
  const agent = options.agent || A;
  await harness.act(() => controller().open(agent));
  await harness.act(() => controller().float(agent));
  assert.ok(chatOf(harness, view), 'it floats');
  return opened;
}

export const flushed = (harness) => harness.act(async () => { await new Promise((r) => setTimeout(r, 20)); });
export const bringOnline = (snapshot, online) => {
  const next = structuredClone(snapshot);
  next.groups[0].members[0].online = online;
  return next;
};

export const mailable = (extra = {}) => {
  const snapshot = { ...structuredClone(snapshotWith()), ...extra };
  snapshot.groups[0].members[0].state.harness = 'claude';
  snapshot.agents = [{ agent_id: A, conv_id: 'conv-a', state: { harness: 'claude' } }];
  return snapshot;
};
