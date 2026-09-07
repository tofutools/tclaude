import {clone, freshID, lines} from './process-model.js';

const el = (tag, text) => { const e = document.createElement(tag); if (text !== undefined) e.textContent = text; return e; };
const button = (label, run) => { const e = el('button', label); e.type = 'button'; e.onclick = run; return e; };
const opt = (value, label = value) => ({value, label});
const draftOf = ({Definition: d, Revision: r}) => ({ID: d.ID, Name: d.Name, Kind: 'team', SchemaVersion: r.SchemaVersion, Source: r.Source, Parameters: clone(r.Parameters || []), Dependencies: clone(r.Dependencies || []), Team: clone(r.Team)});
const blank = () => ({ID: freshID('definition_'), Name: 'New team', Kind: 'team', SchemaVersion: 1, Source: 'Created in the team editor.', Parameters: [], Dependencies: [], Team: {Members: [], Waves: [], Briefings: [], WorkspacePolicy: 'shared', AdvisoryPhases: [], Automation: []}});

export async function openTeamEditor({api, result, onSaved}) {
  const [authority, profiles, rules] = await Promise.all([api('/v2/authority'), api('/v2/configuration-profiles'), api('/v2/automation/rules')]);
  const configurations = await Promise.all((profiles || []).map(p => api('/v2/configuration-profiles/' + encodeURIComponent(p.ID) + '?revision_id=' + encodeURIComponent(p.CurrentRevisionID))));
  const rhythms = await Promise.all((rules || []).filter(r => !r.Tombstoned).map(r => api('/v2/automation/rules/' + encodeURIComponent(r.ID))));
  return new TeamEditor({api, result, onSaved, roles: authority.Roles || [], configurations, rhythms});
}

class TeamEditor {
  constructor({api, result, onSaved, roles, configurations, rhythms}) {
    Object.assign(this, {api, onSaved, roles, configurations, rhythms});
    this.adopt(result ? draftOf(result) : blank(), result?.Definition.Revision || 0);
    this.tab = 'Members'; this.unapplied = false; this.busy = false;
    this.dialog = el('dialog'); this.dialog.id = 'team-editor'; this.dialog.setAttribute('aria-labelledby', 'team-editor-heading');
    const title = el('h2', 'Team template editor'); title.id = 'team-editor-heading';
    this.status = el('p'); this.status.id = 'team-editor-status'; this.status.role = 'status';
    this.error = el('div'); this.error.id = 'team-editor-error'; this.error.role = 'alert';
    this.toolbar = el('div'); this.toolbar.className = 'process-toolbar';
    this.name = el('input'); this.name.value = this.draft.Name; this.name.setAttribute('aria-label', 'Team template name');
    this.name.onchange = () => { if (this.unapplied) { this.name.value = this.draft.Name; this.fail(new Error('Apply field changes first.')); return; } this.change(d => { d.Name = this.name.value; }, false); };
    this.undo = button('Undo', () => this.history(false)); this.redo = button('Redo', () => this.history(true));
    this.toolbar.append(this.name, this.undo, this.redo, button('Validate team', () => this.save(false)), button('Save team revision', () => this.save(true)), button('Export team', () => this.export()), button('Import team copy', () => this.import()), button('Close team editor', () => this.close()));
    const tabs = el('nav'); tabs.className = 'process-toolbar'; tabs.setAttribute('aria-label', 'Team authoring sections');
    for (const name of ['Members', 'Waves', 'Briefings', 'Workspace and phases', 'Parameters', 'Rhythms', 'Source']) tabs.append(button(name, () => { if (this.discard()) { this.tab = name; this.render(); } }));
    this.content = el('div'); this.content.id = 'team-editor-content';
    this.dialog.append(title, this.toolbar, tabs, this.status, this.error, this.content); document.body.append(this.dialog); this.dialog.showModal();
    this.dialog.addEventListener('cancel', e => { e.preventDefault(); this.close(); });
    this.beforeUnload = e => { if (this.dirty() || this.unapplied) { e.preventDefault(); e.returnValue = ''; } };
    window.addEventListener('beforeunload', this.beforeUnload); this.render();
  }
  adopt(draft, revision) {
    this.draft = clone(draft); const t = this.draft.Team;
    for (const key of ['Members', 'Waves', 'Briefings', 'AdvisoryPhases', 'Automation']) t[key] ||= [];
    this.revision = revision; this.saved = JSON.stringify(this.draft); this.past = []; this.future = []; this.pending = null;
  }
  dirty() { return (this.name && this.name.value !== this.draft.Name) || !this.revision || JSON.stringify(this.draft) !== this.saved; }
  discard() { if (this.busy || (this.unapplied && !confirm('Discard unapplied field changes?'))) return false; this.unapplied = false; return true; }
  change(edit, redraw = true) {
    if (this.busy) return;
    const d = clone(this.draft); edit(d);
    if (JSON.stringify(d) !== JSON.stringify(this.draft)) { this.past.push(this.draft); if (this.past.length > 100) this.past.shift(); this.future = []; this.draft = d; }
    if (redraw) this.render(); else this.renderHeader();
  }
  history(redo) { if (!this.discard()) return; const from = redo ? this.future : this.past, to = redo ? this.past : this.future; if (from.length) { to.push(this.draft); this.draft = from.pop(); this.render(); } }
  fail(error) { this.error.replaceChildren(el('p', error.message || String(error))); }
  lock(value) { this.busy = value; this.dialog.querySelectorAll('button,input,textarea,select').forEach(e => { e.disabled = value; }); if (!value) { this.undo.disabled = !this.past.length; this.redo.disabled = !this.future.length; } }
  renderHeader() {
    this.name.value = this.draft.Name; this.status.textContent = `${this.revision ? 'Revision ' + this.revision : 'New team'} · ${this.dirty() ? 'unsaved changes' : 'saved'}`;
    this.undo.disabled = !this.past.length; this.redo.disabled = !this.future.length;
  }
  render() {
    this.renderHeader(); this.error.replaceChildren(); this.content.replaceChildren();
    const t = this.draft.Team;
    if (this.tab === 'Members') {
      this.content.append(button('Add member', () => this.member()));
      for (const m of t.Members) this.card(m.Name || m.Key, `${m.Key} · ${m.Desired.Harness || 'Choose harness'} / ${m.Desired.Model || 'Choose model'}${m.Owner ? ' · owner' : ''}${m.Required ? ' · required' : ''}`, () => this.member(m), () => this.removeMember(m));
    } else if (this.tab === 'Waves') {
      this.content.append(el('p', 'Each member belongs to one wave. Dependencies control launch order; readiness and briefing gates wait for their evidence.'), button('Add wave', () => this.wave()));
      for (const w of t.Waves) this.card(w.ID, `Members: ${w.MemberKeys.join(', ')} · after: ${(w.DependsOn || []).join(', ') || 'none'}`, () => this.wave(w), () => this.change(d => { d.Team.Waves = d.Team.Waves.filter(x => x.ID !== w.ID); for (const x of d.Team.Waves) x.DependsOn = (x.DependsOn || []).filter(id => id !== w.ID); }));
    } else if (this.tab === 'Briefings') {
      this.content.append(button('Add briefing', () => this.briefing()));
      for (const b of t.Briefings) this.card(b.ID, `${b.Timing} · ${(b.MemberKeys || []).join(', ') || 'all members'}${b.Required ? ' · required' : ''}\n${b.Body}`, () => this.briefing(b), () => this.change(d => { d.Team.Briefings = d.Team.Briefings.filter(x => x.ID !== b.ID); for (const m of d.Team.Members) m.BriefingIDs = (m.BriefingIDs || []).filter(id => id !== b.ID); }));
    } else if (this.tab === 'Workspace and phases') this.settings();
    else if (this.tab === 'Parameters') {
      this.content.append(button('Add parameter', () => this.parameter()));
      for (const p of this.draft.Parameters) this.card(p.Name, `${p.Type}${p.Required ? ' · required' : ''}`, () => this.parameter(p), () => this.change(d => { d.Parameters = d.Parameters.filter(x => x.Name !== p.Name); }));
    } else if (this.tab === 'Rhythms') this.rhythmForm();
    else this.form('Preserved authoring source', [{key: 'source', label: 'Source', text: true, value: this.draft.Source, required: true}], f => this.change(d => { d.Source = f.source; }));
  }
  card(title, text, edit, remove) {
    const card = el('article'); card.className = 'card'; card.append(el('h3', title), el('p', text), button('Edit ' + title, edit), button('Remove ' + title, () => { if (confirm('Remove ' + title + ' and its references from this draft?')) remove(); })); this.content.append(card);
  }
  form(title, fields, apply) {
    this.content.replaceChildren(el('h3', title)); const form = el('form');
    for (const f of fields) {
      const label = el('label', f.label); let input;
      if (f.options) {
        input = el('select'); input.multiple = !!f.multiple;
        const selected = f.multiple ? f.value || [] : [String(f.value ?? '')], options = [...f.options];
        for (const value of selected) if (value && !options.some(o => String(o.value) === String(value))) options.push(opt(value, 'Retained: ' + value));
        for (const o of options) { const e = el('option', o.label); e.value = o.value; e.selected = selected.includes(String(o.value)); input.append(e); }
      } else { input = el(f.text ? 'textarea' : 'input'); if (!f.text) input.type = f.type || 'text'; input.value = f.value ?? ''; }
      if (f.type === 'checkbox') input.checked = !!f.value;
      input.name = f.key; input.setAttribute('aria-label', f.label); input.required = !!f.required;
      label.append(input); form.append(label);
      if (f.key === 'cwd') label.append(button('Browse directories', async () => { const {pickDirectory} = await import('./directory-picker.js'); if (!input.isConnected || !this.dialog.open) return; const selected = await pickDirectory({api: this.api, initial: input.value}); if (selected !== null && input.isConnected && this.dialog.open) { input.value = selected; input.dispatchEvent(new Event('input', {bubbles: true})); } }));
    }
    form.oninput = () => { this.unapplied = true; };
    const submit = el('button', 'Apply changes'); submit.type = 'submit'; form.append(submit, button('Back to ' + this.tab, () => { if (this.discard()) this.render(); }));
    form.onsubmit = e => {
      e.preventDefault(); if (this.busy) return;
      try { const data = new FormData(form), values = Object.fromEntries(data); for (const f of fields) { if (f.multiple) values[f.key] = data.getAll(f.key); if (f.type === 'checkbox') values[f.key] = form.elements[f.key].checked; } this.unapplied = false; apply(values); }
      catch (error) { this.unapplied = true; this.fail(error); }
    };
    this.content.append(form); return form;
  }
  member(original) {
    const m = original || {Key: '', Name: '', Desired: {}, Roles: [], Required: true, Owner: false, BriefingIDs: []}, desired = m.Desired;
    const fields = [
      {key: 'key', label: 'Stable member key', value: m.Key, required: true}, {key: 'name', label: 'Member name', value: m.Name, required: true},
      {key: 'harness', label: 'Harness', options: [opt('', 'Choose harness'), ...['claude', 'codex', 'opencode', 'copilot'].map(v => opt(v))], value: desired.Harness, required: true},
      {key: 'effort', label: 'Requested native effort / variant (optional)', value: desired.Effort || ''},
      {key: 'model', label: 'Model', value: desired.Model, required: true}, {key: 'cwd', label: 'Configuration working directory', value: desired.WorkingDirectory, required: true},
      {key: 'approval', label: 'Approval', options: ['supervised', 'automatic'].map(v => opt(v)), value: desired.Approval || 'supervised'},
      {key: 'sandbox', label: 'Confinement', options: ['read_only', 'workspace_write', 'unconfined'].map(v => opt(v)), value: desired.Sandbox || 'workspace_write'},
      {key: 'roles', label: 'Roles', multiple: true, options: this.roles.map(r => opt(r.ID, r.Name || r.ID)), value: m.Roles || []},
      {key: 'owner', label: 'Group owner', type: 'checkbox', value: m.Owner}, {key: 'required', label: 'Required member', type: 'checkbox', value: m.Required},
      {key: 'briefs', label: 'Additional briefings', multiple: true, options: this.draft.Team.Briefings.map(b => opt(b.ID)), value: [...new Set([...(m.BriefingIDs || []), ...this.draft.Team.Briefings.filter(b => b.MemberKeys?.includes(m.Key)).map(b => b.ID)])]}
    ];
    const form = this.form('Member', fields, f => {
      if (this.draft.Team.Members.some(x => x.Key === f.key && x.Key !== original?.Key)) throw new Error('Member keys must be unique.');
      if (f.owner && this.draft.Team.Members.some(x => x.Owner && x.Key !== original?.Key)) throw new Error('Choose only one group owner.');
      if (f.effort && !/^[a-z0-9][a-z0-9_-]{0,63}$/.test(f.effort)) throw new Error('Requested effort must be a lowercase native level or variant, at most 64 characters.');
      const member = {...m, Key: f.key, Name: f.name, Desired: {...desired, Harness: f.harness, Model: f.model, Effort: f.effort, WorkingDirectory: f.cwd, Approval: f.approval, Sandbox: f.sandbox}, Roles: f.roles, Owner: f.owner, Required: f.required, BriefingIDs: f.briefs};
      this.change(d => {
        const i = d.Team.Members.findIndex(x => x.Key === original?.Key); if (i < 0) d.Team.Members.push(member); else d.Team.Members[i] = member;
        if (original && original.Key !== f.key) { for (const w of d.Team.Waves) w.MemberKeys = w.MemberKeys.map(k => k === original.Key ? f.key : k); for (const b of d.Team.Briefings) b.MemberKeys = (b.MemberKeys || []).map(k => k === original.Key ? f.key : k); }
        for (const b of d.Team.Briefings) {
          b.MemberKeys = (b.MemberKeys || []).filter(key => key !== f.key);
          if (f.briefs.includes(b.ID)) b.MemberKeys.push(f.key);
        }
        if (!d.Team.Waves.length) d.Team.Waves.push({ID: 'initial', MemberKeys: [f.key], DependsOn: [], RequiredReady: true, RequiredBriefs: true});
        else if (!original) d.Team.Waves[0].MemberKeys.push(f.key);
      });
    });
    const select = el('select'); select.setAttribute('aria-label', 'Copy saved configuration');
    const placeholder = el('option', 'Copy settings from a saved configuration'); placeholder.value = ''; select.append(placeholder);
    this.configurations.forEach((c, i) => { const o = el('option', `${c.Profile.Name} · ${c.Revision.Ref.RevisionID}`); o.value = String(i); select.append(o); });
    select.onchange = () => {
      if (select.value === '') return;
      const d = this.configurations[Number(select.value)].Revision.Desired;
      for (const [key, property] of Object.entries({harness: 'Harness', model: 'Model', effort: 'Effort', cwd: 'WorkingDirectory', approval: 'Approval', sandbox: 'Sandbox'})) form.elements[key].value = d[property] || '';
      this.unapplied = true;
    };
    this.content.prepend(select, el('p', 'Settings are copied into this immutable team revision. Deployment binds each member to the explicitly selected workspace; it does not follow later profile edits.'));
  }
  removeMember(member) {
    this.change(d => {
      d.Team.Members = d.Team.Members.filter(m => m.Key !== member.Key);
      const empty = new Set(); for (const w of d.Team.Waves) { w.MemberKeys = w.MemberKeys.filter(k => k !== member.Key); if (!w.MemberKeys.length) empty.add(w.ID); }
      d.Team.Waves = d.Team.Waves.filter(w => !empty.has(w.ID)); for (const w of d.Team.Waves) w.DependsOn = (w.DependsOn || []).filter(id => !empty.has(id));
      const removedBriefs = new Set(d.Team.Briefings.filter(b => b.MemberKeys?.length === 1 && b.MemberKeys[0] === member.Key).map(b => b.ID));
      d.Team.Briefings = d.Team.Briefings.filter(b => !removedBriefs.has(b.ID));
      for (const b of d.Team.Briefings) b.MemberKeys = (b.MemberKeys || []).filter(k => k !== member.Key);
      for (const m of d.Team.Members) m.BriefingIDs = (m.BriefingIDs || []).filter(id => !removedBriefs.has(id));
    });
  }
  wave(original) {
    const w = original || {ID: '', MemberKeys: [], DependsOn: [], RequiredReady: true, RequiredBriefs: true};
    this.form('Wave', [{key: 'id', label: 'Wave key', value: w.ID, required: true},
      {key: 'members', label: 'Wave members', multiple: true, options: this.draft.Team.Members.map(m => opt(m.Key, m.Name)), value: w.MemberKeys, required: true},
      {key: 'after', label: 'Launch after waves', multiple: true, options: this.draft.Team.Waves.filter(x => x.ID !== original?.ID).map(x => opt(x.ID)), value: w.DependsOn || []},
      {key: 'ready', label: 'Wait for member readiness', type: 'checkbox', value: w.RequiredReady}, {key: 'briefs', label: 'Wait for required briefings', type: 'checkbox', value: w.RequiredBriefs}], f => {
        if (this.draft.Team.Waves.some(x => x.ID === f.id && x.ID !== original?.ID)) throw new Error('Wave keys must be unique.');
        this.change(d => {
          const wave = {...w, ID: f.id, MemberKeys: f.members, DependsOn: f.after, RequiredReady: f.ready, RequiredBriefs: f.briefs};
          const i = d.Team.Waves.findIndex(x => x.ID === original?.ID); if (i < 0) d.Team.Waves.push(wave); else d.Team.Waves[i] = wave;
          for (const x of d.Team.Waves) if (x !== wave) { x.MemberKeys = x.MemberKeys.filter(k => !f.members.includes(k)); if (original) x.DependsOn = (x.DependsOn || []).map(id => id === original.ID ? f.id : id); }
        });
      });
  }
  briefing(original) {
    const b = original || {ID: '', Body: '', Timing: 'before_first_work', Required: true, MemberKeys: this.draft.Team.Members.map(m => m.Key)};
    this.form('Briefing', [{key: 'id', label: 'Briefing key', value: b.ID, required: true}, {key: 'body', label: 'Briefing text', text: true, value: b.Body, required: true},
      {key: 'timing', label: 'Deliver briefing', options: [opt('before_first_work', 'Before first work'), opt('after_ready', 'After ready')], value: b.Timing},
      {key: 'required', label: 'Required briefing', type: 'checkbox', value: b.Required},
      {key: 'members', label: 'Briefing recipients', required: true, multiple: true, options: this.draft.Team.Members.map(m => opt(m.Key, m.Name)), value: [...new Set([...(b.MemberKeys || []), ...this.draft.Team.Members.filter(m => m.BriefingIDs?.includes(b.ID)).map(m => m.Key)])]}], f => {
        if (this.draft.Team.Briefings.some(x => x.ID === f.id && x.ID !== original?.ID)) throw new Error('Briefing keys must be unique.');
        this.change(d => { const brief = {...b, ID: f.id, Body: f.body, Timing: f.timing, Required: f.required, MemberKeys: f.members}; const i = d.Team.Briefings.findIndex(x => x.ID === original?.ID); if (i < 0) d.Team.Briefings.push(brief); else d.Team.Briefings[i] = brief; for (const m of d.Team.Members) { m.BriefingIDs = (m.BriefingIDs || []).filter(id => id !== original?.ID && id !== f.id); if (f.members.includes(m.Key)) m.BriefingIDs.push(f.id); } });
      });
  }
  settings() {
    this.form('Workspace and advisory phases', [{key: 'workspace', label: 'Workspace policy', options: [opt('shared', 'Shared workspace'), opt('per_member', 'Separate member workspaces')], value: this.draft.Team.WorkspacePolicy}, {key: 'phases', label: 'Advisory phases (one per line)', text: true, value: this.draft.Team.AdvisoryPhases.join('\n')}], f => this.change(d => { d.Team.WorkspacePolicy = f.workspace; d.Team.AdvisoryPhases = lines(f.phases); }));
  }
  parameter(original) {
    const p = original || {};
    this.form('Parameter', [{key: 'name', label: 'Parameter name', value: p.Name, required: true}, {key: 'type', label: 'Parameter type', options: ['string', 'number', 'boolean', 'object', 'array'].map(v => opt(v)), value: p.Type || 'string'}, {key: 'required', label: 'Required parameter', type: 'checkbox', value: p.Required}, {key: 'description', label: 'Description', value: p.Description}, {key: 'default', label: 'Default value (JSON, optional)', value: p.Default === undefined || p.Default === null ? '' : JSON.stringify(p.Default)}], f => {
      if (this.draft.Parameters.some(x => x.Name === f.name && x.Name !== original?.Name)) throw new Error('Parameter names must be unique.');
      const parameter = {Name: f.name, Type: f.type, Required: f.required, Description: f.description}; if (f.default.trim()) parameter.Default = JSON.parse(f.default);
      this.change(d => { const i = d.Parameters.findIndex(x => x.Name === original?.Name); if (i < 0) d.Parameters.push(parameter); else d.Parameters[i] = parameter; });
    });
  }
  rhythmForm() {
    const refs = this.rhythms.map(r => ({ref: {RuleID: r.Rule.ID, RevisionID: r.Revision.ID, ContentHash: r.Revision.ContentHash}, label: `${r.Rule.Name} · revision ${r.Revision.Number}`}));
    for (const ref of this.draft.Team.Automation) if (!refs.some(x => x.ref.RevisionID === ref.RevisionID)) refs.push({ref, label: 'Previously pinned rule ' + ref.RuleID});
    this.form('Pinned team rhythms', [{key: 'rules', label: 'Automation revisions', multiple: true, options: refs.map(r => opt(r.ref.RevisionID, r.label)), value: this.draft.Team.Automation.map(r => r.RevisionID)}], f => this.change(d => { d.Team.Automation = f.rules.map(id => clone(refs.find(r => r.ref.RevisionID === id).ref)); }));
  }
  async save(write) {
    if (this.busy) return;
    if (this.unapplied) { this.fail(new Error('Apply field changes before saving or validating.')); return; }
    if (write && !this.dirty()) { this.status.textContent = 'Revision ' + this.revision + ' · saved'; return; }
    this.lock(true);
    try {
      const validated = await this.api('/v2/definitions/validate', {draft: clone(this.draft)});
      if (!write) { this.status.textContent = 'Validation passed'; return; }
      const fingerprint = JSON.stringify(this.draft);
      if (this.pending?.fingerprint !== fingerprint) this.pending = {fingerprint, request: freshID('request_'), revision: freshID('revision_')};
      const result = await this.api('/v2/definitions', {request_id: this.pending.request, expected_revision: this.revision, draft: {...clone(this.draft), RevisionID: this.pending.revision}});
      if (result.Revision.ContentHash !== validated.Revision.ContentHash) { const e = new Error('A newer revision is current. Local edits are retained.'); e.code = 'conflict'; throw e; }
      this.adopt(draftOf(result), result.Definition.Revision); this.render(); await this.onSaved();
    } catch (error) {
      this.fail(error);
      if (error.code === 'conflict') this.error.append(button('Reload saved team (discard local edits)', async () => {
        if (!this.discard() || !confirm('Discard local edits and load the saved team?')) return;
        this.lock(true); try { const r = await this.api('/v2/definitions/' + encodeURIComponent(this.draft.ID)); this.adopt(draftOf(r), r.Definition.Revision); this.render(); } catch (e) { this.fail(e); } finally { this.lock(false); }
      }));
    } finally { this.lock(false); }
  }
  export() { const url = URL.createObjectURL(new Blob([JSON.stringify({format: 'tclaude-team-v2', draft: this.draft}, null, 2)], {type: 'application/json'})); const link = el('a'); link.href = url; link.download = 'team.json'; link.click(); setTimeout(() => URL.revokeObjectURL(url), 1000); }
  import() {
    if (!this.discard()) return;
    const input = el('input'); input.type = 'file'; input.accept = '.json,application/json'; input.onchange = async () => {
      try { const file = input.files[0]; if (!file) return; if (file.size > 512 * 1024) throw new Error('Team import exceeds 512 KiB.'); const value = JSON.parse(await file.text()); if (value.format !== 'tclaude-team-v2' || value.draft?.Kind !== 'team') throw new Error('Choose an exported v2 team.'); if (this.dirty() && !confirm('Replace this draft with an imported copy?')) return; const draft = value.draft; draft.ID = freshID('definition_'); delete draft.RevisionID; this.lock(true); const r = await this.api('/v2/definitions/validate', {draft}); this.adopt(draftOf(r), 0); this.render(); } catch (e) { this.fail(e); } finally { this.lock(false); }
    }; input.click();
  }
  close() { if (this.busy || ((this.dirty() || this.unapplied) && !confirm('Discard unsaved team changes?'))) return; window.removeEventListener('beforeunload', this.beforeUnload); this.dialog.close(); this.dialog.remove(); }
}
