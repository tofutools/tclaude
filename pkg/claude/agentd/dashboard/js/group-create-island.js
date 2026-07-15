import { h, render } from 'preact';
import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay } from './management-overlay.js';
import {
  createGroupCreateDraft,
  findGroupCreateTemplate,
  groupCreateDraftIsDirty,
  reconcileGroupCreateTemplates,
  selectGroupCreateSource,
  selectGroupCreateTemplate,
  validateGroupCreateDraft,
} from './group-create-model.js';
import {
  openGroupCreateModal,
  registerGroupCreateController,
} from './group-create-controller.js';
import {
  templateReadbackBadges,
  templateRosterRowsHTML,
} from './template-readback.js';

const html = htm.bind(h);

function Words({ plain, wizard, classPrefix = 'tpl-word' }) {
  return html`<span class=${`${classPrefix}-regular`}>${plain}</span
    ><span class=${`${classPrefix}-wizard`}>${wizard}</span>`;
}

function TemplatePreview({ template, name }) {
  if (!template) return null;
  const markup = `<div class="tp-badges">${templateReadbackBadges(template)}</div>`
    + templateRosterRowsHTML(template, name);
  return html`<div id="group-create-template-preview" class="template-preview"
    dangerouslySetInnerHTML=${{ __html: markup }} />`;
}

function GroupCreateDialog({
  current, state, actions, confirmDiscard, words,
}) {
  const baseline = useMemo(() => createGroupCreateDraft({
    templates: current.templates,
    groups: current.groups,
    presetTemplate: current.presetTemplate,
    parentGroup: current.parentGroup,
  }), [current]);
  const [draft, setDraft] = useState(baseline);
  const [templates, setTemplates] = useState(current.templates);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [browseBusy, setBrowseBusy] = useState(false);
  const nameRef = useRef(null);
  const submitLock = useRef(false);
  const templateRefresh = useRef(0);
  const directoryReturn = useRef(0);
  const template = findGroupCreateTemplate(templates, draft.template);
  const templateMode = !!template;
  const dirty = groupCreateDraftIsDirty(draft, baseline);

  useEffect(() => () => {
    templateRefresh.current += 1;
    directoryReturn.current += 1;
  }, []);

  const modelOptions = {
    templates,
    groups: current.groups,
    parentGroup: current.parentGroup,
  };
  const setField = (key, value) => setDraft((valueBefore) => ({
    ...valueBefore,
    [key]: value,
    ...(key === 'cwd' ? { cwdOrigin: 'user' } : {}),
  }));
  const changeTemplate = (name) => setDraft((value) =>
    selectGroupCreateTemplate(value, name, modelOptions));
  const changeSource = (name) => setDraft((value) =>
    selectGroupCreateSource(value, name, modelOptions));

  const close = async () => {
    if (busy) return;
    if (!dirty || await confirmDiscard()) state.close();
  };

  const submit = async () => {
    // Preact signal/state publication is asynchronous. This ref is deliberately
    // claimed before setBusy so two clicks/Enter events in one turn cannot
    // launch duplicate create or instantiate requests.
    if (submitLock.current) return;
    submitLock.current = true;
    const validation = validateGroupCreateDraft(draft, { templateMode });
    if (validation) {
      setError(validation);
      submitLock.current = false;
      return;
    }
    setError('');
    setBusy(true);
    try {
      const result = await actions.submit(draft, template, current.parentGroup);
      if (!state.isCurrent(current.generation)) return;
      state.close();
      actions.complete(result, current.parentGroup);
    } catch (cause) {
      if (state.isCurrent(current.generation)) {
        setError(cause?.message || String(cause));
        setBusy(false);
        submitLock.current = false;
      }
    }
  };

  const submitOnEnter = (event) => {
    if (
      event.key !== 'Enter' || event.isComposing || event.keyCode === 229 ||
      event.ctrlKey || event.metaKey
    ) return;
    event.preventDefault();
    void submit();
  };

  const browse = async () => {
    if (browseBusy) return;
    const request = ++directoryReturn.current;
    const generation = current.generation;
    setError('');
    setBrowseBusy(true);
    try {
      const result = await actions.pickDirectory({
        startDir: draft.cwd.trim(),
        title: 'Select the group default working directory',
      });
      if (
        request !== directoryReturn.current ||
        !state.isCurrent(generation)
      ) return;
      if (result.error) setError(result.error);
      else if (result.path) {
        setField('cwd', result.path);
        queueMicrotask(() => document.querySelector('#group-create-cwd')?.focus());
      }
    } finally {
      if (
        request === directoryReturn.current &&
        state.isCurrent(generation)
      ) setBrowseBusy(false);
    }
  };

  const refreshTemplates = async () => {
    const request = ++templateRefresh.current;
    const generation = current.generation;
    try {
      const next = await actions.loadTemplates();
      if (
        request !== templateRefresh.current ||
        !state.isCurrent(generation)
      ) return;
      setTemplates(next);
      setDraft((value) => reconcileGroupCreateTemplates(value, {
        templates: next,
        groups: current.groups,
        parentGroup: current.parentGroup,
      }));
    } catch (_) {
      // The live snapshot remains a usable fallback. Manager close must not
      // erase the operator's draft merely because its immediate rescan failed.
    }
  };

  const manageTemplates = () => {
    try {
      actions.openTemplateManager(() => { void refreshTemplates(); });
    } catch (cause) {
      setError(cause?.message || String(cause));
    }
  };

  const regularTitle = current.parentGroup
    ? `Create a subgroup under ${current.parentGroup}`
    : 'Create a new agent group';
  const wizardTitle = current.parentGroup
    ? `⚔ Form a sub-party under ${current.parentGroup}`
    : '⚔ Form a party';
  const sourceVisible = templateMode && !current.parentGroup;
  const parentVisible = sourceVisible && !!draft.source;
  const disabled = busy;

  return html`<${Overlay}
    id="group-create-modal"
    labelledby="group-create-title"
    onClose=${state.close}
    dirty=${dirty}
    blocked=${busy}
    confirmDiscard=${confirmDiscard}
    initialFocusRef=${nameRef}
  >
    <h3 id="group-create-title"><${Words}
      classPrefix="group-create-title"
      plain=${regularTitle}
      wizard=${wizardTitle}
    /></h3>
    <label class="cron-create-row">
      <span class="cron-create-label"><${Words}
        plain="Party profile" wizard="Summoning circle" /></span>
      <select id="group-create-template" value=${draft.template} disabled=${disabled}
        onChange=${(event) => changeTemplate(event.currentTarget.value)}>
        <option value="">${words('(blank party)', '(no circle — a blank party)')}</option>
        ${templates.map((entry) => html`<option key=${entry.name} value=${entry.name}>${entry.name}</option>`)}
      </select>
      <button id="group-create-manage-templates" type="button" class="tool"
        disabled=${disabled}
        title="Open the group templates manager to create or edit a circle — the created/edited circle is available in this picker when you close it"
        onClick=${manageTemplates}><${Words}
          plain="⧉ manage templates…" wizard="⧉ manage circles…" /></button>
    </label>
    <div class="cron-create-row" id="group-create-template-preview-row" hidden=${!templateMode}>
      <span class="cron-create-label"><${Words} plain="Roster" wizard="Party" /></span>
      <${TemplatePreview} template=${template} name=${draft.name} />
    </div>
    <label class="cron-create-row">
      <span class="cron-create-label">Name</span>
      <input ref=${nameRef} id="group-create-name" type="text" value=${draft.name}
        disabled=${disabled} onInput=${(event) => setField('name', event.currentTarget.value)}
        onKeyDown=${submitOnEnter} placeholder="kebab-or-snake-case label"
        autocomplete="off" spellcheck="false" />
    </label>
    <label class="cron-create-row" id="group-create-source-row" hidden=${!sourceVisible}>
      <span class="cron-create-label">Mirror settings</span>
      <select id="group-create-source" value=${draft.source} disabled=${disabled}
        onChange=${(event) => changeSource(event.currentTarget.value)}>
        <option value="">${words('template settings (top-level)', 'circle lore (top-level)')}</option>
        ${current.groups.filter((group) => group?.name).map((group) =>
          html`<option key=${group.name} value=${group.name}>${group.name}</option>`)}
      </select>
    </label>
    <label class="cron-create-enabled" id="group-create-parent-row" hidden=${!parentVisible}>
      <input id="group-create-parent" type="checkbox" checked=${draft.nested}
        disabled=${disabled} onChange=${(event) => setField('nested', event.currentTarget.checked)} />
      <span>Deploy as subgroup under the mirrored group</span>
    </label>
    <label class="cron-create-row group-create-descr-row">
      <span class="cron-create-label">Descr</span>
      <input id="group-create-descr" type="text" value=${draft.descr} disabled=${disabled}
        onInput=${(event) => setField('descr', event.currentTarget.value)}
        onKeyDown=${submitOnEnter} placeholder="optional one-line description"
        autocomplete="off" spellcheck="false" />
    </label>
    <label class="cron-create-row">
      <span class="cron-create-label">Default cwd</span>
      <input id="group-create-cwd" type="text" value=${draft.cwd} disabled=${disabled}
        onInput=${(event) => setField('cwd', event.currentTarget.value)}
        onKeyDown=${submitOnEnter}
        placeholder="optional — absolute path (~ OK) pre-filled when spawning agents into this group"
        autocomplete="off" spellcheck="false" />
      <button id="group-create-cwd-browse" type="button" class="dir-browse-btn"
        disabled=${disabled || browseBusy}
        title="Open a native directory picker on the daemon's desktop"
        onClick=${() => { void browse(); }}>${browseBusy ? 'Opening…' : 'Browse…'}</button>
    </label>
    <label class="cron-create-row">
      <span class="cron-create-label">Startup context</span>
      <textarea id="group-create-context" class="modal-context-textarea" rows="5"
        value=${draft.context} disabled=${disabled}
        onInput=${(event) => setField('context', event.currentTarget.value)}
        placeholder="optional — shared guidance delivered to the inbox of every agent spawned into this group (multi-line OK)"
        spellcheck="false"></textarea>
    </label>
    <label class="cron-create-row" id="group-create-task-row" hidden=${!templateMode}>
      <span class="cron-create-label">Task / project</span>
      <textarea id="group-create-task" class="modal-context-textarea" rows="4"
        value=${draft.task} disabled=${disabled}
        onInput=${(event) => setField('task', event.currentTarget.value)}
        placeholder="the assignment for this party — folded into the group context under ## Task so every spawned agent sees it (multi-line OK)"
        spellcheck="false"></textarea>
    </label>
    <label class="cron-create-row" id="group-create-max-members-row" hidden=${templateMode}>
      <span class="cron-create-label">Max members</span>
      <input id="group-create-max-members" type="number" min="0" step="1"
        value=${draft.maxMembers} disabled=${disabled}
        onInput=${(event) => setField('maxMembers', event.currentTarget.value)}
        placeholder="optional — 0 = unlimited; a spawn that would exceed it is refused"
        autocomplete="off" />
    </label>
    <div class="cron-create-error" id="group-create-error" role=${error ? 'alert' : undefined}>${error}</div>
    <div class="modal-buttons">
      <button id="group-create-cancel" type="button" disabled=${busy} onClick=${() => { void close(); }}>Cancel</button>
      <span class="spacer"></span>
      <button id="group-create-submit" class="primary" type="button" disabled=${busy}
        aria-busy=${busy ? 'true' : undefined}
        onClick=${() => { void submit(); }}>${busy
          ? (templateMode ? 'Creating & spawning…' : 'Creating…')
          : (templateMode ? 'Create & spawn' : 'Create')}</button>
    </div>
  </${Overlay}>`;
}

export function GroupCreateApp(props) {
  const current = props.state.dialog.value;
  if (!current) return null;
  return html`<${GroupCreateDialog}
    key=${current.key}
    current=${current}
    state=${props.state}
    actions=${props.actions}
    confirmDiscard=${props.confirmDiscard}
    words=${props.words}
  />`;
}

export function mountGroupCreateIsland({
  host, state, actions, confirmDiscard, words, registerCleanup,
}) {
  const controller = Object.freeze({ open: state.open });
  const unregister = registerGroupCreateController(controller);
  const toolbar = document.querySelector('#group-create-open');
  const openFromToolbar = () => openGroupCreateModal();
  toolbar?.addEventListener('click', openFromToolbar);
  render(html`<${GroupCreateApp}
    state=${state} actions=${actions} confirmDiscard=${confirmDiscard} words=${words}
  />`, host);
  registerCleanup(() => {
    toolbar?.removeEventListener('click', openFromToolbar);
    unregister();
    state.dispose();
    render(null, host);
  });
}
