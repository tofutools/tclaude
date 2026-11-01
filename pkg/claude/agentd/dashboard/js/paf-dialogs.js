import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { ManagementOverlay as Overlay, useGuardedOverlayClose } from './management-overlay.js';
import { dashboardState } from './snapshot-store.js';
import { WorkflowImportButton } from './process-workflow-share.js';
import {
  confirmGroups, downloadBlob, findingsText, lossSummary, modelsFor, provenanceRows, redactionText, rungBadge,
  safeFilename, spawnableHarnesses,
} from './paf-model.js';

const html = htm.bind(h);

function message(error) { return error?.message || String(error); }

function Check({ id, checked, disabled, onChange, children, hint }) {
  return html`<label class="paf-check"><input id=${id} type="checkbox" checked=${checked} disabled=${disabled}
    onChange=${(event) => onChange(event.currentTarget.checked)} /><span>${children}${hint && html`<span class="paf-hint">${hint}</span>`}</span></label>`;
}

// FindingsPanel shows a credential scan. Counts, kinds and locations only.
function FindingsPanel({ findings, title = 'Credential scan' }) {
  const lines = findingsText(findings);
  return html`<div class="paf-findings" id="paf-findings">
    <div class="paf-findings-head">⚠ ${title}: ${lines.length} finding${lines.length === 1 ? '' : 's'}</div>
    <ul>${lines.map((line) => html`<li key=${line}>${line}</li>`)}</ul>
  </div>`;
}

const REDACT_DEFAULTS = { mask_flagged: true, paths_to_placeholders: true, stub_tool_results: false, drop_reasoning: false, drop_media: false };

// RedactPanel previews and downloads a redacted copy. source() resolves the
// unredacted PAF; nothing is written on this node.
function RedactPanel({ actions, source, filename, onDone }) {
  const [opts, setOpts] = useState(REDACT_DEFAULTS);
  const [report, setReport] = useState(null);
  const [busy, setBusy] = useState('');
  const [error, setError] = useState('');
  const [ack, setAck] = useState(false);
  const set = (key) => (value) => { setOpts({ ...opts, [key]: value }); setReport(null); setAck(false); };
  const stillFlagged = (report?.findings || []).length > 0;
  const run = async (preview) => {
    setBusy(preview ? 'preview' : 'download');
    setError('');
    try {
      const blob = await source();
      const out = await actions.redactPAF(blob, opts, preview);
      if (preview) setReport(out);
      else { downloadBlob(out, filename); onDone?.(); }
    } catch (e) { setError(message(e)); } finally { setBusy(''); }
  };
  return html`<div class="paf-redact" id="paf-redact">
    <div class="paf-sec">Redact before sharing</div>
    <${Check} id="paf-redact-mask" checked=${opts.mask_flagged} onChange=${set('mask_flagged')}>Mask suspected credentials</${Check}>
    <${Check} id="paf-redact-paths" checked=${opts.paths_to_placeholders} onChange=${set('paths_to_placeholders')}>Replace home, working dir and user name with placeholders</${Check}>
    <${Check} id="paf-redact-stub" checked=${opts.stub_tool_results} onChange=${set('stub_tool_results')}>Stub tool results older than the last 3 turns</${Check}>
    <${Check} id="paf-redact-reasoning" checked=${opts.drop_reasoning} onChange=${set('drop_reasoning')}>Drop reasoning</${Check}>
    <${Check} id="paf-redact-media" checked=${opts.drop_media} onChange=${set('drop_media')}>Drop images and documents</${Check}>
    <div class="paf-hint">Redacting always drops the native history; the conversation travels in neutral form.</div>
    ${report && html`<div class="paf-report" id="paf-redact-report">
      ${redactionText(report).map((line) => html`<div key=${line}>✓ ${line}</div>`)}
      ${(report.findings || []).length
        ? html`<${FindingsPanel} findings=${report.findings} title="Still flagged after redaction" />
          <${Check} id="paf-redact-ack" checked=${ack} onChange=${setAck}>Download anyway; I have checked the remaining findings</${Check}>`
        : html`<div class="paf-ok">✓ No suspected credentials remain.</div>`}
    </div>`}
    <div class="cron-create-error" role="alert">${error}</div>
    <div class="paf-row-buttons">
      <button type="button" id="paf-redact-preview" disabled=${!!busy} onClick=${() => run(true)}>${busy === 'preview' ? 'Previewing…' : 'Preview redaction'}</button>
      <button type="button" id="paf-redact-download" class="primary" disabled=${!!busy || !report || (stillFlagged && !ack)} title=${report ? '' : 'Preview the redaction first'}
        onClick=${() => run(false)}>${busy === 'download' ? 'Redacting…' : '⤓ Download redacted .paf'}</button>
    </div>
  </div>`;
}

function processesOn() { return dashboardState.snapshot.value?.processes_enabled === true; }

// AgentExportDialog exports one agent as a PAF (agent ⚙ → export (PAF)…).
// An export the credential scan flags is refused until the human either
// acknowledges the findings or downloads a redacted copy instead.
export function AgentExportDialog({ descriptor, state, actions, confirmDiscard }) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const [history, setHistory] = useState(true);
  const [native, setNative] = useState(true);
  const [neutral, setNeutral] = useState(true);
  const [workflows, setWorkflows] = useState(false);
  const [findings, setFindings] = useState(null);
  const [ack, setAck] = useState(false);
  const [redacting, setRedacting] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const flaggedBlob = useRef(null);
  const label = descriptor.label || descriptor.agent;
  const filename = safeFilename(label, '.paf');
  const choices = (allowFlagged) => ({ agent: descriptor.agent, history, native, neutral, allowFlagged, workflows });
  const reset = (fn) => (value) => { fn(value); setFindings(null); setAck(false); setRedacting(false); flaggedBlob.current = null; };
  const download = async () => {
    if (busy) return;
    setBusy(true);
    setError('');
    try {
      const blob = await actions.exportAgentPAF(choices(!!findings && ack));
      downloadBlob(blob, filename);
      state.closeDialog();
    } catch (e) {
      if (e.status === 422 && e.body?.code === 'flagged_credentials') setFindings(e.body.findings || []);
      else setError(message(e));
    } finally { setBusy(false); }
  };
  // The redact step reads the flagged export once and redacts it here.
  const unredacted = async () => {
    if (!flaggedBlob.current) flaggedBlob.current = await actions.exportAgentPAF(choices(true));
    return flaggedBlob.current;
  };
  return html`<${Overlay} id="paf-export-modal" labelledby="paf-export-title" onClose=${state.closeDialog}
    dirty=${false} blocked=${busy} confirmDiscard=${confirmDiscard} registerClose=${registerClose}>
    <h3 id="paf-export-title">Export ${label} as PAF</h3>
    <div class="modal-meta">A portable file you can import anywhere or read with <code>tclaude paf render</code>. It is a fork point: importing it creates a new agent.</div>
    <${Check} id="paf-export-history" checked=${history} disabled=${busy} onChange=${reset(setHistory)}
      hint="Without it only the definition and environment travel.">Conversation history</${Check}>
    <div class="paf-sub">
      <${Check} id="paf-export-native" checked=${native} disabled=${busy || !history} onChange=${reset(setNative)}
        hint="Byte-exact; resumes losslessly on the same harness.">Native history</${Check}>
      <${Check} id="paf-export-neutral" checked=${neutral} disabled=${busy || !history} onChange=${reset(setNeutral)}
        hint="Lets other harnesses and readers land it.">Harness-neutral context</${Check}>
    </div>
    ${processesOn() && html`<${Check} id="paf-export-workflows" checked=${workflows} disabled=${busy} onChange=${reset(setWorkflows)}
      hint="The templates of its active runs. An older tclaude cannot import a file that carries workflows.">Carry its workflows</${Check}>`}
    ${history && !native && !neutral && html`<div class="cron-create-error">Keep at least one copy of the conversation, or untick Conversation history.</div>`}
    ${findings && html`<${FindingsPanel} findings=${findings} />
      ${!redacting && html`<div class="paf-flagged-actions">
        <${Check} id="paf-export-ack" checked=${ack} onChange=${setAck}>Export anyway; I have checked the findings</${Check}>
        <button type="button" id="paf-export-redact" onClick=${() => setRedacting(true)}>Redact…</button>
      </div>`}
      ${redacting && html`<${RedactPanel} actions=${actions} source=${unredacted} filename=${safeFilename(label, '.redacted.paf')} onDone=${state.closeDialog} />`}`}
    <div class="cron-create-error" role="alert">${error}</div>
    <div class="modal-buttons">
      <button type="button" id="paf-export-cancel" disabled=${busy} onClick=${() => { void requestClose(); }}>Cancel</button>
      <span class="spacer"></span>
      <button type="button" id="paf-export-download" class="primary"
        disabled=${busy || (history && !native && !neutral) || (!!findings && !ack)} onClick=${download}>
        ${busy ? 'Exporting…' : '⤓ Download .paf'}</button>
    </div>
  </${Overlay}>`;
}

// GroupExportDialog picks the group archive format: a group PAF, the
// default, or the legacy .zip an older tclaude imports.
export function GroupExportDialog({ descriptor, state, actions, confirmDiscard }) {
  const { requestClose, registerClose } = useGuardedOverlayClose();
  const [format, setFormat] = useState('paf');
  const [workflows, setWorkflows] = useState(false);
  const [findings, setFindings] = useState(null);
  const [ack, setAck] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const download = async () => {
    if (busy) return;
    setBusy(true);
    setError('');
    try {
      const { blob, filename } = await actions.exportGroupArchive(descriptor.group, format, !!findings && ack, '', format === 'paf' && workflows);
      downloadBlob(blob, filename);
      state.closeDialog();
    } catch (e) {
      if (e.status === 422 && e.body?.code === 'flagged_credentials') setFindings(e.body.findings || []);
      else setError(message(e));
    } finally { setBusy(false); }
  };
  const pick = (value) => { setFormat(value); setFindings(null); setAck(false); };
  return html`<${Overlay} id="group-export-modal" labelledby="group-export-title" onClose=${state.closeDialog}
    dirty=${false} blocked=${busy} confirmDiscard=${confirmDiscard} registerClose=${registerClose}>
    <h3 id="group-export-title">Export group ${descriptor.group}</h3>
    <div class="modal-meta">Members, roles, permissions, messages and every conversation, in one file.</div>
    <label class="paf-check"><input type="radio" name="group-export-format" id="group-export-paf" checked=${format === 'paf'} onChange=${() => pick('paf')} />
      <span>.paf<span class="paf-hint">Group PAF: every member with its own harness's history; other tools can read it.</span></span></label>
    <label class="paf-check"><input type="radio" name="group-export-format" id="group-export-zip" checked=${format === 'zip'} onChange=${() => pick('zip')} />
      <span>.zip (legacy)<span class="paf-hint">For importing into an older tclaude. Conversations on harnesses other than Claude Code export without history.</span></span></label>
    ${format === 'paf' && processesOn() && html`<${Check} id="group-export-workflows" checked=${workflows} disabled=${busy} onChange=${(value) => { setWorkflows(value); setFindings(null); setAck(false); }}
      hint="Its saved processes. An older tclaude cannot import a file that carries workflows.">Carry its workflows</${Check}>`}
    ${findings && html`<${FindingsPanel} findings=${findings} />
      <${Check} id="group-export-ack" checked=${ack} onChange=${setAck}>Export anyway; I have checked the findings</${Check}>`}
    <div class="cron-create-error" role="alert">${error}</div>
    <div class="modal-buttons">
      <button type="button" disabled=${busy} onClick=${() => { void requestClose(); }}>Cancel</button>
      <span class="spacer"></span>
      <button type="button" id="group-export-download" class="primary" disabled=${busy || (!!findings && !ack)} onClick=${download}>
        ${busy ? 'Exporting…' : `⤓ Download .${format}`}</button>
    </div>
  </${Overlay}>`;
}

// LandingPosture says how a landed agent launches. A refused posture blocks the
// import, so it reads as the warning it is.
function LandingPosture({ posture }) {
  if (!posture) return null;
  if (posture.refused) return html`<div class="gi-warn">⚠ ${posture.refused}</div>`;
  const profiles = posture.sandbox_profiles?.length ? ` · ${posture.sandbox_profiles.join(', ')}` : '';
  return html`<span title=${posture.resolved_by || ''}>${posture.sandbox_implementation}${profiles}</span>
    ${posture.opencode_state && html`<div class="paf-hint">OpenCode state: ${posture.opencode_state}</div>`}`;
}

// LandingSelects picks a landing harness and model. A new harness clears the
// model in the same update (onChange gets both), so neither is lost.
function LandingSelects({ harness, model, onChange, idPrefix, sameLabel = '(own harness)' }) {
  const snapshot = dashboardState.snapshot.value;
  const harnesses = spawnableHarnesses(snapshot);
  const models = harness ? modelsFor(snapshot, harness) : [];
  return html`<span class="paf-landing-selects">
    <select id=${`${idPrefix}-harness`} value=${harness} onChange=${(event) => onChange({ harness: event.currentTarget.value, model: '' })}>
      <option value="">${sameLabel}</option>
      ${harnesses.map((item) => html`<option key=${item.name} value=${item.name}>${item.display_name || item.name}</option>`)}
    </select>
    <select id=${`${idPrefix}-model`} value=${model} onChange=${(event) => onChange({ harness, model: event.currentTarget.value })}>
      <option value="">(model: default)</option>
      ${models.map((name) => html`<option key=${name} value=${name}>${name}</option>`)}
    </select>
  </span>`;
}

function HistoryLanding({ landing }) {
  if (!landing) return null;
  const badge = rungBadge(landing.rung);
  const from = [landing.source_harness, landing.source_model].filter(Boolean).join(' · ');
  const to = [landing.harness, landing.model].filter(Boolean).join(' · ');
  return html`<div class="paf-box" id="paf-history-landing">
    <div><b>History:</b> <span class=${`paf-rung ${badge.cls}`} title=${badge.title}>${badge.text}</span> ${from}${to && to !== from ? ` → ${to}` : ''}</div>
    ${landing.losses && html`<div class="paf-hint">${lossSummary(landing.losses)}</div>`}
    ${landing.rung !== 'native' && html`<div class="paf-hint">The full transcript stays readable with <code>tclaude paf render</code>.</div>`}
  </div>`;
}

// ImportWarnings lists what an import warned of, on its preview and its result.
export function ImportWarnings({ id, title, warnings }) {
  if (!warnings?.length) return null;
  return html`<div class="paf-preview paf-box" id=${id}><b>${title}</b>
    ${warnings.map((warning) => html`<div key=${warning} class="gi-warn">⚠ ${warning}</div>`)}</div>`;
}

// ImportConfirmList is the list of what a file asks for beyond a spawn here:
// each row keeps or drops one item, and the server checks the choice again. A
// value is not in the preview: Reveal asks for that one, for that click.
export function ImportConfirmList({ confirm, overrides, setOverrides, reveal }) {
  const [shown, setShown] = useState({});
  useEffect(() => setShown({}), [confirm?.digest]);
  if (!confirm?.items?.length) return null;
  const canKeep = !!confirm.can_keep;
  const show = async (item) => {
    try {
      const { value } = await reveal(item.id);
      setShown((current) => ({ ...current, [item.id]: { value } }));
    } catch (err) {
      setShown((current) => ({ ...current, [item.id]: { error: message(err) } }));
    }
  };
  return html`<div class="paf-preview paf-box" id="import-confirm">
    <b>Elevated items in this file</b>
    <div class="paf-hint">${canKeep ? 'Untick what this import should not keep. Anything unticked is dropped.' : 'Only a human on this node can keep these, so all of them are dropped.'}</div>
    ${confirmGroups(confirm.items).map((group) => html`<div key=${group.heading} class="import-confirm-group">
      <div class="gi-head">${group.heading}</div>
      ${group.items.map((item) => html`<div key=${item.id} class="import-confirm-item" data-item=${item.id}>
        <${Check} id=${`import-keep-${item.id}`} checked=${canKeep && (overrides[item.id] ?? !!item.default)} disabled=${!canKeep}
          onChange=${(value) => setOverrides({ ...overrides, [item.id]: value })} hint=${item.gives}>${item.label}</${Check}>
        ${item.revealable && canKeep && shown[item.id]?.value === undefined && html`<button type="button" class="import-confirm-reveal" onClick=${() => show(item)}>Reveal value</button>`}
        ${shown[item.id]?.value !== undefined && html`<code data-capture-mask class="import-confirm-value">${shown[item.id].value}</code>`}
        ${shown[item.id]?.error && html`<div class="gi-warn">⚠ ${shown[item.id].error}</div>`}
      </div>`)}
    </div>`)}
  </div>`;
}

// ImportOutcome lists what became of a file's elevated items.
export function ImportOutcome({ kept, dropped }) {
  if (!kept?.length && !dropped?.length) return null;
  return html`<div class="paf-preview paf-box" id="import-outcome">
    ${kept?.length > 0 && html`<div id="import-outcome-kept"><b>Kept</b> ${kept.join(', ')}</div>`}
    ${dropped?.length > 0 && html`<div id="import-outcome-dropped"><b>Dropped</b> ${dropped.join(', ')}</div>`}
  </div>`;
}

// AgentImportPreview is the agent half of the ⤒ import… dialog: the import
// preview for an agent PAF or legacy agent bundle, with where and how the
// agent lands. "Import (offline)" lands it without starting; "Import &
// start" also starts it.
// CarriedWorkflows says what else an agent or group file holds. Importing it
// stores none of these: each goes through the workflow review, on the same file.
export function CarriedWorkflows({ workflows, file }) {
  if (!workflows?.length) return null;
  const names = workflows.map((w) => w.root.split('@')[0]);
  return html`<div class="paf-box" id="paf-carried-workflows"><b>Also carries ${names.length === 1 ? 'a workflow' : `${names.length} workflows`}</b>
    <div>${names.join(', ')}. This import stores none of them.</div>
    ${dashboardState.snapshot.value?.processes_enabled && html`<${WorkflowImportButton} from=${file} id="" label="Review workflows…" className="tool" />`}</div>`;
}

export function AgentImportPreview({ preview, choices, setChoices, overrides, setOverrides, reveal }) {
  const snapshot = dashboardState.snapshot.value;
  const groups = (snapshot?.groups || []).map((group) => group.name);
  const agent = preview.agent || {};
  const set = (key) => (value) => setChoices({ ...choices, [key]: value });
  const rows = provenanceRows(agent);
  const unresolved = preview.unresolved || [];
  const warnings = (preview.confirm?.can_keep && preview.confirm.items?.length ? preview.confirm.warnings : preview.warnings) || [];
  return html`<div class="paf-preview" id="agent-import-preview">
    <div class="gi-head">Agent “${agent.name || '(unnamed)'}”${agent.role ? ` · ${agent.role}` : ''}</div>
    ${rows.length > 0 && html`<div class="paf-box"><b>Provenance</b>${rows.map(([key, value]) => html`<div key=${key} class="gi-row"><span class="gi-k">${key}</span><span class="gi-v">${value}</span></div>`)}</div>`}
    <label class="cron-create-row"><span class="cron-create-label">Name</span>
      <input id="agent-import-name" value=${choices.name} placeholder=${agent.name || ''} onInput=${(event) => set('name')(event.currentTarget.value)} /></label>
    <label class="cron-create-row"><span class="cron-create-label">Group</span>
      <select id="agent-import-group" value=${choices.group} onChange=${(event) => set('group')(event.currentTarget.value)}>
        <option value="">(pick a group)</option>
        ${groups.map((name) => html`<option key=${name} value=${name}>${name}</option>`)}
      </select></label>
    <label class="cron-create-row"><span class="cron-create-label">Working dir</span>
      <input id="agent-import-cwd" value=${choices.cwd} placeholder=${agent.paths?.cwd || 'absolute path on this machine'} disabled=${choices.keepPaths}
        onInput=${(event) => set('cwd')(event.currentTarget.value)} /></label>
    <${Check} id="agent-import-keep-paths" checked=${choices.keepPaths} onChange=${set('keepPaths')}>Keep the recorded paths</${Check}>
    ${choices.agent && html`<label class="cron-create-row"><span class="cron-create-label">Role</span>
      <input id="agent-import-role" value=${choices.role} onInput=${(event) => set('role')(event.currentTarget.value)} /></label>`}
    <div class="cron-create-row"><span class="cron-create-label">Land on</span>
      <${LandingSelects} idPrefix="agent-import" harness=${choices.harness} model=${choices.model}
        onChange=${(patch) => setChoices({ ...choices, ...patch })} sameLabel=${`${agent.harness || 'own harness'} (as exported)`} /></div>
    <${HistoryLanding} landing=${preview.history_landing} />
    ${preview.posture && html`<div class="paf-box" id="paf-landing-posture"><b>Sandbox</b> <${LandingPosture} posture=${preview.posture} /></div>`}
    ${historyCutNeedsConfirm(preview) && html`<${Check} id="agent-import-confirm-loss" checked=${!!choices.confirmHistoryLoss} onChange=${set('confirmHistoryLoss')}
      hint="The full history stays in the file and is saved beside the agent, which is told where. A model with a larger window keeps more.">Accept cutting about ${preview.history_landing.losses.dropped_percent}% of the conversation to fit the landing model</${Check}>`}
    ${(preview.findings || []).length > 0 && html`<${FindingsPanel} findings=${preview.findings} title="This node's scan" />`}
    ${unresolved.length > 0 && html`<div class="gi-warn" id="agent-import-unresolved">⚠ ${unresolved.length} recorded path${unresolved.length === 1 ? '' : 's'} need a value here: set a working dir or keep the recorded paths.</div>`}
    <${ImportConfirmList} confirm=${preview.confirm} overrides=${overrides} setOverrides=${setOverrides} reveal=${reveal} />
    <div class="paf-box"><b>Security posture</b><div>${preview.security}</div>
      ${warnings.map((warning) => html`<div key=${warning} class="gi-warn">⚠ ${warning}</div>`)}</div>
  </div>`;
}

// agentImportParams maps the preview choices to the import query. An
// applied import with start false lands the agent offline (start=false).
export function agentImportParams(choices, apply = false, start = true) {
  return {
    apply, group: choices.group, name: choices.name.trim(), harness: choices.harness, model: choices.model,
    cwd: choices.keepPaths ? '' : choices.cwd.trim(), keep_paths: choices.keepPaths,
    start: apply && !start ? 'false' : '',
    confirm_history_loss: apply && choices.confirmHistoryLoss ? 'true' : '',
    agent: choices.agent || '', role: choices.agent ? (choices.role || '').trim() : '',
  };
}

// historyCutNeedsConfirm says the landing cuts so much of the conversation
// (25% or more) that the import needs the importer's confirmation, as the
// route does (history_confirmation_required).
export function historyCutNeedsConfirm(preview) {
  return !!(preview?.history && preview.history_landing?.losses?.needs_summary);
}

// offlineImportBlocker says why an agent cannot land offline, or '' when it
// can: offline needs a conversation that lands (resume starts it). The route
// refuses the same case (no_start).
export function offlineImportBlocker(preview) {
  if (!preview) return '';
  if (!preview.history) return 'No conversation lands, so there is nothing to resume: import and start it instead';
  return '';
}

// memberCutsToConfirm lists the members whose landing cuts so much of their
// conversation that the group import needs the importer's confirmation, as
// the route does (history_confirmation_required).
export function memberCutsToConfirm(landings) {
  return (landings || []).filter((row) => row.rung === 'translated' && row.losses?.needs_summary);
}

// MemberLandings is the group preview's per-member landing table: each
// member's harness and model (defaulting to the all-members choice) and the
// rung its history takes, with the losses a translation would cause.
export function MemberLandings({ landings, landing, setLanding, confirmCut, setConfirmCut }) {
  if (!landings?.length) return null;
  const cuts = memberCutsToConfirm(landings);
  const member = (key) => landing.members?.[key] || { harness: '', model: '' };
  const setMember = (key, patch) => setLanding({ ...landing, members: { ...landing.members, [key]: { ...member(key), ...patch } } });
  return html`<div class="paf-members" id="group-import-landings">
    <div class="cron-create-row"><span class="cron-create-label">All members</span>
      <${LandingSelects} idPrefix="group-import-all" harness=${landing.harness} model=${landing.model}
        onChange=${(patch) => setLanding({ ...landing, ...patch })} sameLabel="keep own harness" /></div>
    <table class="paf-member-table"><thead><tr><th>Member</th><th>From</th><th>Land on</th><th>History</th><th>Sandbox</th></tr></thead><tbody>
      ${landings.map((row) => {
        // Keyed by conv id: always unique, and safe in a member=value binding.
        const key = row.conv_id;
        const badge = rungBadge(row.rung);
        const choice = member(key);
        return html`<tr key=${row.conv_id} data-member=${row.title || key}>
          <td>${row.title || row.conv_id.slice(0, 8)}</td>
          <td>${[row.source_harness, row.source_model].filter(Boolean).join(' · ')}</td>
          <td><${LandingSelects} idPrefix=${`group-import-member-${row.conv_id}`} harness=${choice.harness} model=${choice.model}
            onChange=${(patch) => setMember(key, patch)} sameLabel="(as above)" /></td>
          <td><span class=${`paf-rung ${badge.cls}`} title=${badge.title}>${badge.text}</span>
            ${row.losses && html`<div class="paf-hint">${lossSummary(row.losses)}</div>`}
            ${(row.warnings || []).map((warning) => html`<div key=${warning} class="gi-warn">⚠ ${warning}</div>`)}</td>
          <td><${LandingPosture} posture=${row.posture} /></td>
        </tr>`;
      })}
    </tbody></table>
    ${cuts.length > 0 && html`<${Check} id="group-import-confirm-loss" checked=${!!confirmCut} onChange=${setConfirmCut}
      hint="The full history stays in the file and is saved beside each agent, which is told where. A model with a larger window keeps more.">Accept cutting the conversation of ${cuts.map((row) => `${row.title || row.conv_id.slice(0, 8)} (about ${row.losses.dropped_percent}%)`).join(', ')} to fit the landing model</${Check}>`}
  </div>`;
}
