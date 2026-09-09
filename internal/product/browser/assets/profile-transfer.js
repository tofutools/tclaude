// Adapted from the legacy management-island ProfileExport/ProfileImport components.
// The view keeps selection, file/paste preview, rename and explicit replacement;
// this adapter supplies stable catalog identities and one atomic import command.
import {h, render} from './vendor/profile-transfer/preact.module.js';
import {useState, useEffect, useRef} from './vendor/profile-transfer/hooks.module.js';
import htm from './vendor/profile-transfer/htm.module.js';
const html=htm.bind(h), limit=1024*1024;
const message=e=>e.message||String(e);
const id=()=>`r_${crypto.randomUUID()}`;
let currentClose=null;
export function closeProfileTransfer(){currentClose?.();}
function Overlay({title,children,close,dirty=false,busy=false}){
 const ref=useRef(null),latest=useRef({dirty,busy});latest.current={dirty,busy};
 const requestClose=()=>{const state=latest.current;if(!state.busy&&(!state.dirty||confirm('Discard this import draft?')))close()};
 useEffect(()=>{ref.current.showModal();const unload=e=>{if(latest.current.dirty){e.preventDefault();e.returnValue=''}};window.addEventListener('beforeunload',unload);return()=>window.removeEventListener('beforeunload',unload)},[]);
 return html`<dialog ref=${ref} class="profile-transfer-dialog" aria-label=${title} onCancel=${e=>{e.preventDefault();requestClose()}}><h3>${title}</h3>${children}<button type="button" disabled=${busy} onClick=${requestClose}>Cancel</button></dialog>`;
}
function ProfileExport({profiles,actions,close}){
 const [selected,setSelected]=useState(()=>new Set(profiles.map(item=>item.ID))),[error,setError]=useState(''),[busy,setBusy]=useState(false);
 const toggle=key=>setSelected(old=>{const next=new Set(old);next.has(key)?next.delete(key):next.add(key);return next});
 const submit=async()=>{if(!selected.size){setError('Select at least one configuration');return}setBusy(true);try{await actions.exportProfileBundle([...selected]);close()}catch(e){setError(message(e))}finally{setBusy(false)}};
 return html`<${Overlay} title="Export configurations" close=${close} busy=${busy}><div class="profile-transfer-list">${profiles.map(item=>html`<label key=${item.ID} class="profile-transfer-row"><input type="checkbox" checked=${selected.has(item.ID)} disabled=${busy} onChange=${()=>toggle(item.ID)}/><span>${ProfilePresentation.label(item,true)} · ${item.ID}${item.Archived?' · Archived':''}</span></label>`)}</div><div role="alert">${error}</div><button disabled=${busy} onClick=${submit}>${busy?'Exporting…':'Export'}</button></${Overlay}>`;
}
function ProfileImportRow({row,decision,profiles,update,busy}){
 return html`<div class="profile-transfer-row"><label><input type="checkbox" disabled=${busy} checked=${decision.include} onChange=${e=>update({include:e.currentTarget.checked})}/> ${ProfilePresentation.label(row,true)}${row.archived?' · Archived':''}</label><select aria-label=${`Import action for ${row.name}`} disabled=${busy} value=${decision.action} onChange=${e=>update({action:e.currentTarget.value})}><option value="create">Create a renamed copy</option><option value="update">Replace exact configuration</option></select>${decision.action==='update'&&html`<select aria-label=${`Replace target for ${row.name}`} disabled=${busy} value=${decision.target} onChange=${e=>update({target:e.currentTarget.value})}><option value="">Choose exact target</option>${profiles.map(p=>html`<option value=${p.ID}>${ProfilePresentation.label(p,true)} · ${p.ID} · revision ${p.Revision}</option>`)}</select>`}<label>Name <input aria-label=${`Import name for ${row.name}`} disabled=${busy} value=${decision.name} onInput=${e=>update({name:e.currentTarget.value})}/></label></div>`;
}
function ProfileImport({profiles,actions,close}){
 const [raw,setRaw]=useState(''),[preview,setPreview]=useState(null),[decisions,setDecisions]=useState({}),[error,setError]=useState(''),[busy,setBusy]=useState('');
 const readGeneration=useRef(0),mounted=useRef(true);useEffect(()=>()=>{mounted.current=false;readGeneration.current++},[]);
 const changeRaw=value=>{readGeneration.current++;setRaw(value);setPreview(null);setError('')};
 const inspect=async()=>{setError('');setBusy('inspect');try{if(new TextEncoder().encode(raw).length>limit)throw Error('Bundle exceeds 1 MiB');const parsed=JSON.parse(raw),found=await actions.inspectProfiles(parsed);if(!mounted.current)return;setPreview(found);const initial={};for(const row of found.profiles)initial[row.key]={include:true,action:'create',name:`${row.name}-copy`,target:''};setDecisions(initial)}catch(e){if(mounted.current)setError(message(e))}finally{if(mounted.current)setBusy('')}};
 const update=(key,patch)=>setDecisions(old=>({...old,[key]:{...old[key],...patch}}));
 const submit=async()=>{setError('');setBusy('import');try{await actions.importProfileBundle(preview,decisions);if(mounted.current)close()}catch(e){if(mounted.current)setError(message(e))}finally{if(mounted.current)setBusy('')}};
 return html`<${Overlay} title="Import configurations" close=${close} dirty=${!!raw} busy=${!!busy}><p>Replacing a configuration also replaces its enabled/disabled state and reason with the previewed values.</p><label>File <input type="file" accept=".json,application/json" disabled=${!!busy} onChange=${async e=>{const file=e.currentTarget.files?.[0];if(!file)return;const generation=++readGeneration.current;setBusy('read');try{if(file.size>limit)throw Error('Bundle exceeds 1 MiB');const value=new TextDecoder('utf-8',{fatal:true}).decode(await file.arrayBuffer());if(mounted.current&&generation===readGeneration.current)changeRaw(value)}catch(e){if(mounted.current&&generation===readGeneration.current)setError(message(e))}finally{if(mounted.current)setBusy('')}}}/></label><label>Or paste <textarea aria-label="Configuration bundle" rows="6" disabled=${!!busy} value=${raw} onInput=${e=>changeRaw(e.currentTarget.value)}/></label><button disabled=${!!busy} onClick=${inspect}>Preview</button>${preview&&html`<div class="profile-transfer-list">${preview.profiles.map(row=>html`<${ProfileImportRow} key=${row.key} row=${row} decision=${decisions[row.key]} profiles=${profiles} update=${patch=>update(row.key,patch)} busy=${!!busy}/>`)}<p>Selected entries commit together. Replacing creates a new immutable revision and preserves existing pinned agent settings. Bundle archive state replaces target archive state. Defaults and agents are unchanged.</p></div>`}<div role="alert">${error}</div><button disabled=${!!busy||!preview} onClick=${submit}>${busy==='import'?'Importing…':'Import selected'}</button></${Overlay}>`;
}
export function openProfileTransfer({mode,profiles,api,refresh}){
 closeProfileTransfer();const host=document.createElement('div');document.body.append(host);let live=true,lastIntent='',pending=null;
 const ensure=()=>{if(!live)throw Error('Transfer view closed')};
 const close=()=>{if(!live)return;live=false;render(null,host);host.remove();document.removeEventListener('workspace-signout',close);window.removeEventListener('pagehide',close);if(currentClose===close)currentClose=null};currentClose=close;
 document.addEventListener('workspace-signout',close);window.addEventListener('pagehide',close);
 const actions={
  async exportProfileBundle(selected){
   if(selected.length>128)throw Error('Select at most 128 configurations');const entries=[];for(const key of selected){const profile=profiles.find(p=>p.ID===key),read=await api(`/v2/configuration-profiles/${encodeURIComponent(key)}?revision_id=${encodeURIComponent(profile.CurrentRevisionID)}`);ensure();if(read.Profile.Revision!==profile.Revision)throw Error('Configuration changed; reopen export');entries.push({key,name:profile.Name,desired:read.Revision.Desired,startup:read.Revision.Startup,archived:!!profile.Archived,disabled:!!profile.Disabled,disabled_reason:profile.DisabledReason||''})}
   const exported=JSON.stringify({format:'tclaude-configuration-profiles',version:1,profiles:entries},null,2);if(new TextEncoder().encode(exported).length>limit)throw Error('Selection exceeds 1 MiB; export fewer configurations');const blob=new Blob([exported],{type:'application/json'}),url=URL.createObjectURL(blob),link=document.createElement('a');link.href=url;link.download='configurations.json';link.click();setTimeout(()=>URL.revokeObjectURL(url),1000);
  },
  async inspectProfiles(bundle){ensure();if(bundle?.format!=='tclaude-configuration-profiles'||bundle?.version!==1)throw Error('Unsupported bundle. Legacy profile files may contain settings this catalog cannot represent; they are not silently converted.');const result=await api('/v2/configuration-transfer/inspect',bundle);ensure();return result},
  async importProfileBundle(bundle,decisions){
   ensure();const intent=JSON.stringify({bundle,decisions});if(intent!==lastIntent){const selections=[];for(const row of bundle.profiles){const choice=decisions[row.key];if(!choice.include)continue;const target=profiles.find(p=>p.ID===choice.target);if(choice.action==='update'&&!target)throw Error('Choose an exact replacement target');if(!choice.name.trim())throw Error('Each selected configuration needs a name');selections.push({key:row.key,id:choice.action==='update'?target.ID:id(),revision_id:id(),expected_revision:choice.action==='update'?target.Revision:0,name:choice.name})}if(!selections.length)throw Error('Select at least one configuration');pending={request_id:id(),bundle,selections};lastIntent=intent}
   await api('/v2/configuration-transfer/import',pending);ensure();await refresh();
  }
 };
 render(html`<${mode==='export'?ProfileExport:ProfileImport} profiles=${profiles} actions=${actions} close=${close}/>`,host);
}
