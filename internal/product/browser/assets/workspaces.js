'use strict';

class WorkspaceBrowser {
 constructor({host,api,el,button,refresh,row}){
  Object.assign(this,{host,api,el,button,refresh,row});this.generation=0;this.busy=false;this.snapshot={};
  const controls=el('form',undefined,'toolbar');controls.setAttribute('aria-label','Workspace filters');controls.onsubmit=e=>e.preventDefault();
  this.query=el('input');this.query.setAttribute('aria-label','Search workspaces');this.query.placeholder='Path, repository, branch or ID';
  const select=(label,options)=>{const input=el('select');input.setAttribute('aria-label',label);for(const[value,text]of options){const o=el('option',text);o.value=value;input.append(o)}return input};
  this.ownership=select('Workspace ownership',[['','All ownership'],['owned','Owned checkouts'],['external','External directories'],['shared','Shared resources']]);
  this.state=select('Workspace state',[['','All states'],['available','Available'],['pending','Pending'],['uncertain','Uncertain'],['removed','Removed']]);
  this.dirty=select('Workspace Git status',[['','All Git observations'],['dirty','Changes observed'],['clean','Clean observed'],['unknown','No Git observation']]);
  this.sort=select('Sort workspaces',[['path','Path'],['branch','Branch'],['state','State']]);
  controls.append(this.query,this.ownership,this.state,this.dirty,this.sort);for(const c of controls.children)c.oninput=()=>this.draw();
  this.inspect=el('button','Inspect visible workspaces');this.inspect.type='button';this.inspect.dataset.uiText='Inspect visible workspaces';this.inspect.onclick=()=>this.inspectVisible();this.export=button('Export workspace inventory',()=>this.download());this.count=el('p');this.report=el('div');this.report.setAttribute('role','status');this.list=el('div');
  host.replaceChildren(controls,this.inspect,this.export,this.count,this.report,this.list);
  window.addEventListener('pagehide',()=>{this.generation++;this.busy=false;this.draw()});
 }
 observation(space){return space.Observation?.RepositoryRoot&&Date.parse(space.Observation.ObservedAt)>0?(space.Observation.Dirty?'dirty':'clean'):'unknown'}
 update(snapshot){this.snapshot=snapshot;this.draw()}
 draw(){const q=this.query.value.toLocaleLowerCase(),spaces=this.snapshot.workspaces||[];
  this.visible=spaces.filter(s=>(!this.ownership.value||s.Intent.Ownership===this.ownership.value)&&(!this.state.value||s.State===this.state.value)&&(!this.dirty.value||this.observation(s)===this.dirty.value)&&[s.ID,s.Intent.Repository,s.Intent.IntendedPath,s.Intent.Branch,s.Observation?.ActualPath,s.Observation?.RepositoryRoot,s.Observation?.Branch].filter(Boolean).join(' ').toLocaleLowerCase().includes(q));
  const key=s=>this.sort.value==='branch'?(s.Observation?.Branch||s.Intent.Branch||''):this.sort.value==='state'?s.State:s.Observation?.ActualPath||s.Intent.IntendedPath||s.ID;
  this.visible.sort((a,b)=>key(a).localeCompare(key(b))||a.ID.localeCompare(b.ID));
  this.count.textContent=`${this.visible.length} of ${spaces.length} workspaces · Git status is the last recorded observation, not a live guarantee.`;
  this.list.replaceChildren(...this.visible.map(s=>this.row(s)));if(!this.visible.length)this.list.append(this.el('p',spaces.length?'No workspaces match these filters.':'No registered workspaces.'));
  this.inspect.disabled=this.busy||!this.visible.length;this.export.disabled=!this.visible.length;
 }
 async inspectVisible(){
  if(this.busy)return;const targets=this.visible.slice(0,50).map(s=>s.ID),generation=++this.generation;this.busy=true;this.inspect.disabled=true;this.report.replaceChildren();
  if(this.visible.length>50)this.report.append(this.el('p','Inspecting the first 50 visible workspaces. Narrow the filters to inspect others.'));
  try{for(const id of targets){if(generation!==this.generation)return;let result='Inspected';try{await this.api('/v2/workspaces/'+encodeURIComponent(id))}catch(e){result='Inspection failed: '+(e.message||e)}if(generation!==this.generation)return;this.report.append(this.el('p',`${id} · ${result}`))}if(generation===this.generation)await this.refresh()}
  catch(e){if(generation===this.generation)this.report.append(this.el('p',e.message||String(e)))}
  finally{if(generation===this.generation){this.busy=false;this.draw()}}
 }
 download(){const ids=new Set(this.visible.map(s=>s.ID));const payload={exported_at:new Date().toISOString(),filters:{search:this.query.value,ownership:this.ownership.value,state:this.state.value,git_status:this.dirty.value,sort:this.sort.value},workspaces:this.visible,active_claims:(this.snapshot.workspace_uses||[]).filter(u=>ids.has(u.WorkspaceID)&&!u.ReleasedAt)};const url=URL.createObjectURL(new Blob([JSON.stringify(payload,null,2)],{type:'application/json'})),link=this.el('a');link.href=url;link.download='tclaude-workspaces.json';link.click();setTimeout(()=>URL.revokeObjectURL(url),1000)}
 clear(){this.generation++;this.busy=false;this.snapshot={};this.query.value='';this.ownership.value='';this.state.value='';this.dirty.value='';this.sort.value='path';this.report.replaceChildren();this.draw()}
}

// Shared with standalone checkout creation and launch forms.
function checkoutFields(){return [{name:'repository',label:'Repository path'},{name:'path',label:'Checkout path'},{name:'base',label:'Base commit or branch',value:'HEAD'},{name:'branch',label:'Worker branch'}]}
function checkoutIntent(form){return {Repository:form.repository,IntendedPath:form.path,BaseRevision:form.base,Branch:form.branch,Provenance:'platform_created',Ownership:'owned',RetainOnFinish:true}}
class WorkspaceChoice {
 constructor(workspaces,api){this.api=api;this.workspaces=workspaces.filter(w=>w.State==='available'&&w.Observation?.ActualPath);this.fingerprint='';this.creationID='';this.created=null;}
 fields(){return [{name:'checkout',label:'Checkout',required:false,value:'',options:[{value:'',label:'Use the configured working directory'},...this.workspaces.map(w=>({value:'existing:'+w.ID,label:w.Observation.ActualPath})),{value:'new',label:'Create a new checkout'}]},...checkoutFields().map(field=>({...field,required:false}))]}
 attach(form){this.form=form;const update=()=>{const creating=form.elements.checkout.value==='new';for(const field of checkoutFields()){const input=form.elements[field.name];input.closest('label').hidden=!creating;input.disabled=!creating;input.required=creating;}const cwd=form.elements.cwd;if(cwd){cwd.disabled=!!form.elements.checkout.value;cwd.closest('label').hidden=!!form.elements.checkout.value;}};form.elements.checkout.addEventListener('change',update);update();}
 async read(form){
  if(!form.checkout)return null;
  let workspace=this.workspaces.find(w=>'existing:'+w.ID===form.checkout);
  if(form.checkout==='new'){
   const intent=checkoutIntent(form),fingerprint=JSON.stringify(intent);
   if(fingerprint!==this.fingerprint){this.fingerprint=fingerprint;this.creationID=requestID();this.created=null;}
   if(!this.created){try{this.created=(await this.api('/v2/workspaces/create',{request_id:this.creationID,id:this.creationID,intent})).Workspace;}catch(error){throw new Error(error.message+' Checkout reference: '+this.creationID+'. Inspect Workspaces before creating another checkout.');}}
   workspace=this.created;
  }
  if(!workspace||workspace.State!=='available')throw new Error('Select an available checkout.');
  return {WorkspaceID:workspace.ID,ExpectedRevision:workspace.Revision};
 }
}
