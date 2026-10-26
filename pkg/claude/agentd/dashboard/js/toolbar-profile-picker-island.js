import { Fragment, h, render } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { registerToolbarProfilePickerController } from './toolbar-profile-picker.js';
import { isWizardActive } from './slop.js';
import {
  GLOBAL_DEFAULT_PROFILE_ROLE, GLOBAL_SANDBOX_PROFILE_ROLE,
} from './resolved-defaults.js';
import { pickToolbarNode, toolbarNode, toolbarNodes, toolbarRefusal, toolbarTarget } from './toolbar-node.js';
import { featureState } from './peer-view-limits.js';
import { UsageProfileChip } from './usage-profile-chip.js';

const html = htm.bind(h);
const NEW_PROFILE = '/new-profile';

// On a peer's page the picker sets the peer's defaults (a peer grant), but
// profiles are created only on their own node: no "new profile" entry.
const remotePage = () => Boolean(globalThis.__tclaudeRemoteNode?.id);

// GLOBAL_DEFAULT_PROFILE_NAME is the accessible name AND the tooltip's opening
// words. They were allowed to differ once — the tooltip said "Global", the
// aria-label said "Dashboard" — which gave one control two names depending on
// how you perceive it.
const GLOBAL_DEFAULT_PROFILE_NAME = 'Global default spawn profile';
const GLOBAL_SANDBOX_PROFILE_NAME = 'Global sandbox profile';

// nodeName names the node the control shows (a fused Groups view, whose
// toolbar follows one of its nodes); blank on one node's page.
function labels(kind, current, nodeName = '') {
  const on = nodeName ? ` on ${nodeName}` : '';
  if (kind === 'sandbox') {
    const name = `${GLOBAL_SANDBOX_PROFILE_NAME}${on}`;
    return {
      id: 'dashboard-default-sandbox-profile', icon: '🛡', className: 'global-sandbox-profile',
      act: 'set-dash-sandbox-profile', dataName: 'data-sandbox-profile', create: '＋ new sandbox profile…', none: '(none)',
      name,
      aria: current
        ? `${name}: ${current}. Click to change.`
        : `Set ${name.toLowerCase()}`,
      title: current
        ? `${name}: ${current} — ${GLOBAL_SANDBOX_PROFILE_ROLE}. Click to change.`
        : `No ${name.toLowerCase()} — click to set one. It would become ${GLOBAL_SANDBOX_PROFILE_ROLE}.`,
    };
  }
  const name = `${GLOBAL_DEFAULT_PROFILE_NAME}${on}`;
  return {
    id: 'dashboard-default-profile', icon: '🧠', className: 'user-default-model',
    act: 'set-dash-profile', dataName: 'data-profile', create: isWizardActive() ? '＋ new pattern…' : '＋ new profile…', none: '(none)',
    name,
    aria: current
      ? `${name}: ${current}. Click to change.`
      : `Set ${name.toLowerCase()}`,
    title: current
      ? `${name}: ${current} — ${GLOBAL_DEFAULT_PROFILE_ROLE}. Click to change.`
      : `No ${name.toLowerCase()} — click to set one. It is ${GLOBAL_DEFAULT_PROFILE_ROLE}.`,
  };
}

// hiddenDefaults reports that node withholds its defaults from this one: its
// snapshot carries them only with its spawn catalog or defaults grant.
function hiddenDefaults(node) {
  return !node.local && featureState('spawn.catalog', node.peerView) !== 'shared'
    && featureState('spawn.defaults', node.peerView) !== 'shared';
}

// ToolbarNodeChip says whose defaults the toolbar shows in a fused Groups
// view, in that node's colour, and picks another of its nodes. It follows the
// group last clicked there, and flashes when it does.
function ToolbarNodeChip() {
  const nodes = toolbarNodes.value;
  const node = toolbarNode.value;
  const [flash, setFlash] = useState(false);
  const seen = useRef(node?.id || '');
  useEffect(() => {
    const id = node?.id || '';
    if (id === seen.current) return undefined;
    const first = !seen.current;
    seen.current = id;
    if (first || !id) return undefined;
    setFlash(true);
    const timer = setTimeout(() => setFlash(false), 900);
    return () => clearTimeout(timer);
  }, [node?.id]);
  if (nodes.length < 2 || !node) return null;
  return html`<span class=${`toolbar-node${flash ? ' flash' : ''}`} style=${`--nc:${node.color || 'currentColor'}`}
    title="The node whose global defaults and + new group this toolbar shows and sets. It follows the group you last clicked; pick another here.">
    <i aria-hidden="true"></i>
    <select class="toolbar-node-select" aria-label="Node the toolbar's global defaults act on" value=${node.id}
      onChange=${(event) => pickToolbarNode(event.currentTarget.value)}>
      ${nodes.map((n) => html`<option key=${n.id} value=${n.id}>${n.local ? '⌂ ' : ''}${n.name}</option>`)}
    </select>
  </span>`;
}

function ToolbarProfileControl({ kind, state, actions }) {
  const descriptor = state.editor.value;
  const active = descriptor?.kind === kind;
  const node = toolbarNode.value;
  const target = toolbarTarget(node);
  const hidden = Boolean(target) && hiddenDefaults(node);
  const current = !target ? state.values[kind].value : (kind === 'sandbox' ? node.sandbox : node.profile) || '';
  const [choices, setChoices] = useState([]);
  const [loading, setLoading] = useState(false);
  const [saving, setSaving] = useState(false);
  const savingRef = useRef(false);
  const [error, setError] = useState('');
  const selectRef = useRef(null);
  const triggerRef = useRef(null);
  const restoreFocusRef = useRef(false);
  const copy = labels(kind, current, node?.name || '');
  const refusal = toolbarRefusal(node, `[data-act="${copy.act}"]`);

  // The editor belongs to the node it opened on: the toolbar moving to
  // another node closes it.
  const openedOn = useRef(target);
  useLayoutEffect(() => {
    if (openedOn.current !== target && descriptor?.kind === kind) state.close(descriptor);
    openedOn.current = target;
  }, [target]);

  useLayoutEffect(() => {
    if (!active) return undefined;
    let live = true;
    setLoading(true);
    setError('');
    queueMicrotask(() => selectRef.current?.focus());
    actions.load(kind, target).then((result) => {
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
    // Another node's control runs here, gated by that node's grants: a
    // refused one keeps no data-act (nothing opens) and says why.
    const shown = hidden ? '?' : current;
    return html`<button ref=${triggerRef} type="button" id=${copy.id}
      class=${`${copy.className}${shown ? '' : ' unset'}${refusal ? ' toolbar-node-refused' : ''}`}
      data-act=${refusal ? undefined : copy.act} data-node=${target || undefined}
      aria-disabled=${refusal ? 'true' : undefined}
      ...${{ [copy.dataName]: current }} aria-label=${refusal ? `${copy.name}: ${refusal}` : copy.aria}
      title=${refusal ? `${refusal}\n\n${hidden ? copy.name : copy.title}` : copy.title}
    >${copy.icon}${shown ? ` ${shown}` : ''}</button>`;
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
        const committed = await actions.commit(kind, name, target, node?.name || '');
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
    ${remotePage() || target ? null : html`<option value=${NEW_PROFILE}>${copy.create}</option>`}
    <option value="">${copy.none}</option>
    ${choices.map((choice) => html`<option key=${choice.value} value=${choice.value}>${choice.label}</option>`)}
    ${missing ? html`<option value=${current}>${current} (missing)</option>` : null}
  </select>${error ? html`<span id=${errorID} class="toolbar-profile-error" role="alert" title=${error}>⚠ ${error}</span>` : null}</${Fragment}>`;
}

// ToolbarUsageProfile is the 💰 global default usage profile. Usage rules are
// this node's own, so it hides while the toolbar acts on another node.
function ToolbarUsageProfile() {
  if (toolbarTarget(toolbarNode.value) || remotePage()) return null;
  return html`<${UsageProfileChip} scope="global" className="global-usage-profile"/>`;
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
    render(html`<${ToolbarNodeChip}/><${ToolbarProfileControl} kind="profile" state=${state} actions=${actions}/>`, profileHost);
    render(html`<${ToolbarProfileControl} kind="sandbox" state=${state} actions=${actions}/><${ToolbarUsageProfile}/>`, sandboxHost);
    registerCleanup(cleanup);
  } catch (error) {
    try { cleanup(); } catch (cleanupError) {
      throw new AggregateError([error, cleanupError], 'toolbar profile picker initialization failed');
    }
    throw error;
  }
}
