'use strict';
// Shared select-field contract for launch dialogs. Resolve a chosen revision
// once and retain it for response-loss retries, independently of later edits.
async function sandboxSelectionInput(api) {
 const profiles=(await api('/v2/sandbox-profiles')||[]).filter(p=>!p.Archived&&!p.Imported);
 const choices=new Map(profiles.map(p=>['profile:'+p.ID,{...p}]));
 const resolved=new Map();
 return {
  field:{name:'host_sandbox',label:'Host sandbox',value:'none',options:[{value:'none',label:'No host sandbox — start an unconfined shell'},...profiles.map(p=>({value:'profile:'+p.ID,label:p.Name+' · '+p.ID+' · '+p.HeadRevisionID}))],help:'A selected profile is pinned to the displayed revision. Unsupported host policy features refuse launch; they never fall back to an unconfined shell.'},
  async read(value){
   if(value==='none')return null;
   const chosen=choices.get(value);if(!chosen)throw new Error('Select a listed sandbox profile.');
   if(resolved.has(value))return resolved.get(value);
   const current=await api('/v2/sandbox-profiles/'+encodeURIComponent(chosen.ID));
   if(current.Profile.Archived||current.Profile.Imported||current.Revision.Ref.RevisionID!==chosen.HeadRevisionID)throw new Error('The sandbox profile changed. Reopen this dialog and review its current revision.');
   const selected=await api('/v2/sandbox-profiles/selection',{scopes:[{Scope:'explicit',Ref:current.Revision.Ref}]});
   resolved.set(value,selected);return selected;
  }
 };
}
