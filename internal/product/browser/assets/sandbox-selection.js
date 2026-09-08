'use strict';
// Shared profile selector. Persist IDs; each launch resolves current content.
async function sandboxSelectionInput(api,{retained=null,shell=true}={}) {
 retained=sandboxProfileReferences(retained);
 const profiles=(await api('/v2/sandbox-profiles')||[]).filter(p=>!p.Archived);
 const choices=new Map(profiles.map(p=>['profile:'+p.ID,p]));
 return {
  field:{name:'host_sandbox',label:'Host sandbox',value:retained?'retained':'none',options:[...(retained?[retainedSandboxOption(retained,profiles)]:[]),{value:'none',label:shell?'No host sandbox — start an unconfined shell':'No additional host sandbox'},...profiles.map(p=>({value:'profile:'+p.ID,label:p.Name}))],help:'Profile changes take effect the next time the agent starts or restarts.'},
  async read(value){
   if(value==='none')return null;
   if(value==='retained'&&retained)return structuredClone(retained);
   const chosen=choices.get(value);if(!chosen)throw new Error('Select a listed sandbox profile.');
   return {Scopes:[{Scope:'explicit',Ref:{ProfileID:chosen.ID}}]};
  }
 };
}

function sandboxProfileReferences(value){return value?{Scopes:value.Scopes.map(s=>({Scope:s.Scope,Ref:{ProfileID:s.Ref.ProfileID}}))}:null}
function retainedSandboxOption(value,profiles=[]){return{value:'retained',label:'Keep '+value.Scopes.map(s=>profiles.find(p=>p.ID===s.Ref.ProfileID)?.Name||s.Ref.ProfileID).join(' / ')}}

// The ordinary form component owns labels, submission and errors. This shared
// control supplies selection/loading and stable profile-ID serialization.
class SandboxSelectionControl {
 constructor(api,value){
  this.retained=sandboxProfileReferences(value);
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

// Shared profile picker for delegated launch bounds. Values are stable IDs;
// names are labels and editing a profile does not require replacing a grant.
class SandboxProfileAllowList {
 constructor(api,values=[]){
  this.host=document.createElement('select');this.host.multiple=true;
  this.host.setAttribute('aria-label','Allowed sandbox profiles');
  this.original=[...values];
  for(const id of values){const option=document.createElement('option');option.value=id;option.textContent=id;option.selected=true;this.host.append(option)}
  api('/v2/sandbox-profiles').then(profiles=>{
   const selected=new Set(this.read());
   for(const profile of profiles||[]){
    let option=Array.from(this.host.options).find(o=>o.value===profile.ID);
    if(!option&&!profile.Archived){option=document.createElement('option');option.value=profile.ID;this.host.append(option)}
    if(option){option.textContent=profile.Name;option.selected=selected.has(profile.ID)}
   }
  }).catch(()=>{});
 }
 read(){return Array.from(this.host.selectedOptions,o=>o.value)}
}
