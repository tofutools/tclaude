import { h, render } from 'preact';
import { useEffect } from 'preact/hooks';
import htm from 'htm';
import { DOCTOR_CATEGORIES } from './doctor-state.js';

const html = htm.bind(h);

// The badge poll is slow on purpose: one check forks bwrap, and the server
// caches results for a minute anyway. Entering the tab always refetches.
const POLL_MS = 60_000;

const ICON = { decision: '🔵', blocking: '🔴', warning: '🟡', info: '⚪', ok: '✅' };

async function readJSON(response) {
  const text = await response.text();
  let body = null;
  try { body = text ? JSON.parse(text) : null; } catch { body = text; }
  if (!response.ok) {
    const message = body?.error || body?.message || (typeof body === 'string' ? body : '') || `HTTP ${response.status}`;
    throw new Error(message);
  }
  return body;
}

export function createDoctorActions({ state, notify, fetchImpl = globalThis.fetch }) {
  async function load({ refresh = false } = {}) {
    if (state.phase.value === 'idle') state.phase.value = 'loading';
    try {
      const body = await readJSON(await fetchImpl(`/api/doctor${refresh ? '?refresh=1' : ''}`, { credentials: 'same-origin' }));
      state.checks.value = Array.isArray(body?.checks) ? body.checks : [];
      state.error.value = null;
      state.phase.value = 'ready';
    } catch (error) {
      state.error.value = error.message || String(error);
      state.phase.value = 'error';
    }
  }
  async function setSandboxDefault(value) {
    if (state.busy.value) return;
    state.busy.value = true;
    try {
      await readJSON(await fetchImpl('/api/sandbox-impl-default', {
        method: 'PUT', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ value }),
      }));
      notify?.(value === 'tclaude-layer'
        ? "New agents will use tclaude's sandbox"
        : "New agents will use each harness's own sandbox");
      await load({ refresh: true });
    } catch (error) {
      notify?.(`Could not save: ${error.message || error}`, true);
    } finally {
      state.busy.value = false;
    }
  }
  return Object.freeze({ load, setSandboxDefault });
}

function copy(text, notify) {
  globalThis.navigator?.clipboard?.writeText(text).then(
    () => notify?.('Copied'),
    () => notify?.('Copy failed — select the command and copy it by hand', true));
}

function SandboxDefaultControl({ check, state, actions }) {
  const busy = state.busy.value;
  if (check.severity === 'decision' || !check.value) {
    return html`<div class="doctor-choice">
      <div class="doctor-opt rec">
        <div class="doctor-opt-title">tclaude sandbox <span class="doctor-tag">recommended</span></div>
        <div class="doctor-opt-detail">bubblewrap on Linux, Seatbelt on macOS. The same wall for every harness, and it enforces your sandbox profiles.</div>
      </div>
      <div class="doctor-opt">
        <div class="doctor-opt-title">Harness sandboxes</div>
        <div class="doctor-opt-detail">Keep current behaviour: each harness confines itself with its own sandbox, or not at all.</div>
      </div>
      <div class="doctor-acts">
        <button type="button" class="tool doctor-primary" disabled=${busy} onClick=${() => actions.setSandboxDefault('tclaude-layer')}>Use tclaude sandbox</button>
        <button type="button" class="tool" disabled=${busy} onClick=${() => actions.setSandboxDefault('harness')}>Keep harness sandboxes</button>
      </div>
    </div>`;
  }
  const other = check.value === 'tclaude-layer' ? 'harness' : 'tclaude-layer';
  return html`<div class="doctor-acts">
    <button type="button" class="tool" disabled=${busy} onClick=${() => actions.setSandboxDefault(other)}>
      ${other === 'tclaude-layer' ? 'Switch to tclaude sandbox' : 'Switch to harness sandboxes'}
    </button>
  </div>`;
}

function CheckRow({ check, state, actions, notify }) {
  const categoryLabel = DOCTOR_CATEGORIES.find((c) => c.id === check.category)?.label;
  const showCategory = state.category.value === 'attention' || state.category.value === 'all';
  return html`<div class=${`doctor-row sev-${check.severity}`} data-check=${check.id}>
    <span class="doctor-icon" aria-label=${check.severity}>${ICON[check.severity] || '•'}</span>
    <div class="doctor-body">
      <div class="doctor-title">${check.title}</div>
      ${(check.detail || (showCategory && categoryLabel)) && html`<div class="doctor-detail">
        ${showCategory && categoryLabel && html`<span class="doctor-cat">${categoryLabel}</span>`}${showCategory && categoryLabel && check.detail ? ' · ' : ''}${check.detail}
      </div>`}
      ${check.fix && html`<div class="doctor-fix"><code>${check.fix}</code>
        <button type="button" class="doctor-copy" onClick=${() => copy(check.fix, notify)}>copy</button></div>`}
      ${check.action === 'sandbox-default' && html`<${SandboxDefaultControl} check=${check} state=${state} actions=${actions} />`}
    </div>
  </div>`;
}

function DoctorApp({ state, actions, notify }) {
  useEffect(() => {
    if (state.active.value) actions.load({ refresh: true });
  }, [state.active.value]);
  const counts = state.counts.value;
  const category = state.category.value;
  const visible = state.visible.value;
  const select = (id) => { state.category.value = id; };
  const heading = category === 'attention' ? 'Needs attention'
    : category === 'all' ? 'All checks'
      : DOCTOR_CATEGORIES.find((c) => c.id === category)?.label;
  let lede = '';
  if (category === 'attention') {
    lede = counts.attention === 0 ? 'Nothing needs your attention.'
      : `${counts.attention} item${counts.attention === 1 ? '' : 's'} across ${Object.keys(counts.byCategory).length} categor${Object.keys(counts.byCategory).length === 1 ? 'y' : 'ies'}.`;
  }
  return html`<div class="doctor-island">
    <div class="doctor-side" role="tablist" aria-label="Doctor categories">
      <button type="button" role="tab" class=${category === 'attention' ? 'on' : ''} aria-selected=${category === 'attention'}
        onClick=${() => select('attention')}>Needs attention <span>${counts.attention}</span></button>
      <div class="doctor-sep"></div>
      ${DOCTOR_CATEGORIES.map((c) => html`<button key=${c.id} type="button" role="tab" class=${category === c.id ? 'on' : ''}
        aria-selected=${category === c.id} onClick=${() => select(c.id)}>${c.label} <span>${counts.byCategory[c.id] || 0}</span></button>`)}
      <div class="doctor-sep"></div>
      <button type="button" role="tab" class=${category === 'all' ? 'on' : ''} aria-selected=${category === 'all'}
        onClick=${() => select('all')}>All checks <span>${counts.all}</span></button>
    </div>
    <div class="doctor-main">
      <div class="doctor-head">
        <div><h2>${heading}</h2>${lede && html`<p class="doctor-lede">${lede}</p>`}</div>
        <button type="button" class="tool" onClick=${() => actions.load({ refresh: true })}>Re-check</button>
      </div>
      ${state.phase.value === 'error' && html`<div class="doctor-error">Could not load checks: ${state.error.value}</div>`}
      ${state.phase.value === 'loading' && html`<p class="doctor-lede">Checking…</p>`}
      ${state.phase.value === 'ready' && (visible.length
        ? html`<div class="doctor-list">${visible.map((check) => html`<${CheckRow} key=${check.id}
            check=${check} state=${state} actions=${actions} notify=${notify} />`)}</div>`
        : html`<p class="doctor-lede">✅ All good here.</p>`)}
    </div>
  </div>`;
}

function DoctorBadge({ state }) {
  const { decision, blocking } = state.counts.value;
  return html`<span class="doctor-badges">
    <span class="tab-badge doctor-ask" hidden=${decision === 0} title="Decisions waiting for you">${decision}</span>
    <span class="tab-badge" hidden=${blocking === 0} title="Blocking problems">${blocking}</span>
  </span>`;
}

export function mountDoctorIsland({ host, badgeHost, state, notify, registerCleanup }) {
  const actions = createDoctorActions({ state, notify });
  render(html`<${DoctorApp} state=${state} actions=${actions} notify=${notify} />`, host);
  if (badgeHost) render(html`<${DoctorBadge} state=${state} />`, badgeHost);
  actions.load();
  const timer = setInterval(() => { if (!state.active.value) actions.load(); }, POLL_MS);
  registerCleanup(() => {
    clearInterval(timer);
    render(null, host);
    if (badgeHost) render(null, badgeHost);
  });
}
