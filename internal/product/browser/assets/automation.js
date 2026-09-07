import {clone, freshID, lines, seconds, taskPerformers} from './process-model.js';

const refOf = r => ({DefinitionID: r.Definition.ID, RevisionID: r.Revision.ID, ContentHash: r.Revision.ContentHash, Kind: r.Definition.Kind});
const field = (name, label, value = '', extra = {}) => ({name, label, value, required: false, ...extra});
const choice = (value, label = value) => ({value, label});
const number = (form, name, min = 0) => { const n = Number(form[name]); if (!Number.isFinite(n) || n < min) throw new Error(name + ' must be at least ' + min); return n; };
const ruleBody = ({Rule: rule, Revision: r}) => ({id: rule.ID, name: rule.Name, enabled: rule.Enabled, owner: clone(r.Owner), delegation: clone(r.Delegation), condition: clone(r.Condition), action: clone(r.Action), policy: clone(r.Policy), dependencies: clone(r.Dependencies || [])});

export function automationWorkspace({api, el, button, edit, getSnapshot, openWork}) {
  let query = '', kindFilter = '', stateFilter = '', parent;
  async function render(target = parent) {
    parent = target;
    const [rules, definitions, authority] = await Promise.all([api('/v2/automation/rules?include_tombstoned=true'), api('/v2/definitions'), api('/v2/authority')]);
    parent.replaceChildren();
    const toolbar = el('div', undefined, 'toolbar');
    for (const kind of ['schedule', 'trigger', 'standing_order']) toolbar.append(button('New ' + kind.replaceAll('_', ' '), () => open(null, kind, null, authority)));
    const templates = el('select'); templates.setAttribute('aria-label', 'Automation process or team template');
    for (const d of definitions || []) { const o = el('option', `${d.Name} · ${d.Kind}`); o.value = d.ID; templates.append(o); }
    toolbar.append(templates, button('Schedule selected template', async () => {
      if (!templates.value) throw new Error('Save a process or team template first.');
      await open(null, 'schedule', await api('/v2/definitions/' + encodeURIComponent(templates.value)), authority);
    }));
    parent.append(toolbar);
    const filter = el('form'); filter.className = 'toolbar';
    const text = el('input'); text.value = query; text.placeholder = 'Search automation'; text.setAttribute('aria-label', 'Search automation');
    const kind = el('select'), state = el('select'); kind.setAttribute('aria-label', 'Automation kind'); state.setAttribute('aria-label', 'Automation state');
    for (const value of ['', 'schedule', 'trigger', 'standing_order']) { const o = el('option', value || 'All kinds'); o.value = value; kind.append(o); } kind.value = kindFilter;
    for (const value of ['', 'enabled', 'disabled', 'archived', 'all']) { const o = el('option', value === 'all' ? 'All including archived' : value || 'Active catalog'); o.value = value; state.append(o); } state.value = stateFilter;
    const submit = el('button', 'Filter'); filter.append(text, kind, state, submit);
    filter.onsubmit = e => { e.preventDefault(); query = text.value; kindFilter = kind.value; stateFilter = state.value; render().catch(error => { parent.append(el('p', error.message)); }); }; parent.append(filter);
    const records = await Promise.all((rules || []).filter(r => r.Name.toLowerCase().includes(query.toLowerCase())).map(r => api('/v2/automation/rules/' + encodeURIComponent(r.ID))));
    let count = 0;
    for (const result of records) {
      const r = result.Rule, revision = result.Revision;
      if (kindFilter && revision.Condition.Kind !== kindFilter) continue;
      if (stateFilter === 'archived' ? !r.Tombstoned : stateFilter !== 'all' && (r.Tombstoned || stateFilter && r.Enabled !== (stateFilter === 'enabled'))) continue;
      count++;
      const card = el('article', undefined, 'card'); card.dataset.rule = r.ID;
      const identity=el('code',r.ID);identity.setAttribute('aria-label','Automation rule ID');card.append(identity);
      card.append(el('h2', r.Name), el('p', `${revision.Condition.Kind.replaceAll('_', ' ')} · ${r.Tombstoned ? 'archived' : r.Enabled ? 'enabled' : 'disabled'} · revision ${r.Revision}`));
      const c = revision.Condition;
      card.append(el('p', c.Schedule ? `${c.Schedule.Cron || 'Every ' + c.Schedule.Interval / 1e9 + ' seconds'} · ${c.Schedule.Timezone}` : c.Trigger ? `${c.Trigger.SourceID} · ${c.Trigger.FactKind} · ${(c.Trigger.Values || []).join(', ')}` : `${c.StandingOrder.FactKind} · ${c.StandingOrder.Pattern || 'any'} · same continuation`));
      card.append(el('p', `Action: ${revision.Action.Kind} · authority expires ${new Date(revision.Delegation.ExpiresAt).toLocaleString()}`));
      if (r.DeploymentID) card.append(el('p', 'Managed by deployment ' + r.DeploymentID));
      else if (!r.Tombstoned) card.append(button('Edit rule', () => open(result, c.Kind, null, authority)), button(r.Enabled ? 'Disable' : 'Enable', async id => {
        await api('/v2/automation/rules/' + encodeURIComponent(r.ID) + '/enabled', {request_id: id, expected_revision: r.Revision, enabled: !r.Enabled}); await render();
      }));
      if (!r.DeploymentID) card.append(button(r.Tombstoned ? 'Restore rule' : 'Archive rule', () => edit(r.Tombstoned ? 'Restore archived rule' : 'Archive automation rule', [field('confirm', r.Tombstoned ? `Type ${r.ID} to restore disabled` : `Type ${r.ID} to stop fresh dispatch and archive (admitted work continues)`, '', {required:true})], async f => {if(f.confirm!==r.ID)throw new Error('Rule ID does not match');await api('/v2/automation/rules/'+encodeURIComponent(r.ID)+'/archived',{request_id:f.requestID,expected_revision:r.Revision,archived:!r.Tombstoned});await render()})));
      if (!r.Tombstoned && r.Enabled && c.Kind !== 'standing_order') card.append(button('Run now', async id => {
        await api('/v2/automation/run', {request_id: id, rule_id: r.ID, expected_rule_revision: r.Revision, occurrence_id: id, source_occurrence_key: 'browser:' + id}); await history(r, card);
      }));
      card.append(button('Occurrence history', () => history(r, card))); parent.append(card);
    }
    if (!count) parent.append(el('p', 'No rules match. Create a schedule, trigger or standing order.'));
  }
  async function history(rule, card) {
    card.querySelector('.occurrence-history')?.remove();
    const records = await api('/v2/automation/occurrences?rule_id=' + encodeURIComponent(rule.ID)), list = el('div', undefined, 'occurrence-history');
    list.append(el('h3', 'Occurrences'));
    for (const {Occurrence: o} of [...records || []].reverse()) {
      const row = el('article', undefined, 'card'); row.append(el('strong', o.State), el('p', `${o.ID} · ${new Date(o.CreatedAt).toLocaleString()}`), el('p', `Pinned rule revision ${o.RuleRevisionID}`));
      for (const r of o.Recipients || []) row.append(el('p', `${getSnapshot().agents?.find(a => a.ID === r.AgentID)?.Name || r.AgentID}: ${r.Disposition}${r.Detail ? ' · ' + r.Detail : ''}`));
      if (o.WorkRunID) row.append(button('Inspect occurrence work', () => openWork(o.WorkRunID)));
      if (o.DeploymentID) row.append(el('p', 'Deployment ' + o.DeploymentID)); list.append(row);
    }
    if (!records?.length) list.append(el('p', 'No occurrences yet.')); card.append(list);
  }
  async function open(result, kind, selected, authority) {
    const snapshot = getSnapshot(), agents = (snapshot.agents || []).filter(a => a.Lifecycle !== 'retired'), groups = snapshot.groups || [], spaces = (snapshot.workspaces || []).filter(w => w.State === 'available');
    const d = result ? ruleBody(result) : {id: freshID('rule_'), name: '', enabled: false, owner: {Kind: 'operator'}, delegation: {Actions: [], Resources: [], Bounds: {}, ExpiresAt: new Date(Date.now() + 86400000).toISOString()}, condition: {Kind: kind}, action: {Kind: selected?.Definition.Kind === 'process' ? 'work' : selected ? 'team' : 'message'}, policy: {MissedTicks: 'skip', OfflineDelivery: 'queue', ExpiresAfter: seconds(3600), Overlap: 'forbid', MaxActive: 1, Deadline: seconds(3600), Retry: {MaxAttempts: 1}}, dependencies: []};
    const pinned = d.action.Work?.Definition || d.action.Team?.Definition;
    if (pinned) { selected = await api('/v2/definitions/' + encodeURIComponent(pinned.DefinitionID) + '?revision_id=' + encodeURIComponent(pinned.RevisionID)); if (selected.Revision.ContentHash !== pinned.ContentHash) throw new Error('Pinned template revision does not match.'); }
    const fields = [field('name', 'Rule name', d.name, {required: true})];
    if (!result) fields.push(field('enabled', 'Rule state', d.enabled ? 'enabled' : 'disabled', {options: ['disabled', 'enabled']}));
    if (kind === 'schedule') {
      const s = d.condition.Schedule || {};
      fields.push(field('timezone', 'Schedule timezone (IANA)', s.Timezone || Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC', {required: true}), field('cron', 'Cron (five fields; leave blank for interval)', s.Cron), field('interval', 'Interval seconds (minimum 30; unused with cron)', s.Interval / 1e9 || 60), field('anchor', 'Schedule starts at (ISO timestamp, optional)', s.Anchor?.startsWith('0001-') ? '' : s.Anchor));
    } else if (kind === 'trigger') {
      const t = d.condition.Trigger || {}, resource = t.Resource || {};
      fields.push(field('source', 'Configured source name', t.SourceID || 'application', {required: true}), field('fact', 'Fact kind', t.FactKind || 'agent.idle', {options: ['agent.idle', 'agent.awaiting_input', 'work.succeeded', 'work.failed', 'work.cancelled', 'message.delivered', 'message.denied', 'operation.succeeded', 'operation.failed', 'pull_request.changed', 'ci.completed']}), field('resource_kind', 'Observed resource kind', resource.Kind || 'agent', {options: ['agent', 'work', 'message', 'operation', 'repository_pull_request']}), field('resource_id', 'Observed resource ID (except pull requests)', resource.ID), field('repository', 'Repository (owner/name for pull requests)', resource.Repository), field('pull', 'Pull request number', resource.PullRequest || ''), field('values', 'Matching values (one per line)', (t.Values || ['true']).join('\n'), {multiline: true, required: true}), field('dwell', 'Continuous matching seconds', t.Dwell / 1e9 || 0), field('cooldown', 'Cooldown seconds', t.Cooldown / 1e9 || 0), field('debounce', 'Debounce seconds', t.Debounce / 1e9 || 0), field('freshness', 'Observation freshness seconds', t.Freshness / 1e9 || 60));
    } else {
      const s = d.condition.StandingOrder || {};
      fields.push(field('fact', 'Native event kind', s.FactKind || 'user_prompt', {required: true}), field('pattern', 'Matching text (regular expression)', s.Pattern || ''), field('dispatch', 'Synchronous response deadline seconds', s.DispatchDeadline / 1e9 || 10));
    }
    let buildAction;
    if (d.action.Kind === 'message') {
      const m = d.action.Message || {};
      fields.push(field('body', 'Message or guidance', m.Body, {required: true, multiline: true}), field('recipients', 'Explicit agent recipients', m.AgentIDs || [], {multiple: true, options: agents.map(a => choice(a.ID, a.Name))}), field('group', 'Recipient group', m.GroupID || '', {options: [choice('', 'No group restriction'), ...groups.map(g => choice(g.ID, g.Name))]}), field('role', 'Recipient role', m.RoleID || '', {options: [choice('', 'No role restriction'), ...(authority.Roles || []).map(r => choice(r.ID, r.Name))]}));
      buildAction = f => ({Kind: 'message', Message: {...m, Body: f.body, AgentIDs: f.recipients, GroupID: f.group, RoleID: f.role}});
    } else {
      const action = selected?.Definition.Kind || (d.action.Kind === 'work' ? 'process' : 'team');
      const start = clone(d.action.Work || d.action.Team || {}), r = selected?.Revision;
      const parameters = r?.Parameters || Object.keys(start.Parameters || {}).map(Name => ({Name, Type: 'object'}));
      for (const [i, p] of parameters.entries()) { const v = start.Parameters?.[p.Name] ?? p.Default; fields.push(field('param_' + i, p.Description || p.Name, v === undefined || v === null ? '' : p.Type === 'string' ? v : JSON.stringify(v), {required: p.Required, multiline: p.Type === 'object' || p.Type === 'array'})); }
      const params = f => Object.fromEntries(parameters.flatMap((p, i) => f['param_' + i] === '' ? [] : [[p.Name, p.Type === 'string' ? f['param_' + i] : JSON.parse(f['param_' + i])]]));
      if (action === 'process') {
        const graph = r?.Process.Graph || start.InlineGraph, keys = [...new Set(taskPerformers(graph).map(p => p.Agent?.MemberKey).filter(Boolean))];
        fields.push(field('workspace', 'Process workspace', start.Scope?.WorkspaceID || spaces[0]?.ID || '', {required: true, options: spaces.map(w => choice(w.ID, w.Intent.Name || w.Observation.ActualPath))}));
        for (const [i, key] of keys.entries()) fields.push(field('bind_' + i, 'Worker for ' + key, start.PerformerBindings?.[key]?.Agent?.AgentID || agents[0]?.ID || '', {required: true, options: agents.map(a => choice(a.ID, a.Name))}));
        buildAction = f => ({Kind: 'work', Work: {...start, ...(selected ? {Definition: refOf(selected)} : {}), Scope: {...start.Scope, WorkspaceID: f.workspace}, Parameters: params(f), PerformerBindings: {...start.PerformerBindings, ...Object.fromEntries(keys.map((key, i) => [key, {Kind: 'agent', Agent: {AgentID: f['bind_' + i]}}]))}, AuthorizedProgramProfiles: taskPerformers(graph).filter(p => p.Program).map(p => p.Program.Profile)}});
      } else {
        fields.push(field('team_member_scope', 'Allow selected effects on members of the target team', 'no', {options: [choice('no', 'No additional team-member authority'), choice('yes', 'Include members of the explicit target group')]}), field('team_target', 'Team target', start.Target?.Kind || (start.GroupID ? 'new_group' : 'existing_group'), {options: ['existing_group', 'new_group']}), field('new_group', 'New group ID (only for new-group deployment)', start.Target?.Kind === 'new_group' ? start.Target.GroupID : start.GroupID || ''), field('mission', 'Team mission', start.Mission, {required: true, multiline: true}), field('team_group', 'Existing group (only for reinforcement)', start.Target?.Kind === 'existing_group' ? start.Target.GroupID : '', {options: [choice('', 'Select group'), ...groups.map(g => choice(g.ID, g.Name))]}));
        const shared = r.Team.WorkspacePolicy === 'shared', members = shared ? [{Key: 'shared', Name: 'Shared team'}] : r.Team.Members;
        for (const [i, m] of members.entries()) fields.push(field('space_' + i, m.Name + ' workspace', (shared ? start.Workspaces?.Shared : start.Workspaces?.Members?.[m.Key])?.WorkspaceID || spaces[0]?.ID || '', {required: true, options: spaces.map(w => choice(w.ID, w.Intent.Name || w.Observation.ActualPath))}));
        buildAction = f => { const bindings = Object.fromEntries(members.map((m, i) => { const previous = shared ? start.Workspaces?.Shared : start.Workspaces?.Members?.[m.Key]; if (previous?.WorkspaceID === f['space_' + i]) return [m.Key, clone(previous)]; const w = spaces.find(w => w.ID === f['space_' + i]); if (!w) throw new Error('Select an available workspace.'); return [m.Key, {WorkspaceID: w.ID, ExpectedRevision: w.Revision}]; })); return {Kind: 'team', Team: {...start, Definition: refOf(selected), Mission: f.mission, Target: {Kind: f.team_target, GroupID: f.team_target === 'new_group' ? f.new_group : f.team_group}, GroupID: '', Parameters: params(f), Workspaces: shared ? {Shared: bindings.shared} : {Members: bindings}}}; };
      }
    }
    const p = d.policy, bounds = d.delegation.Bounds || {};
    fields.push(field('offline', 'Offline recipient policy', p.OfflineDelivery, {options: ['queue', 'skip']}), field('missed', 'Missed schedule policy', p.MissedTicks, {options: ['skip', 'coalesce_latest']}), field('overlap', 'Overlapping occurrences', p.Overlap, {options: d.action.Kind === 'work' ? ['forbid', 'allow', 'replace'] : ['forbid', 'allow']}), field('active', 'Maximum active occurrences', p.MaxActive || 1), field('expiry', 'Occurrence expiry seconds', p.ExpiresAfter / 1e9), field('deadline', 'Work deadline seconds per occurrence', p.Deadline / 1e9), field('attempts', 'Maximum delivery attempts', p.Retry?.MaxAttempts || 1), field('backoff', 'Retry delay seconds', p.Retry?.Backoff / 1e9 || 0));
    const resources = [choice(JSON.stringify({Kind: 'automation_rule', AutomationRuleID: d.id}), 'This automation rule'), ...agents.map(a => choice(JSON.stringify({Kind: 'agent', AgentID: a.ID}), 'Agent: ' + a.Name)), ...groups.flatMap(g => [choice(JSON.stringify({Kind: 'group_members', GroupID: g.ID}), 'Members of ' + g.Name), choice(JSON.stringify({Kind: 'group', GroupID: g.ID}), 'Group: ' + g.Name)]), ...spaces.map(w => choice(JSON.stringify({Kind: 'workspace', WorkspaceID: w.ID}), 'Workspace: ' + (w.Intent.Name || w.ID)))];
    const resourceValue = (d.delegation.Resources || []).map(r => JSON.stringify(r)); for (const value of resourceValue) if (!resources.some(o => o.value === value)) resources.push(choice(value, 'Retained scope: ' + value));
    fields.push(field('owner', 'Authority owner', d.owner.Kind === 'operator' ? 'operator' : d.owner.Kind === 'execution' ? 'execution:' + d.owner.ExecutionID : d.owner.AgentID, {options: [choice('operator', 'Operator'), ...agents.map(a => choice(a.ID, a.Name))]}), field('allowed_actions', 'Allowed effects (intersected with current authority)', d.delegation.Actions || [], {multiple: true, options: ['message.send', 'execution.interact', 'work.start', 'execution.launch', 'execution.context.change', 'program.execute', 'workspace.inspect', 'workspace.create', 'automation.run', 'group.membership.manage']}), field('allowed_resources', 'Allowed effect targets', resourceValue, {multiple: true, options: resources}), field('authority_expiry', 'Authority expires at (ISO timestamp)', d.delegation.ExpiresAt, {required: true}), field('harnesses', 'Allowed launch harnesses (one per line)', (bounds.Harnesses || []).join('\n'), {multiline: true}), field('models', 'Allowed launch models (one per line)', (bounds.Models || []).join('\n'), {multiline: true}), field('roots', 'Allowed working directory roots (one per line)', (bounds.WorkingDirectoryRoots || []).join('\n'), {multiline: true}), field('approvals', 'Allowed launch approval modes', bounds.ApprovalModes || [], {multiple: true, options: ['supervised', 'automatic']}), field('environments', 'Exact allowed launch environments', bounds.Environments||[], {environmentSets:true}), field('sandboxes', 'Allowed launch confinement modes', bounds.SandboxModes || [], {multiple: true, options: ['read_only', 'workspace_write', 'unconfined']}));
    edit(result ? 'Edit automation rule' : 'Create ' + kind.replaceAll('_', ' '), fields, async f => {
      let condition;
      if (kind === 'schedule') { const s = {Timezone: f.timezone, Cron: f.cron.trim(), Interval: f.cron.trim() ? 0 : seconds(number(f, 'interval', 30))}; if (f.anchor) { const date = new Date(f.anchor); if (!Number.isFinite(date.getTime())) throw new Error('Invalid schedule start timestamp.'); s.Anchor = date.toISOString(); } condition = {Kind: kind, Schedule: s}; }
      else if (kind === 'trigger') condition = {Kind: kind, Trigger: {SourceID: f.source, FactKind: f.fact, Resource: f.resource_kind === 'repository_pull_request' ? {Kind: f.resource_kind, Repository: f.repository, PullRequest: number(f, 'pull', 1)} : {Kind: f.resource_kind, ID: f.resource_id}, Values: lines(f.values), Dwell: seconds(number(f, 'dwell')), Cooldown: seconds(number(f, 'cooldown')), Debounce: seconds(number(f, 'debounce')), Freshness: seconds(number(f, 'freshness', 0.001))}};
      else condition = {Kind: kind, StandingOrder: {FactKind: f.fact, Pattern: f.pattern, Timing: 'same_continuation', DispatchDeadline: seconds(number(f, 'dispatch', 0.001))}};
      const expiry = new Date(f.authority_expiry); if (!Number.isFinite(expiry.getTime())) throw new Error('Invalid authority expiry.');
      const action = buildAction(f); if (action.Message && !action.Message.AgentIDs.length && !action.Message.GroupID && !action.Message.RoleID) throw new Error('Choose explicit agent, group or role recipients.');
      const body = {...d, request_id: f.requestID, revision_id: f.requestID, expected_revision: result?.Rule.Revision || 0, name: f.name, enabled: result ? d.enabled : f.enabled === 'enabled', condition, action, owner: f.owner === 'operator' ? {Kind: 'operator'} : f.owner.startsWith('execution:') ? {Kind: 'execution', ExecutionID: f.owner.slice(10)} : {Kind: 'agent', AgentID: f.owner}, delegation: {Actions: f.allowed_actions, Resources: [...f.allowed_resources.map(x => JSON.parse(x)), ...(f.team_member_scope === 'yes' ? [{Kind: 'group_members', GroupID: action.Team.Target.GroupID}] : [])], ExpiresAt: expiry.toISOString(), Bounds: {Harnesses: lines(f.harnesses), Models: lines(f.models), WorkingDirectoryRoots: lines(f.roots), ApprovalModes: f.approvals, SandboxModes: f.sandboxes, Environments:f.environments}}, policy: {...p, OfflineDelivery: f.offline, MissedTicks: f.missed, Overlap: f.overlap, MaxActive: number(f, 'active', 1), ExpiresAfter: seconds(number(f, 'expiry', 0.001)), Deadline: seconds(number(f, 'deadline', 0.001)), Retry: {...p.Retry, MaxAttempts: number(f, 'attempts', 1), Backoff: seconds(number(f, 'backoff'))}}};
      const saved = await api('/v2/automation/rules', body);
      if (saved.Revision.ID !== f.requestID) throw new Error('This request was saved, but another revision is now current. Reopen it before editing further.');
      await render();
    }, {skipUnchanged: !!result});
  }
  return {render};
}
