import { h, render } from 'preact';
import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';
import { registerToolbarProfilePickerController } from './toolbar-profile-picker.js';
import { hasShownOverlay } from './overlay-stack.js';
import { wizWord } from './slop.js';

const html = htm.bind(h);
const NEW_PROFILE = '/new-profile';

function PickerDialog({ current, state, actions }) {
  const [choices, setChoices] = useState([]);
  const [value, setValue] = useState(current.current);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  const selectRef = useRef(null);
  const cancelRef = useRef(null);
  const submitLock = useRef(false);
  const labels = useMemo(() => current.kind === 'sandbox' ? {
    title: 'Set global sandbox profile', none: '(none)', create: '＋ new sandbox profile…',
  } : {
    title: 'Set dashboard default spawn profile', none: '(none)',
    create: wizWord('＋ new profile…', '＋ new pattern…'),
  }, [current.kind]);

  useEffect(() => {
    let live = true;
    actions.load(current.kind).then((result) => {
      if (!live) return;
      setChoices(result.choices);
      setLoading(false);
    }).catch((cause) => {
      if (!live) return;
      setError(cause?.message || String(cause));
      setLoading(false);
    });
    return () => { live = false; };
  }, [current]);

  useLayoutEffect(() => {
    const select = selectRef.current;
    const dialog = select?.closest('[role="dialog"]');
    if (!loading && dialog?.contains(select.ownerDocument.activeElement)) select.focus();
  }, [loading]);

  const close = () => {
    state.close();
    setTimeout(() => {
      if (!state.dialog.value && !hasShownOverlay()) {
        document.getElementById(current.producerId)?.focus();
      }
    }, 0);
  };
  const submit = async (selected = value) => {
    if (submitLock.current || loading || saving) return;
    if (selected === NEW_PROFILE) {
      state.close();
      actions.openNew(current.kind, (name) => { void actions.commitFromEditor(current.kind, name); });
      return;
    }
    if (selected === current.current) { close(); return; }
    submitLock.current = true;
    setSaving(true);
    setError('');
    try {
      if (await actions.commit(current.kind, selected)) close();
    } catch (cause) {
      setError(cause?.message || String(cause));
    } finally {
      submitLock.current = false;
      setSaving(false);
    }
  };

  return html`<${Overlay} id="toolbar-profile-picker-modal" labelledby="toolbar-profile-picker-title"
    onClose=${close} blocked=${saving} initialFocusRef=${loading ? cancelRef : selectRef}>
    <h3 id="toolbar-profile-picker-title">${labels.title}</h3>
    <label class="cron-create-row"><span class="cron-create-label">Profile</span>
      <select ref=${selectRef} class="group-default-profile-select" value=${value} disabled=${loading || saving}
        onChange=${(event) => {
          const selected = event.currentTarget.value;
          setValue(selected);
          void submit(selected);
        }}>
        <option value=${NEW_PROFILE}>${labels.create}</option>
        <option value="">${labels.none}</option>
        ${choices.map((choice) => html`<option key=${choice.value} value=${choice.value}>${choice.label}</option>`)}
        ${current.current && !choices.some((choice) => choice.value === current.current)
          ? html`<option value=${current.current}>${current.current} (missing)</option>` : null}
      </select>
    </label>
    <div class="cron-create-error" role="alert">${error}</div>
    <div class="modal-buttons"><button ref=${cancelRef} type="button" disabled=${saving} onClick=${close}>Cancel</button>
      <span class="spacer"></span><button type="button" class="primary" disabled=${loading || saving} onClick=${() => { void submit(); }}>Apply</button></div>
  </${Overlay}>`;
}

function App({ state, actions }) {
  const current = state.dialog.value;
  return current ? html`<${PickerDialog} key=${current.key} current=${current} state=${state} actions=${actions}/>` : null;
}

export function mountToolbarProfilePickerIsland({ host, state, actions, registerCleanup }) {
  const unregister = registerToolbarProfilePickerController(Object.freeze({ open: state.open }));
  render(html`<${App} state=${state} actions=${actions}/>`, host);
  registerCleanup(() => {
    unregister();
    state.dispose();
    render(null, host);
  });
}
