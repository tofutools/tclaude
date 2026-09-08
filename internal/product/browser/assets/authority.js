'use strict';
const AUTHORITY_ACTIONS=["identity.read", "status.read", "inbox.read", "inbox.mark_read", "message.send", "execution.launch", "execution.interact", "execution.file.stage","execution.file.read", "execution.attach", "execution.stop", "execution.context.change", "agent.configuration.update", "agent.retire", "agent.reactivate", "group.membership.manage", "group.disband", "attachment.read", "history.read", "history.refresh", "usage.read", "usage.refresh", "activity.read", "history.metadata.set", "workspace.register", "workspace.create", "workspace.inspect", "workspace.remove", "workspace.restore", "work.start", "work.evidence.record", "work.decide", "work.cancel", "work.resolve", "shell.start", "definition.read", "definition.manage", "program_profile.manage", "program_profile.read", "program.execute", "automation.manage", "automation.read", "automation.run"];
class AuthorityWorkspace {
 constructor({host,api,el,button,edit,getSnapshot,report}){Object.assign(this,{host,api,el,button,edit,getSnapshot});document.getElementById('new-grant').onclick=()=>this.editGrant().catch(report)}
 async render(){this.state=await this.api('/v2/authority');this.host.replaceChildren();const toolbar=this.el('div',undefined,'toolbar');toolbar.append(this.button('Create role',()=>this.editRole()));this.host.append(toolbar);
  const grants=this.el('section');grants.append(this.el('h2','Direct grants'));
  for(const grant of this.state.Grants||[]){const card=this.el('article',undefined,'card');card.dataset.grant=grant.ID;card.append(this.el('strong',grant.Action),this.el('p',this.describe(grant.Subject)+' → '+this.describe(grant.Resource)),this.el('p',grant.ID+' · revision '+grant.Revision,'muted'),this.el('p',this.boundsText(grant.Bounds),'muted'));if(grant.ExpiresAt)card.append(this.el('p','Expires '+new Date(grant.ExpiresAt).toLocaleString()));card.append(this.button('Edit grant',()=>this.editGrant(grant)),this.button('Revoke grant',()=>this.confirm('Revoke grant','Revokes '+grant.Action+' for '+this.describe(grant.Subject),async()=>{await this.api('/v2/authority/grants/'+encodeURIComponent(grant.ID),{expected_revision:grant.Revision},'DELETE');await this.render()})));grants.append(card)}
  if(!this.state.Grants?.length)grants.append(this.el('p','No direct grants.'));this.host.append(grants);
  const roles=this.el('section');roles.append(this.el('h2','Roles and assignments'));
  for(const role of this.state.Roles||[]){const card=this.el('article',undefined,'card');card.dataset.role=role.ID;card.append(this.el('h3',role.Name),this.el('p',(role.Actions||[]).join(', ')),this.el('p',role.ID+' · revision '+role.Revision,'muted'));
   if(role.ID==='group_owner')card.append(this.el('p','Owner assignments are managed through Group settings.'));
   else card.append(this.button('Edit role',()=>this.editRole(role)),this.button('Assign role',()=>this.editAssignment(role)));
   for(const assignment of (this.state.Assignments||[]).filter(a=>a.RoleID===role.ID)){const row=this.el('div',undefined,'card');row.append(this.el('strong',this.describe(assignment.Subject)+' → '+this.describe(assignment.Resource)),this.el('p','Assignment revision '+assignment.Revision,'muted'),this.el('p',this.boundsText(assignment.Bounds),'muted'));
    if(role.ID!=='group_owner')row.append(this.button('Edit assignment limits',()=>this.editAssignment(role,assignment)),this.button('Remove assignment',()=>this.confirm('Remove role assignment',role.Name+' from '+this.describe(assignment.Subject),async()=>{await this.api('/v2/authority/roles/'+encodeURIComponent(role.ID)+'/assignments',{subject:assignment.Subject,resource:assignment.Resource,expected_revision:assignment.Revision},'DELETE');await this.render()})));card.append(row)}roles.append(card)
  }if(!this.state.Roles?.length)roles.append(this.el('p','No roles.'));this.host.append(roles)
 }
 describe(value){const s=this.getSnapshot(),kind=value.Kind,id=value.AgentID||value.ExecutionID||value.GroupID||value.ConversationID||value.WorkspaceID||value.WorkRunID||value.DefinitionID||value.ProgramProfileID||value.AutomationRuleID||'';const named=(s.agents||[]).find(a=>a.ID===id)||(s.groups||[]).find(g=>g.ID===id);return kind+(id?': '+(named?named.Name+' · '+id:id):'')}
 boundsText(b={}){return ['Harnesses','Models','WorkingDirectoryRoots','ApprovalModes','SandboxModes'].every(k=>b[k]?.length)?`Configuration limits: ${(b.Harnesses||[]).join(', ')} / ${(b.Models||[]).join(', ')} / ${(b.WorkingDirectoryRoots||[]).join(', ')} / ${(b.ApprovalModes||[]).join(', ')} / ${(b.SandboxModes||[]).join(', ')} / environments: ${JSON.stringify(b.Environments||[])}`:'No configuration-bearing authority (incomplete or empty bounds).'}
 confirm(title,detail,run){this.edit(title,[{name:'confirm',label:detail,options:['Confirm']}],run)}
 editRole(role){this.edit(role?'Edit role actions for every assignment':'Create role',[
  {name:'name',label:'Role name',value:role?.Name||''},{name:'actions',label:'Allowed actions',multiple:true,value:role?.Actions||[],options:AUTHORITY_ACTIONS}
 ],async f=>{if(!f.actions.length)throw new Error('Select at least one action.');await this.api('/v2/authority/roles/'+encodeURIComponent(role?.ID||f.requestID),{name:f.name,actions:f.actions,expected_revision:role?.Revision||0},'PUT');await this.render()},{skipUnchanged:!!role})}
 boundsFields(b={}){const complete=['Harnesses','Models','WorkingDirectoryRoots','ApprovalModes','SandboxModes'].every(k=>b[k]?.length),lines=v=>(v||[]).join('\n');return[
  {name:'configuration',label:'Configuration-bearing authority',value:complete?'listed':'disabled',options:[{value:'disabled',label:'No launch or configuration changes'},{value:'listed',label:'Only the complete allow-lists below'}]},
  {name:'harnesses',label:'Allowed harnesses (one per line)',multiline:true,required:false,value:lines(b.Harnesses)},
  {name:'models',label:'Allowed models (one per line)',multiline:true,required:false,value:lines(b.Models)},
  {name:'roots',label:'Allowed working directory roots (one per line)',multiline:true,required:false,value:lines(b.WorkingDirectoryRoots)},
  {name:'approvals',label:'Allowed approval modes',multiple:true,required:false,value:b.ApprovalModes||[],options:['supervised','automatic']},
  {name:'environments',label:'Exact allowed launch environments',environmentSets:true,value:b.Environments||[]},
  {name:'sandboxes',label:'Allowed confinement modes',multiple:true,required:false,value:b.SandboxModes||[],options:['read_only','workspace_write','unconfined']}
 ]}
 parseBounds(f){if(f.configuration==='disabled')return{};const lines=v=>v.split('\n').map(v=>v.trim()).filter(Boolean),bounds={Harnesses:lines(f.harnesses),Models:lines(f.models),WorkingDirectoryRoots:lines(f.roots),ApprovalModes:f.approvals,SandboxModes:f.sandboxes};if(Object.values(bounds).some(v=>!v.length))throw new Error('Supply all five allow-lists or choose no configuration-bearing authority.');bounds.Environments=f.environments;return bounds}
 subjects(){const s=this.getSnapshot();return [...(s.agents||[]).filter(a=>a.Lifecycle!=='retired').map(a=>({value:JSON.stringify({Kind:'agent',AgentID:a.ID}),label:'Agent: '+a.Name+' · '+a.ID})),...(s.executions||[]).filter(e=>!e.agent_id&& !['failed','exited'].includes(e.state)).map(e=>({value:JSON.stringify({Kind:'execution',ExecutionID:e.id}),label:'Standalone execution: '+e.id}))]}
 async resources(){const s=this.getSnapshot(),[definitions,profiles,rules]=await Promise.all([this.api('/v2/definitions'),this.api('/v2/program-profiles'),this.api('/v2/automation/rules')]);const out=[{value:JSON.stringify({Kind:'self'}),label:'Self (derived from the caller)'},{value:JSON.stringify({Kind:'operator'}),label:'Operator inbox'}];
  const add=(kind,field,id,label)=>out.push({value:JSON.stringify({Kind:kind,[field]:id}),label});
  for(const a of s.agents||[])add('agent','AgentID',a.ID,'Agent: '+a.Name+' · '+a.ID);
  for(const g of s.groups||[]){add('group','GroupID',g.ID,'Group: '+g.Name+' · '+g.ID);add('group_members','GroupID',g.ID,'Current members of group: '+g.Name+' · '+g.ID)}
  for(const e of s.executions||[])add('execution','ExecutionID',e.id,'Execution: '+e.id);
  for(const c of s.conversations||[])add('conversation','ConversationID',c.ID,'Conversation: '+c.ID);
  for(const w of s.workspaces||[])add('workspace','WorkspaceID',w.ID,'Workspace: '+(w.Observation?.ActualPath||w.ID));
  for(const w of s.work_runs||[])add('work_run','WorkRunID',w.run.id,'Work: '+w.run.id);
  for(const d of definitions||[])add('definition','DefinitionID',d.ID,'Definition: '+d.Name+' · '+d.ID);
  for(const p of profiles||[])add('program_profile','ProgramProfileID',p.ID,'Program profile: '+p.Name+' · '+p.ID);
  for(const r of rules||[])add('automation_rule','AutomationRuleID',r.ID,'Automation: '+r.Name+' · '+r.ID);
  return out
 }
 async editGrant(grant){this.edit(grant?'Edit direct grant':'Grant permission',[
  {name:'subject',label:'Recipient of authority',options:this.subjects(),value:grant?JSON.stringify(grant.Subject):undefined},
  {name:'action',label:'Allowed action',options:AUTHORITY_ACTIONS,value:grant?.Action},
  {name:'resource',label:'Exact resource scope',options:await this.resources(),value:grant?JSON.stringify(grant.Resource):undefined},
  {name:'expiry',label:'Expiry (RFC3339, blank means no expiry)',required:false,value:grant?.ExpiresAt||''},...this.boundsFields(grant?.Bounds)
 ],async f=>{let expiry=null;if(f.expiry.trim()){const date=new Date(f.expiry);if(!Number.isFinite(date.getTime()))throw new Error('Invalid expiry timestamp.');expiry=date.toISOString()};await this.api('/v2/authority/grants/'+encodeURIComponent(grant?.ID||f.requestID),{subject:JSON.parse(f.subject),action:f.action,resource:JSON.parse(f.resource),expires_at:expiry,bounds:this.parseBounds(f),expected_revision:grant?.Revision||0},'PUT');await this.render()},{skipUnchanged:!!grant})}
 async editAssignment(role,prior){const identity=prior?[]:[{name:'subject',label:'Recipient of authority',options:this.subjects()},{name:'resource',label:'Exact resource scope',options:await this.resources()}];this.edit(prior?'Edit exact assignment limits':'Assign '+role.Name,[...identity,...this.boundsFields(prior?.Bounds)],async f=>{await this.api('/v2/authority/roles/'+encodeURIComponent(role.ID)+'/assignments',{subject:prior?.Subject||JSON.parse(f.subject),resource:prior?.Resource||JSON.parse(f.resource),bounds:this.parseBounds(f),expected_revision:prior?.Revision||0},'PUT');await this.render()},{skipUnchanged:!!prior})}
}
