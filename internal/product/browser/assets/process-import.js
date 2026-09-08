import {freshID,clone} from './process-model.js';
import {openProcessEditor} from './process-editor.js';
const el=(tag,text)=>{const node=document.createElement(tag);if(text!==undefined)node.textContent=text;return node};
const button=(text,click)=>{const b=el('button',text);b.type='button';b.onclick=click;return b};
const option=(value,text)=>{const o=el('option',text);o.value=value;return o};
let currentClose=null;
export function openLegacyProcessImport({api,agents=[],onSaved}){
 currentClose?.();
 const dialog=el('dialog');dialog.id='process-import';dialog.setAttribute('aria-label','Import legacy process');
 const source=el('textarea');source.setAttribute('aria-label','Legacy process source');source.rows=12;
 const file=el('input');file.type='file';file.accept='.yaml,.yml,.json';file.setAttribute('aria-label','Legacy process file');
 const status=el('p');status.role='status';const errors=el('p');errors.role='alert';
 const preview=el('div');preview.id='process-import-preview';
 let live=true,generation=0,inspection=null,accepted='',rows=[],configurations=[],programs=[],pendingDraft=null;
 const close=()=>{if(!live)return;live=false;generation++;dialog.close();dialog.remove();document.removeEventListener('workspace-signout',close);window.removeEventListener('pagehide',close);window.removeEventListener('beforeunload',unload);if(currentClose===close)currentClose=null};
 const unload=e=>{if(source.value){e.preventDefault();e.returnValue=''}};
 const cancel=()=>{if(!source.value||confirm('Discard this import draft?'))close()};
 const invalidate=()=>{generation++;inspection=null;accepted='';rows=[];pendingDraft=null;preview.replaceChildren();convert.disabled=true;open.disabled=true;status.textContent='';errors.textContent=''};
 const fail=e=>{errors.textContent=e.message||String(e)};
 source.oninput=invalidate;
 file.onchange=async()=>{invalidate();source.value='';const selected=file.files?.[0],token=generation;if(!selected)return;try{if(selected.size>1024*1024)throw Error('Source exceeds 1 MiB');const text=new TextDecoder('utf-8',{fatal:true}).decode(await selected.arrayBuffer());if(live&&token===generation)source.value=text}catch(e){if(live&&token===generation)fail(e)}};
 const inspect=button('Inspect source',async()=>{
  invalidate();const token=generation,text=source.value;
  try{
   if(new TextEncoder().encode(text).length>1024*1024)throw Error('Source exceeds 1 MiB');
   const result=await api('/v2/process-import/inspect',{source:text});if(!live||token!==generation)return;
   status.textContent=`${result.Name||result.OriginalID||'Process'} · source ${result.SourceHash}`;
   for(const d of result.Diagnostics||[])preview.append(el('p',`${d.severity}: ${d.path||''} ${d.message}`));
   if((result.Diagnostics||[]).some(d=>d.severity==='error'))return;
   const [catalog,programCatalog]=await Promise.all([api('/v2/configuration-profiles'),api('/v2/program-profiles')]);
   const [cs,ps]=await Promise.all([Promise.all((catalog||[]).filter(p=>!p.Archived).map(p=>api('/v2/configuration-profiles/'+encodeURIComponent(p.ID)+'?revision_id='+encodeURIComponent(p.CurrentRevisionID)))),Promise.all((programCatalog||[]).map(p=>api('/v2/program-profiles/'+encodeURIComponent(p.ID))))]);
   if(!live||token!==generation)return;configurations=cs;programs=ps;inspection=result;accepted=text;
   for(const r of result.Requirements||[]){
    const card=el('fieldset');card.dataset.path=r.Path;card.append(el('legend',`${r.Path} · ${r.Kind}`),el('p',`Source profile: ${r.Profile||'(none)'}; assignee: ${r.Assignee||'(none)'}`));
    const select=el('select');select.setAttribute('aria-label','Mapping for '+r.Path);select.append(option('','Choose an explicit mapping'));
    const role=el('input');role.setAttribute('aria-label','Role ID for '+r.Path);role.placeholder='Exact role ID';role.hidden=true;
    if(r.Kind==='human'){select.append(option('operator','Operator'),option('role','Exact role'));agents.forEach(a=>select.append(option('agent:'+a.ID,a.Name+' · '+a.ID)))}
    if(r.Kind==='agent')configurations.forEach((c,i)=>select.append(option(String(i),c.Profile.Name+' · '+c.Revision.Ref.RevisionID)));
    if(r.Kind==='program')programs.forEach((p,i)=>{if(p.Revision.Executable===r.Executable&&!(p.Revision.ArgumentPrefix||[]).length)select.append(option(String(i),p.Profile.Name+' · '+p.Revision.ID))});
    const details=el('pre');
    const changed=()=>{generation++;pendingDraft=null;preview.querySelector('.process-import-notes')?.remove();open.disabled=true;convert.disabled=false;role.hidden=select.value!=='role';errors.textContent='';details.textContent='';if(select.value!==''&&r.Kind==='agent')details.textContent=JSON.stringify(configurations[Number(select.value)].Revision.Desired,null,2);if(select.value!==''&&r.Kind==='program')details.textContent=JSON.stringify(programs[Number(select.value)].Revision,null,2)};
    select.onchange=changed;role.oninput=changed;
    card.append(select,role,details);
    const timeout=el('input');timeout.setAttribute('aria-label','Decision timeout for '+r.Path);timeout.placeholder='Explicit decision timeout, e.g. 1h';timeout.oninput=changed;if(r.Decision)card.append(timeout);
    if(r.Kind==='agent')card.append(el('p',`Independent worker settings; source model ${r.Model||'(unchanged)'} and effort ${r.Effort||'(unchanged)'} override the selected copy. No worker is created.`));
    if(r.Kind==='program')card.append(el('p',`Literal executable: ${r.Executable}. The selected profile supplies its displayed environment, working directory, timeout and authority requirements; arguments come from Source.`));
    preview.append(card);rows.push({r,select,role,timeout});
   }
   convert.disabled=false;
  }catch(e){if(live&&token===generation)fail(e)}
 });
 const convert=button('Preview converted draft',async()=>{
  if(!inspection||source.value!==accepted)return;const token=++generation;pendingDraft=null;open.disabled=true;convert.disabled=true;errors.textContent='';
  try{
   const bindings={};for(const {r,select,role,timeout}of rows){if(select.value==='')throw Error('Choose a mapping for '+r.Path);let performer;
    if(r.Kind==='human'){const h=select.value==='operator'?{Operator:true}:select.value==='role'?{RoleID:role.value}:{AgentID:select.value.slice(6)};performer={Kind:'human',Human:h}}
    if(r.Kind==='agent')performer={Kind:'agent',Agent:{CreateDesired:clone(configurations[Number(select.value)].Revision.Desired)}};
    if(r.Kind==='program'){const p=programs[Number(select.value)];performer={Kind:'program',Program:{Profile:{ProfileID:p.Profile.ID,RevisionID:p.Revision.ID,ContentHash:p.Revision.ContentHash}}}}
    bindings[r.Path]={Performer:performer,DecisionTimeout:timeout.value};
   }
   const result=await api('/v2/process-import/convert',{source:accepted,id:freshID('definition_'),bindings});if(!live||token!==generation)return;
   pendingDraft=result.Draft;status.textContent='Converted draft is unsaved. Review the mapping notes and open the editor to save explicitly.';
   const notes=el('div');notes.className='process-import-notes';for(const note of result.Notices||[])notes.append(el('p',note));notes.append(el('pre',JSON.stringify(result.Draft.Process,null,2)));preview.querySelector('.process-import-notes')?.remove();preview.append(notes);open.disabled=false;
  }catch(e){if(live&&token===generation)fail(e)}finally{if(live&&token===generation)convert.disabled=false}
 });convert.disabled=true;
 const open=button('Open unsaved copy',async()=>{if(!pendingDraft)return;const token=generation;open.disabled=true;try{await openProcessEditor({api,draft:pendingDraft,agents,onSaved,canOpen:()=>live&&token===generation});if(live&&token===generation)close()}catch(e){if(live&&token===generation){fail(e);open.disabled=false}}});open.disabled=true;
 dialog.append(el('h2','Import legacy process'),el('p','Inspect YAML or JSON without saving or starting work. Choose exact target identities and configurations, then review an independent draft. Unsupported semantics are reported; they are never silently discarded.'),file,source,inspect,status,errors,preview,convert,open,button('Cancel',cancel));
 dialog.addEventListener('cancel',e=>{e.preventDefault();cancel()});document.addEventListener('workspace-signout',close);window.addEventListener('pagehide',close);window.addEventListener('beforeunload',unload);document.body.append(dialog);dialog.showModal();currentClose=close;
}
