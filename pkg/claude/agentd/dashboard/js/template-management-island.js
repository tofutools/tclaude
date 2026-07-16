import { h } from 'preact';
import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { profileSummary, getDashDefaultProfile, findProfileByHandle, profileChoices } from './profiles.js';
import { pickDirectory } from './helpers.js';
import { ManagementOverlay as Overlay, useGuardedOverlayClose } from './management-overlay.js';
import { wizWord } from './slop.js';
import {
  agentHasLegacyLaunch,
  agentInheritsDeployDefault,
  blankTemplateAgent,
  effectiveTemplateOwner,
  filterTemplates,
  moveItem,
  templateDraft,
  templatePayload,
  templateWaveCount,
} from './template-management-model.js';

const html = htm.bind(h);
const clone = (value) => JSON.parse(JSON.stringify(value));
const message = (error) => error?.message || String(error);

function Words({ plain, wizard }) {
  return html`<span class="tpl-word-regular">${plain}</span
    ><span class="tpl-word-wizard">${wizard}</span>`;
}

function oneLine(value) {
  const first = String(value || '')
    .split('\n')[0]
    .trim();
  return first.length > 60 ? `${first.slice(0, 57)}…` : first;
}

function TemplateCard({ template, groups, profiles, actions }) {
  const agents = template.agents || [];
  const forces = groups.filter(
    (group) => group.source_template === template.name,
  );
  const waves = templateWaveCount(template);
  return html`<div
    class="template-card"
    data-key=${template.name}
    data-template=${template.name}
  >
    <div class="tc-head">
      <span class="tc-name">${template.name}</span>${template.descr &&
      html`<span class="tc-descr">${template.descr}</span>`}
      <span class="tc-count"
        >${agents.length}
        <${Words}
          plain=${`agent${agents.length === 1 ? '' : 's'}`}
          wizard=${`familiar${agents.length === 1 ? '' : 's'}`}
      /></span>
      ${template.work_pattern?.length > 0 &&
      html`<span
        class="tc-count"
        title=${wizWord(
          'work pattern — ordered briefing messages delivered after the team spawns',
          'rite of command — ordered whispers delivered once the party stands',
        )}
        >⇶ ${template.work_pattern.length}-step
        <${Words} plain="pattern" wizard="rite"
      /></span>`}
      ${template.process?.length > 0 &&
      html`<span
        class="tc-count"
        title=${wizWord(
          'process — an ordered, advisory phase plan tracked at runtime',
          'quest plan — an ordered, advisory chapter plan tracked as the party works',
        )}
        >◆ ${template.process.length}-<${Words}
          plain="phase process"
          wizard="chapter quest"
      /></span>`}
      ${template.rhythms?.length > 0 &&
      html`<span
        class="tc-count"
        title=${wizWord(
          'rhythms — recurring nudges materialized as group cron jobs at deploy',
          'drumbeats — recurring nudges cast as group cron jobs when the party is summoned',
        )}
        >🥁 ${template.rhythms.length}
        <${Words}
          plain=${`rhythm${template.rhythms.length === 1 ? '' : 's'}`}
          wizard=${`drumbeat${template.rhythms.length === 1 ? '' : 's'}`}
      /></span>`}
      ${waves > 1 &&
      html`<span
        class="tc-count"
        title=${wizWord(
          'staged spawn — agents span multiple waves; higher waves spawn once the prior wave settles',
          'marching order — the party musters in waves, each after the last has drawn breath',
        )}
        >🌊 ${waves} <${Words} plain="waves" wizard="ranks"
      /></span>`}
      <span class="tc-actions"
        ><button
          class="primary"
          data-tact="deploy"
          data-template=${template.name}
          title=${wizWord(
            'Summon a team from this template — state a mission to deploy against, or leave it blank to just create the group',
            'Summon a hero party from this circle — name a quest to send them on, or leave it blank to just gather the party',
          )}
          onClick=${() => actions.openTemplateDeploy(template.name)}
        >
          <${Words} plain="🚀 deploy" wizard="🧙 summon" /></button
        ><button
          class="tool"
          data-tact="edit"
          data-template=${template.name}
          onClick=${() => actions.openTemplateEditor(template)}
        >
          edit</button
        ><button
          class="tool"
          data-tact="duplicate"
          data-template=${template.name}
          title=${wizWord(
            'Make a full copy of this template under a new name',
            'Mirror this circle — chalk an identical copy under a new name',
          )}
          onClick=${() => actions.openTemplateDuplicate(template.name)}
        >
          <${Words} plain="⧉ duplicate" wizard="🪞 mirror" /></button
        ><button
          class="tool"
          data-tact="export"
          data-template=${template.name}
          title=${wizWord(
            'Download this template as a portable .task-force.json file to share or re-import',
            'Inscribe this circle onto a scroll — a portable .task-force.json to carry or copy',
          )}
          onClick=${() => actions.exportTemplate(template.name)}
        >
          <${Words} plain="⇪ export" wizard="📜 inscribe" /></button
        ><button
          class="tool"
          data-tact="delete"
          data-template=${template.name}
          onClick=${() => actions.removeTemplate(template.name)}
        >
          delete
        </button></span
      >
    </div>
    ${agents.length > 0 &&
    html`<div class="tc-agents">
      ${agents.map((agent, index) => {
        const inline = agent.profile_inline?.permission_overrides || {};
        const slugs = new Set([
          ...(agent.permissions || []),
          ...Object.keys(inline),
        ]);
        return html`<span key=${`${agent.name}:${index}`} class="tc-agent"
          >${effectiveTemplateOwner(agent, profiles) &&
          html`<span class="tc-owner" title="group owner">★</span>`}
          ${agent.name}${agent.role &&
          html` <span class="tc-role">${agent.role}</span>`}${slugs.size > 0 &&
          html` <span class="tc-role" title=${[...slugs].join(', ')}
            >+${slugs.size}🔑</span
          >`}</span
        >`;
      })}
    </div>`}
    ${forces.length > 0 &&
    html`<div
      class="tc-forces"
      title=${wizWord(
        'task forces deployed from this template',
        'hero parties summoned from this circle',
      )}
    >
      <span class="tc-forces-label"
        ><${Words} plain="🚀 forces:" wizard="⚔ parties:"
      /></span>
      ${forces.map(
        (group) =>
          html`<span
            key=${group.name}
            class="tc-force"
            data-force-group=${group.name}
            title=${group.mission || ''}
            >${group.name}${group.mission &&
            html` <span class="tc-force-mission"
              >— ${oneLine(group.mission)}</span
            >`}</span
          >`,
      )}
    </div>`}
  </div>`;
}

export function TemplateManager({ current, state, actions, confirmDiscard }) {
  const list = filterTemplates(current.templates, current.templateFilter);
  const filtered = !!current.templateFilter.trim();
  return h(
    Overlay,
    {
      id: 'templates-manage-modal',
      manage: true,
      labelledby: 'templates-manage-title',
      onClose: state.closeTemplateManager,
      confirmDiscard,
      resizeKey: 'tclaude.dash.modalSize.templates-manage',
      fitContent: false,
    },
    html`
      <h3 id="templates-manage-title">
        <${Words} plain="Group templates" wizard="🕯 Summoning circles" />
      </h3>
      <p class="manage-intro">
        <${Words}
          plain="Reusable blueprints for a working group — a name, shared context and an ordered list of agent specs. Instantiate one to spawn a whole team in a single shot."
          wizard="Reusable summoning circles for a whole party — a name, shared lore and an ordered roster of familiars. Cast one to summon the entire party in a single rite."
        />
      </p>
      <div class="filter-bar">
        <input
          id="filter-templates"
          type="text"
          value=${current.templateFilter}
          onInput=${(event) =>
            state.setTemplateFilter(event.currentTarget.value)}
          placeholder="Filter (template name / descr / agent name / role)"
          autocomplete="off"
          spellcheck="false"
          autofocus
        /><span class="filter-count" id="filter-templates-count"
          >${filtered
            ? `${list.length} / ${current.templates.length}`
            : current.templates.length}</span
        ><button
          class="clear-filter"
          id="filter-templates-clear"
          title="Clear filter"
          onClick=${() => state.setTemplateFilter('')}
        >
          ×</button
        ><span class="spacer"></span>
        <button
          id="scribe-templates-open"
          class="tool"
          title="Summon a chat agent (a scribe) that edits your templates for you — pre-briefed and already holding the templates.manage permission. Tell it what to change in plain words; its edits appear here live."
          onClick=${actions.editTemplatesWithAgent}
        >
          <${Words}
            plain="🤖 Edit with agent"
            wizard="📜 Dictate to a scribe"
          />
        </button>
        <button
          id="template-from-group-open"
          class="tool"
          title="Snapshot an existing group's structure — roles, owner, per-agent permissions, context — into a new reusable template"
          onClick=${actions.openTemplateFromGroup}
        >
          <${Words} plain="⤓ from a group" wizard="⤓ trace a party" />
        </button>
        <button
          id="template-import-open"
          class="tool"
          title="Import a template from a portable .task-force.json file (from ⇪ export) — share task forces between machines or with friends"
          onClick=${actions.openTemplateImport}
        >
          <${Words} plain="⤒ import" wizard="📜 read a scroll" />
        </button>
        <button
          id="starters-open"
          class="tool"
          title="Browse bundled starter task forces — copy a curated, ready-to-run team into your own templates list (it does not spawn a team; deploy or edit it afterwards)"
          onClick=${actions.openTemplateStarters}
        >
          <${Words} plain="⭐ starters" wizard="⭐ conjure a preset party" />
        </button>
        <button
          id="template-create-open"
          class="primary"
          title="Define a new group template"
          onClick=${() => actions.openTemplateEditor()}
        >
          <${Words} plain="+ new template" wizard="+ chalk a new circle" />
        </button>
      </div>
      <div id="templates-list">
        ${list.length
          ? list.map(
              (template) =>
                html`<${TemplateCard}
                  key=${template.name}
                  template=${template}
                  groups=${current.templateGroups}
                  profiles=${current.profiles}
                  actions=${actions}
                />`,
            )
          : html`<div class="template-empty">
              ${current.templates.length
                ? html`<${Words}
                    plain="No templates match the filter."
                    wizard="No circles match the filter."
                  />`
                : html`<${Words}
                    plain="No templates yet — use + new template, ⤓ from a group, or ⭐ starters above."
                    wizard="No summoning circles chalked yet — chalk one, trace a party, or conjure a preset party above."
                  />`}
            </div>`}
      </div>
      <div class="modal-buttons">
        <span class="spacer"></span
        ><button
          id="templates-manage-close"
          type="button"
          onClick=${state.closeTemplateManager}
        >
          Close
        </button>
      </div>
    `,
  );
}

export function TemplateDuplicateDialog({
  descriptor,
  state,
  actions,
  confirmDiscard,
}) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const [name, setName] = useState(`${descriptor.source.name}-copy`);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const submit = async () => {
    const next = name.trim();
    setError('');
    if (!next) {
      setError('name is required');
      return;
    }
    if (next === descriptor.source.name) {
      setError('pick a different name for the copy');
      return;
    }
    setBusy(true);
    try {
      await actions.duplicateTemplate(descriptor.source, next);
    } catch (err) {
      setError(message(err));
    } finally {
      setBusy(false);
    }
  };
  return h(
    Overlay,
    {
      id: 'template-duplicate-modal',
      labelledby: 'template-duplicate-title',
      onClose: state.closeDialog,
      onSubmitHotkey: busy ? null : submit,
      dirty: name !== `${descriptor.source.name}-copy`,
      blocked: busy,
      confirmDiscard,
      registerClose,
    },
    html`<h3 id="template-duplicate-title">
        <${Words} plain="Duplicate a template" wizard="🪞 Mirror the circle" />
      </h3>
      <div class="modal-meta">
        <span class="tpl-word-regular"
          >Makes a full copy of
          <b id="template-duplicate-source">${descriptor.source.name}</b> under
          a new name — every agent, wave, process phase, rhythm, work-pattern
          step, description and default context. Only the name changes.</span
        ><span class="tpl-word-wizard"
          >Chalks an identical twin of <b>the circle</b> under a new name —
          every familiar, rank, quest chapter, drumbeat, rite, lore and shared
          context. Only the name changes.</span
        >
      </div>
      <label class="cron-create-row"
        ><span class="cron-create-label">New name</span
        ><input
          id="template-duplicate-name"
          type="text"
          value=${name}
          onInput=${(event) => setName(event.currentTarget.value)}
          placeholder="kebab-or-snake-case label for the copy"
          autocomplete="off"
          spellcheck="false"
          autofocus
          data-select-on-focus
      /></label>
      <div class="cron-create-error" id="template-duplicate-error" role="alert">
        ${error}
      </div>
      <div class="modal-buttons">
        <button
          id="template-duplicate-cancel"
          disabled=${busy}
          onClick=${() => { void requestClose(); }}
        >
          Cancel</button
        ><span class="spacer"></span
        ><button
          id="template-duplicate-submit"
          class="primary"
          disabled=${busy}
          onClick=${submit}
        >
          ${busy ? 'Duplicating…' : 'Duplicate template'}
        </button>
      </div> `,
  );
}

export function TemplateImportDialog({ state, actions, confirmDiscard }) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const [raw, setRaw] = useState('');
  const [file, setFile] = useState(null);
  const [as, setAs] = useState('');
  const [update, setUpdate] = useState(false);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const submit = async () => {
    setError('');
    setBusy(true);
    try {
      const source = file ? (await file.text()).trim() : raw.trim();
      if (!source) throw new Error('pick a file or paste the task-force JSON');
      try {
        JSON.parse(source);
      } catch (err) {
        throw new Error(`not valid JSON: ${message(err)}`);
      }
      await actions.importTemplate(source, { as: as.trim(), update });
    } catch (err) {
      setError(message(err));
    } finally {
      setBusy(false);
    }
  };
  return h(
    Overlay,
    {
      id: 'template-import-modal',
      labelledby: 'template-import-title',
      onClose: state.closeDialog,
      dirty: !!(file || raw || as || update),
      blocked: busy,
      confirmDiscard,
      registerClose,
    },
    html`<h3 id="template-import-title">
        <${Words}
          plain="Import a task-force template"
          wizard="📜 Read a scroll into a circle"
        />
      </h3>
      <div class="modal-meta">
        <${Words}
          plain="Import a portable .task-force.json file produced by export — pick a file or paste its JSON. Missing references degrade with a warning rather than failing."
          wizard="Read an exported scroll into a summoning circle — pick a scroll or transcribe its runes. Unknown bindings fade with a warning rather than breaking the rite."
        />
      </div>
      <label class="cron-create-row"
        ><span class="cron-create-label">File</span
        ><input
          id="template-import-file"
          type="file"
          accept=".json,application/json"
          onChange=${(event) =>
            setFile(event.currentTarget.files?.[0] || null)} /></label
      ><label class="cron-create-row" style="align-items:flex-start"
        ><span class="cron-create-label">or paste</span
        ><textarea
          id="template-import-paste"
          rows="6"
          value=${raw}
          onInput=${(event) => setRaw(event.currentTarget.value)}
          placeholder="… or paste the task-force JSON here"
          autocomplete="off"
          spellcheck="false"
          style="flex:1;font-family:monospace;font-size:12px"
          autofocus
        /></label
      ><label class="cron-create-row"
        ><span class="cron-create-label">Import as</span
        ><input
          id="template-import-as"
          type="text"
          value=${as}
          onInput=${(event) => setAs(event.currentTarget.value)}
          placeholder="optional — import under a different name (required when the name is already taken, unless overwriting)"
          autocomplete="off"
          spellcheck="false" /></label
      ><label class="cron-create-row" style="justify-content:flex-start;gap:6px"
        ><input
          id="template-import-update"
          type="checkbox"
          checked=${update}
          onChange=${(event) => setUpdate(event.currentTarget.checked)}
          style="width:auto"
        /><span class="cron-create-label" style="flex:0 0 auto"
          >Overwrite if it already exists</span
        ></label
      >
      <div class="cron-create-error" id="template-import-error" role="alert">
        ${error}
      </div>
      <div class="modal-buttons">
        <button
          id="template-import-cancel"
          disabled=${busy}
          onClick=${() => { void requestClose(); }}
        >
          Cancel</button
        ><span class="spacer"></span
        ><button
          id="template-import-submit"
          class="primary"
          disabled=${busy}
          onClick=${submit}
        >
          ${busy ? 'Importing…' : 'Import'}
        </button>
      </div> `,
  );
}

export function TemplateFromGroupDialog({
  descriptor,
  current,
  state,
  actions,
  confirmDiscard,
}) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const [group, setGroup] = useState(
    descriptor.presetGroup || descriptor.groups[0] || '',
  );
  const [name, setName] = useState(descriptor.presetGroup || '');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const updating = current.templates.some(
    (template) => template.name === name.trim(),
  );
  const submit = async () => {
    setError('');
    if (!group) {
      setError('pick a group');
      return;
    }
    if (!name.trim()) {
      setError('template name is required');
      return;
    }
    setBusy(true);
    try {
      await actions.snapshotTemplateFromGroup(group, name.trim(), updating);
    } catch (err) {
      setError(message(err));
    } finally {
      setBusy(false);
    }
  };
  return h(
    Overlay,
    {
      id: 'template-from-group-modal',
      labelledby: 'template-from-group-title',
      onClose: state.closeDialog,
      onSubmitHotkey: busy ? null : submit,
      dirty:
        name !== (descriptor.presetGroup || '') ||
        group !== (descriptor.presetGroup || descriptor.groups[0] || ''),
      blocked: busy,
      confirmDiscard,
      registerClose,
    },
    html`<h3 id="template-from-group-title">
        <${Words}
          plain="Save a group as a template"
          wizard="🕯 Trace the party's circle"
        />
      </h3>
      <label class="cron-create-row"
        ><span class="cron-create-label"
          ><${Words} plain="Group" wizard="Party" /></span
        ><select
          id="template-from-group-group"
          value=${group}
          onChange=${(event) => setGroup(event.currentTarget.value)}
        >
          ${descriptor.groups.map(
            (value) =>
              html`<option key=${value} value=${value}>${value}</option>`,
          )}
        </select></label
      ><label class="cron-create-row"
        ><span class="cron-create-label">Template name</span
        ><input
          id="template-from-group-name"
          type="text"
          value=${name}
          onInput=${(event) => setName(event.currentTarget.value)}
          placeholder="kebab-or-snake-case label for the new template"
          autocomplete="off"
          spellcheck="false"
          autofocus
          data-select-on-focus=${!!descriptor.presetGroup}
      /></label>
      <div class="modal-meta">
        Captures the group's roles, owner, per-agent permission grants and
        shared context. Per-agent task briefs come through blank — fill them in
        the editor that opens next.
      </div>
      ${updating &&
      html`<div class="modal-meta" id="template-from-group-update-note">
        <${Words}
          plain=${`⚠ A template “${name.trim()}” already exists — saving re-snapshots it in place; curated per-agent task briefs are kept for matching agents.`}
          wizard=${`⚠ A circle “${name.trim()}” is already chalked — tracing redraws it in place; curated familiar briefs are kept for matching names.`}
        />
      </div>`}
      <div
        class="cron-create-error"
        id="template-from-group-error"
        role="alert"
      >
        ${error}
      </div>
      <div class="modal-buttons">
        <button
          id="template-from-group-cancel"
          disabled=${busy}
          onClick=${() => { void requestClose(); }}
        >
          Cancel</button
        ><span class="spacer"></span
        ><button
          id="template-from-group-submit"
          class="primary"
          disabled=${busy}
          onClick=${submit}
        >
          ${busy
            ? 'Saving…'
            : updating
              ? 'Update template'
              : 'Save as template'}
        </button>
      </div> `,
  );
}

export function TemplateStartersDialog({
  descriptor,
  state,
  actions,
  confirmDiscard,
}) {
  const [busy, setBusy] = useState('');
  const [error, setError] = useState(descriptor.request.error || '');
  const rows = descriptor.request.data || [];
  const visibleError = error || descriptor.request.error || '';
  const install = async (name) => {
    setBusy(name);
    setError('');
    try {
      await actions.installTemplateStarter(name);
    } catch (err) {
      setError(message(err));
    } finally {
      setBusy('');
    }
  };
  return h(
    Overlay,
    {
      id: 'starters-modal',
      labelledby: 'starters-title',
      onClose: state.closeDialog,
      blocked: !!busy,
      confirmDiscard,
    },
    html`<h3 id="starters-title">
        <${Words} plain="Starter task forces" wizard="⭐ Preset parties" />
      </h3>
      <div class="modal-meta">
        <${Words}
          plain="Copies a ready-made team into your own templates list — it does not spawn anything yet. Copying never overwrites a template of the same name."
          wizard="Copies a ready-made party into your own circles — it summons no familiars yet. Copying never overwrites a circle of the same name."
        />
      </div>
      <div id="starters-list" class="starters-list">
        ${descriptor.request.phase === 'loading'
          ? html`<div class="template-empty">
              <${Words} plain="Loading starters…" wizard="Conjuring presets…" />
            </div>`
          : rows.length
            ? rows.map(
                (starter) =>
                  html`<div
                    key=${starter.name}
                    class="starter-row"
                    data-starter=${starter.name}
                  >
                    <div class="starter-head">
                      <span class="tc-name">${starter.name}</span
                      >${starter.descr &&
                      html`<span class="tc-descr">${starter.descr}</span>`}
                    </div>
                    <div class="starter-meta">
                      <span class="tc-count">${starter.agents || 0} agents</span
                      >${starter.waves > 1 &&
                      html`<span class="tc-count"
                        >🌊 ${starter.waves} waves</span
                      >`}${starter.process > 0 &&
                      html`<span class="tc-count"
                        >◆ ${starter.process}-phase</span
                      >`}${starter.rhythms > 0 &&
                      html`<span class="tc-count"
                        >🥁 ${starter.rhythms}</span
                      >`}${starter.work_pattern > 0 &&
                      html`<span class="tc-count"
                        >⇶ ${starter.work_pattern}</span
                      >`}
                    </div>
                    <div class="starter-actions">
                      <button
                        class="tool"
                        data-sact="install"
                        data-starter=${starter.name}
                        disabled=${!!busy}
                        onClick=${() => install(starter.name)}
                      >
                        <${Words}
                          plain=${busy === starter.name
                            ? 'Copying…'
                            : '⤓ copy to my templates'}
                          wizard=${busy === starter.name
                            ? 'Copying…'
                            : '⭐ copy into my circles'}
                        />
                      </button>
                    </div>
                  </div>`,
              )
            : descriptor.request.phase === 'ready' &&
              html`<div class="template-empty">
                <${Words}
                  plain="No starters bundled."
                  wizard="No presets bound."
                />
              </div>`}
      </div>
      <div class="cron-create-error" id="starters-error" role="alert">
        ${visibleError}
      </div>
      <div class="modal-buttons">
        <span class="spacer"></span
        ><button
          id="starters-close"
          disabled=${!!busy}
          onClick=${state.closeDialog}
        >
          Close
        </button>
      </div>`,
  );
}

function GroupImportPreview({ inspection, into }) {
  if (!inspection) return null;
  const collisions = inspection.conv_collisions || [];
  const rows = [
    ['Source group', inspection.source_group || '(unnamed)'],
    ['Agents', String(inspection.agent_count)],
    ['Messages', String(inspection.message_count)],
    [
      'Conversations',
      `${inspection.conv_count} conversation${inspection.conv_count === 1 ? '' : 's'}${inspection.missing_convs > 0 ? ` (${inspection.missing_convs} with no .jsonl content)` : ''}`,
    ],
    ...(inspection.source_os || inspection.source_home
      ? [
          [
            'Source machine',
            `${inspection.source_os || '?'}${inspection.source_home ? `, home ${inspection.source_home}` : ''}`,
          ],
        ]
      : []),
    ...(inspection.exported_at ? [['Exported', inspection.exported_at]] : []),
    ['Format version', `v${inspection.format_version} — supported`],
  ];
  return html`<div class="group-import-preview" id="group-import-preview">
    <div class="gi-head">Archive contents</div>
    ${rows.map(
      ([key, value], index) =>
        html`<div key=${key} class="gi-row">
          <span class="gi-k">${key}</span
          ><span class=${`gi-v${index === rows.length - 1 ? ' gi-ok' : ''}`}
            >${value}</span
          >
        </div>`,
    )}
    <div class="gi-sep gi-head">Collisions on this machine</div>
    ${collisions.length
      ? html`<div class="gi-warn">
            ⚠ ${collisions.length}
            conversation${collisions.length === 1 ? '' : 's'} already exist
            locally — each is imported as a fresh copy, its agent retitled
            “-i-N”:
          </div>
          <ul class="gi-collide-list">
            ${collisions.map(
              (collision) =>
                html`<li key=${collision.conv_id}>
                  ${collision.title || collision.conv_id}
                  <span class="gi-k"
                    >(${(collision.conv_id || '').slice(0, 8)})</span
                  >
                </li>`,
            )}
          </ul>`
      : html`<div class="gi-ok">
          ✓ No conv-id collisions — every conversation id is preserved.
        </div>`}
    <div class="gi-sep"></div>
    ${!inspection.target_name_valid
      ? html`<div class="gi-verdict gi-bad">
          ✗ Invalid group name “${inspection.target_name}”:
          ${inspection.target_name_error || ''}
        </div>`
      : inspection.group_name_taken
        ? html`<div class="gi-verdict gi-bad">
            ✗ A group named “${inspection.target_name}” already exists here.
            Fill “Import as” with a free name.
          </div>`
        : !into.trim()
          ? html`<div class="gi-verdict gi-warn">
              ⚠ Fill “Into dir” with a target directory to enable the import.
            </div>`
          : html`<div class="gi-verdict gi-ok">
              ✓ Ready — ${inspection.agent_count}
              agent${inspection.agent_count === 1 ? '' : 's'} will be imported
              into group “${inspection.target_name}”.
            </div>`}
  </div>`;
}

export function GroupImportDialog({ state, actions, confirmDiscard }) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const [file, setFile] = useState(null);
  const [into, setInto] = useState('');
  const [as, setAs] = useState('');
  const [inspection, setInspection] = useState(null);
  const [phase, setPhase] = useState('idle');
  const [error, setError] = useState('');
  const request = useRef(0);
  useEffect(() => {
    const token = ++request.current;
    setInspection(null);
    if (!file) {
      setPhase('idle');
      return undefined;
    }
    setPhase('loading');
    const timer = setTimeout(
      async () => {
        try {
          const result = await actions.inspectGroupImport(file, as.trim());
          if (token === request.current) {
            setInspection(result);
            setPhase('ready');
            setError('');
          }
        } catch (err) {
          if (token === request.current) {
            setPhase('error');
            setError(message(err));
          }
        }
      },
      as ? 350 : 0,
    );
    return () => {
      clearTimeout(timer);
    };
  }, [file, as]);
  const ready =
    inspection?.target_name_valid &&
    !inspection?.group_name_taken &&
    !!into.trim();
  const submit = async () => {
    setError('');
    setPhase('importing');
    try {
      await actions.importGroup(file, into.trim(), as.trim());
    } catch (err) {
      setError(
        `Import failed: ${message(err)} — nothing was written. The import is all-or-nothing, so the group, its agents and conversations are exactly as before.`,
      );
      setPhase('ready');
    }
  };
  return h(
    Overlay,
    {
      id: 'group-import-modal',
      labelledby: 'group-import-title',
      onClose: state.closeDialog,
      onSubmitHotkey: ready && phase !== 'importing' ? submit : null,
      dirty: !!(file || into || as),
      blocked: phase === 'importing',
      confirmDiscard,
      registerClose,
    },
    html`<h3 id="group-import-title">
        <${Words}
          plain="Import a group from a .zip archive"
          wizard="⤒ Unseal a party archive"
        />
      </h3>
      <div class="modal-meta">
        Recreates a group exported with ⤓ export — its members, roles, owners,
        permissions, cron jobs, message history and every conversation. Imported
        agents land offline. The import is transactional: if anything fails,
        nothing is written.
      </div>
      <label class="cron-create-row"
        ><span class="cron-create-label">Archive</span
        ><input
          id="group-import-file"
          type="file"
          accept=".zip,application/zip"
          onChange=${(event) => setFile(event.currentTarget.files?.[0] || null)}
          autofocus /></label
      ><label class="cron-create-row"
        ><span class="cron-create-label">Into dir</span
        ><input
          id="group-import-into"
          type="text"
          value=${into}
          onInput=${(event) => setInto(event.currentTarget.value)}
          placeholder="required — absolute working directory (~ OK) for the imported agents (the browser can't browse the server; type the path)"
          autocomplete="off"
          spellcheck="false" /></label
      ><label class="cron-create-row"
        ><span class="cron-create-label">Import as</span
        ><input
          id="group-import-as"
          type="text"
          value=${as}
          onInput=${(event) => setAs(event.currentTarget.value)}
          placeholder="optional — import under a different group name (required when the exported name is already taken)"
          autocomplete="off"
          spellcheck="false" /></label
      >${phase === 'loading' &&
      html`<div class="group-import-preview" id="group-import-preview">
        <div class="gi-head">Inspecting archive…</div>
      </div>`}${phase === 'error' &&
      html`<div class="group-import-preview" id="group-import-preview">
        <div class="gi-head">Archive</div>
        <div class="gi-verdict gi-bad">✗ ${error}</div>
        <div class="gi-bad">
          This file is not an importable group archive — pick a .zip produced by
          the ⤓ export button.
        </div>
      </div>`}${inspection &&
      html`<${GroupImportPreview} inspection=${inspection} into=${into} />`}
      <div class="cron-create-error" id="group-import-error" role="alert">
        ${phase === 'error' ? '' : error}
      </div>
      <div class="modal-buttons">
        <button
          id="group-import-cancel"
          disabled=${phase === 'importing'}
          onClick=${() => { void requestClose(); }}
        >
          Cancel</button
        ><span class="spacer"></span
        ><button
          id="group-import-submit"
          class="primary"
          disabled=${!ready || phase === 'importing'}
          onClick=${submit}
        >
          ${phase === 'importing' ? 'Importing…' : 'Import'}
        </button>
      </div>`,
  );
}

export function GroupContextDialog({
  descriptor,
  state,
  actions,
  confirmDiscard,
}) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const [context, setContext] = useState(descriptor.context);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const submit = async () => {
    setBusy(true);
    setError('');
    try {
      await actions.saveGroupContext(descriptor.group, context);
    } catch (err) {
      setError(message(err));
    } finally {
      setBusy(false);
    }
  };
  return h(
    Overlay,
    {
      id: 'group-context-modal',
      labelledby: 'group-context-title',
      onClose: state.closeDialog,
      onSubmitHotkey: busy ? null : submit,
      dirty: context !== descriptor.context,
      blocked: busy,
      confirmDiscard,
      registerClose,
    },
    html`<h3 id="group-context-title">
        <${Words}
          plain="Group startup context"
          wizard="📜 The party's standing orders"
        />
      </h3>
      <div class="modal-meta" id="group-context-meta">
        group: ${descriptor.group}
      </div>
      <p class="modal-hint">
        Shared guidance auto-injected into every agent spawned into this group,
        right after the spawn welcome. Multi-line is fine. Leave empty to clear
        it.
      </p>
      <label class="cron-create-row"
        ><span class="cron-create-label">Context</span
        ><textarea
          id="group-context-text"
          class="modal-context-textarea"
          rows="10"
          value=${context}
          onInput=${(event) => setContext(event.currentTarget.value)}
          placeholder="multi-line guidance for agents spawned into this group"
          spellcheck="false"
          autofocus
        />
      </label>
      <div class="cron-create-error" id="group-context-error" role="alert">
        ${error}
      </div>
      <div class="modal-buttons">
        <button
          id="group-context-cancel"
          disabled=${busy}
          onClick=${() => { void requestClose(); }}
        >
          Cancel</button
        ><span class="spacer"></span
        ><button
          id="group-context-submit"
          class="primary"
          disabled=${busy}
          onClick=${submit}
        >
          ${busy ? 'Saving…' : 'Save'}
        </button>
      </div> `,
  );
}

function GroupClonePreview({ source, withAgents, copyOwners }) {
  if (!source)
    return html`<div class="gcp-title">
      Preview unavailable (group not in current snapshot)
    </div>`;
  const members = (source.members || []).filter(
    (member) => member && !(member.role === 'owner' && !member.descr),
  );
  const owners = (source.members || []).filter((member) => member?.owner);
  const row = (key, value, muted = false) =>
    html`<div class="gcp-row">
      <span class="gcp-key">${key}</span
      ><span class=${`gcp-val${muted ? ' muted' : ''}`}>${value}</span>
    </div>`;
  return html`<div class="gcp-title">Clone will carry</div>
    ${row(
      '📁 directory',
      source.default_cwd || 'none',
      !source.default_cwd,
    )}${row('📝 description', source.descr || 'none', !source.descr)}${row(
      '📋 startup context',
      source.default_context
        ? `${source.default_context.length} chars`
        : 'none',
      !source.default_context,
    )}${row(
      '🧠 profile',
      source.default_profile || 'none',
      !source.default_profile,
    )}${row(
      '🔑 group permissions',
      source.permissions?.length ? String(source.permissions.length) : 'none',
      !source.permissions?.length,
    )}${row(
      '👥 max members',
      source.max_members ? String(source.max_members) : 'unlimited',
      !source.max_members,
    )}${row('🔔 notifications', source.notify_enabled ? 'on' : 'off')}${row(
      '👤 owners',
      owners.length
        ? `${owners.length} — ${copyOwners ? 'copied' : 'skipped'}`
        : 'none',
      !owners.length || !copyOwners,
    )}${row(
      '🤖 member agents',
      members.length
        ? `${members.length} (${members.filter((member) => member.online).length} online) — ${withAgents ? 'cloned with history' : 'skipped'}`
        : 'none',
      !members.length || !withAgents,
    )}`;
}

export function GroupCloneDialog({
  descriptor,
  state,
  actions,
  confirmDiscard,
}) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const [name, setName] = useState(descriptor.defaultName);
  const [withAgents, setWithAgents] = useState(false);
  const [copyOwners, setCopyOwners] = useState(false);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const submit = async () => {
    setBusy(true);
    setError('');
    try {
      await actions.cloneGroup(
        descriptor.group,
        descriptor.defaultName,
        name.trim(),
        withAgents,
        copyOwners,
      );
    } catch (err) {
      setError(message(err));
    } finally {
      setBusy(false);
    }
  };
  return h(
    Overlay,
    {
      id: 'group-clone-modal',
      labelledby: 'group-clone-title',
      onClose: state.closeDialog,
      onSubmitHotkey: busy ? null : submit,
      dirty: name !== descriptor.defaultName || withAgents || copyOwners,
      blocked: busy,
      confirmDiscard,
      registerClose,
    },
    html`<h3 id="group-clone-title">
        <${Words} plain="Clone group" wizard="⧉ Mirror the party" />
      </h3>
      <div class="modal-meta" id="group-clone-meta">
        source: ${descriptor.group}
      </div>
      <p class="modal-hint">
        Creates a new group from the source. The source group is left untouched.
        Review what it carries below, set a name and choose what else comes
        along.
      </p>
      <label class="cron-create-row"
        ><span class="cron-create-label">New name</span
        ><input
          id="group-clone-name"
          type="text"
          value=${name}
          onInput=${(event) => setName(event.currentTarget.value)}
          placeholder="leave blank for <source>-c-N"
          autocomplete="off"
          spellcheck="false"
          autofocus
          data-select-on-focus /></label
      ><label class="cron-create-enabled"
        ><input
          id="group-clone-with-agents"
          type="checkbox"
          checked=${withAgents}
          onChange=${(event) => setWithAgents(event.currentTarget.checked)}
        />
        Clone member agents too</label
      ><label class="cron-create-enabled"
        ><input
          id="group-clone-copy-owners"
          type="checkbox"
          checked=${copyOwners}
          onChange=${(event) => setCopyOwners(event.currentTarget.checked)}
        />
        Copy source owners too</label
      >
      <p class="modal-hint modal-hint-small">
        Defaults to settings only. Agents and owner grants are copied only when
        selected.
      </p>
      <div class="group-clone-preview" id="group-clone-preview">
        <${GroupClonePreview}
          source=${descriptor.source}
          withAgents=${withAgents}
          copyOwners=${copyOwners}
        />
      </div>
      <div class="cron-create-error" id="group-clone-error" role="alert">
        ${error}
      </div>
      <div class="modal-buttons">
        <button
          id="group-clone-cancel"
          disabled=${busy}
          onClick=${() => { void requestClose(); }}
        >
          Cancel</button
        ><span class="spacer"></span
        ><button
          id="group-clone-submit"
          class="primary"
          disabled=${busy}
          onClick=${submit}
        >
          ${busy ? 'Cloning…' : 'Clone group'}
        </button>
      </div> `,
  );
}

const WT_NEW = '__new__';
function deploySlug(value) {
  const slug = String(value || '')
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '');
  return slug.length > 40 ? slug.slice(0, 40).replace(/-+$/g, '') : slug;
}
function bareURL(value) {
  const text = String(value || '')
    .trim()
    .toLowerCase();
  return (
    !!text &&
    !/\s/.test(text) &&
    (text.startsWith('http://') ||
      text.startsWith('https://') ||
      text.startsWith('linear.app/') ||
      text.startsWith('www.'))
  );
}
function combineContext(group, template) {
  const left = String(group || '').trim();
  const right = String(template || '').trim();
  return left && right
    ? `## Mirrored group context\n\n${left}\n\n## Template context\n\n${right}`
    : left || right;
}
function suggestedGroup(name, groups) {
  const used = new Set(groups.map((group) => group.name));
  let suffix = 2;
  while (used.has(`${name}-${suffix}`)) suffix += 1;
  return `${name}-${suffix}`;
}

function DeployRosterPreview({ template, prefix, profiles, defaultProfile }) {
  if (!template?.agents?.length)
    return html`<span class="tp-empty"
      ><${Words}
        plain="this template has no agents"
        wizard="this circle names no familiars"
    /></span>`;
  const shown = prefix.trim() || '‹group›';
  const fallback = findProfileByHandle(profiles, defaultProfile);
  return html`${template.agents.map((agent, index) => {
    const adopts =
      fallback && !agent.spawn_profile && agentInheritsDeployDefault(agent);
    const profile = agent.profile_inline
      ? 'custom'
      : agent.spawn_profile || (adopts ? `${defaultProfile} (default)` : '');
    const owner =
      effectiveTemplateOwner(agent, profiles) || (adopts && fallback.is_owner);
    let permissions =
      (agent.permissions || []).length +
      Object.keys(agent.profile_inline?.permission_overrides || {}).length +
      (adopts ? Object.keys(fallback.permission_overrides || {}).length : 0);
    const meta = [
      agent.role,
      profile && `⚙ ${profile}`,
      permissions && `+${permissions}🔑`,
      owner && '★ owner',
    ]
      .filter(Boolean)
      .join(' · ');
    return html`<div key=${`${agent.name}:${index}`} class="tp-row">
      <span class="tp-name">${shown}-${agent.name}</span>${meta &&
      html` <span class="tp-meta">${meta}</span>`}
    </div>`;
  })}`;
}

export function TemplateDeployDialog({
  descriptor,
  current,
  state,
  actions,
  confirmDiscard,
}) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const groups = current.templateGroups;
  const initialTemplate =
    current.templates.find((item) => item.name === descriptor.presetName) ||
    current.templates[0];
  const dropGroup =
    groups.find((group) => group.name === descriptor.dropGroup) || null;
  const availableProfile = (name) => findProfileByHandle(current.profiles, name) ? name : '';
  const seededProfile = availableProfile(
    dropGroup?.default_profile || getDashDefaultProfile() || '',
  );
  const [templateName, setTemplateName] = useState(initialTemplate?.name || '');
  const [mission, setMission] = useState('');
  const [groupName, setGroupName] = useState(
    dropGroup
      ? suggestedGroup(dropGroup.name, groups)
      : deploySlug(initialTemplate?.name || ''),
  );
  const [groupEdited, setGroupEdited] = useState(false);
  const [mode, setMode] = useState(dropGroup ? 'subgroup' : '');
  const [source, setSource] = useState('');
  const [parent, setParent] = useState(false);
  const [descr, setDescr] = useState(dropGroup?.descr || '');
  const [context, setContext] = useState(
    combineContext(
      dropGroup?.default_context,
      initialTemplate?.default_context,
    ),
  );
  const [cwd, setCwd] = useState(dropGroup?.default_cwd || '');
  const [repo, setRepo] = useState(dropGroup?.default_cwd || '');
  const [repoEdited, setRepoEdited] = useState(false);
  const [worktree, setWorktreeValue] = useState('');
  const [branch, setBranch] = useState(groupName);
  const [base, setBaseValue] = useState('');
  const baseEdited = useRef(false);
  const baseRepo = useRef(repo.trim());
  const [syncBranch, setSyncBranchValue] = useState(true);
  const [worktreeEdited, setWorktreeEdited] = useState(false);
  const [perAgent, setPerAgent] = useState(
    !!initialTemplate?.per_agent_worktrees,
  );
  const [defaultProfile, setDefaultProfile] = useState(seededProfile);
  const [profileEdited, setProfileEdited] = useState(false);
  const [wtData, setWtData] = useState(null);
  const [wtPhase, setWtPhase] = useState('idle');
  const wtRequest = useRef(0);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const setWorktree = (value) => {
    setWorktreeValue(value);
    setWorktreeEdited(true);
  };
  const setBase = (value) => {
    setBaseValue(value);
    baseEdited.current = true;
    setWorktreeEdited(true);
  };
  const setSyncBranch = (value) => {
    setSyncBranchValue(value);
    setWorktreeEdited(true);
  };
  const template =
    current.templates.find((item) => item.name === templateName) ||
    initialTemplate;
  const reinforcing = !!dropGroup && mode === 'reinforce';
  const copying = !!dropGroup && mode !== 'reinforce';
  const mirror =
    !dropGroup && source ? groups.find((group) => group.name === source) : null;
  const worktreesHidden = reinforcing;
  const initialName = dropGroup
    ? suggestedGroup(dropGroup.name, groups)
    : deploySlug(initialTemplate?.name || '');
  const initialContext = combineContext(
    dropGroup?.default_context,
    initialTemplate?.default_context,
  );
  const initialProfile = seededProfile;
  const dirty =
    templateName !== initialTemplate?.name ||
    !!mission ||
    groupEdited ||
    groupName !== initialName ||
    mode !== (dropGroup ? 'subgroup' : '') ||
    !!source ||
    parent ||
    descr !== (dropGroup?.descr || '') ||
    context !== initialContext ||
    cwd !== (dropGroup?.default_cwd || '') ||
    repo !== (dropGroup?.default_cwd || '') ||
    worktreeEdited ||
    branch !== initialName ||
    perAgent !== !!initialTemplate?.per_agent_worktrees ||
    defaultProfile !== initialProfile;
  useEffect(() => {
    const normalizedRepo = repo.trim();
    if (baseRepo.current !== normalizedRepo) {
      baseRepo.current = normalizedRepo;
      baseEdited.current = false;
      setBaseValue('');
      setWorktreeValue('');
      setWtData(null);
    }
    const token = ++wtRequest.current;
    if (!normalizedRepo) {
      setWtData(null);
      setWtPhase('idle');
      return undefined;
    }
    if (worktreesHidden) {
      // Reinforce does not use worktrees, but mode toggles must not discard a
      // picker selection that belongs to the still-open create/copy draft.
      setWtPhase('idle');
      return undefined;
    }
    setWtPhase('loading');
    const timer = setTimeout(async () => {
      try {
        const data = await actions.loadDeployWorktrees(normalizedRepo);
        if (token === wtRequest.current) {
          setWtData(data || {});
          setWtPhase('ready');
          if (!baseEdited.current) setBaseValue(data.default_branch || '');
        }
      } catch (_) {
        if (token === wtRequest.current) {
          setWtData({});
          setWtPhase('error');
        }
      }
    }, 350);
    return () => clearTimeout(timer);
  }, [repo, worktreesHidden]);
  useEffect(() => {
    if (perAgent && wtData?.is_repo) setWorktreeValue(WT_NEW);
  }, [perAgent, wtData]);
  useEffect(() => {
    if (syncBranch && groupName) {
      setBranch(groupName);
      if (wtData?.is_repo) setWorktreeValue(WT_NEW);
    }
  }, [groupName, syncBranch, wtData?.is_repo]);
  const chooseTemplate = (name) => {
    const next = current.templates.find((item) => item.name === name);
    setTemplateName(name);
    setPerAgent(!!next?.per_agent_worktrees);
    if (!dropGroup && !groupEdited)
      setGroupName(
        deploySlug(bareURL(mission) ? name : mission) || deploySlug(name),
      );
    const mirrored = dropGroup || groups.find((group) => group.name === source);
    if (mirrored) {
      setDescr(mirrored.descr || '');
      setContext(
        combineContext(mirrored.default_context, next?.default_context),
      );
      setCwd(mirrored.default_cwd || '');
      if (!repoEdited) setRepo(mirrored.default_cwd || '');
      if (!profileEdited)
        setDefaultProfile(
          availableProfile(
            mirrored.default_profile || getDashDefaultProfile() || '',
          ),
        );
    }
  };
  const changeMission = (value) => {
    setMission(value);
    if (!dropGroup && !groupEdited)
      setGroupName(
        deploySlug(bareURL(value) ? templateName : value) ||
          deploySlug(templateName),
      );
  };
  const chooseSource = (name) => {
    setSource(name);
    setParent(false);
    const group = groups.find((item) => item.name === name);
    if (!group) return;
    setDescr(group.descr || '');
    setContext(
      combineContext(group.default_context, template?.default_context),
    );
    setCwd(group.default_cwd || '');
    if (!repoEdited) setRepo(group.default_cwd || '');
    if (!profileEdited)
      setDefaultProfile(
        availableProfile(
          group.default_profile || getDashDefaultProfile() || '',
        ),
      );
  };
  const browse = async (kind) => {
    const value = kind === 'cwd' ? cwd : repo;
    const result = await pickDirectory({
      startDir: value,
      title:
        kind === 'cwd'
          ? 'Select the working directory for the task force'
          : 'Select the git repo to worktree',
    });
    if (result.path) {
      if (kind === 'cwd') {
        setCwd(result.path);
        if (!repoEdited) setRepo(result.path);
      } else {
        setRepo(result.path);
        setRepoEdited(true);
      }
    } else if (result.error) setError(result.error);
  };
  const agentProfiles = () => {
    if (!defaultProfile) return {};
    const result = {};
    for (const agent of template?.agents || [])
      if (
        agent.name &&
        !agent.spawn_profile &&
        agentInheritsDeployDefault(agent)
      )
        result[agent.name] = defaultProfile;
    return result;
  };
  const applyWorktree = async (payload) => {
    const cleanCwd = cwd.trim();
    const cleanRepo = repo.trim();
    if (perAgent) {
      if (!cleanRepo)
        throw new Error('per-agent worktrees need a worktree repo');
      if (!branch.trim())
        throw new Error('enter a branch prefix for per-agent worktrees');
      payload.per_agent_worktrees = {
        repo: cleanRepo,
        branch_prefix: branch.trim(),
        from_branch: base || '',
        worktree_as_cwd: !cleanCwd || cleanRepo === cleanCwd,
      };
      if (cleanCwd) payload.cwd = cleanCwd;
      return;
    }
    let selection = { path: '', branch: '' };
    if (worktree.startsWith('wt:')) {
      const row = wtData?.worktrees?.find(
        (item) => item.path === worktree.slice(3),
      );
      selection = { path: worktree.slice(3), branch: row?.branch || '' };
    } else if (worktree === WT_NEW) {
      if (!branch.trim())
        throw new Error('enter a branch name for the new worktree');
      selection = await actions.createDeployWorktree({
        repo: wtData?.repo_root || cleanRepo,
        branch: branch.trim(),
        from_branch: base || '',
      });
    }
    if (selection.path && cleanRepo && cleanRepo !== cleanCwd) {
      payload.cwd = cleanCwd;
      payload.worktree_path = selection.path;
      payload.worktree_branch = selection.branch || branch.trim();
    } else if (selection.path) payload.cwd = selection.path;
    else if (cleanCwd) payload.cwd = cleanCwd;
  };
  const submit = async () => {
    setError('');
    if (!templateName) {
      setError('pick a template');
      return;
    }
    if (reinforcing && !dropGroup) {
      setError('no target group to reinforce');
      return;
    }
    if (!reinforcing && !groupName.trim() && !mission.trim()) {
      setError('enter a mission, or a group name to summon the team under');
      return;
    }
    setBusy(true);
    try {
      const profiles = agentProfiles();
      if (reinforcing) {
        const payload = { group_name: dropGroup.name };
        if (mission.trim()) payload.task = mission.trim();
        if (cwd.trim()) payload.cwd = cwd.trim();
        if (Object.keys(profiles).length) payload.agent_profiles = profiles;
        await actions.deployTemplate(templateName, 'reinforce', payload, mode);
      } else if (copying) {
        if (!groupName.trim())
          throw new Error('enter a name for the new group');
        const payload = {
          group_name: groupName.trim(),
          context_override: context,
          descr_override: descr.trim(),
        };
        if (mission.trim()) payload.task = mission.trim();
        if (mode === 'subgroup') payload.parent = dropGroup.name;
        await applyWorktree(payload);
        if (Object.keys(profiles).length) payload.agent_profiles = profiles;
        await actions.deployTemplate(
          templateName,
          'instantiate',
          payload,
          mode,
        );
      } else {
        const payload = { mission: mission.trim() };
        if (groupName.trim()) payload.group_name = groupName.trim();
        if (source) {
          payload.descr_override = descr.trim();
          payload.context_override = context;
          if (parent) payload.parent = source;
        }
        await applyWorktree(payload);
        if (Object.keys(profiles).length) payload.agent_profiles = profiles;
        await actions.deployTemplate(templateName, 'deploy', payload);
      }
    } catch (err) {
      setError(message(err));
    } finally {
      setBusy(false);
    }
  };
  const options = wtData?.is_repo
    ? [
        { value: '', label: '(no worktree — use Default cwd above)' },
        ...(wtData.worktrees || []).map((item) => ({
          value: `wt:${item.path}`,
          label: `${item.branch || '(detached)'}${item.is_main ? ' [main]' : ''} — ${item.path}`,
          branch: item.branch || '',
        })),
        { value: WT_NEW, label: '+ create new worktree…' },
      ]
    : [
        {
          value: '',
          label:
            wtPhase === 'loading'
              ? 'loading…'
              : repo.trim()
                ? wtData?.sub_repos?.length
                  ? '(not a git repo — pick a sub-repo above)'
                  : '(not a git repo — worktrees unavailable)'
                : '(enter a CWD to enable worktrees)',
        },
      ];
  const showNew = !worktreesHidden && wtData?.is_repo && worktree === WT_NEW;
  const worktreeLoading =
    !reinforcing && wtPhase === 'loading' && !!(worktree || perAgent);
  return h(
    Overlay,
    {
      id: 'template-deploy-modal',
      labelledby: 'template-deploy-title',
      onClose: state.closeDialog,
      onSubmitHotkey: busy || worktreeLoading ? null : submit,
      dirty,
      blocked: busy || worktreeLoading,
      confirmDiscard,
      registerClose,
    },
    html`<h3 id="template-deploy-title">
        <${Words} plain="Deploy a task force" wizard="🧙 Summon a hero party" />
      </h3>
      ${dropGroup &&
      html`<div
        class="deploy-mode"
        id="template-deploy-mode"
        role="radiogroup"
        aria-label="Drop mode"
      >
        ${[
          [
            'subgroup',
            'New subgroup copying this group’s settings',
            'New sub-party in this party’s image',
          ],
          ['reinforce', 'Reinforce this group', 'Summon into this party'],
          [
            'copy',
            'New top-level group copying settings',
            'New top-level party in this party’s image',
          ],
        ].map(
          ([value, plain, wizard]) =>
            html`<label key=${value}
              ><input
                type="radio"
                name="template-deploy-mode"
                value=${value}
                checked=${mode === value}
                onChange=${() => setMode(value)} /><span
                ><${Words} plain=${plain} wizard=${wizard} /></span
            ></label>`,
        )}
      </div>`}<label class="cron-create-row"
        ><span class="cron-create-label"
          ><${Words} plain="Template" wizard="Circle" /></span
        ><select
          id="template-deploy-template"
          value=${templateName}
          onChange=${(event) => chooseTemplate(event.currentTarget.value)}
        >
          ${current.templates.map(
            (item) =>
              html`<option key=${item.name} value=${item.name}>
                ${item.name}
              </option>`,
          )}
        </select></label
      ><label class="cron-create-row"
        ><span class="cron-create-label"
          ><${Words} plain="Mission" wizard="Quest" /></span
        ><textarea
          id="template-deploy-mission"
          class="modal-context-textarea"
          rows="4"
          value=${mission}
          onInput=${(event) => changeMission(event.currentTarget.value)}
          placeholder="optional — the mission / task to summon against; folded into group context under Mission"
          spellcheck="false"
          autofocus
        /></label
      ><label class="cron-create-row"
        ><span class="cron-create-label"
          ><${Words} plain="Group name" wizard="Party name" /></span
        ><input
          id="template-deploy-group"
          type="text"
          value=${reinforcing ? dropGroup.name : groupName}
          readonly=${reinforcing}
          class=${reinforcing ? 'locked' : ''}
          onInput=${(event) => {
            setGroupName(event.currentTarget.value);
            setGroupEdited(true);
          }}
          placeholder="derived from the mission if blank — required when mission is empty"
          autocomplete="off"
          spellcheck="false" /></label
      >${dropGroup &&
      html`<div class="deploy-group-note" id="template-deploy-group-note">
        ${reinforcing
          ? `Deploying into existing group “${dropGroup.name}”; its settings stay unchanged.`
          : `${mode === 'subgroup' ? 'Creating a subgroup under' : 'Creating a top-level group from'} “${dropGroup.name}”.`}
      </div>`}<label
        class="cron-create-row"
        id="template-deploy-source-row"
        hidden=${!!dropGroup}
        ><span class="cron-create-label">Mirror settings</span
        ><select
          id="template-deploy-source"
          value=${source}
          onChange=${(event) => chooseSource(event.currentTarget.value)}
        >
          <option value="">template defaults (top-level)</option>
          ${groups.map(
            (group) =>
              html`<option key=${group.name} value=${group.name}>
                ${group.name}
              </option>`,
          )}
        </select></label
      ><label
        class="cron-create-enabled"
        id="template-deploy-parent-row"
        hidden=${!!dropGroup || !source}
        ><input
          id="template-deploy-parent"
          type="checkbox"
          checked=${parent}
          onChange=${(event) => setParent(event.currentTarget.checked)}
        /><span>Deploy as subgroup under the mirrored group</span></label
      ><label
        class="cron-create-row"
        id="template-deploy-descr-row"
        hidden=${!copying && !mirror}
        ><span class="cron-create-label"
          ><${Words} plain="Description" wizard="Lore" /></span
        ><input
          id="template-deploy-descr"
          type="text"
          value=${descr}
          onInput=${(event) => setDescr(event.currentTarget.value)}
          placeholder="the new group's description"
          autocomplete="off"
          spellcheck="false" /></label
      ><label class="cron-create-row"
        ><span class="cron-create-label">Default cwd</span
        ><input
          id="template-deploy-cwd"
          type="text"
          value=${cwd}
          onInput=${(event) => {
            setCwd(event.currentTarget.value);
            if (!repoEdited) setRepo(event.currentTarget.value);
          }}
          placeholder="optional — absolute path (~ OK) the force spawns in"
          autocomplete="off"
          spellcheck="false"
        /><button
          id="template-deploy-cwd-browse"
          type="button"
          class="dir-browse-btn"
          title="Open a native directory picker on the daemon's desktop"
          onClick=${() => browse('cwd')}
        >
          Browse…
        </button></label
      ><label class="cron-create-row" hidden=${worktreesHidden}
        ><span class="cron-create-label">Worktree repo</span
        ><input
          id="template-deploy-wt-repo"
          type="text"
          list="template-deploy-subrepo-list"
          value=${repo}
          onInput=${(event) => {
            setRepo(event.currentTarget.value);
            setRepoEdited(true);
          }}
          placeholder="git repo to worktree — defaults to CWD"
          autocomplete="off"
          spellcheck="false"
        /><datalist id="template-deploy-subrepo-list">
          ${(wtData?.sub_repos || []).map(
            (item) =>
              html`<option key=${item.path} value=${item.path}>
                ${item.rel}
              </option>`,
          )}</datalist
        ><button
          id="template-deploy-wt-repo-browse"
          type="button"
          class="dir-browse-btn"
          title="Open a native directory picker on the daemon's desktop"
          onClick=${() => browse('repo')}
        >
          Browse…
        </button></label
      ><label class="cron-create-row" hidden=${worktreesHidden}
        ><span class="cron-create-label">Worktree</span
        ><select
          id="template-deploy-worktree"
          value=${worktree}
          disabled=${!wtData?.is_repo}
          onChange=${(event) => setWorktree(event.currentTarget.value)}
        >
          ${options.map(
            (item) =>
              html`<option key=${item.value} value=${item.value}>
                ${item.label}
              </option>`,
          )}
        </select></label
      ><label
        class=${`cron-create-enabled cron-check-aligned${!wtData?.is_repo ? ' disabled' : ''}`}
        id="template-deploy-wt-sync-row"
        hidden=${worktreesHidden}
        ><input
          id="template-deploy-wt-sync"
          type="checkbox"
          checked=${syncBranch}
          disabled=${!wtData?.is_repo}
          onChange=${(event) => setSyncBranch(event.currentTarget.checked)}
        />
        Sync worktree branch with group name</label
      ><label
        class="cron-create-enabled cron-check-aligned"
        id="template-deploy-wt-per-agent-row"
        hidden=${worktreesHidden}
        ><input
          id="template-deploy-wt-per-agent"
          type="checkbox"
          checked=${perAgent}
          disabled=${!wtData?.is_repo}
          onChange=${(event) => setPerAgent(event.currentTarget.checked)}
        />
        Give each agent its own worktree</label
      ><label
        class="cron-create-row"
        id="template-deploy-wt-new-row"
        hidden=${!showNew}
        ><span class="cron-create-label">New branch</span
        ><input
          id="template-deploy-wt-branch"
          type="text"
          value=${branch}
          onInput=${(event) => {
            setBranch(event.currentTarget.value);
            setSyncBranch(false);
          }}
          placeholder="branch name for shared worktree, or prefix for per-agent branches"
          autocomplete="off"
          spellcheck="false" /></label
      ><label
        class="cron-create-row"
        id="template-deploy-wt-base-row"
        hidden=${!showNew || wtData?.has_commits === false}
        ><span class="cron-create-label">Base branch</span
        ><select
          id="template-deploy-wt-base"
          value=${base}
          onChange=${(event) => setBase(event.currentTarget.value)}
        >
          ${(wtData?.branches || []).map(
            (value) =>
              html`<option key=${value} value=${value}>${value}</option>`,
          )}
        </select></label
      >${showNew &&
      wtData?.has_commits === false &&
      html`<p class="wt-orphan-warn" id="template-deploy-wt-orphan-hint">
        ⚠ This repo has no commits yet, so the worktree will be cut as an
        <strong>orphan branch</strong>.
      </p>`}<label
        class="cron-create-row"
        id="template-deploy-context-row"
        hidden=${!copying && !mirror}
        ><span class="cron-create-label"
          ><${Words} plain="Startup context" wizard="Party lore" /></span
        ><textarea
          id="template-deploy-context"
          class="modal-context-textarea"
          rows="5"
          value=${context}
          onInput=${(event) => setContext(event.currentTarget.value)}
          placeholder="the new group's shared startup context"
          spellcheck="false"
        /></label
      ><label class="cron-create-row" id="template-deploy-default-profile-row"
        ><span class="cron-create-label">Default profile</span
        ><select
          id="template-deploy-default-profile"
          value=${findProfileByHandle(current.profiles, defaultProfile)
            ? defaultProfile
            : ''}
          onChange=${(event) => {
            setDefaultProfile(event.currentTarget.value);
            setProfileEdited(true);
          }}
        >
          <option value="">(none — harness default)</option>
          ${profileChoices(current.profiles).map(
            (choice) =>
              html`<option key=${choice.value} value=${choice.value}>
                ${choice.label}
              </option>`,
          )}
        </select></label
      >
      <div class="cron-create-row">
        <span class="cron-create-label">Preview</span>
        <div id="template-deploy-preview" class="template-preview">
          <${DeployRosterPreview}
            template=${template}
            prefix=${reinforcing ? dropGroup.name : groupName}
            profiles=${current.profiles}
            defaultProfile=${defaultProfile}
          />
        </div>
      </div>
      <div class="cron-create-error" id="template-deploy-error" role="alert">
        ${error}
      </div>
      <div class="modal-buttons">
        <button
          id="template-deploy-cancel"
          disabled=${busy || worktreeLoading}
          onClick=${() => { void requestClose(); }}
        >
          Cancel</button
        ><span class="spacer"></span
        ><button
          id="template-deploy-submit"
          class="primary"
          disabled=${busy || worktreeLoading}
          onClick=${submit}
        >
          ${busy
            ? reinforcing
              ? 'Reinforcing…'
              : copying
                ? 'Creating…'
                : 'Deploying…'
            : reinforcing
              ? 'Reinforce group'
              : mode === 'subgroup'
                ? 'Create subgroup'
                : copying
                  ? 'Create group'
                  : 'Deploy task force'}
        </button>
      </div>`,
  );
}

function RoleInspect({ roleName, roles }) {
  if (!roleName) return null;
  const role = roles.find((item) => item.name === roleName);
  if (!role)
    return html`<div class="role-inspect role-inspect-missing">
      ⚠ This role is no longer in the library. A referencing agent falls back
      to its own inline overrides at deploy.
    </div>`;
  const launch = [
    ['profile', role.spawn_profile && `⚙ ${role.spawn_profile}`],
    ['harness', role.harness],
    ['model', role.model],
    ['effort', role.effort],
    ['sandbox', role.sandbox],
    ['approval', role.approval],
  ].filter(([, value]) => value);
  const brief = (role.brief || '').trim();
  return html`<div class="role-inspect">
    ${role.descr && html`<div class="role-inspect-descr">${role.descr}</div>`}
    <div class="role-inspect-row">
      <span class="role-inspect-key">launch</span>${launch.length
        ? html`<span class="role-inspect-vals"
            >${launch.map(
              ([key, value]) =>
                html`<span key=${key} class="role-inspect-chip"
                  ><b>${key}</b> ${value}</span
                >`,
            )}</span
          >`
        : html`<span class="role-inspect-muted"
            >inherits (no defaults set)</span
          >`}
    </div>
    <div class="role-inspect-row">
      <span class="role-inspect-key">grants</span>${role.permissions?.length
        ? html`<span class="role-inspect-vals"
            >${role.permissions.map(
              (slug) =>
                html`<span key=${slug} class="role-inspect-slug"
                  >${slug}</span
                >`,
            )}</span
          >`
        : html`<span class="role-inspect-muted">none</span>`}
    </div>
    ${brief
      ? html`<details class="role-inspect-brief">
          <summary>
            <span class="role-inspect-key">brief</span> ${brief.split('\n')[0]}
          </summary>
          <pre class="role-inspect-brieftext">${brief}</pre>
        </details>`
      : html`<div class="role-inspect-row">
          <span class="role-inspect-key">brief</span
          ><span class="role-inspect-muted">none</span>
        </div>`}
    <div class="role-inspect-foot">
      Resolved at deploy — editing the role changes future deploys;
      already-deployed agents are untouched.
    </div>
  </div>`;
}

function profileOptions(agent, profiles) {
  const choices = profileChoices(profiles);
  const names = choices.map((choice) => choice.value);
  const selected = agent.spawn_profile || '';
  const preferred = getDashDefaultProfile();
  const blank =
    preferred && names.includes(preferred) && agentInheritsDeployDefault(agent)
      ? `(default → ${preferred})`
      : '(none)';
  return [
    ['', blank],
    ...choices.map((choice) => [choice.value, choice.label]),
    ...(selected && !names.includes(selected)
      ? [[selected, `⚠ ${selected} (missing)`]]
      : []),
  ];
}

function ProfileReadback({ agent, profiles }) {
  if (agent.spawn_profile) {
    const profile = findProfileByHandle(profiles, agent.spawn_profile);
    if (!profile)
      return html`<span class="ta-profile-summary-missing"
        >⚠ no profile named “${agent.spawn_profile}” here — pick another or
        manage profiles</span
      >`;
    return (
      profileSummary(profile) ||
      html`<span class="ta-profile-summary-empty"
        >(profile sets no launch fields)</span
      >`
    );
  }
  const preferred = getDashDefaultProfile();
  const profile =
    preferred &&
    agentInheritsDeployDefault(agent) &&
    findProfileByHandle(profiles, preferred);
  return profile
    ? html`<span class="ta-profile-summary-default"
        >inherits default
        ${preferred}${profileSummary(profile)
          ? ` · ${profileSummary(profile)}`
          : ''}</span
      >`
    : null;
}

function AgentRow({
  agent,
  index,
  agents,
  roles,
  profiles,
  setAgents,
  actions,
  confirm,
}) {
  const update = (patch) =>
    setAgents((current) =>
      current.map((item, itemIndex) =>
        itemIndex === index ? { ...item, ...patch } : item,
      ),
    );
  const legacy = [
    agent.harness && `harness ${agent.harness}`,
    agent.model && `model ${agent.model}`,
    agent.effort && `effort ${agent.effort}`,
    agent.sandbox && `sandbox ${agent.sandbox}`,
    agent.approval && `approval ${agent.approval}`,
    agent.permissions?.length &&
      `${agent.permissions.length} inline perm${agent.permissions.length === 1 ? '' : 's'}`,
  ].filter(Boolean);
  const customSummary =
    agent.profile_inline && profileSummary(agent.profile_inline);
  const newProfile = () =>
    actions.openProfileEditor(null, {
      editExisting: false,
      onSaved: (name) => update({ spawn_profile: name }),
    });
  const custom = () => {
    const seed =
      agent.profile_inline ||
      findProfileByHandle(profiles, agent.spawn_profile) ||
      null;
    actions.openProfileEditor(seed ? clone(seed) : null, {
      local: { onSave: (payload) => update({ profile_inline: payload }) },
    });
  };
  const removeCustom = async () => {
    if (
      await confirm({
        title: 'Remove custom launch config?',
        body: 'Discard this agent’s template-local launch config? It is not saved anywhere else — the agent falls back to its picked profile / role / the defaults. Applies to the stored template when you save.',
        meta: agent.name || `agent #${index + 1}`,
        okLabel: 'Remove custom config',
      })
    )
      update({ profile_inline: null });
  };
  const removeLegacy = async () => {
    if (
      await confirm({
        title: 'Remove legacy inline settings?',
        body: `Discard ${legacy.join(' · ')} from this agent? It will use its picked launch profile — or the deploy defaults — instead.`,
        meta: agent.name || `agent #${index + 1}`,
        okLabel: 'Remove',
      })
    )
      update({
        harness: '',
        model: '',
        effort: '',
        sandbox: '',
        approval: '',
        permissions: [],
      });
  };
  const extract = () => {
    const permission_overrides = Object.fromEntries(
      (agent.permissions || []).filter(Boolean).map((slug) => [slug, 'grant']),
    );
    actions.openProfileEditor(
      {
        harness: agent.harness || '',
        model: agent.model || '',
        effort: agent.effort || '',
        sandbox: agent.sandbox || '',
        approval: agent.approval || '',
        permission_overrides,
      },
      {
        editExisting: false,
        onSaved: (name) =>
          update({
            spawn_profile: name,
            harness: '',
            model: '',
            effort: '',
            sandbox: '',
            approval: '',
            permissions: [],
          }),
      },
    );
  };
  const roleOptions = [
    '',
    ...roles.map((role) => role.name),
    ...(agent.role_ref && !roles.some((role) => role.name === agent.role_ref)
      ? [agent.role_ref]
      : []),
  ];
  return html`<div class="template-agent-row" data-idx=${index}>
    <div class="template-agent-row-head">
      <span class="template-agent-num"
        ><${Words} plain="Agent" wizard="Familiar" /> ${index + 1}</span
      ><label
        class="template-agent-owner"
        title="Mark this agent as an owner of the instantiated group — a group can have several owners. This is unioned with the picked profile's own owner default (either one makes the agent an owner)."
        ><input
          type="checkbox"
          class="ta-owner"
          checked=${agent.is_owner}
          onChange=${(event) =>
            update({ is_owner: event.currentTarget.checked })}
        />
        owner</label
      ><button
        type="button"
        class="tool ta-remove"
        title="Remove this agent"
        onClick=${() =>
          setAgents((current) =>
            current.filter((_, itemIndex) => itemIndex !== index),
          )}
      >
        ✕
      </button>
    </div>
    <div class="template-agent-grid">
      <input
        type="text"
        class="ta-name"
        placeholder="name (e.g. PO, dev1)"
        value=${agent.name}
        onInput=${(event) => update({ name: event.currentTarget.value })}
      /><input
        type="text"
        class="ta-role"
        placeholder="role label (e.g. product-owner)"
        value=${agent.role}
        onInput=${(event) => update({ role: event.currentTarget.value })}
      /><input
        type="number"
        class="ta-wave"
        min="0"
        title="Staged-spawn wave (JOH-244): wave 0 spawns first; higher waves spawn once the prior wave is up and idle. All wave 0 = one synchronous pass."
        placeholder="wave (0)"
        value=${agent.wave || 0}
        onInput=${(event) => update({ wave: event.currentTarget.value })}
      />
    </div>
    <label
      class="template-agent-roleref"
      title="Reference a role from the library: the agent inherits that role's canonical brief, default launch shape and default permissions."
      ><span>Role library</span
      ><select
        class="ta-role-ref"
        value=${agent.role_ref}
        onChange=${(event) => update({ role_ref: event.currentTarget.value })}
      >
        ${roleOptions.map(
          (name) =>
            html`<option key=${name} value=${name}>
              ${name
                ? roles.some((role) => role.name === name)
                  ? name
                  : `⚠ ${name} (missing)`
                : '(none)'}
            </option>`,
        )}
      </select></label
    >
    <div class="ta-role-inspect">
      <${RoleInspect} roleName=${agent.role_ref} roles=${roles} />
    </div>
    <input
      type="text"
      class="ta-descr"
      placeholder="one-line description (dashboard column)"
      value=${agent.descr}
      onInput=${(event) => update({ descr: event.currentTarget.value })}
    /><textarea
      class="ta-initmsg"
      rows="3"
      placeholder="task brief for this agent — delivered to its inbox at spawn (newlines OK)"
      value=${agent.initial_message}
      onInput=${(event) =>
        update({ initial_message: event.currentTarget.value })}
    />
    <div class="template-agent-launch">
      <label
        class="template-agent-roleref ta-launch-pick"
        title="Launch profile: the agent's harness, model, effort, sandbox/approval and birth-time permissions come from the picked spawn profile."
        ><span>Launch profile</span
        ><select
          class="ta-profile-select"
          value=${agent.spawn_profile}
          onChange=${(event) =>
            update({ spawn_profile: event.currentTarget.value })}
        >
          ${profileOptions(agent, profiles).map(
            ([value, label]) =>
              html`<option key=${value} value=${value}>${label}</option>`,
          )}
        </select></label
      >
      <div class="ta-launch-actions">
        <button type="button" class="tool ta-custom-launch" onClick=${custom}>
          ${agent.profile_inline ? '✎ edit custom…' : '✎ custom…'}</button
        ><button
          type="button"
          class="tool ta-profile-new"
          onClick=${newProfile}
        >
          ＋ new</button
        ><button
          type="button"
          class="tool ta-profile-manage"
          onClick=${() => actions.openManager('profiles')}
        >
          ⧉ manage…
        </button>
      </div>
      <div class="ta-profile-summary">
        <${ProfileReadback} agent=${agent} profiles=${profiles} />
      </div>
      ${agent.profile_inline &&
      html`<div class="ta-legacy-note ta-custom-note">
        <span class="ta-legacy-text"
          >✎ <${Words} plain="custom" wizard="bespoke" />:
          ${customSummary || '(sets no launch fields)'}</span
        ><button
          type="button"
          class="tool ta-custom-remove"
          onClick=${removeCustom}
        >
          ✕
        </button>
      </div>`}
      ${agentHasLegacyLaunch(agent) &&
      html`<div class="ta-legacy-note">
        <span class="ta-legacy-text"
          >⚠ legacy inline: ${legacy.join(' · ')}</span
        ><button
          type="button"
          class="tool ta-extract-profile"
          onClick=${extract}
        >
          Extract to profile…</button
        ><button
          type="button"
          class="tool ta-legacy-remove"
          onClick=${removeLegacy}
        >
          ✕
        </button>
      </div>`}
    </div>
  </div>`;
}

function ReorderButtons({ prefix, label, index, count, update }) {
  return html`<button
      type="button"
      class=${`tool ${prefix}-up`}
      title=${`Move this ${label} up`}
      disabled=${index === 0}
      onClick=${() => update(-1)}
    >
      ↑</button
    ><button
      type="button"
      class=${`tool ${prefix}-down`}
      title=${`Move this ${label} down`}
      disabled=${index === count - 1}
      onClick=${() => update(1)}
    >
      ↓</button
    ><button
      type="button"
      class=${`tool ${prefix}-remove`}
      title=${`Remove this ${label}`}
      onClick=${() => update(0)}
    >
      ✕
    </button>`;
}

function Concepts() {
  return html`<details class="tpl-concepts" id="template-editor-concepts">
    <summary>
      <${Words}
        plain="ⓘ How deploying works — pattern, process & rhythms"
        wizard="🔮 How a summoning works — rite, quest & drumbeats"
      />
    </summary>
    <div class="tpl-concepts-body tpl-concepts-plain">
      <p>
        Three things shape a deployed force and they work <b>together</b>: the
        <b>work pattern</b> briefs it once, the <b>process</b> gives it a shared
        map of phases to advance through, and the <b>rhythms</b> keep it moving
        between them.
      </p>
      <ul class="tpl-concepts-list">
        <li>
          <b>Work pattern — the briefing.</b> An ordered list of messages
          delivered <b>once</b>, after the whole team has spawned. Each step
          goes to one member (often the lead, who then fans the work out) or to
          everyone. It fires at deploy and does not repeat — but
          <b>Re-brief</b> re-delivers the template's <i>current</i> pattern to
          the live team on demand.
        </li>
        <li>
          <b>Process — the plan.</b> An ordered map of phases the team advances
          through. It is <b>advisory</b>: advancing records a transition and
          nudges the roles now active — nothing is blocked, no permissions
          change, nothing auto-advances. A shared sense of where the work is,
          not a gate.
        </li>
        <li>
          <b>Rhythms — the pulse.</b> Recurring nudges materialized as group
          cron jobs <b>when you deploy</b> — a snapshot taken then. Editing the
          template afterwards does <b>not</b> retune a force already in the
          field. Standing the force down deletes them; a retire that empties the
          group disables them (a later resume re-enables them).
        </li>
      </ul>
      <table class="tpl-concepts-table">
        <thead>
          <tr>
            <th></th>
            <th>Delivered</th>
            <th>Repeats?</th>
            <th>Enforced?</th>
            <th>On stand-down</th>
          </tr>
        </thead>
        <tbody>
          <tr>
            <th>Work pattern</th>
            <td>once, after the team is up</td>
            <td>no — re-brief re-sends on demand</td>
            <td>no — it is a briefing</td>
            <td>—</td>
          </tr>
          <tr>
            <th>Process</th>
            <td>snapshot at deploy</td>
            <td>advance by hand</td>
            <td>no — advisory</td>
            <td>phase history kept</td>
          </tr>
          <tr>
            <th>Rhythms</th>
            <td>cron jobs at deploy</td>
            <td>yes, on a schedule</td>
            <td>no — nudges</td>
            <td>cron jobs deleted</td>
          </tr>
        </tbody>
      </table>
    </div>
    <div class="tpl-concepts-body tpl-concepts-wizard">
      <p>
        Three powers shape a summoned party, woven <b>together</b>: the
        <b>rite of command</b> briefs it once, the <b>quest plan</b> hands it a
        shared map of chapters, and the <b>drumbeats</b> keep it marching
        between them.
      </p>
      <ul class="tpl-concepts-list">
        <li>
          <b>Rite of command — the opening whispers.</b> An ordered set of
          whispers spoken <b>once</b>, after the whole party has stood. Each
          verse reaches one familiar (often the lead, who parcels out the quest)
          or the whole party. It sounds once at the summoning and does not echo
          — though a <b>re-brief</b> speaks the circle's <i>current</i> rite to
          the living party again on command.
        </li>
        <li>
          <b>Quest plan — the chapters.</b> An ordered map of chapters the party
          moves through. It is a map, not a wall: turning to the next chapter
          marks the passage and rouses the heroes it calls for — nothing is
          barred, no wards shift, no page turns itself. A shared sense of where
          the tale stands, not a locked door.
        </li>
        <li>
          <b>Drumbeats — the pulse.</b> Recurring nudges struck as cron
          <b>the moment the party is summoned</b> — a cast taken then. Redrawing
          the circle later will <b>not</b> retune a party already afield.
          Standing the party down silences them for good; a retire that empties
          the ranks stills them (a later resume wakes them again).
        </li>
      </ul>
      <table class="tpl-concepts-table">
        <thead>
          <tr>
            <th></th>
            <th>Sounded</th>
            <th>Echoes?</th>
            <th>Barred?</th>
            <th>On stand-down</th>
          </tr>
        </thead>
        <tbody>
          <tr>
            <th>Rite of command</th>
            <td>once, after the party stands</td>
            <td>no — a re-brief re-speaks it</td>
            <td>no — a briefing</td>
            <td>—</td>
          </tr>
          <tr>
            <th>Quest plan</th>
            <td>cast at summoning</td>
            <td>turned by hand</td>
            <td>no — advisory</td>
            <td>chapter history kept</td>
          </tr>
          <tr>
            <th>Drumbeats</th>
            <td>cron at summoning</td>
            <td>yes, on a schedule</td>
            <td>no — nudges</td>
            <td>drums silenced</td>
          </tr>
        </tbody>
      </table>
    </div>
  </details>`;
}

export function TemplateEditor({
  descriptor,
  current,
  state,
  actions,
  confirmDiscard,
  confirm,
}) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const baseline = useMemo(() => templateDraft(descriptor.seed), [descriptor]);
  const [draft, setDraft] = useState(() => clone(baseline));
  const dirty = JSON.stringify(draft) !== JSON.stringify(baseline);
  const editing = descriptor.seed && !descriptor.options?.asNew;
  const saving = current.busy === 'template-save';
  const setAgents = (value) =>
    setDraft((old) => ({
      ...old,
      agents: typeof value === 'function' ? value(old.agents) : value,
    }));
  const updateList = (key, index, direction) =>
    setDraft((old) => ({
      ...old,
      [key]:
        direction === 0
          ? old[key].filter((_, i) => i !== index)
          : moveItem(old[key], index, direction),
    }));
  const submit = async () => {
    state.error.value = '';
    if (!draft.name.trim()) {
      state.error.value = 'template name is required';
      return;
    }
    await actions.saveTemplate({
      draft,
      originalName: editing ? descriptor.seed.name : '',
      payload: templatePayload(draft),
    });
  };
  const handoff = async () => {
    if (saving || (dirty && !(await confirmDiscard()))) return;
    state.closeTemplateDialog();
    await actions.editTemplateWithAgent(
      editing ? descriptor.seed.name : draft.name.trim(),
    );
  };
  return h(
    Overlay,
    {
      id: 'template-editor-modal',
      labelledby: 'template-editor-title',
      onClose: state.closeTemplateDialog,
      onSubmitHotkey: saving ? null : submit,
      dirty,
      blocked: saving,
      confirmDiscard,
      registerClose,
      resizeKey: 'tclaude.dash.modalSize.template-editor',
    },
    html`<h3 id="template-editor-title">
        ${editing
          ? html`<${Words}
              plain=${`Edit template: ${descriptor.seed.name}`}
              wizard=${`Redraw the circle: ${descriptor.seed.name}`}
            />`
          : html`<${Words}
              plain="New group template"
              wizard="Chalk a new summoning circle"
            />`}
      </h3>
      <label class="cron-create-row"
        ><span class="cron-create-label">Name</span
        ><input
          id="template-editor-name"
          type="text"
          value=${draft.name}
          onInput=${(event) =>
            setDraft((old) => ({ ...old, name: event.currentTarget.value }))}
          placeholder="kebab-or-snake-case label"
          autocomplete="off"
          spellcheck="false"
          autofocus
      /></label>
      <label class="cron-create-row"
        ><span class="cron-create-label">Descr</span
        ><input
          id="template-editor-descr"
          type="text"
          value=${draft.descr}
          onInput=${(event) =>
            setDraft((old) => ({ ...old, descr: event.currentTarget.value }))}
          placeholder="optional one-line description"
          autocomplete="off"
          spellcheck="false"
      /></label>
      <label class="cron-create-row"
        ><span class="cron-create-label">Default context</span
        ><textarea
          id="template-editor-context"
          class="modal-context-textarea"
          rows="4"
          value=${draft.default_context}
          onInput=${(event) =>
            setDraft((old) => ({
              ...old,
              default_context: event.currentTarget.value,
            }))}
          placeholder="optional — reusable group-wide boilerplate; the per-instantiation task is appended to this and delivered to every spawned agent's inbox"
          spellcheck="false"
        />
      </label>
      <label
        class="cron-create-enabled cron-check-aligned"
        title="Default the summon/deploy dialog to one fresh git worktree per roster agent. This is only a template default: it can still be toggled for each spawn."
        ><input
          id="template-editor-per-agent-worktrees"
          type="checkbox"
          checked=${draft.per_agent_worktrees}
          onChange=${(event) =>
            setDraft((old) => ({
              ...old,
              per_agent_worktrees: event.currentTarget.checked,
            }))} /><span
          ><${Words}
            plain="Give each agent its own worktree by default"
            wizard="Give each familiar its own enchanted grove by default" /></span
      ></label>
      <div class="template-agents-head">
        <span><${Words} plain="Agents" wizard="Familiars" /></span
        ><button
          type="button"
          id="template-editor-add-agent"
          class="tool"
          onClick=${() =>
            setAgents((agents) => [...agents, blankTemplateAgent()])}
        >
          <${Words} plain="+ add agent" wizard="+ add familiar" />
        </button>
      </div>
      <div id="template-editor-agents">
        ${draft.agents.map(
          (agent, index) =>
            html`<${AgentRow}
              key=${index}
              agent=${agent}
              index=${index}
              agents=${draft.agents}
              roles=${current.roles}
              profiles=${current.profiles}
              setAgents=${setAgents}
              actions=${actions}
              confirm=${confirm}
            />`,
        )}
      </div>
      <${Concepts} />
      <div
        class="template-agents-head"
        title="Ordered briefing messages delivered after the whole roster has spawned."
      >
        <span><${Words} plain="Work pattern" wizard="Rite of command" /></span
        ><button
          type="button"
          id="template-editor-add-pattern"
          class="tool"
          onClick=${() =>
            setDraft((old) => ({
              ...old,
              work_pattern: [
                ...old.work_pattern,
                { send_to: 'all', value: '' },
              ],
            }))}
        >
          <${Words} plain="+ add step" wizard="+ add verse" />
        </button>
      </div>
      <div id="template-editor-pattern">
        ${draft.work_pattern.map((step, index) => {
          const names = draft.agents
            .map((agent) => agent.name.trim())
            .filter(Boolean);
          const options = [
            'all',
            ...names,
            ...(step.send_to &&
            step.send_to !== 'all' &&
            !names.includes(step.send_to)
              ? [step.send_to]
              : []),
          ];
          return html`<div
            key=${index}
            class="template-agent-row template-pattern-row"
            data-idx=${index}
          >
            <div class="template-agent-row-head">
              <span class="template-agent-num"
                ><${Words} plain="Step" wizard="Verse" /> ${index + 1}</span
              ><label class="template-pattern-sendto"
                ><${Words} plain="send to" wizard="whisper to" /><select
                  class="tw-sendto"
                  value=${step.send_to}
                  onChange=${(event) =>
                    setDraft((old) => ({
                      ...old,
                      work_pattern: old.work_pattern.map((item, i) =>
                        i === index
                          ? { ...item, send_to: event.currentTarget.value }
                          : item,
                      ),
                    }))}
                >
                  ${options.map(
                    (name) =>
                      html`<option key=${name} value=${name}>
                        ${name === 'all'
                          ? 'all members'
                          : !names.includes(name)
                            ? `⚠ ${name} (no such agent)`
                            : name}
                      </option>`,
                  )}
                </select></label
              ><${ReorderButtons}
                prefix="tw"
                label="step"
                index=${index}
                count=${draft.work_pattern.length}
                update=${(direction) =>
                  updateList('work_pattern', index, direction)}
              />
            </div>
            <textarea
              class="tw-value"
              rows="2"
              value=${step.value}
              onInput=${(event) =>
                setDraft((old) => ({
                  ...old,
                  work_pattern: old.work_pattern.map((item, i) =>
                    i === index
                      ? { ...item, value: event.currentTarget.value }
                      : item,
                  ),
                }))}
              placeholder="briefing message delivered after the whole team is up — {{task}} is replaced with the dispatch task (newlines OK)"
            />
          </div>`;
        })}
      </div>
      <div class="template-agents-head">
        <span><${Words} plain="Process" wizard="Quest plan" /></span
        ><button
          type="button"
          id="template-editor-add-phase"
          class="tool"
          onClick=${() =>
            setDraft((old) => ({
              ...old,
              process: [
                ...old.process,
                { name: '', roles: [], roles_text: '', criteria: '' },
              ],
            }))}
        >
          <${Words} plain="+ add phase" wizard="+ add chapter" />
        </button>
      </div>
      <div id="template-editor-process">
        ${draft.process.map(
          (phase, index) =>
            html`<div
              key=${index}
              class="template-agent-row template-process-row"
              data-idx=${index}
            >
              <div class="template-agent-row-head">
                <span class="template-agent-num"
                  ><${Words} plain="Phase" wizard="Chapter" /> ${index +
                  1}</span
                ><${ReorderButtons}
                  prefix="tpp"
                  label="phase"
                  index=${index}
                  count=${draft.process.length}
                  update=${(direction) =>
                    updateList('process', index, direction)}
                />
              </div>
              <div class="template-agent-grid">
                <input
                  type="text"
                  class="tpp-name"
                  value=${phase.name}
                  onInput=${(event) =>
                    setDraft((old) => ({
                      ...old,
                      process: old.process.map((item, i) =>
                        i === index
                          ? { ...item, name: event.currentTarget.value }
                          : item,
                      ),
                    }))}
                  placeholder="phase name (e.g. design, build, review)"
                /><input
                  type="text"
                  class="tpp-roles"
                  value=${phase.roles_text ?? phase.roles.join(', ')}
                  onInput=${(event) =>
                    setDraft((old) => ({
                      ...old,
                      process: old.process.map((item, i) =>
                        i === index
                          ? { ...item, roles_text: event.currentTarget.value }
                          : item,
                      ),
                    }))}
                  placeholder="active roles, comma-separated (e.g. dev, reviewer; 'all' = everyone)"
                />
              </div>
              <textarea
                class="tpp-criteria"
                rows="2"
                value=${phase.criteria}
                onInput=${(event) =>
                  setDraft((old) => ({
                    ...old,
                    process: old.process.map((item, i) =>
                      i === index
                        ? { ...item, criteria: event.currentTarget.value }
                        : item,
                    ),
                  }))}
                placeholder="criteria — entry / exit / handoff in plain words (advisory, not enforced)"
              />
            </div>`,
        )}
      </div>
      <div class="template-agents-head">
        <span><${Words} plain="Rhythms" wizard="Drumbeats" /></span
        ><button
          type="button"
          id="template-editor-add-rhythm"
          class="tool"
          onClick=${() =>
            setDraft((old) => ({
              ...old,
              rhythms: [
                ...old.rhythms,
                {
                  name: '',
                  target_role: '',
                  interval: '',
                  cron_expr: '',
                  subject: '',
                  body: '',
                },
              ],
            }))}
        >
          <${Words} plain="+ add rhythm" wizard="+ add drumbeat" />
        </button>
      </div>
      <div id="template-editor-rhythms">
        ${draft.rhythms.map((rhythm, index) => {
          const update = (patch) =>
            setDraft((old) => ({
              ...old,
              rhythms: old.rhythms.map((item, i) =>
                i === index ? { ...item, ...patch } : item,
              ),
            }));
          return html`<div
            key=${index}
            class="template-agent-row template-rhythm-row"
            data-idx=${index}
          >
            <div class="template-agent-row-head">
              <span class="template-agent-num"
                ><${Words} plain="Rhythm" wizard="Drumbeat" /> ${index +
                1}</span
              ><${ReorderButtons}
                prefix="trh"
                label="rhythm"
                index=${index}
                count=${draft.rhythms.length}
                update=${(direction) => updateList('rhythms', index, direction)}
              />
            </div>
            <div class="template-agent-grid">
              <input
                type="text"
                class="trh-name"
                value=${rhythm.name}
                onInput=${(event) =>
                  update({ name: event.currentTarget.value })}
                placeholder="name (e.g. status-ping)"
              /><input
                type="text"
                class="trh-role"
                value=${rhythm.target_role}
                onInput=${(event) =>
                  update({ target_role: event.currentTarget.value })}
                placeholder="role filter (blank / 'all' = whole group)"
              />
            </div>
            <div class="template-agent-grid">
              <input
                type="text"
                class="trh-interval"
                value=${rhythm.interval}
                onInput=${(event) =>
                  update({ interval: event.currentTarget.value })}
                placeholder="interval (e.g. 10m) — OR cron below"
              /><input
                type="text"
                class="trh-cron"
                value=${rhythm.cron_expr}
                onInput=${(event) =>
                  update({ cron_expr: event.currentTarget.value })}
                placeholder="cron expr (e.g. '0 * * * *') — OR interval"
              />
            </div>
            <input
              type="text"
              class="trh-subject"
              value=${rhythm.subject}
              onInput=${(event) =>
                update({ subject: event.currentTarget.value })}
              placeholder="subject (optional)"
            /><textarea
              class="trh-body"
              rows="2"
              value=${rhythm.body}
              onInput=${(event) => update({ body: event.currentTarget.value })}
              placeholder="message body the nudge sends each tick (newlines OK)"
            />
          </div>`;
        })}
      </div>
      <label
        class="template-editor-field"
        title="Cap (seconds) on how long each staged-spawn wave waits for the prior wave to go idle before the next wave spawns anyway. 0 = built-in default (8 min)."
        ><span
          ><${Words}
            plain="Wave max-wait (s)"
            wizard="Marching-order patience (s)" /></span
        ><input
          type="number"
          id="template-editor-wave-max-wait"
          min="0"
          placeholder="0 = default"
          value=${draft.wave_max_wait || ''}
          onInput=${(event) =>
            setDraft((old) => ({
              ...old,
              wave_max_wait: event.currentTarget.value,
            }))}
      /></label>
      <div class="cron-create-error" id="template-editor-error" role="alert">
        ${current.error}
      </div>
      <div class="modal-buttons">
        <button
          id="template-editor-cancel"
          type="button"
          disabled=${saving}
          onClick=${() => { void requestClose(); }}
        >
          Cancel</button
        ><button
          id="scribe-editor-open"
          type="button"
          disabled=${saving}
          onClick=${handoff}
        >
          <${Words}
            plain="🤖 Edit with agent"
            wizard="📜 Dictate to a scribe"
          /></button
        ><span class="spacer"></span
        ><button
          id="template-editor-submit"
          class="primary"
          type="button"
          disabled=${saving}
          onClick=${submit}
        >
          ${saving ? 'Saving…' : 'Save template'}
        </button>
      </div> `,
  );
}
