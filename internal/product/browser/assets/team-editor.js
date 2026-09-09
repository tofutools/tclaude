import {wireDefinitionDraft} from './process-durations.js';
const {stringifyExact,parameterDefaultText}=globalThis.ExactJSONTools;
import {clone, freshID, lines} from './process-model.js';

const el = (tag, text) => { const e = document.createElement(tag); if (text !== undefined) e.textContent = text; return e; };
const button = (label, run) => { const e = el('button', label); e.type = 'button'; e.onclick = run; return e; };
const opt = (value, label = value) => ({value, label});
const draftOf = ({Definition: d, Revision: r}) => ({ID: d.ID, Name: d.Name, Kind: 'team', SchemaVersion: r.SchemaVersion, Source: r.Source, Parameters: clone(r.Parameters || []), Dependencies: clone(r.Dependencies || []), Team: clone(r.Team)});
const blank = () => ({ID: freshID('definition_'), Name: 'New team', Kind: 'team', SchemaVersion: 1, Source: 'Created in the team editor.', Parameters: [], Dependencies: [], Team: {Members: [], Waves: [], Briefings: [], WorkspacePolicy: 'shared', AdvisoryPhases: [], Automation: []}});

export function teamDraftFromGroup(group, agents) {
  const draft = blank(), byID = new Map(agents.map(agent => [agent.ID, agent]));
  const members = (group.Members || []).map(id => {
    const agent = byID.get(id);
    if (!agent) throw new Error('Reload the group before capturing its member settings.');
    return agent;
  }).filter(agent => agent.Lifecycle === 'active');
  draft.Name = group.Name + ' team';
  draft.Source = `Captured displayed group ${group.ID} at revision ${group.Revision}. Member settings are independent copies; review before saving.\n` +
    [group.Details?.Description, group.Details?.Mission].filter(Boolean).join('\n');
  draft.Team.Members = members.map((agent, index) => ({Key: 'member_' + (index + 1), Name: agent.Name, Labels:clone(agent.Labels?.Groups?.[group.ID]||{Role:agent.Labels?.Role||'',Description:agent.Labels?.Description||''}), Desired: clone(agent.Desired), Roles: [], Owner: false, Required: true, BriefingIDs: []}));
  draft.Team.Waves = members.length ? [{ID: 'initial', MemberKeys: draft.Team.Members.map(member => member.Key), DependsOn: [], RequiredReady: true, RequiredBriefs: true, WaitForIdle: true, MaxWaitSeconds: 0}] : [];
  draft.Team.WorkspacePolicy = 'per_member';
  return draft;
}

export async function openTeamEditor({api, result, draft, onSaved, canOpen = () => true}) {
  const [authority, profiles, rules] = await Promise.all([api('/v2/authority'), api('/v2/configuration-profiles'), api('/v2/automation/rules')]);
  const configurations = await Promise.all((profiles || []).filter(p => !p.Archived).map(p => api('/v2/configuration-profiles/' + encodeURIComponent(p.ID) + '?revision_id=' + encodeURIComponent(p.CurrentRevisionID))));
  const rhythms = await Promise.all((rules || []).filter(r => !r.Tombstoned).map(r => api('/v2/automation/rules/' + encodeURIComponent(r.ID))));
  if (!canOpen()) return;
  return new TeamEditor({api, result, draft, onSaved, roles: authority.Roles || [], configurations, rhythms});
}

class TeamEditor {
  constructor({api, result, draft, onSaved, roles, configurations, rhythms}) {
    Object.assign(this, {api, onSaved, roles, configurations, rhythms});
    this.adopt(result ? draftOf(result) : draft || blank(), result?.Definition.Revision || 0);
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
    const captureNotice = el('p');
    if (draft) captureNotice.textContent = 'Captured active direct members in displayed order, with independent desired settings. Retired members, child groups, owner and role authority, running work, messages and rhythms are not copied. Description and mission are retained as source notes, not sent as briefings. Review owner, roles, workspaces and briefings here before saving. Nothing starts when you save.';
    this.dialog.append(title, captureNotice, this.toolbar, tabs, this.status, this.error, this.content); document.body.append(this.dialog); this.dialog.showModal();
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
  lock(value) { this.busy = value; this.dialog.querySelectorAll('button,input,textarea,select').forEach(e => { if(value){e.dataset.wasDisabled=String(e.disabled);e.disabled=true;}else{e.disabled=e.dataset.wasDisabled==='true';delete e.dataset.wasDisabled;} }); if (!value) { this.undo.disabled = !this.past.length; this.redo.disabled = !this.future.length; } }
  renderHeader() {
    this.name.value = this.draft.Name; this.status.textContent = `${this.revision ? 'Revision ' + this.revision : 'New team'} · ${this.dirty() ? 'unsaved changes' : 'saved'}`;
    this.undo.disabled = !this.past.length; this.redo.disabled = !this.future.length;
  }
  render() {
    this.renderHeader(); this.error.replaceChildren(); this.content.replaceChildren();
    const t = this.draft.Team;
    if (this.tab === 'Members') {
      this.content.append(button('Add member', () => this.member()));
      for (const m of t.Members) this.card(m.Name || m.Key, `${m.Key} · ${this.memberConfigurationSummary(m)}${m.Owner ? ' · owner' : ''}${m.Required ? ' · required' : ''}`, () => this.member(m), () => this.removeMember(m));
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
  memberConfigurationSummary(member) {
    if (member.ProfileID) {
      const selected = this.configurations.find(c => c.Profile.ID === member.ProfileID);
      if (!selected) return 'Unavailable saved configuration: ' + member.ProfileID;
      const base = selected.Revision.Desired, overrides = member.Overrides || {};
      const harness = overrides.Harness ?? base.Harness;
      const model = overrides.Model ?? (harness === base.Harness ? base.Model : '');
      return `${ProfilePresentation.label(selected.Profile)} · ${harness} / ${model || 'Default model'}`;
    }
    return `${member.Desired.Harness || 'Choose harness'} / ${member.Desired.Model || 'Choose model'}`;
  }
  card(title, text, edit, remove) {
    const card = el('article'); card.className = 'card'; card.append(el('h3', title), el('p', text), button('Edit ' + title, edit), button('Remove ' + title, () => { if (confirm('Remove ' + title + ' and its references from this draft?')) remove(); })); this.content.append(card);
  }
  form(title, fields, apply, resolve) {
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
    form.onsubmit = async e => {
      e.preventDefault(); if (this.busy) return;
      try { const data = new FormData(form), values = Object.fromEntries(data); for (const f of fields) { if (f.multiple) values[f.key] = data.getAll(f.key); if (f.type === 'checkbox') values[f.key] = form.elements[f.key].checked; } if(resolve){this.lock(true);await resolve(values);this.lock(false);} this.unapplied = false; apply(values); }
      catch (error) { this.unapplied = true; this.fail(error); } finally { if(this.busy)this.lock(false); }
    };
    this.content.append(form); attachLaunchSupportPreview({host:form,api:this.api}); return form;
  }
  member(original) {
    const m = original || {Key: '', Name: '', Desired: {}, Roles: [], Required: true, Owner: false, BriefingIDs: []}, desired = m.Desired;
    let environment, sandbox;
    const overrideProperties = {harness:"Harness",model:"Model",effort:"Effort",tool_governance:"ToolGovernance",fast_mode:"FastMode",approval:"Approval",sandbox:"Sandbox"};
    const fields = [
      {key: 'key', label: 'Stable member key', value: m.Key, required: true}, {key: 'name', label: 'Member name', value: m.Name, required: true},
      {key:'role_label',label:'Display role',value:m.Labels?.Role||''},
      {key:'description',label:'Description',text:true,value:m.Labels?.Description||''},
      {key: 'profile', label: 'Saved configuration', options: [opt('', 'Custom settings'), ...this.configurations.map(c => opt(c.Profile.ID, ProfilePresentation.label(c.Profile)))], value: m.ProfileID || ''},
      {key: 'harness', label: 'Harness', options: [opt('', 'Choose harness'), ...['claude', 'codex', 'opencode', 'copilot'].map(v => opt(v))], value: desired.Harness, required: true},
      {key:'fast_mode',label:'Codex fast mode',options:launchFastModeChoices().map(v=>opt(v.value,v.label)),value:desired.FastMode||'',required:false},
      {key:'tool_governance',label:'OpenCode tool governance',options:launchToolGovernanceChoices().map(v=>opt(v.value,v.label)),value:desired.ToolGovernance||'',required:false},
      {key: 'effort', label: 'Requested native effort / variant (optional)', value: desired.Effort || ''},
      {key: 'model', label: 'Model', value: desired.Model, required: true}, {key: 'cwd', label: 'Configuration working directory (optional; deployment uses its selected workspace)', value: desired.WorkingDirectory, required: false},
      {key: 'approval', label: 'Approval', options: launchApprovalChoices().map(v => opt(v)), value: desired.Approval || 'supervised'},
      {key: 'sandbox', label: 'Confinement', options: ['read_only', 'workspace_write', 'unconfined'].map(v => opt(v)), value: desired.Sandbox || 'workspace_write'},
      {key: 'roles', label: 'Roles', multiple: true, options: this.roles.map(r => opt(r.ID, r.Name || r.ID)), value: m.Roles || []},
      {key: 'owner', label: 'Group owner', type: 'checkbox', value: m.Owner}, {key: 'required', label: 'Required member', type: 'checkbox', value: m.Required},
      {key: 'briefs', label: 'Additional briefings', multiple: true, options: this.draft.Team.Briefings.map(b => opt(b.ID)), value: [...new Set([...(m.BriefingIDs || []), ...this.draft.Team.Briefings.filter(b => b.MemberKeys?.includes(m.Key)).map(b => b.ID)])]}
    ];
    for (const [key, property] of Object.entries(overrideProperties)) {
      const index = fields.findIndex(field => field.key === key);
      fields.splice(index,0,{key:'override_'+key,label:'Override profile '+key,type:'checkbox',value:m.Overrides?.[property] !== undefined});
    }
    const form = this.form('Member', fields, f => {
      if (this.draft.Team.Members.some(x => x.Key === f.key && x.Key !== original?.Key)) throw new Error('Member keys must be unique.');
      if (f.effort && !/^[a-z0-9][a-z0-9_-]{0,63}$/.test(f.effort)) throw new Error('Requested effort must be a lowercase native level or variant, at most 64 characters.');
      const overrides = {};
      if(f.profile)for(const [key,property] of Object.entries(overrideProperties))if(f['override_'+key])overrides[property]=f[key];
      const member = {...m, Overrides:Object.keys(overrides).length ? overrides : undefined, Key: f.key, Name: f.name, Labels:{Role:f.role_label,Description:f.description}, ProfileID: f.profile || undefined, Desired: f.profile ? {} : {...desired, Harness: f.harness, Model: f.model, Effort: f.effort, ToolGovernance:f.tool_governance||undefined,FastMode:f.fast_mode||undefined, WorkingDirectory: f.cwd, Approval: f.approval, Sandbox: f.sandbox, HostSandbox: f.resolvedSandbox||undefined, Environment: environment.read()}, Roles: f.roles, Owner: f.owner, Required: f.required, BriefingIDs: f.briefs};
      this.change(d => {
        const i = d.Team.Members.findIndex(x => x.Key === original?.Key); if (i < 0) d.Team.Members.push(member); else d.Team.Members[i] = member;
        if (original && original.Key !== f.key) { for (const w of d.Team.Waves) w.MemberKeys = w.MemberKeys.map(k => k === original.Key ? f.key : k); for (const b of d.Team.Briefings) b.MemberKeys = (b.MemberKeys || []).map(k => k === original.Key ? f.key : k); }
        for (const b of d.Team.Briefings) {
          b.MemberKeys = (b.MemberKeys || []).filter(key => key !== f.key);
          if (f.briefs.includes(b.ID)) b.MemberKeys.push(f.key);
        }
        if (!d.Team.Waves.length) d.Team.Waves.push({ID: 'initial', MemberKeys: [f.key], DependsOn: [], RequiredReady: true, RequiredBriefs: true, WaitForIdle: true, MaxWaitSeconds: 0});
        else if (!original) d.Team.Waves[0].MemberKeys.push(f.key);
      });
    },async f=>{if(!f.profile)f.resolvedSandbox=await sandbox.read(f.host_sandbox)});
    const environmentField = el('fieldset'); environmentField.setAttribute('aria-label', 'Member launch environment');
    const showEnvironment = values => {
      environment = new LaunchEnvironment(values || {});
      environmentField.replaceChildren(el('legend', 'Member launch environment — literal values'), environment.host);
    };
    showEnvironment(desired.Environment);
    environmentField.addEventListener('click', event => { if (event.target.closest('button')) this.unapplied = true; });
    form.insertBefore(environmentField, form.querySelector('button[type=submit]'));
    const sandboxField=el('fieldset');sandboxField.setAttribute('aria-label','Member host sandbox');
    const showSandbox=value=>{sandbox=new SandboxSelectionControl(this.api,value);sandbox.host.name='host_sandbox';sandbox.host.setAttribute('aria-label','Member host sandbox');sandboxField.replaceChildren(el('legend','Member host sandbox'),sandbox.host,sandbox.status)};
    showSandbox(desired.HostSandbox);form.insertBefore(sandboxField,form.querySelector('button[type=submit]'));
    const select = el('select'); select.setAttribute('aria-label', 'Copy saved configuration');
    const placeholder = el('option', 'Copy settings from a saved configuration'); placeholder.value = ''; select.append(placeholder);
    this.configurations.forEach((c, i) => { const o = el('option', `${ProfilePresentation.label(c.Profile)} · ${c.Revision.Ref.RevisionID}`); o.value = String(i); select.append(o); });
    select.onchange = () => {
      if (select.value === '') return;
      const d = this.configurations[Number(select.value)].Revision.Desired;
      for (const [key, property] of Object.entries({harness: 'Harness', model: 'Model', effort: 'Effort',tool_governance:'ToolGovernance',fast_mode:'FastMode', cwd: 'WorkingDirectory', approval: 'Approval', sandbox: 'Sandbox'})) form.elements[key].value = d[property] || '';
      showEnvironment(d.Environment);
      showSandbox(d.HostSandbox);
      form.elements.harness.dispatchEvent(new Event('change', {bubbles: true}));
      this.unapplied = true;
    };
    const syncFast=attachFastModeControl(form,()=>!form.elements.profile.value||form.elements.override_fast_mode.checked);
    const syncTools=attachToolGovernanceControl(form,()=>!form.elements.profile.value||form.elements.override_tool_governance.checked);
    let updatingProfile=false;
    const updateProfile = (initialize=false) => {
      if(updatingProfile)return;updatingProfile=true;
      const values = {};
      for(const [key,property] of Object.entries(overrideProperties))if(form.elements["override_"+key].checked)values[key]=initialize ? m.Overrides?.[property] : form.elements[key].value;
      const selected = !!form.elements.profile.value;
      const profile = this.configurations.find(c => c.Profile.ID === form.elements.profile.value);
      if (profile) {
        const d = profile.Revision.Desired;
        for (const [key, property] of Object.entries({harness:'Harness',model:'Model',effort:'Effort',tool_governance:'ToolGovernance',fast_mode:'FastMode',cwd:'WorkingDirectory',approval:'Approval',sandbox:'Sandbox'})) form.elements[key].value = d[property] || '';
        for(const [key,value] of Object.entries(values))form.elements[key].value=value??'';
        if(form.elements.harness.value!==d.Harness)for(const key of ['model','effort','fast_mode','tool_governance'])if(!form.elements['override_'+key].checked)form.elements[key].value='';
        showEnvironment(d.Environment); showSandbox(d.HostSandbox);
        form.elements.harness.dispatchEvent(new Event('change', {bubbles:true}));
      }
      for (const key of ['harness','model','effort','fast_mode','tool_governance','cwd','approval','sandbox']) form.elements[key].disabled = selected && !form.elements['override_'+key]?.checked;
      for(const key of Object.keys(overrideProperties))form.elements['override_'+key].parentElement.hidden=!selected;
      environmentField.disabled = selected; sandboxField.disabled = selected;
      select.disabled = selected;
      syncTools();syncFast();
      updatingProfile=false;
    };
    form.elements.harness.addEventListener('change',()=>{if(form.elements.profile.value)updateProfile();});
    form.elements.profile.addEventListener('change', () => { updateProfile(); this.unapplied = true; });
    for(const key of Object.keys(overrideProperties))form.elements['override_'+key].addEventListener('change',()=>{updateProfile();this.unapplied=true;});
    form.addEventListener('launch-policy-support', event => {
      const profile = this.configurations.find(c => c.Profile.ID === form.elements.profile.value);
      const support = event.detail;
      if (!profile || form.elements.harness.value === profile.Revision.Desired.Harness) return;
      if (support.Harness !== form.elements.harness.value || !support.PolicyKnown) return;
      for(const [field,modes,value] of [['sandbox',support.SandboxModes,support.DefaultSandbox],['approval',support.ApprovalModes,support.DefaultApproval]]) {
        if(!form.elements['override_'+field].checked && !(modes||[]).includes(form.elements[field].value) && (modes||[]).includes(value)) form.elements[field].value=value;
      }
    });
    updateProfile(true);
    this.content.prepend(select, el('p', 'A saved configuration uses its current settings at each new deployment. Custom settings and copied settings stay with this template. Deployment supplies the working directory.'));
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
    const w = original || {ID: '', MemberKeys: [], DependsOn: [], RequiredReady: true, RequiredBriefs: true, WaitForIdle: true, MaxWaitSeconds: 0};
    this.form('Wave', [{key: 'id', label: 'Wave key', value: w.ID, required: true},
      {key: 'members', label: 'Wave members', multiple: true, options: this.draft.Team.Members.map(m => opt(m.Key, m.Name)), value: w.MemberKeys, required: true},
      {key: 'after', label: 'Launch after waves', multiple: true, options: this.draft.Team.Waves.filter(x => x.ID !== original?.ID).map(x => opt(x.ID)), value: w.DependsOn || []},
      {key: 'ready', label: 'Wait for member readiness', type: 'checkbox', value: w.RequiredReady}, {key: 'briefs', label: 'Wait for required briefings', type: 'checkbox', value: w.RequiredBriefs},
      {key: 'idle', label: 'Wait for first turn before launching later waves', type: 'checkbox', value: !!w.WaitForIdle},
      {key: 'maxWait', label: 'Maximum wait in seconds (0 uses 8 minutes)', value: String(w.MaxWaitSeconds || 0)}], f => {
        const maxWait = Number(f.maxWait);
        if (!/^\d+$/.test(f.maxWait) || !Number.isSafeInteger(maxWait) || maxWait > 9223372036) throw new Error('Maximum wait must be a nonnegative whole number of seconds.');
        if (this.draft.Team.Waves.some(x => x.ID === f.id && x.ID !== original?.ID)) throw new Error('Wave keys must be unique.');
        this.change(d => {
          const wave = {...w, ID: f.id, MemberKeys: f.members, DependsOn: f.after, RequiredReady: f.ready, RequiredBriefs: f.briefs, WaitForIdle: f.idle, MaxWaitSeconds: maxWait};
          const i = d.Team.Waves.findIndex(x => x.ID === original?.ID); if (i < 0) d.Team.Waves.push(wave); else d.Team.Waves[i] = wave;
          for (const x of d.Team.Waves) if (x !== wave) { x.MemberKeys = x.MemberKeys.filter(k => !f.members.includes(k)); if (original) x.DependsOn = (x.DependsOn || []).map(id => id === original.ID ? f.id : id); }
        });
      });
  }
  briefing(original) {
    const b = original || {ID: '', Body: '', Timing: 'before_first_work', Required: true, MemberKeys: this.draft.Team.Members.map(m => m.Key)};
    this.form('Briefing', [{key: 'id', label: 'Briefing key', value: b.ID, required: true}, {key: 'body', label: 'Briefing text', text: true, value: b.Body, required: true},
      {key:'syntax',label:'Mission placeholders',options:[opt('','Literal text'),opt('mission-v1','Expand {{task}} and {{mission}}')],value:b.Syntax||''},
      {key: 'timing', label: 'Deliver briefing', options: [opt('before_first_work', 'Before first work'), opt('after_ready', 'After ready')], value: b.Timing},
      {key: 'required', label: 'Required briefing', type: 'checkbox', value: b.Required},
      {key: 'members', label: 'Briefing recipients', required: true, multiple: true, options: this.draft.Team.Members.map(m => opt(m.Key, m.Name)), value: [...new Set([...(b.MemberKeys || []), ...this.draft.Team.Members.filter(m => m.BriefingIDs?.includes(b.ID)).map(m => m.Key)])]}], f => {
        if (this.draft.Team.Briefings.some(x => x.ID === f.id && x.ID !== original?.ID)) throw new Error('Briefing keys must be unique.');
        this.change(d => { const brief = {...b, ID: f.id, Body: f.body, Syntax:f.syntax||undefined, Timing: f.timing, Required: f.required, MemberKeys: f.members}; const i = d.Team.Briefings.findIndex(x => x.ID === original?.ID); if (i < 0) d.Team.Briefings.push(brief); else d.Team.Briefings[i] = brief; for (const m of d.Team.Members) { m.BriefingIDs = (m.BriefingIDs || []).filter(id => id !== original?.ID && id !== f.id); if (f.members.includes(m.Key)) m.BriefingIDs.push(f.id); } });
      });
  }
  phases() {
    return this.draft.Team.AdvisoryProcess?.length ? this.draft.Team.AdvisoryProcess : this.draft.Team.AdvisoryPhases.map(Name => ({Name, Roles: [], Criteria: ''}));
  }
  settings() {
    const phases = this.phases();
    this.form('Workspace and advisory phases', [{key: 'workspace', label: 'Workspace policy', options: [opt('shared', 'Shared workspace'), opt('per_member', 'Separate member workspaces')], value: this.draft.Team.WorkspacePolicy}, {key: 'phases', label: 'Advisory phases (one per line)', text: true, value: phases.map(p => p.Name).join('\n')}], f => this.change(d => {
      const names = lines(f.phases);
      if (new Set(names.map(n => n.toLowerCase())).size !== names.length) throw new Error('Phase names must be unique.');
      d.Team.WorkspacePolicy = f.workspace;
      d.Team.AdvisoryProcess = names.map(Name => ({...(phases.find(p => p.Name.toLowerCase() === Name.toLowerCase()) || {Roles: [], Criteria: ''}), Name}));
      d.Team.AdvisoryPhases = [];
    }));
    this.content.append(el('p', 'Phases are advisory guidance. They do not grant permissions, gate work, or advance automatically. Reorder the names above to change their order.'));
    for (const phase of phases) this.card(phase.Name, `Active roles: ${(phase.Roles || []).join(', ') || 'none specified'}\n${phase.Criteria || ''}`, () => { if (this.discard()) this.phase(phase); }, () => { if (this.discard()) this.change(d => { d.Team.AdvisoryProcess = phases.filter(p => p.Name !== phase.Name); d.Team.AdvisoryPhases = []; }); });
  }
  phase(original) {
    const phases = this.phases();
    this.form('Advisory phase', [{key: 'phase_name', label: 'Phase name', value: original.Name, required: true}, {key: 'phase_roles', label: 'Active role labels (one per line; all means every member)', text: true, value: (original.Roles || []).join('\n')}, {key: 'criteria', label: 'Completion and handoff guidance', text: true, value: original.Criteria || ''}], f => {
      const name = f.phase_name.trim();
      if (!name || phases.some(p => p.Name !== original.Name && p.Name.toLowerCase() === name.toLowerCase())) throw new Error('Phase names must be nonempty and unique.');
      this.change(d => { d.Team.AdvisoryProcess = phases.map(p => p.Name === original.Name ? {Name: name, Roles: lines(f.phase_roles), Criteria: f.criteria} : p); d.Team.AdvisoryPhases = []; });
    });
  }
  parameter(original) {
    const p = original || {};
    this.form('Parameter', [{key: 'name', label: 'Parameter name', value: p.Name, required: true}, {key: 'type', label: 'Parameter type', options: ['string', 'number', 'boolean', 'object', 'array'].map(v => opt(v)), value: p.Type || 'string'}, {key: 'required', label: 'Required parameter', type: 'checkbox', value: p.Required}, {key: 'display_name',label:'Display name (optional)',value:p.DisplayName},{key: 'description', label: 'Description', value: p.Description},{key:'doc',label:'Parameter documentation',text:true,value:p.Doc}, {key: 'default', label: 'Default value (JSON, optional)', value: parameterDefaultText(p)}], f => {
      if (this.draft.Parameters.some(x => x.Name === f.name && x.Name !== original?.Name)) throw new Error('Parameter names must be unique.');
      const parameter = {Name: f.name, Type: f.type, Required: f.required, Description: f.description}; if(f.display_name)parameter.DisplayName=f.display_name;if(f.doc)parameter.Doc=f.doc; if (f.default.trim()) parameter.DefaultJSON = (JSON.parse(f.default),f.default);
      this.change(d => { const i = d.Parameters.findIndex(x => x.Name === original?.Name); if (i < 0) d.Parameters.push(parameter); else d.Parameters[i] = parameter; });
    });
  }
  rhythmForm() {
    const refs = this.rhythms.map(r => ({ref: {RuleID: r.Rule.ID, RevisionID: r.Revision.ID, ContentHash: r.Revision.ContentHash}, label: `${r.Rule.Name} · revision ${r.Revision.Number}`}));
    for (const ref of this.draft.Team.Automation) if (!refs.some(x => x.ref.RevisionID === ref.RevisionID)) refs.push({ref, label: 'Previously pinned rule ' + ref.RuleID});
    this.form('Pinned team rhythms', [{key: 'rules', label: 'Automation revisions', multiple: true, options: refs.map(r => opt(r.ref.RevisionID, r.label)), value: this.draft.Team.Automation.map(r => r.RevisionID)}], f => this.change(d => { d.Team.Automation = f.rules.map(id => clone(refs.find(r => r.ref.RevisionID === id).ref)); }));
    this.content.append(button('Add recurring nudge', () => { if (this.discard()) this.editRhythm(); }));
    for (const rhythm of this.draft.Team.Rhythms || []) this.card(rhythm.Name, `${rhythm.Interval || rhythm.Cron} · ${rhythm.Timezone} · ${rhythm.RoleLabel || this.roles.find(r => r.ID === rhythm.RoleID)?.Name || (rhythm.RoleID ? 'Retained role' : 'All members')}`, () => { if (this.discard()) this.editRhythm(rhythm); }, () => { if (this.discard()) this.change(d => { d.Team.Rhythms = d.Team.Rhythms.filter(r => r.Name !== rhythm.Name); }); });
  }
  editRhythm(original) {
    const r = original || {};
    this.form('Recurring team nudge', [{key:'name',label:'Nudge name',value:r.Name,required:true},{key:'role_label',label:'Target display role (blank for all members)',value:r.RoleLabel || ''},...(r.RoleID?[{key:'role',label:'Existing permission-role filter',value:r.RoleID,options:[opt('','Remove permission-role filter'),...this.roles.map(role=>opt(role.ID,role.Name))]}]:[]),{key:'interval',label:'Interval (for example 10m; leave blank for cron)',value:r.Interval},{key:'cron',label:'Cron (leave blank for interval)',value:r.Cron},{key:'timezone',label:'Schedule timezone',value:r.Timezone || 'UTC',required:true},{key:'subject',label:'Message subject (optional)',value:r.Subject},{key:'body',label:'Nudge message',text:true,value:r.Body,required:true}], f => {
      if (!!f.interval.trim() === !!f.cron.trim()) throw new Error('Choose either interval or cron.');
      if ((this.draft.Team.Rhythms || []).some(x => x.Name !== original?.Name && x.Name.trim().toLowerCase() === f.name.trim().toLowerCase())) throw new Error('Nudge names must be unique.');
      if(f.role_label&&f.role)throw new Error('Remove the existing permission-role filter before choosing a display role.');
      const rhythm = {Name:f.name,RoleLabel:f.role_label,RoleID:f.role||'',Interval:f.interval,Cron:f.cron,Timezone:f.timezone,Subject:f.subject,Body:f.body};
      this.change(d => { d.Team.Rhythms ||= []; const i=d.Team.Rhythms.findIndex(x=>x.Name===original?.Name); if(i<0)d.Team.Rhythms.push(rhythm);else d.Team.Rhythms[i]=rhythm; });
    });
  }
  async save(write) {
    if (this.busy) return;
    if (this.unapplied) { this.fail(new Error('Apply field changes before saving or validating.')); return; }
    if (write && !this.dirty()) { this.status.textContent = 'Revision ' + this.revision + ' · saved'; return; }
    this.lock(true);
    try {
      const validated = await this.api('/v2/definitions/validate', {draft: wireDefinitionDraft(clone(this.draft))});
      if (!write) { this.status.textContent = 'Validation passed'; return; }
      const fingerprint = JSON.stringify(this.draft);
      if (this.pending?.fingerprint !== fingerprint) this.pending = {fingerprint, request: freshID('request_'), revision: freshID('revision_')};
      const result = await this.api('/v2/definitions', {request_id: this.pending.request, expected_revision: this.revision, draft: wireDefinitionDraft({...clone(this.draft), RevisionID: this.pending.revision})});
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
  export() { const url = URL.createObjectURL(new Blob([stringifyExact({format: 'tclaude-team-v2', draft: wireDefinitionDraft(this.draft,{exporting:true})}, 2)], {type: 'application/json'})); const link = el('a'); link.href = url; link.download = 'team.json'; link.click(); setTimeout(() => URL.revokeObjectURL(url), 1000); }
  import() {
    if (!this.discard()) return;
    const input = el('input'); input.type = 'file'; input.accept = '.json,application/json'; input.onchange = async () => {
      try { const file = input.files[0]; if (!file) return; if (file.size > 512 * 1024) throw new Error('Team import exceeds 512 KiB.'); const value = JSON.parse(await file.text()); if (value.format !== 'tclaude-team-v2' || value.draft?.Kind !== 'team') throw new Error('Choose an exported v2 team.'); if (this.dirty() && !confirm('Replace this draft with an imported copy?')) return; const draft = value.draft; draft.ID = freshID('definition_'); delete draft.RevisionID; this.lock(true); const r = await this.api('/v2/definitions/validate', {draft:wireDefinitionDraft(draft)}); this.adopt(draftOf(r), 0); this.render(); } catch (e) { this.fail(e); } finally { this.lock(false); }
    }; input.click();
  }
  close() { if (this.busy || ((this.dirty() || this.unapplied) && !confirm('Discard unsaved team changes?'))) return; window.removeEventListener('beforeunload', this.beforeUnload); this.dialog.close(); this.dialog.remove(); }
}
