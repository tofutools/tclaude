// node-update-settings.js — a node's auto-update settings (updates.self)
// inside the Fleet node update dialog: this node's own, or a peer's through
// the peer proxy.
//
// The daemon's GET says how much the caller may change ("access"): full (the
// node's own operator, or an unrestricted peer), settings (a peer holding
// node.update.settings: no free-text fields, no switching to a
// package-manager, custom or checkout method) or read (node.update only).
// The node re-checks every save; this only draws what it will accept.

import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { Button } from './ui-components.js';
import { errText } from './cell-format.js';
import { SELF_UPDATE_METHODS, SETUP_FLAGS, SETUP_HARNESSES, TCLAUDE_DRIVEN, WEEKDAYS, managerName } from './config-self-update.js';

const html = htm.bind(h);

// localOnlyMethod: these methods need free text (or opting in to a package manager), so
// only the node itself or an unrestricted peer may switch a node to them.
const FREE_TEXT_METHODS = ['custom', 'source_checkout'];
export function localOnlyMethod(method) {
  return FREE_TEXT_METHODS.includes(method) || !!method && !!SELF_UPDATE_METHODS.find((m) => m.id === method)?.package_manager;
}

function methodLabel(view, id) {
  if (!id) return 'not chosen';
  return (view?.methods || []).find((m) => m.id === id)?.label || SELF_UPDATE_METHODS.find((m) => m.id === id)?.label || id;
}

const cap = (s) => s ? s[0].toUpperCase() + s.slice(1) : s;

// scheduleText is the read-only one-liner for a schedule.
export function scheduleText(s, onStart) {
  const sched = s?.frequency && s.frequency !== 'off'
    ? `${cap(s.frequency)}${s.frequency === 'weekly' ? ` · ${cap(s.weekday || 'monday')}` : ''} · ${s.window_start || '03:00'}–${s.window_end || '05:00'}${s.defer_while_agents_working ? ' · when no agent is working' : ''}`
    : 'Off';
  return onStart ? `${sched} · also when the daemon starts` : sched;
}

function setupText(p) {
  if (!p?.enabled) return 'Off';
  const args = [...(p.flags || []).map((f) => `--${f}`), ...(p.harness ? ['--harness', p.harness] : [])];
  return `On${args.length ? ` · ${args.join(' ')}` : ''}`;
}

const clone = (v) => JSON.parse(JSON.stringify(v || {}));

// NodeUpdateSettings reads and edits one node's updates.self. actions is
// createNodeUpdateActions(); confirm asks before opting in to a package
// manager.
export function NodeUpdateSettings({ node, actions, confirm, toast }) {
  const [view, setView] = useState(null);
  const [error, setError] = useState(null);
  const [draft, setDraft] = useState(null);
  const [busy, setBusy] = useState(false);
  const [saveError, setSaveError] = useState('');
  const [showFlags, setShowFlags] = useState(false);

  const adopt = (v) => { setView(v); setDraft(clone(v?.settings)); setSaveError(''); };
  useEffect(() => {
    let off = false;
    actions.settings(node).then((v) => { if (!off) adopt(v); }).catch((e) => { if (!off) setError(e); });
    return () => { off = true; };
  }, [node.id]);

  if (error) {
    // An older node has no settings route: the dialog simply has no block.
    if (error?.status === 404) return null;
    return html`<div class="nus" id="fleet-node-update-settings"><div class="nus-head"><b>Auto-update settings</b></div>
      <div class="muted" role="alert">${error?.status === 403 ? `${node.label} does not let you read its update settings (needs node.update).` : `Could not read the update settings: ${errText(error)}`}</div></div>`;
  }
  if (!view || !draft) return html`<div class="nus" id="fleet-node-update-settings"><div class="nus-head"><b>Auto-update settings</b></div><div class="muted">Loading…</div></div>`;

  const access = view.access || (node.local ? 'full' : 'read');
  const s = view.settings || {};
  const d = view.detected || {};
  const where = node.local ? 'this node' : node.label;
  const head = html`<div class="nus-head"><b>Auto-update settings</b>
    <span class="muted" id="fleet-node-update-settings-access">${access === 'read' ? 'read-only' : access === 'settings' ? 'limited' : `stored on ${where}`}</span></div>`;
  const pmNote = d.managed_by && TCLAUDE_DRIVEN.includes((draft || s).method) && html`<div class="nus-alert" id="fleet-node-update-settings-pm">
    ${managerName(d.managed_by)} still owns tclaude on ${where}${d.untrack_command ? html`; untrack it there with <code>${d.untrack_command}</code>` : ''}.
    ${access !== 'full' && html` Only ${node.local ? 'this node' : `${node.label} itself`} or an unrestricted peer can acknowledge that${(draft || s).package_manager_ack ? ' (acknowledged)' : ' — not acknowledged, so its updates stay refused until then'}.`}</div>`;

  if (access === 'read') {
    return html`<div class="nus" id="fleet-node-update-settings">${head}
      <div class="nus-alert">You can read these settings but not change them. Changing them needs the <code>node.update.settings</code> grant on ${node.label} (unrestricted also works).</div>
      ${pmNote}
      <div class="fa-dl nus-ro">
        <span class="fa-k">Method</span><span id="fleet-node-update-settings-method">${methodLabel(view, s.method)}${s.method === 'source_checkout' && s.source_checkout?.path ? html` · <code>${s.source_checkout.path}</code>` : ''}</span>
        <span class="fa-k">Schedule</span><span id="fleet-node-update-settings-schedule">${scheduleText(s.schedule, s.update_on_start)}</span>
        <span class="fa-k">After update</span><span>${setupText(s.post_update_setup)}</span>
      </div></div>`;
  }

  const full = access === 'full';
  const set = (mutate) => setDraft((cur) => { const next = clone(cur); mutate(next); return next; });
  const sched = draft.schedule || {};
  const setup = draft.post_update_setup || {};
  const dirty = JSON.stringify(draft) !== JSON.stringify(s);
  const redacted = (view.redacted || []).includes('custom.command');

  const save = async () => {
    const info = SELF_UPDATE_METHODS.find((m) => m.id === draft.method);
    const optIn = !!info?.package_manager && draft.method !== s.method;
    if (optIn && !await confirm({
      title: `${methodLabel(view, draft.method)} on ${where}?`,
      body: `${where === 'this node' ? 'tclaude' : `${node.label}`} will run its package manager for scheduled, start-up and manual updates, then check the new version.`,
      okLabel: methodLabel(view, draft.method),
    })) return;
    setBusy(true);
    setSaveError('');
    try {
      const body = { settings: draft };
      if (optIn) body.confirm_package_manager = true;
      const v = await actions.saveSettings(node, body);
      adopt(v);
      toast(`Saved ${node.local ? 'this node' : node.label}'s update settings`);
    } catch (e) {
      const list = e?.body?.errors;
      setSaveError(list?.length ? list.join('; ') : errText(e));
    } finally {
      setBusy(false);
    }
  };

  return html`<div class="nus" id="fleet-node-update-settings">${head}
    ${access === 'settings' && html`<div class="nus-note">🔒 The checkout path, a custom command, the package-manager acknowledgement and switching to a package-manager, custom or checkout method can be set only on ${node.label} itself or by an unrestricted peer.</div>`}
    <div class="nus-grid">
      <label class="fa-k" for="fleet-nus-method">Method</label>
      <span><select class="ui-select ui-sm" id="fleet-nus-method" value=${draft.method || ''} onChange=${(e) => { const v = e.currentTarget.value; set((n) => { n.method = v; }); }}>
        <option value="">Not chosen (no automatic updates)</option>
        ${SELF_UPDATE_METHODS.map(({ id }) => {
          const m = (view.methods || []).find((x) => x.id === id) || {};
          const lockedOut = !full && localOnlyMethod(id) && id !== s.method;
          const reason = lockedOut ? 'only on the node or by an unrestricted peer' : m.available === false ? m.unavailable_reason : '';
          return html`<option key=${id} value=${id} disabled=${lockedOut || (m.available === false && id !== s.method)}>${methodLabel(view, id)}${reason ? ` — ${reason}` : ''}</option>`;
        })}
      </select>${d.method && html` <span class="muted">detected: ${methodLabel(view, d.method)}</span>`}</span>

      ${draft.method === 'source_checkout' && html`<label class="fa-k" for="fleet-nus-checkout">Checkout</label>
        <span>${full ? html`<input class="ui-input ui-sm" id="fleet-nus-checkout" spellcheck="false" placeholder="~/git/tclaude" value=${draft.source_checkout?.path || ''}
            onInput=${(e) => { const v = e.currentTarget.value; set((n) => { n.source_checkout = { ...(n.source_checkout || {}), path: v }; }); }} />`
          : html`<code>${draft.source_checkout?.path || '(not set)'}</code> 🔒`}</span>`}
      ${draft.method === 'custom' && html`<label class="fa-k" for="fleet-nus-command">Command</label>
        <span>${full ? html`<textarea class="ui-textarea ui-sm" id="fleet-nus-command" rows="2" spellcheck="false" value=${draft.custom?.command || ''}
            onInput=${(e) => { const v = e.currentTarget.value; set((n) => { n.custom = { ...(n.custom || {}), command: v }; }); }}></textarea>`
          : html`<span class="muted">${redacted ? 'hidden from peers' : (draft.custom?.command || '(not set)')}</span> 🔒`}</span>`}

      <label class="fa-k" for="fleet-nus-frequency">Schedule</label>
      <span class="nus-row">
        <select class="ui-select ui-sm" id="fleet-nus-frequency" value=${sched.frequency || 'off'} onChange=${(e) => { const v = e.currentTarget.value; set((n) => { n.schedule = { ...(n.schedule || {}), frequency: v }; }); }}>
          <option value="off">Off</option><option value="daily">Daily</option><option value="weekly">Weekly</option>
        </select>
        ${sched.frequency === 'weekly' && html`<select class="ui-select ui-sm" id="fleet-nus-weekday" aria-label="Day" value=${sched.weekday || 'monday'} onChange=${(e) => { const v = e.currentTarget.value; set((n) => { n.schedule = { ...(n.schedule || {}), weekday: v }; }); }}>
          ${WEEKDAYS.map((w) => html`<option key=${w} value=${w}>${cap(w)}</option>`)}</select>`}
        ${sched.frequency && sched.frequency !== 'off' && html`
          <input class="ui-input ui-sm nus-time" id="fleet-nus-start" aria-label="Window start (HH:MM)" placeholder="03:00" maxlength="5" value=${sched.window_start || ''} onInput=${(e) => { const v = e.currentTarget.value; set((n) => { n.schedule = { ...(n.schedule || {}), window_start: v }; }); }} />
          –<input class="ui-input ui-sm nus-time" id="fleet-nus-end" aria-label="Window end (HH:MM)" placeholder="05:00" maxlength="5" value=${sched.window_end || ''} onInput=${(e) => { const v = e.currentTarget.value; set((n) => { n.schedule = { ...(n.schedule || {}), window_end: v }; }); }} />`}
      </span>
      ${sched.frequency && sched.frequency !== 'off' && html`<span></span><label class="nus-check"><input type="checkbox" id="fleet-nus-defer" checked=${!!sched.defer_while_agents_working}
        onChange=${(e) => { const v = e.currentTarget.checked; set((n) => { n.schedule = { ...(n.schedule || {}), defer_while_agents_working: v }; }); }} /> wait while agents are working</label>`}
      <span></span><label class="nus-check"><input type="checkbox" id="fleet-nus-on-start" checked=${!!draft.update_on_start}
        onChange=${(e) => { const v = e.currentTarget.checked; set((n) => { n.update_on_start = v; }); }} /> also update when the daemon starts</label>

      <span class="fa-k">After update</span>
      <span class="nus-row"><label class="nus-check"><input type="checkbox" id="fleet-nus-setup" checked=${!!setup.enabled}
        onChange=${(e) => { const v = e.currentTarget.checked; set((n) => { n.post_update_setup = { ...(n.post_update_setup || {}), enabled: v }; }); }} /> run <code>tclaude setup --yes</code></label>
        <span class="muted">${setupText(setup).replace(/^On · |^On$|^Off$/, '')}</span>
        <${Button} size="sm" variant="ghost" id="fleet-nus-setup-edit" aria-expanded=${showFlags ? 'true' : 'false'} onClick=${() => setShowFlags(!showFlags)}>${showFlags ? 'Hide options' : 'Options…'}<//></span>
      ${showFlags && html`<span></span><span class="nus-flags">
        ${SETUP_FLAGS.map(({ flag, label }) => html`<label key=${flag} class="nus-check"><input type="checkbox" data-nus-flag=${flag} checked=${(setup.flags || []).includes(flag)}
          onChange=${(e) => { const on = e.currentTarget.checked; set((n) => {
            const flags = (n.post_update_setup?.flags || []).filter((f) => f !== flag);
            n.post_update_setup = { ...(n.post_update_setup || {}), flags: on ? [...flags, flag] : flags };
          }); }} /> ${label}</label>`)}
        <label class="nus-check">Harness <select class="ui-select ui-sm" id="fleet-nus-harness" value=${setup.harness || ''}
          onChange=${(e) => { const v = e.currentTarget.value; set((n) => { n.post_update_setup = { ...(n.post_update_setup || {}), harness: v }; }); }}>
          <option value="">(setup's default)</option>${SETUP_HARNESSES.map((x) => html`<option key=${x} value=${x}>${x}</option>`)}</select></label>
      </span>`}
    </div>
    ${!draft.method && (sched.frequency && sched.frequency !== 'off' || draft.update_on_start) && html`<div class="fa-warn" id="fleet-nus-unchosen">No method chosen: the schedule and start-up updates do nothing until one is.</div>`}
    ${pmNote}
    ${pmNote && full && html`<label class="nus-check"><input type="checkbox" id="fleet-nus-ack" checked=${!!draft.package_manager_ack}
      onChange=${(e) => { const v = e.currentTarget.checked; set((n) => { n.package_manager_ack = v; }); }} /> I understand ${managerName(d.managed_by)} still manages tclaude there</label>`}
    ${saveError && html`<div class="fa-danger" role="alert" id="fleet-node-update-settings-error">${saveError}</div>`}
    <div class="nus-actions">
      <${Button} size="sm" id="fleet-nus-save" disabled=${busy || !dirty} busy=${busy} onClick=${() => { void save(); }}>Save settings<//>
      ${dirty && html`<${Button} size="sm" variant="ghost" id="fleet-nus-revert" disabled=${busy} onClick=${() => adopt(view)}>Revert<//>`}
      <span class="muted">CLI: <code>tclaude update config set${node.local ? '' : ` --node ${node.id}`} …</code></span>
    </div>
  </div>`;
}
