import { h, render } from 'preact';
import { useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { registerToolbarProfilePickerController } from './toolbar-profile-picker.js';
import { SelectControl } from './select-control.js';
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

function useWizardMode() {
  const [wizard, setWizard] = useState(isWizardActive());
  useLayoutEffect(() => {
    const sync = () => setWizard(isWizardActive());
    document.addEventListener('tclaude:wizard', sync);
    return () => document.removeEventListener('tclaude:wizard', sync);
  }, []);
  return wizard;
}

function labels(kind, current, wizard) {
  if (kind === 'sandbox') {
    return {
      id: 'dashboard-default-sandbox-profile', icon: '🛡', className: 'global-sandbox-profile',
      create: wizard ? '＋ new ward…' : '＋ new sandbox profile…', none: '(none)',
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
    create: wizard ? '＋ new pattern…' : '＋ new profile…', none: '(none)',
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
  const wizard = useWizardMode();
  const descriptor = state.editor.value;
  const active = descriptor?.kind === kind;
  const current = state.values[kind].value;
  const [choices, setChoices] = useState([]);
  const [loading, setLoading] = useState(false);
  const [saving, setSaving] = useState(false);
  const savingRef = useRef(false);
  const [error, setError] = useState('');
  const copy = labels(kind, current, wizard);

  useLayoutEffect(() => {
    if (!active) return undefined;
    let live = true;
    setLoading(true);
    setError('');
    actions.load(kind).then((result) => {
      if (live) setChoices(result.choices);
    }).catch((cause) => {
      if (live) setError(cause?.message || String(cause));
    }).finally(() => {
      if (live) setLoading(false);
    });
    return () => { live = false; };
  }, [active, kind]);

  const missing = current && !choices.some((choice) => choice.value === current);
  const close = () => state.close(descriptor);
  const options = [
    { key: 'new', value: NEW_PROFILE, label: copy.create },
    { key: 'none', value: '', label: copy.none },
    ...choices,
    ...(missing ? [{ key: 'missing', value: current, label: `${current} (missing)` }] : []),
  ];
  return html`<${SelectControl}
    id=${copy.id}
    className=${`${copy.className}${current ? '' : ' unset'}`}
    popupClassName="toolbar-profile-popover"
    value=${current} options=${options} open=${active}
    busy=${saving} loading=${loading} error=${error}
    ariaLabel=${copy.name} title=${copy.title}
    onOpenChange=${(next) => {
      if (savingRef.current) return;
      if (next) state.open({ kind, current });
      else if (active) close();
    }}
    onValueChange=${async (name) => {
      if (savingRef.current) return;
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
          close();
        }
      } catch (cause) {
        setError(cause?.message || String(cause));
        savingRef.current = false;
        setSaving(false);
      }
    }}
  >${copy.icon}${current ? ` ${current}` : ''}</${SelectControl}>`;
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
