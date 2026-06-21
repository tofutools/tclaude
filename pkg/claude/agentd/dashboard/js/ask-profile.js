import { $, esc } from './helpers.js';
import { toast } from './refresh.js';

// ===================================================================
// Ask defaults — the Config tab's small editor for `tclaude ask`'s
// default model/effort profile (JOH-253).
//
// A thin live editor over config.json's "ask" block, backed by the
// dedicated /api/ask-profile endpoint (GET resolved profile + the
// harness catalog for the selectors; POST validates + merges). It is a
// twin of the Costs tab's cost-factor control, placed in the Config tab
// because that is where the spec puts the ask defaults. Applied
// immediately by its own button — independent of the big form's
// dry-run/diff Save, which round-trips the ask block untouched.
//
// After a successful apply the on-disk config has changed under the big
// form, so we fire a 'config-disk-changed' event the Config tab listens
// for to resync its baseline (avoiding a spurious drift 409 on a later
// Save). See config.js.
// ===================================================================
let askProfileLoaded = false;

function setAskProfileStatus(msg, isError) {
  const el = $('#ask-profile-status');
  if (!el) return;
  el.textContent = msg || '';
  el.classList.toggle('error', !!isError);
}

// fillSelect (re)builds a selector: a leading "Fast default (X)" option
// with an empty value (= unpinned, resolves to the built-in default),
// then one option per catalog value. When the stored value is set but
// absent from the catalog (e.g. a hand-edited full model ID), it is
// added so the form shows what is actually on disk rather than silently
// snapping to a different option.
function fillSelect(sel, values, defaultLabel, current, isSet) {
  if (!sel) return;
  const opts = [`<option value="">Fast default (${esc(defaultLabel)})</option>`];
  const list = (values || []).slice();
  if (isSet && current && !list.includes(current)) list.unshift(current);
  for (const v of list) opts.push(`<option value="${esc(v)}">${esc(v)}</option>`);
  sel.innerHTML = opts.join('');
  // An unpinned field selects the "Fast default" (empty) option even
  // though it resolves to the same alias, so the human sees "unpinned"
  // rather than a concrete pin they did not choose.
  sel.value = isSet ? current : '';
}

// loadAskProfile fetches the resolved profile + catalog and populates the
// two selectors. Best-effort: a failure leaves the selectors empty rather
// than blocking the rest of the Config tab.
async function loadAskProfile() {
  try {
    const r = await fetch('/api/ask-profile', { credentials: 'same-origin' });
    if (!r.ok) throw new Error('HTTP ' + r.status);
    const data = await r.json();
    fillSelect($('#ask-model'), data.models, data.default_model || 'haiku',
      data.model, !!data.model_set);
    fillSelect($('#ask-effort'), data.efforts, data.default_effort || 'low',
      data.effort, !!data.effort_set);
    askProfileLoaded = true;
    setAskProfileStatus('');
  } catch (e) {
    setAskProfileStatus('could not load', true);
  }
}

// saveAskProfile persists the two selectors' values. An empty value
// clears that field (resolves back to the fast default); the server
// validates against the harness catalog and 400s an invalid value. On
// success it reloads to reflect the canonical resolved/“set” state and
// signals the Config tab to resync its baseline.
async function saveAskProfile() {
  const modelSel = $('#ask-model');
  const effortSel = $('#ask-effort');
  if (!modelSel || !effortSel) return;
  const btn = $('#ask-profile-apply');
  if (btn) btn.disabled = true;
  setAskProfileStatus('saving…');
  try {
    const r = await fetch('/api/ask-profile', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ model: modelSel.value, effort: effortSel.value }),
    });
    if (!r.ok) {
      const d = await r.json().catch(() => ({}));
      throw new Error(d.error || ('HTTP ' + r.status));
    }
    setAskProfileStatus('saved');
    await loadAskProfile();
    // The on-disk config changed under the big form — let it resync so a
    // later "Save changes" doesn't 409 on stale-baseline drift.
    document.dispatchEvent(new CustomEvent('config-disk-changed'));
    toast('Ask defaults saved');
  } catch (e) {
    setAskProfileStatus(e.message || String(e), true);
  } finally {
    if (btn) btn.disabled = false;
  }
}

// bindAskProfileSection lazy-loads on the first Config tab activation and
// wires the Apply button. Mirrors config.js's bindConfigTab; both listen
// on the same nav button, which is fine.
function bindAskProfileSection() {
  const navBtn = $('nav button[data-tab="config"]');
  if (navBtn) navBtn.addEventListener('click', () => {
    if (!askProfileLoaded) loadAskProfile();
  });
  const btn = $('#ask-profile-apply');
  if (btn) btn.addEventListener('click', saveAskProfile);
}

export { bindAskProfileSection };
