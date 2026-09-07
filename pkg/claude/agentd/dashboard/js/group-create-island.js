import { h, render } from 'preact';
import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import {
  ManagementOverlay as Overlay,
  useGuardedOverlayClose,
} from './management-overlay.js';
import {
  createGroupCreateDraft,
  derivedCloneDestination,
  findGroupCreateTemplate,
  groupCreateDraftIsDirty,
  reconcileGroupCreateTemplates,
  selectGroupCreateOrigin,
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
import { HelpDisclosure } from './help-field.js';

const html = htm.bind(h);
const GROUP_ENVIRONMENT_HELP = 'Inherited by fresh spawns and group terminals; profile and per-spawn values override matching names.';
const GROUP_ENVIRONMENT_HELP_WIZARD = 'Inherited by newly summoned familiars and sanctum terminals; pattern and per-summon runes override matching names.';

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

function GroupSourceSummary({ source, cloneMode, withAgents, copyOwners }) {
  if (!source) return null;
  const members = (source.members || []).filter(
    (member) => member && !(member.role === 'owner' && !member.descr),
  );
  const owners = (source.members || []).filter((member) => member?.owner);
  const row = (key, value, muted = false) => html`<div class="gcp-row">
    <span class="gcp-key">${key}</span>
    <span class=${`gcp-val${muted ? ' muted' : ''}`}>${value}</span>
  </div>`;
  return html`<div class="group-clone-preview group-create-source-summary"
    id="group-create-source-summary">
    <div class="gcp-title">${`Prefilled from ${source.name}`}</div>
    ${row('📁 directory', source.default_cwd || 'none', !source.default_cwd)}
    ${row('📝 description', source.descr || 'none', !source.descr)}
    ${row('📋 startup context', source.default_context
      ? `${source.default_context.length} chars` : 'none', !source.default_context)}
    ${row('🌐 environment', source.environment?.length
      ? `${source.environment.length} variable${source.environment.length === 1 ? '' : 's'}` : 'none',
    !source.environment?.length)}
    ${row('📎 attachment / link',
      source.attachment_label || source.attachment_label_override || source.attachment_url || 'none',
      !source.attachment_url)}
    ${cloneMode ? html`${row('🧠 profile', source.default_profile || 'none', !source.default_profile)}
      ${row('🔑 group permissions', source.permissions?.length
        ? String(source.permissions.length) : 'none', !source.permissions?.length)}
      ${row('👥 max members', source.max_members ? String(source.max_members) : 'unlimited', !source.max_members)}
      ${row('🔔 notifications', source.notify_enabled ? 'on' : 'off')}
      ${row('👤 owners', owners.length
        ? `${owners.length} — ${copyOwners ? 'copied' : 'skipped'}` : 'none',
      !owners.length || !copyOwners)}
      ${row('🤖 member agents', members.length
        ? `${members.length} (${members.filter((member) => member.online).length} online) — ${withAgents ? 'cloned with history' : 'skipped'}` : 'none',
      !members.length || !withAgents)}` : null}
  </div>`;
}

function GroupCreateDialog({
  current, state, actions, confirmDiscard, words,
}) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const baseline = useMemo(() => createGroupCreateDraft({
    templates: current.templates,
    groups: current.groups,
    presetTemplate: current.presetTemplate,
    parentGroup: current.parentGroup,
    cloneGroup: current.cloneGroup,
    defaultName: current.defaultName,
    placement: current.placement,
    cloneTransport: actions.cloneTransport(),
  }), [current]);
  const [draft, setDraft] = useState(baseline);
  const [templates, setTemplates] = useState(current.templates);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [browseBusy, setBrowseBusy] = useState(false);
  const [environmentHelpOpen, setEnvironmentHelpOpen] = useState('');
  const nameRef = useRef(null);
  const submitLock = useRef(false);
  const templateRefresh = useRef(0);
  const directoryReturn = useRef(0);
  const template = findGroupCreateTemplate(templates, draft.template);
  const templateMode = !!template;
  const cloneMode = !!draft.cloneGroup;
  const sourceGroupName = draft.source;
  const sourceGroup = current.groups.find((group) => group?.name === sourceGroupName) || null;
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
  const changeRepository = (repository) => setDraft((value) => {
    const previousAuto = derivedCloneDestination(value.repository);
    const nextAuto = derivedCloneDestination(repository);
    const destination = !value.cloneDestination.trim() || value.cloneDestination === previousAuto
      ? nextAuto : value.cloneDestination;
    return { ...value, repository, cloneDestination: destination };
  });
  const changeCloneTransport = (transport) => {
    actions.rememberCloneTransport(transport);
    setField('cloneTransport', transport);
  };
  const changeTemplate = (name) => setDraft((value) =>
    selectGroupCreateTemplate(value, name, modelOptions));
  const changeSource = (name) => setDraft((value) =>
    selectGroupCreateSource(value, name, modelOptions));
  const changeOrigin = (origin) => setDraft((value) =>
    selectGroupCreateOrigin(value, origin, modelOptions));

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
      actions.complete(result, draft.parent);
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
        startDir: (draft.workspaceMode === 'clone' ? draft.cloneDestination : draft.cwd).trim(),
        title: draft.workspaceMode === 'clone'
          ? 'Select where the repository should be cloned'
          : 'Select the group default working directory',
      });
      if (
        request !== directoryReturn.current ||
        !state.isCurrent(generation)
      ) return;
      if (result.error) setError(result.error);
      else if (result.path) {
        const field = draft.workspaceMode === 'clone' ? 'cloneDestination' : 'cwd';
        setField(field, result.path);
        queueMicrotask(() => document.querySelector(
          draft.workspaceMode === 'clone' ? '#group-create-clone-destination' : '#group-create-cwd',
        )?.focus());
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

  const regularTitle = 'Create group';
  const wizardTitle = '⚔ Form a party';
  const sourceVisible = templateMode;
  const disabled = busy;

  return html`<${Overlay}
    id="group-create-modal"
    labelledby="group-create-title"
    onClose=${state.close}
    onSubmitHotkey=${submit}
    dirty=${dirty}
    blocked=${busy}
    confirmDiscard=${confirmDiscard}
    registerClose=${registerClose}
    initialFocusRef=${nameRef}
  >
    <h3 id="group-create-title"><${Words}
      classPrefix="group-create-title"
      plain=${regularTitle}
      wizard=${wizardTitle}
    /></h3>
    <p class="modal-hint">Start blank or prefill from an existing group or template.</p>
    <div class="cron-create-row group-create-origin-row">
      <span class="cron-create-label">Start from</span>
      <div class="group-create-origin-options" role="group" aria-label="Group source">
        ${[
          ['blank', words('Blank', 'Blank')],
          ['group', words('Group', 'Party')],
          ['template', words('Template', 'Circle')],
        ].map(([value, label]) => html`<button type="button" key=${value}
          class=${draft.origin === value ? 'selected' : ''}
          aria-pressed=${draft.origin === value} disabled=${disabled}
          onClick=${() => changeOrigin(value)}>${label}</button>`)}
      </div>
    </div>
    ${draft.origin === 'group' ? html`<label class="cron-create-row">
      <span class="cron-create-label"><${Words} plain="Source group" wizard="Source party" /></span>
      <select id="group-create-group-source" value=${draft.source}
        data-current-value=${draft.source} disabled=${disabled}
        onChange=${(event) => changeSource(event.currentTarget.value)}>
        <option value="">${words('(select group)', '(choose a party)')}</option>
        ${current.groups.filter((group) => group?.name).map((group) =>
          html`<option key=${group.name} value=${group.name}>${group.name}</option>`)}
      </select>
    </label>` : null}
    ${draft.origin === 'template' ? html`<label class="cron-create-row">
      <span class="cron-create-label"><${Words}
        plain="Group template" wizard="Summoning circle" /></span>
      <select id="group-create-template" value=${draft.template}
        data-current-value=${draft.template} disabled=${disabled}
        onChange=${(event) => changeTemplate(event.currentTarget.value)}>
        <option value="">${words('(blank group)', '(no circle — a blank party)')}</option>
        ${templates.map((entry) => html`<option key=${entry.name} value=${entry.name}>${entry.name}</option>`)}
      </select>
      <button id="group-create-manage-templates" type="button" class="tool"
        disabled=${disabled}
        title=${words(
          'Open the group templates manager to create or edit a template — the created/edited template is available in this picker when you close it',
          'Open the group templates manager to create or edit a circle — the created/edited circle is available in this picker when you close it',
        )}
        onClick=${manageTemplates}><${Words}
          plain="⧉ manage templates…" wizard="⧉ manage circles…" /></button>
    </label>` : null}
    <${GroupSourceSummary} source=${sourceGroup} cloneMode=${cloneMode}
      withAgents=${draft.withAgents} copyOwners=${draft.copyOwners} />
    <label class="cron-create-row">
      <span class="cron-create-label">${cloneMode ? 'New name' : 'Name'}</span>
      <input ref=${nameRef} id="group-create-name" type="text" value=${draft.name}
        disabled=${disabled} onInput=${(event) => setField('name', event.currentTarget.value)}
        onKeyDown=${submitOnEnter} placeholder="kebab-or-snake-case label"
        data-select-on-focus=${cloneMode || undefined}
        autocomplete="off" spellcheck="false" />
    </label>
    <label class="cron-create-row group-create-placement-row">
      <span class="cron-create-label">Place under</span>
      <select id="group-create-placement" value=${draft.parent}
        data-current-value=${draft.parent} disabled=${disabled}
        onChange=${(event) => setField('parent', event.currentTarget.value)}>
        <option value="">Top level</option>
        ${current.groups.filter((group) => group?.name && group.name !== draft.name).map((group) =>
          html`<option key=${group.name} value=${group.name}>${group.name}</option>`)}
      </select>
    </label>
    ${cloneMode ? html`
      <label class="cron-create-enabled">
        <input id="group-create-with-agents" type="checkbox" checked=${draft.withAgents}
          disabled=${disabled} onChange=${(event) => setField('withAgents', event.currentTarget.checked)} />
        Clone member agents too
      </label>
      <label class="cron-create-enabled">
        <input id="group-create-copy-owners" type="checkbox" checked=${draft.copyOwners}
          disabled=${disabled} onChange=${(event) => setField('copyOwners', event.currentTarget.checked)} />
        Copy source owners too
      </label>
    ` : null}
    <div class="cron-create-row" id="group-create-template-preview-row" hidden=${!templateMode}>
      <span class="cron-create-label"><${Words} plain="Roster" wizard="Party" /></span>
      <${TemplatePreview} template=${template} name=${draft.name} />
    </div>
    <label class="cron-create-row" id="group-create-source-row" hidden=${!sourceVisible}>
      <span class="cron-create-label">Mirror settings</span>
      <select id="group-create-source" value=${draft.source} disabled=${disabled}
        onChange=${(event) => changeSource(event.currentTarget.value)}>
        <option value="">${words('template settings (top-level)', 'circle lore (top-level)')}</option>
        ${current.groups.filter((group) => group?.name).map((group) =>
          html`<option key=${group.name} value=${group.name}>${group.name}</option>`)}
      </select>
    </label>
    <label class="cron-create-row group-create-descr-row">
      <span class="cron-create-label">Descr</span>
      <input id="group-create-descr" type="text" value=${draft.descr} disabled=${disabled}
        onInput=${(event) => setField('descr', event.currentTarget.value)}
        onKeyDown=${submitOnEnter} placeholder="optional one-line description"
        autocomplete="off" spellcheck="false" />
    </label>
    <fieldset class="group-create-workspace">
      <legend>Workspace</legend>
      <div class="group-create-workspace-modes" role="group" aria-label="Workspace source">
        <button type="button" class=${draft.workspaceMode === 'existing' ? 'selected' : ''}
          aria-pressed=${draft.workspaceMode === 'existing'} disabled=${disabled}
          onClick=${() => setField('workspaceMode', 'existing')}>Use existing directory</button>
        <button type="button" class=${draft.workspaceMode === 'clone' ? 'selected' : ''}
          aria-pressed=${draft.workspaceMode === 'clone'} disabled=${disabled}
          onClick=${() => setField('workspaceMode', 'clone')}>Clone repository</button>
      </div>
      ${draft.workspaceMode === 'existing' ? html`
        <label class="cron-create-row group-create-workspace-row">
          <span class="cron-create-label">Directory</span>
          <input id="group-create-cwd" type="text" value=${draft.cwd} disabled=${disabled}
            onInput=${(event) => setField('cwd', event.currentTarget.value)}
            onKeyDown=${submitOnEnter}
            placeholder="absolute path (~ OK) pre-filled when spawning agents"
            autocomplete="off" spellcheck="false" />
          <button id="group-create-cwd-browse" type="button" class="dir-browse-btn"
            disabled=${disabled || browseBusy} title="Browse for a directory"
            onClick=${() => { void browse(); }}>${browseBusy ? 'Opening…' : 'Browse…'}</button>
        </label>
      ` : html`
        <label class="cron-create-row group-create-workspace-row group-create-workspace-simple-row">
          <span class="cron-create-label">Repository</span>
          <input id="group-create-repository" type="text" value=${draft.repository} disabled=${disabled}
            onInput=${(event) => changeRepository(event.currentTarget.value)}
            onKeyDown=${submitOnEnter} placeholder="github.com/owner/repository"
            autocomplete="off" spellcheck="false" />
        </label>
        <div class="cron-create-row group-create-workspace-row group-create-workspace-simple-row">
          <span class="cron-create-label">Clone with</span>
          <div class="group-create-clone-transport" role="group" aria-label="Clone transport">
            <button type="button" class=${draft.cloneTransport === 'ssh' ? 'selected' : ''}
              aria-pressed=${draft.cloneTransport === 'ssh'} disabled=${disabled}
              onClick=${() => changeCloneTransport('ssh')}>SSH</button>
            <button type="button" class=${draft.cloneTransport === 'https' ? 'selected' : ''}
              aria-pressed=${draft.cloneTransport === 'https'} disabled=${disabled}
              onClick=${() => changeCloneTransport('https')}>HTTPS</button>
          </div>
        </div>
        <label class="cron-create-row group-create-workspace-row">
          <span class="cron-create-label">Clone into</span>
          <input id="group-create-clone-destination" type="text" value=${draft.cloneDestination}
            disabled=${disabled} onInput=${(event) => setField('cloneDestination', event.currentTarget.value)}
            onKeyDown=${submitOnEnter} placeholder="absolute path (~ OK); missing parent directories are created"
            autocomplete="off" spellcheck="false" />
          <button id="group-create-clone-browse" type="button" class="dir-browse-btn"
            disabled=${disabled || browseBusy} title="Browse for a clone destination"
            onClick=${() => { void browse(); }}>${browseBusy ? 'Opening…' : 'Browse…'}</button>
        </label>
        <label class="group-create-attach-repository">
          <input type="checkbox" checked=${draft.attachRepository} disabled=${disabled}
            onChange=${(event) => setField('attachRepository', event.currentTarget.checked)} />
          <span>Show repository link on the group</span>
        </label>
        <div class="group-create-clone-hint">The checkout becomes this group’s default working directory.</div>
      `}
    </fieldset>
    <label class="cron-create-row">
      <span class="cron-create-label">Startup context</span>
      <textarea id="group-create-context" class="modal-context-textarea" rows="5"
        value=${draft.context} disabled=${disabled}
        onInput=${(event) => setField('context', event.currentTarget.value)}
        placeholder="optional — shared guidance delivered to the inbox of every agent spawned into this group (multi-line OK)"
        spellcheck="false"></textarea>
    </label>
    <div class="cron-create-row" id="group-create-environment-row">
      <span class="cron-create-label"><${Words} plain="Environment" wizard="Summoning runes" /></span>
      <div class="cron-create-target">
        <div class="sbx-rows">${draft.environment.map((entry, index) => html`
          <div key=${index} class="sbx-row sbx-environment-row">
            <input class="sbx-env-name" placeholder="NAME" value=${entry.name}
              disabled=${disabled}
              onInput=${(event) => setField('environment', draft.environment.map((row, rowIndex) =>
                rowIndex === index ? { ...row, name: event.currentTarget.value } : row))} />
            <input class="sbx-env-value" placeholder="value" value=${entry.value}
              disabled=${disabled}
              onInput=${(event) => setField('environment', draft.environment.map((row, rowIndex) =>
                rowIndex === index ? { ...row, value: event.currentTarget.value } : row))} />
            <button type="button" disabled=${disabled}
              onClick=${() => setField('environment', draft.environment.filter((_, rowIndex) => rowIndex !== index))}>×</button>
          </div>`)}
        </div>
        <div class="sbx-add-row-help">
          <button type="button" class="sbx-add-row" disabled=${disabled}
            onClick=${() => setField('environment', [...draft.environment, { name: '', value: '' }])}>
            <${Words} plain="＋ add variable" wizard="✦ bind rune" />
          </button>
          <${HelpDisclosure} id="group-create-environment-help" label="Group environment variables"
            help=${GROUP_ENVIRONMENT_HELP}
            content=${html`<${Words} plain=${GROUP_ENVIRONMENT_HELP} wizard=${GROUP_ENVIRONMENT_HELP_WIZARD} />`}
            open=${environmentHelpOpen === 'group-create-environment-help'} setOpen=${setEnvironmentHelpOpen} />
        </div>
      </div>
    </div>
    <label class="cron-create-row" id="group-create-task-row" hidden=${!templateMode}>
      <span class="cron-create-label">Task / project</span>
      <textarea id="group-create-task" class="modal-context-textarea" rows="4"
        value=${draft.task} disabled=${disabled}
        onInput=${(event) => setField('task', event.currentTarget.value)}
        placeholder=${words(
          'the assignment for this group — folded into the group context under ## Task so every spawned agent sees it (multi-line OK)',
          'the assignment for this party — folded into the group context under ## Task so every spawned agent sees it (multi-line OK)',
        )}
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
    <label class="cron-create-row">
      <span class="cron-create-label"><${Words}
        plain="Link / attachment" wizard="Quest portal" /></span>
      <input id="group-create-attachment-url" type="url" value=${draft.attachmentURL}
        disabled=${disabled}
        onInput=${(event) => setField('attachmentURL', event.currentTarget.value)}
        onKeyDown=${submitOnEnter} placeholder="https://…"
        autocomplete="off" spellcheck="false" />
    </label>
    <label class="cron-create-row">
      <span class="cron-create-label"><${Words}
        plain="Link label" wizard="Portal name" /></span>
      <input id="group-create-attachment-label" type="text" value=${draft.attachmentLabel}
        disabled=${disabled}
        onInput=${(event) => setField('attachmentLabel', event.currentTarget.value)}
        onKeyDown=${submitOnEnter} placeholder="optional display name / alias"
        autocomplete="off" spellcheck="false" />
    </label>
    <div class="cron-create-error" id="group-create-error" role=${error ? 'alert' : undefined}>${error}</div>
    <div class="modal-buttons">
      <button id="group-create-cancel" type="button" disabled=${busy} onClick=${() => { void requestClose(); }}>Cancel</button>
      <span class="spacer"></span>
      <button id="group-create-submit" class="primary" type="button" disabled=${busy}
        aria-busy=${busy ? 'true' : undefined}
        onClick=${() => { void submit(); }}>${busy
          ? (draft.workspaceMode === 'clone' && !cloneMode ? 'Cloning & creating…' : 'Creating…')
          : 'Create group'}</button>
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
  const controller = Object.freeze({ open: state.open, openClone: state.openClone });
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
// dashboard-imperative-boundary: preact-compat
