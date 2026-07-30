import { Fragment, h, render } from 'preact';
import { useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { registerToolbarProfilePickerController } from './toolbar-profile-picker.js';
import { isWizardActive } from './slop.js';
import {
  GLOBAL_DEFAULT_PROFILE_ROLE, GLOBAL_SANDBOX_PROFILE_ROLE,
} from './resolved-defaults.js';

const html = htm.bind(h);
const NEW_PROFILE = '/new-profile';

// GLOBAL_DEFAULT_PROFILE_NAME is the accessible name AND the tooltip's opening
// words. They were allowed to differ once — the tooltip said "Global", the
// aria-label said "Dashboard" — which gave one control two names depending on
// how you perceive it.
const GLOBAL_DEFAULT_PROFILE_NAME = 'Global default spawn profile';
const GLOBAL_SANDBOX_PROFILE_NAME = 'Global sandbox profile';

function labels(kind, current) {
  if (kind === 'sandbox') {
    return {
      id: 'dashboard-default-sandbox-profile', icon: '🛡', className: 'global-sandbox-profile',
      dataName: 'data-sandbox-profile', create: '＋ new sandbox profile…', none: '(none)',
      name: GLOBAL_SANDBOX_PROFILE_NAME,
      aria: current
        ? `${GLOBAL_SANDBOX_PROFILE_NAME}: ${current}. Click to change.`
        : `Set ${GLOBAL_SANDBOX_PROFILE_NAME.toLowerCase()}`,
      title: current
        ? `${GLOBAL_SANDBOX_PROFILE_NAME}: ${current} — ${GLOBAL_SANDBOX_PROFILE_ROLE}. Click to change.`
        : `No ${GLOBAL_SANDBOX_PROFILE_NAME.toLowerCase()} — click to set one. It would become ${GLOBAL_SANDBOX_PROFILE_ROLE}.`,
    };
  }
  return {
    id: 'dashboard-default-profile', icon: '🧠', className: 'user-default-model',
    dataName: 'data-profile', create: isWizardActive() ? '＋ new pattern…' : '＋ new profile…', none: '(none)',
    name: GLOBAL_DEFAULT_PROFILE_NAME,
    aria: current
      ? `${GLOBAL_DEFAULT_PROFILE_NAME}: ${current}. Click to change.`
      : `Set ${GLOBAL_DEFAULT_PROFILE_NAME.toLowerCase()}`,
    title: current
      ? `${GLOBAL_DEFAULT_PROFILE_NAME}: ${current} — ${GLOBAL_DEFAULT_PROFILE_ROLE}. Click to change.`
      : `No ${GLOBAL_DEFAULT_PROFILE_NAME.toLowerCase()} — click to set one. It is ${GLOBAL_DEFAULT_PROFILE_ROLE}.`,
  };
}

function ToolbarProfileControl({ kind, state, actions }) {
  const descriptor = state.editor.value;
  const active = descriptor?.kind === kind;
  const current = state.values[kind].value;
  const [choices, setChoices] = useState([]);
  const [loading, setLoading] = useState(false);
  const [saving, setSaving] = useState(false);
  const savingRef = useRef(false);
  const [error, setError] = useState('');
  const selectRef = useRef(null);
  const triggerRef = useRef(null);
  const restoreFocusRef = useRef(false);
  const copy = labels(kind, current);

  useLayoutEffect(() => {
    if (!active) return undefined;
    let live = true;
    setLoading(true);
    setError('');
    queueMicrotask(() => selectRef.current?.focus());
    actions.load(kind).then((result) => {
      if (live) setChoices(result.choices);
    }).catch((cause) => {
      if (live) setError(cause?.message || String(cause));
    }).finally(() => {
      if (live) setLoading(false);
    });
    return () => { live = false; };
  }, [active, kind]);

  useLayoutEffect(() => {
    if (active || !restoreFocusRef.current) return;
    restoreFocusRef.current = false;
    triggerRef.current?.focus();
  }, [active]);

  if (!active) {
    return html`<button ref=${triggerRef} type="button" id=${copy.id}
      class=${`${copy.className}${current ? '' : ' unset'}`}
      data-act=${kind === 'sandbox' ? 'set-dash-sandbox-profile' : 'set-dash-profile'}
      ...${{ [copy.dataName]: current }} aria-label=${copy.aria} title=${copy.title}
    >${copy.icon}${current ? ` ${current}` : ''}</button>`;
  }

  const missing = current && !choices.some((choice) => choice.value === current);
  const errorID = `toolbar-profile-${kind}-error`;
  const close = (restoreFocus = false) => {
    const closed = state.close(descriptor);
    if (closed && restoreFocus) restoreFocusRef.current = true;
    return closed;
  };
  return html`<${Fragment}><select ref=${selectRef} class="toolbar-profile-select" value=${current} disabled=${saving}
    aria-label=${copy.name}
    aria-busy=${loading ? 'true' : undefined} aria-invalid=${error ? 'true' : undefined}
    aria-describedby=${error ? errorID : undefined} title=${error || undefined}
    onKeyDown=${(event) => {
      if (event.key !== 'Escape' || savingRef.current) return;
      event.preventDefault();
      close(true);
    }}
    onBlur=${() => { if (!savingRef.current) close(); }}
    onChange=${async (event) => {
      if (savingRef.current) return;
      const name = event.currentTarget.value;
      if (name === NEW_PROFILE) {
        if (!close()) return;
        actions.openNew(kind, (created) => { void actions.commitFromEditor(kind, created); });
        return;
      }
      if (name === current) {
        close(true);
        return;
      }
      savingRef.current = true;
      setSaving(true);
      setError('');
      try {
        const committed = await actions.commit(kind, name);
        savingRef.current = false;
        setSaving(false);
        if (committed) {
          close(true);
        } else {
          queueMicrotask(() => selectRef.current?.focus());
        }
      } catch (cause) {
        setError(cause?.message || String(cause));
        savingRef.current = false;
        setSaving(false);
        queueMicrotask(() => selectRef.current?.focus());
      }
    }}>
    <option value=${NEW_PROFILE}>${copy.create}</option>
    <option value="">${copy.none}</option>
    ${choices.map((choice) => html`<option key=${choice.value} value=${choice.value}>${choice.label}</option>`)}
    ${missing ? html`<option value=${current}>${current} (missing)</option>` : null}
  </select>${error ? html`<span id=${errorID} class="toolbar-profile-error" role="alert" title=${error}>⚠ ${error}</span>` : null}</${Fragment}>`;
}

export function mountToolbarProfilePickerIsland({ profileHost, sandboxHost, state, actions, registerCleanup }) {
  let unregister = null;
  let cleaned = false;
  const cleanup = () => {
    if (cleaned) return;
    const failures = [];
    const attempt = (step) => { try { step(); } catch (error) { failures.push(error); } };
    attempt(() => { unregister?.(); unregister = null; });
    attempt(() => state.dispose());
    attempt(() => render(null, profileHost));
    attempt(() => render(null, sandboxHost));
    if (failures.length) throw new AggregateError(failures, 'toolbar profile picker cleanup failed');
    cleaned = true;
  };
  try {
    unregister = registerToolbarProfilePickerController(Object.freeze({ open: state.open, update: state.update }));
    render(html`<${ToolbarProfileControl} kind="profile" state=${state} actions=${actions}/>`, profileHost);
    render(html`<${ToolbarProfileControl} kind="sandbox" state=${state} actions=${actions}/>`, sandboxHost);
    registerCleanup(cleanup);
  } catch (error) {
    try { cleanup(); } catch (cleanupError) {
      throw new AggregateError([error, cleanupError], 'toolbar profile picker initialization failed');
    }
    throw error;
  }
}
