'use strict';
// Shared select-field contract for launch dialogs. Resolve a chosen revision
// once and retain it for response-loss retries, independently of later edits.
async function sandboxSelectionInput(api,{retained=null,shell=true}={}) {
 retained=retained?structuredClone(retained):null;
 const profiles=(await api('/v2/sandbox-profiles')||[]).filter(p=>!p.Archived&&!p.Imported);
 const choices=new Map(profiles.map(p=>['profile:'+p.ID,{...p}]));
 const resolved=new Map();
 return {
  field:{name:'host_sandbox',label:'Host sandbox',value:retained?'retained':'none',options:[...(retained?[retainedSandboxOption(retained)]:[]),{value:'none',label:shell?'No host sandbox — start an unconfined shell':'No additional host sandbox'},...profiles.map(p=>({value:'profile:'+p.ID,label:p.Name+' · '+p.ID+' · '+p.HeadRevisionID}))],help:'A selected profile is pinned to the displayed revision. Unsupported host policy features refuse launch; they never silently remove confinement.'},
  async read(value){
   if(value==='none')return null;
   if(value==='retained'&&retained)return structuredClone(retained);
   const chosen=choices.get(value);if(!chosen)throw new Error('Select a listed sandbox profile.');
   if(resolved.has(value))return resolved.get(value);
   const current=await api('/v2/sandbox-profiles/'+encodeURIComponent(chosen.ID));
   if(current.Profile.Archived||current.Profile.Imported||current.Revision.Ref.RevisionID!==chosen.HeadRevisionID)throw new Error('The sandbox profile changed. Reopen this dialog and review its current revision.');
   const selected=await api('/v2/sandbox-profiles/selection',{scopes:[{Scope:'explicit',Ref:current.Revision.Ref}]});
   resolved.set(value,selected);return selected;
  }
 };
}

function retainedSandboxOption(value){return{value:'retained',label:'Retain selected policy · '+value.Scopes.map(s=>s.Scope+': '+s.Ref.ProfileID+' · '+s.Ref.RevisionID).join(' / ')}}

// The ordinary form component owns labels, submission and errors. This shared
// control supplies selection/loading and exact immutable-value serialization.
class SandboxSelectionControl {
 constructor(api,value){
  this.retained=value?structuredClone(value):null;
  this.host=document.createElement('select');
  this.status=document.createElement('p');this.status.setAttribute('role','status');
  const initial=[...(this.retained?[retainedSandboxOption(this.retained)]:[]),{value:'none',label:'No additional host sandbox'}];
  this.setOptions(initial,this.retained?'retained':'none');
  this.status.textContent='Loading available sandbox profiles…';
  this.loading=sandboxSelectionInput(api,{retained:this.retained,shell:false}).then(input=>{
   this.input=input;
   if(this.host.isConnected){this.setOptions(input.field.options,this.host.value);this.status.textContent=input.field.help;}
  }).catch(error=>{this.error=error;if(this.host.isConnected)this.status.textContent='Available profiles could not be loaded. The retained selection is unchanged.';});
 }
 setOptions(options,value){this.host.replaceChildren();for(const item of options){const option=document.createElement('option');option.value=item.value;option.textContent=item.label;this.host.append(option)}this.host.value=value}
 async read(value){
  if(value==='none')return null;
  if(value==='retained'&&this.retained)return structuredClone(this.retained);
  await this.loading;if(this.error)throw this.error;
  return this.input.read(value);
 }
}
