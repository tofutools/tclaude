// config-service-launch.js — Config › Service › Launch profile: what tclaude
// starts agentd with when it writes the autostart unit, and what an update
// compares the restarted daemon against (docs/dashboard-config.md, "Service").
//
// It reads GET <url> ({ profile, suggested, missing, owned, launcher,
// comparison, access, redacted, version }) and saves with PUT
// { profile, version }; { profile: null } clears it. A stale version is a
// 409 (launch_profile_conflict) carrying the current view: the edits stay,
// over the new version, so Save again overwrites and Discard takes theirs.
// A refused profile is a 400 { errors: [{ path, code, message }] }, each
// shown on its row (profile.flags[i], profile.env[i].key|value,
// profile.inherit_env[i], or "profile" for the whole).
//
// The form edits rows: a serve flag each, and an environment variable each
// with a set | inherit mode. "Use what's running now" copies the running
// daemon's launch (suggested) into the form: a secret flag is left out, with
// a note (a secret is passed through an inherited variable), and a secret
// variable is inherited, its value never stored. Like the rest of the
// Service panel it acts at once, never through the Config form's Save.

import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { Button } from './ui-components.js';
import { RequestError, requestJSON } from './request-json.js';
import { shellConfirm, shellToast } from './shell-state.js';

const html = htm.bind(h);

export const LAUNCH_URL = '/api/node/service/launch';
const REDACTED = '<redacted>';
export const SET = 'set';
export const INHERIT = 'inherit';

// profileDraft is a profile as the form's rows: flags, then each variable
// with its mode, inherited ones after the set ones (as the profile keeps
// them). null (no profile) is an empty form.
export function profileDraft(profile) {
  return {
    flags: [...(profile?.flags || [])],
    env: [
      ...(profile?.env || []).map((e) => ({ key: e.key, mode: SET, value: e.value ?? '' })),
      ...(profile?.inherit_env || []).map((key) => ({ key, mode: INHERIT, value: '' })),
    ],
  };
}

// draftFromSuggested is "Use what's running now": the running daemon's flags
// without the secret ones (named in dropped), and its variables, a secret
// one inherited.
export function draftFromSuggested(suggested) {
  const dropped = [];
  const flags = [];
  for (const flag of suggested?.flags || []) {
    const f = typeof flag === 'string' ? { arg: flag } : flag;
    if (f.secret) dropped.push(f.name || String(f.arg).split('=')[0]);
    else flags.push(f.arg);
  }
  const env = (suggested?.env || []).map((e) => {
    const inherit = e.secret || e.mode === INHERIT || e.value === REDACTED;
    return { key: e.key, mode: inherit ? INHERIT : SET, value: inherit ? '' : e.value ?? '', secret: !!e.secret };
  });
  return { draft: { flags, env }, dropped };
}

// profileOf is the draft as the profile a PUT sends, empty rows left out,
// and where each sent entry came from: paths maps a server error's path to
// the row's key (f<i> for a flag row, e<i> for a variable row).
export function profileOf(draft) {
  const profile = { flags: [], env: [], inherit_env: [] };
  const paths = new Map();
  draft.flags.forEach((flag, i) => {
    const arg = String(flag || '').trim();
    if (!arg) return;
    paths.set(`profile.flags[${profile.flags.length}]`, `f${i}`);
    profile.flags.push(arg);
  });
  draft.env.forEach((row, i) => {
    const key = String(row.key || '').trim();
    if (!key) return;
    if (row.mode === INHERIT) {
      paths.set(`profile.inherit_env[${profile.inherit_env.length}]`, `e${i}`);
      profile.inherit_env.push(key);
      return;
    }
    const at = `profile.env[${profile.env.length}]`;
    for (const part of ['', '.key', '.value']) paths.set(`${at}${part}`, `e${i}`);
    profile.env.push({ key, value: row.value ?? '' });
  });
  return { profile, paths };
}

// rowErrors places a 400's errors on the rows they name; one that names no
// row (the profile as a whole, or a path the form does not know) is general.
export function rowErrors(errors, paths) {
  const rows = {};
  const general = [];
  for (const e of errors || []) {
    const text = e.message || e.code || 'refused';
    const row = paths.get(e.path);
    if (row) rows[row] = rows[row] ? `${rows[row]}; ${text}` : text;
    else general.push(text);
  }
  return { rows, general };
}

// missingKeys are the inherited names agentd does not have now, so the unit
// would get them empty: by the running daemon's variables (suggested), which
// also covers a name just typed, else as the daemon reported for the saved
// profile.
export function missingKeys(view, draft) {
  // The daemon's own word for the saved profile's names; the running
  // variables (suggested) only for a name the saved profile does not inherit.
  const out = new Set(view?.missing || []);
  const running = view?.suggested?.env;
  if (!Array.isArray(running)) return out;
  const have = new Set(running.map((e) => e.key));
  const saved = new Set(view?.profile?.inherit_env || []);
  for (const r of draft.env) {
    if (r.mode === INHERIT && r.key && !saved.has(r.key) && !have.has(r.key)) out.add(r.key);
  }
  return out;
}

const BASELINE = { profile: 'compared with the profile', unit: 'compared with the unit' };
const BASELINE_SIDE = { profile: 'Profile', unit: 'Unit' };

// comparisonTag is the comparison's state as a tag, and what it is against.
export function comparisonTag(comparison) {
  if (!comparison?.state) return null;
  const n = comparison.differences?.length || 0;
  const tag = comparison.state === 'identical' ? { cls: 'ui-tag cfg-launch-same', text: 'identical' }
    : comparison.state === 'differs' ? { cls: 'ui-tag ui-tag-warn', text: `differs · ${n}` }
    : { cls: 'ui-tag', text: 'nothing to compare' };
  return { ...tag, baseline: BASELINE[comparison.baseline] || '' };
}

// shownValue is a difference's value as the table may show it: an
// environment value only on this node, for a key that is not secret, and
// never a redacted one.
export function shownValue(diff, value, { secretKeys, restricted }) {
  if (value === undefined || value === null || value === '' || value === REDACTED) return null;
  if (restricted || (diff.kind === 'env' && secretKeys.has(diff.name))) return null;
  return value;
}

function sameDraft(a, b) { return JSON.stringify(profileOf(a).profile) === JSON.stringify(profileOf(b).profile); }

function Differences({ comparison, secretKeys, restricted }) {
  const diffs = comparison?.differences || [];
  if (!diffs.length) return null;
  const side = BASELINE_SIDE[comparison.baseline] || 'Expected';
  const cell = (d, v) => {
    const shown = shownValue(d, v, { secretKeys, restricted });
    return shown === null ? html`<span class="muted">—</span>` : html`<code>${shown}</code>`;
  };
  return html`<table class="ui-compact cfg-launch-diff" id="cfg-launch-diff"><thead><tr><th>What</th><th>Change</th><th>Name</th><th>Running now</th><th>${side}</th></tr></thead>
    <tbody>${diffs.map((d, i) => html`<tr key=${i} data-launch-diff=${`${d.kind}:${d.name}`}><td>${d.kind}</td><td>${d.change}</td><td><code>${d.name}</code></td>
      <td>${cell(d, d.before)}</td><td>${cell(d, d.after)}</td></tr>`)}</tbody></table>`;
}

function Row({ label, children, id }) {
  return html`<span class="cfg-svc-k">${label}</span><span class="cfg-svc-v" id=${id}>${children}</span>`;
}

// ReadOnly is the profile for a viewer who may not change it: the flags,
// and the variables by name (a restricted peer sees no values).
function ReadOnly({ view }) {
  const p = view.profile;
  if (!p) return html`<${Row} label="Profile"><span class="muted">not set</span><//>`;
  return html`
    <${Row} label="Serve flags">${p.flags?.length ? html`<code>${p.flags.join(' ')}</code>` : html`<span class="muted">none</span>`}<//>
    <${Row} label="Environment">${[...(p.env || []).map((e) => e.key), ...(p.inherit_env || []).map((k) => `${k} (inherit)`)].map((k) => html`<code key=${k}>${k}</code>`)}
      ${view.redacted?.length ? html`<span class="muted">· values hidden</span>` : ''}<//>`;
}

// LaunchProfile is the section. onView hands each read view up (the
// autostart controls defer to a set profile); a new reload (the panel bumps
// it after a restart, install or removal) reads it again, edits kept. fetchImpl/confirm/toast are
// injectable for tests.
export function LaunchProfile({
  open = true, url = LAUNCH_URL, onView = () => {}, reload = 0,
  fetchImpl = (...a) => globalThis.fetch(...a), confirm = shellConfirm, toast = shellToast,
}) {
  const [view, setView] = useState(null);
  const [loadError, setLoadError] = useState(null);
  const [draft, setDraft] = useState(profileDraft(null));
  const [saved, setSaved] = useState(profileDraft(null));
  const [dropped, setDropped] = useState([]);
  const [errors, setErrors] = useState({ rows: {}, general: [] });
  const [note, setNote] = useState('');
  const [busy, setBusy] = useState('');
  const alive = useRef(true);
  const dirtyRef = useRef(false);
  useEffect(() => () => { alive.current = false; }, []);

  const take = (next, { keepDraft = false } = {}) => {
    setView(next);
    onView(next);
    const base = profileDraft(next?.profile);
    setSaved(base);
    if (!keepDraft) { setDraft(base); setDropped([]); setErrors({ rows: {}, general: [] }); }
  };
  const load = async () => {
    try {
      const next = await requestJSON(url, { fetchImpl });
      if (!alive.current) return;
      setLoadError(null);
      take(next, { keepDraft: dirtyRef.current });
    } catch (e) {
      if (alive.current) setLoadError(e);
    }
  };
  const dirty = !sameDraft(draft, saved);
  dirtyRef.current = dirty;
  useEffect(() => { if (open) void load(); }, [open, reload]);

  const put = async (profile, paths, what) => {
    setBusy(what);
    setNote('');
    setErrors({ rows: {}, general: [] });
    try {
      const next = await requestJSON(url, { method: 'PUT', body: { profile, version: view?.version }, fetchImpl });
      if (!alive.current) return;
      take(next);
      toast(profile ? 'Launch profile saved' : 'Launch profile cleared');
    } catch (e) {
      if (!alive.current) return;
      if (e?.status === 409 && e.code === 'launch_profile_conflict') {
        const current = e.body?.view || e.body;
        if (current?.version !== undefined) take(current, { keepDraft: true });
        else await load();
        setNote(profile
          ? 'The profile was changed elsewhere meanwhile. Your edits are kept: Save again to replace it, or Discard to see the new one.'
          : 'The profile was changed elsewhere meanwhile, so it was not cleared. Look at the new one, then clear it again if you still want to.');
      } else if (e?.status === 400 && Array.isArray(e.body?.errors)) {
        setErrors(rowErrors(e.body.errors, paths));
      } else {
        setErrors({ rows: {}, general: [e?.message || String(e)] });
      }
    } finally {
      if (alive.current) setBusy('');
    }
  };
  const save = () => { const { profile, paths } = profileOf(draft); void put(profile, paths, 'save'); };
  const clear = async () => {
    if (!await confirm({
      title: 'Clear the launch profile?',
      body: 'The autostart unit goes back to what agentd runs with when it is written, and updates compare with the daemon as it ran before. Nothing restarts now.',
      okLabel: 'Clear',
    })) return;
    void put(null, new Map(), 'clear');
  };
  const useRunning = () => {
    const { draft: next, dropped: left } = draftFromSuggested(view?.suggested);
    setDraft(next);
    setDropped(left);
    setErrors({ rows: {}, general: [] });
  };
  const discard = () => { setDraft(saved); setDropped([]); setErrors({ rows: {}, general: [] }); setNote(''); };

  if (loadError) {
    const absent = loadError instanceof RequestError && loadError.status === 404;
    return html`<p class="cfg-group-lede" id="cfg-launch-absent">${absent
      ? 'This daemon has no launch profile yet: update it to set one.'
      : html`<span role="alert">Could not read the launch profile: ${loadError.message || String(loadError)}</span>`}</p>`;
  }
  if (!view) return html`<p class="cfg-group-lede muted">Loading…</p>`;

  const restricted = !!view.redacted?.length;
  // Only a viewer who is shown the values may edit: one shown keys only
  // would save its set variables back empty.
  const editable = (view.access === 'full' || !view.access) && !restricted;
  const secretKeys = new Set([
    ...(view.suggested?.env || []).filter((e) => e.secret).map((e) => e.key),
    ...draft.env.filter((r) => r.secret).map((r) => r.key),
  ]);
  const missing = missingKeys(view, draft);
  const tag = comparisonTag(view.comparison);
  const setFlag = (i, value) => setDraft({ ...draft, flags: draft.flags.map((f, j) => (j === i ? value : f)) });
  const setEnv = (i, patch) => setDraft({ ...draft, env: draft.env.map((r, j) => (j === i ? { ...r, ...patch } : r)) });
  // A removed row shifts the ones after it, so the errors placed by index go.
  const drop = (field, i) => { setDraft({ ...draft, [field]: draft[field].filter((_, j) => j !== i) }); setErrors({ ...errors, rows: {} }); };
  const any = !!busy;
  const rowError = (key) => errors.rows[key] && html`<div class="cfg-launch-row-note cfg-launch-err" role="alert">${errors.rows[key]}</div>`;

  return html`<div class="cfg-launch" id="cfg-launch">
    <p class="cfg-group-lede">What tclaude starts agentd with when it writes the autostart unit, and what an update compares the restarted daemon against. Not set: the unit keeps what agentd runs with now. CLI: <code>tclaude service launch</code>.</p>
    <div class="cfg-svc-dl">
      <${Row} label="Profile" id="cfg-launch-state">
        <span>${dirty ? html`<span class="ui-tag ui-tag-accent" id="cfg-launch-dirty">unsaved changes</span>` : view.profile ? 'set' : 'not set'}</span>
        ${tag && html`<span class="cfg-launch-cmp"><span class=${tag.cls} id="cfg-launch-cmp">${tag.text}</span>${tag.baseline && html` <span class="muted" id="cfg-launch-baseline">${tag.baseline}</span>`}</span>`}
      <//>
      ${view.comparison?.differences?.length > 0 && html`<${Row} label="Differences"><${Differences} comparison=${view.comparison} secretKeys=${secretKeys} restricted=${restricted} /><//>`}
      ${view.owned === false && html`<${Row} label=""><div class="cfg-svc-warn" id="cfg-launch-unowned">${view.launcher?.unit ? html`The unit <code>${view.launcher.unit}</code>` : 'The unit'} was not written by tclaude, so tclaude never changes it and a profile is not applied to it.${view.comparison?.baseline === 'unit' ? ' The comparison is with the unit\'s own command and environment.' : ''}</div><//>`}
      ${!editable ? html`<${ReadOnly} view=${view} />
        <${Row} label=""><span class="muted" id="cfg-launch-readonly">${view.access === 'read'
          ? 'Changing it needs node.update.settings on that node.'
          : 'Its values are hidden here, so it is changed only where they are shown: on that node, or with full access.'}</span><//>` : html`
      <${Row} label="Serve flags"><div class="cfg-launch-rows" id="cfg-launch-flags">
        ${draft.flags.map((flag, i) => html`<div key=${`f${i}`} class="cfg-launch-row" data-launch-row=${`f${i}`}>
          <div class="cfg-launch-fields"><input type="text" class=${`ui-input ui-sm cfg-launch-flag${errors.rows[`f${i}`] ? ' is-invalid' : ''}`} spellcheck="false" autocomplete="off"
            aria-label=${`Serve flag ${i + 1}`} placeholder="--no-tray" value=${flag} disabled=${any} onInput=${(e) => setFlag(i, e.currentTarget.value)} />
          <${Button} size="sm" variant="ghost" class="cfg-launch-remove" title="Remove this flag" aria-label="Remove this flag" disabled=${any} onClick=${() => drop('flags', i)}>×<//></div>
          ${rowError(`f${i}`)}
        </div>`)}
        <div><${Button} size="sm" variant="link" id="cfg-launch-add-flag" disabled=${any} onClick=${() => setDraft({ ...draft, flags: [...draft.flags, ''] })}>+ flag<//></div>
        ${dropped.length > 0 && html`<div class="cfg-svc-warn" id="cfg-launch-dropped">Left out ${dropped.map((name, i) => html`${i ? ', ' : ''}<code key=${name}>${name}</code>`)}: a secret is not stored in the profile. Pass it through an inherited environment variable instead.</div>`}
      </div><//>
      <${Row} label="Environment"><div class="cfg-launch-rows" id="cfg-launch-env">
        ${draft.env.map((row, i) => html`<div key=${`e${i}`} class="cfg-launch-row" data-launch-row=${`e${i}`}>
          <div class="cfg-launch-fields">
            <input type="text" class=${`ui-input ui-sm cfg-launch-key${errors.rows[`e${i}`] ? ' is-invalid' : ''}`} spellcheck="false" autocomplete="off" aria-label=${`Variable ${i + 1} name`}
              placeholder="NAME" value=${row.key} disabled=${any} onInput=${(e) => setEnv(i, { key: e.currentTarget.value })} />
            <select class="ui-select ui-sm cfg-launch-mode" aria-label=${`Variable ${i + 1}: set a value or inherit agentd's`} disabled=${any}
              onChange=${(e) => setEnv(i, { mode: e.currentTarget.value, ...(e.currentTarget.value === INHERIT ? { value: '' } : {}) })}>
              <option value=${SET} selected=${row.mode === SET}>set</option>
              <option value=${INHERIT} selected=${row.mode === INHERIT}>inherit</option>
            </select>
            <input type="text" class="ui-input ui-sm cfg-launch-value" spellcheck="false" autocomplete="off" data-capture-mask="" aria-label=${`Variable ${i + 1} value`}
              disabled=${any || row.mode === INHERIT} value=${row.mode === INHERIT ? '' : row.value}
              placeholder=${row.mode === INHERIT ? (row.secret || secretKeys.has(row.key) ? 'secret · inherited, never stored' : "agentd's own value") : 'value'}
              onInput=${(e) => setEnv(i, { value: e.currentTarget.value })} />
            <${Button} size="sm" variant="ghost" class="cfg-launch-remove" title="Remove this variable" aria-label="Remove this variable" disabled=${any} onClick=${() => drop('env', i)}>×<//>
          </div>
          ${row.mode === INHERIT && row.key && missing.has(row.key) && html`<div class="cfg-launch-row-note cfg-svc-warn" data-launch-missing=${row.key}>agentd has no ${row.key} now: the unit would get it empty.</div>`}
          ${rowError(`e${i}`)}
        </div>`)}
        <div><${Button} size="sm" variant="link" id="cfg-launch-add-env" disabled=${any} onClick=${() => setDraft({ ...draft, env: [...draft.env, { key: '', mode: SET, value: '' }] })}>+ variable<//></div>
      </div><//>
      <${Row} label=""><div class="cfg-launch-actions">
        <${Button} size="sm" id="cfg-launch-use-running" disabled=${any || !view.suggested} title="Fill the form with the flags and environment agentd runs with now" onClick=${useRunning}>Use what's running now<//>
        <${Button} size="sm" variant="primary" id="cfg-launch-save" disabled=${any || !dirty} busy=${busy === 'save'} onClick=${save}>Save profile<//>
        <${Button} size="sm" variant="ghost" id="cfg-launch-discard" disabled=${any || !dirty} onClick=${discard}>Discard changes<//>
        ${view.profile && html`<${Button} size="sm" variant="danger" id="cfg-launch-clear" class="cfg-launch-clear" disabled=${any} busy=${busy === 'clear'} onClick=${() => { void clear(); }}>Clear profile…<//>`}
      </div><//>`}
    </div>
    ${note && html`<div class="cfg-svc-warn" role="status" id="cfg-launch-note">${note}</div>`}
    ${errors.general.map((text) => html`<div key=${text} class="cfg-svc-warn cfg-launch-err" role="alert">${text}</div>`)}
  </div>`;
}
