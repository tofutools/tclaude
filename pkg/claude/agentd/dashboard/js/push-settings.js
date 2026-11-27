// push-settings.js -- the Web Push controls (agentd push.go), shared by the
// phone (More › Phone push, a sheet) and the desktop (the bell popover's
// "Phone push…", a dialog): subscribe this device, the categories, the
// content-free mode, the subscribed devices, and a test push. The CLI twin
// is `tclaude notifications push`.

import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { Button, Check, Switch } from './ui-components.js';
import { pushSupport, refreshThisDevice, subscribeThisDevice, unsubscribeThisDevice } from './push-client.js';

const html = htm.bind(h);

// PUSH_CATEGORY_LABELS name each category (config.PushCategories) as "Push
// me when…" finishes it.
export const PUSH_CATEGORY_LABELS = Object.freeze({
  access_requests: 'an agent asks for access',
  operator_questions: 'a question waits for my answer',
  awaiting_permission: 'an agent waits for a permission',
  awaiting_input: 'an agent asks me something',
  human_messages: 'an agent sends any other message',
  present_pr: 'an agent presents a pull request',
  idle: 'an agent goes idle',
  error: 'an agent hits an error',
  exited: 'an agent exits',
});

// pushCategories is the categories in the order agentd lists them, each with
// its label and state.
export function pushCategories(state) {
  const order = Array.isArray(state?.category_order) ? state.category_order : Object.keys(PUSH_CATEGORY_LABELS);
  return order.map((key) => ({ key, label: PUSH_CATEGORY_LABELS[key] || key.replace(/_/g, ' '), on: !!state?.categories?.[key] }));
}

// deviceLine is a device's second line: when it subscribed, when it last got
// a push, and its last error.
export function deviceLine(device, now = Date.now()) {
  const day = (at) => {
    const t = Date.parse(at || '');
    if (!Number.isFinite(t)) return '';
    const mins = Math.round((now - t) / 60000);
    if (mins < 1) return 'just now';
    if (mins < 60) return `${mins} min ago`;
    if (mins < 48 * 60) return `${Math.round(mins / 60)} h ago`;
    return new Date(t).toLocaleDateString();
  };
  const parts = [device?.last_ok_at ? `last push ${day(device.last_ok_at)}` : 'no push yet'];
  if (device?.last_error) parts.push(`last error: ${device.last_error}`);
  return parts.join(' · ');
}

async function readJSON(response) {
  if (!response.ok) throw new Error((await response.text().catch(() => '')).trim() || `HTTP ${response.status}`);
  return response.json();
}

// PushSettings is the controls. notify(text, isError) reports outcomes.
export function PushSettings({ fetchImpl = globalThis.fetch, win = globalThis, notify = () => {}, now = Date.now() }) {
  const [state, setState] = useState(null);
  const [error, setError] = useState('');
  const [thisDevice, setThisDevice] = useState(0);
  const [busy, setBusy] = useState(false);
  const support = pushSupport(win);

  const load = async () => {
    try {
      setState(await readJSON(await fetchImpl('/api/push', { credentials: 'same-origin' })));
      setError('');
    } catch (e) {
      setError(`Could not read the push settings: ${e?.message || e}`);
    }
  };
  useEffect(() => {
    let live = true;
    void load();
    if (support.ok) {
      refreshThisDevice({ fetchImpl, win }).then((id) => { if (live) setThisDevice(id); }).catch(() => {});
    }
    return () => { live = false; };
  }, []);

  const run = async (work, okText) => {
    setBusy(true);
    try {
      const next = await work();
      if (next && typeof next === 'object' && 'categories' in next) setState(next);
      if (okText) notify(okText);
    } catch (e) {
      notify(e?.message || String(e), true);
      await load();
    } finally {
      setBusy(false);
    }
  };
  const post = async (path, body) => readJSON(await fetchImpl(path, {
    method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
  }));

  // Straight from the tap: subscribeThisDevice asks permission first.
  const subscribe = () => run(async () => {
    const next = await subscribeThisDevice({ fetchImpl, win });
    setThisDevice(next?.device?.id || 0);
    return next;
  }, 'This device now gets push notifications.');
  const unsubscribe = () => run(async () => {
    await unsubscribeThisDevice(thisDevice, { fetchImpl, win });
    setThisDevice(0);
    return readJSON(await fetchImpl('/api/push', { credentials: 'same-origin' }));
  }, 'This device no longer gets push notifications.');
  const remove = (id) => run(async () => {
    const next = await readJSON(await fetchImpl(`/api/push/devices/${encodeURIComponent(id)}`, { method: 'DELETE', credentials: 'same-origin' }));
    if (id === thisDevice) {
      await unsubscribeThisDevice(0, { fetchImpl, win });
      setThisDevice(0);
    }
    return next;
  }, 'Device removed.');
  const test = (id) => run(async () => {
    await post('/api/push/test', id ? { device: id } : {});
  }, 'Test notification sent.');
  const setCategory = (key, on) => run(() => post('/api/push/config', { categories: { [key]: on } }));
  const setContentFree = (on) => run(() => post('/api/push/config', { content_free: on }));
  const masterOn = () => run(async () => {
    await post('/api/notifications', { enabled: true });
    return readJSON(await fetchImpl('/api/push', { credentials: 'same-origin' }));
  }, 'Notifications on.');

  if (!state) {
    return html`<div class="push-settings">${error ? html`<p class="ui-error-box" role="alert">${error}</p>` : html`<p class="ui-hint">Loading…</p>`}</div>`;
  }
  const devices = Array.isArray(state.devices) ? state.devices : [];
  return html`<div class="push-settings">
    <p class="ui-hint push-settings-intro">Push notifications reach your devices with the app closed. Nothing is sent until a device is turned on here.</p>
    ${!state.enabled && html`<div class="push-settings-warn" role="status">
      <span>Notifications are off for this node, so nothing is pushed.</span>
      <${Button} size="sm" disabled=${busy} onClick=${masterOn}>Turn on<//>
    </div>`}

    <div class="push-settings-device">
      <label class="push-settings-switch">
        <${Switch} checked=${!!thisDevice} disabled=${busy || (!support.ok && !thisDevice)} aria-describedby="push-settings-why"
          onChange=${() => (thisDevice ? unsubscribe() : subscribe())} />
        <span><b>This device</b></span>
      </label>
      ${thisDevice
        ? html`<${Button} size="sm" disabled=${busy} onClick=${() => test(thisDevice)}>Send test<//>`
        : null}
    </div>
    ${!support.ok && html`<p class="ui-hint push-settings-why" id="push-settings-why">${support.reason}</p>`}

    <h4 class="push-settings-head">Push me when…</h4>
    <div class="push-settings-cats">
      ${pushCategories(state).map((c) => html`<${Check} key=${c.key} data-push-category=${c.key} checked=${c.on} disabled=${busy}
        onChange=${(e) => setCategory(c.key, e.currentTarget.checked)}>${c.label}<//>`)}
    </div>
    <${Check} checked=${!!state.content_free} disabled=${busy} data-push-content-free="1"
      onChange=${(e) => setContentFree(e.currentTarget.checked)}>
      Content-free: show only “tclaude needs you”, with no text the push service could read
    <//>

    <h4 class="push-settings-head">Devices</h4>
    ${devices.length === 0
      ? html`<p class="ui-hint">No device gets pushes yet.</p>`
      : html`<ul class="push-settings-devices">${devices.map((d) => html`<li key=${d.id} class="push-settings-dev">
          <span class="push-settings-dev-text"><span class="push-settings-dev-name">${d.label || `Device ${d.id}`}${d.id === thisDevice && html` <span class="ui-badge">this device</span>`}</span>
            <span class="ui-hint">${deviceLine(d, now)}</span></span>
          <${Button} size="sm" disabled=${busy} onClick=${() => test(d.id)}>Test<//>
          <${Button} size="sm" variant="danger" disabled=${busy} onClick=${() => remove(d.id)}>Remove<//>
        </li>`)}</ul>`}
    ${error && html`<p class="ui-error-box" role="alert">${error}</p>`}
  </div>`;
}
