import {applyDurationProjection,wireDurationNodes} from './process-durations.js';
import {clone, freshID} from './process-model.js';
const node=(tag,text)=>{const n=document.createElement(tag);if(text!==undefined)n.textContent=text;return n};

export class ProcessSnippetLibrary {
 constructor({api,selection,insert}){
  this.api=api;this.selection=clone(selection);this.insert=insert;this.generation=0;this.busy=false;
  this.dialog=node('dialog');this.dialog.id='process-snippets';this.dialog.setAttribute('aria-label','Saved process snippets');
  const heading=node('h2','Saved process snippets'),help=node('p','Selections are reusable draft fragments. Inserting makes new node IDs; validate and save the process separately. Pinned performers and profiles may need updating.');
  this.status=node('p');this.status.role='status';this.list=node('div');
  this.name=node('input');this.name.setAttribute('aria-label','New snippet name');this.name.maxLength=80;
  const save=node('button','Save selected nodes');save.disabled=!selection.nodes.length;
  const id=freshID('snippet_');save.onclick=()=>this.write(save,id,{action:'create',name:this.name.value.trim(),selection:this.selection,expected_revision:0});
  const reload=node('button','Reload snippets');reload.onclick=()=>this.load();const close=node('button','Close snippets');close.onclick=()=>this.close();
  this.dialog.append(heading,help,this.name,save,reload,close,this.status,this.list);document.body.append(this.dialog);this.dialog.showModal();
  this.dialog.addEventListener('cancel',e=>{e.preventDefault();this.close()});this.onLeave=()=>this.close();window.addEventListener('pagehide',this.onLeave);document.addEventListener('workspace-signout',this.onLeave);this.load();
 }
 close(){this.generation++;this.dialog.close();this.dialog.remove();window.removeEventListener('pagehide',this.onLeave);document.removeEventListener('workspace-signout',this.onLeave)}
 async load(force=false){if((this.busy&&!force)||!this.dialog.isConnected)return;const generation=++this.generation;this.status.textContent='Loading snippets…';try{const items=await this.api('/v2/process-snippets');if(generation!==this.generation||!this.dialog.isConnected)return;this.render(items);this.status.textContent=items.length+' saved snippets'}catch(e){if(generation===this.generation&&this.dialog.isConnected)this.status.textContent=e.message}}
 render(items){this.list.replaceChildren();for(const item of items){const row=node('section'),title=node('h3',item.Name),id=node('code',item.ID),name=node('input');name.value=item.Name;name.maxLength=80;name.setAttribute('aria-label','Rename '+item.Name);
  const insert=node('button','Insert '+item.Name);insert.disabled=!item.Available;insert.onclick=()=>{if(this.busy)return;const selection=clone(item.Selection);applyDurationProjection(selection.nodes,item.ProcessDurationNS);this.insert(selection);this.close()};
  const rename=node('button','Rename snippet');rename.onclick=()=>this.write(rename,item.ID,{action:'rename',name:name.value.trim(),expected_revision:item.Revision});
  const remove=node('button','Delete snippet');remove.onclick=()=>{if(!this.busy&&confirm('Delete snippet '+item.Name+' ('+item.ID+')? Existing process drafts and revisions are retained.'))this.write(remove,item.ID,{action:'delete',expected_revision:item.Revision})};
  row.append(title,id,node('p','Revision '+item.Revision),insert,name,rename,remove);if(!item.Available)row.append(node('p','Unavailable stored format. Rename or delete this entry; it cannot be inserted.'));this.list.append(row)
 }}
 async write(button,id,body){if(this.busy||!this.dialog.isConnected)return;const fingerprint=JSON.stringify(body);if(button.intent!==fingerprint){button.intent=fingerprint;button.requestID=freshID('request_');if(body.action==='create')button.targetID=freshID('snippet_')}if(body.action==='create')id=button.targetID;
  this.busy=true;button.disabled=true;const generation=++this.generation;this.status.textContent='Saving snippet…';try{await this.api('/v2/process-snippets/'+encodeURIComponent(id),{...body,...(body.selection?{selection:{...body.selection,nodes:wireDurationNodes(body.selection.nodes)}}:{}),request_id:button.requestID});if(generation!==this.generation||!this.dialog.isConnected)return;await this.load(true)}catch(e){if(generation===this.generation&&this.dialog.isConnected)this.status.textContent=e.message+' Reload to inspect current revisions; your process draft is unchanged.'}finally{this.busy=false;button.disabled=false}
 }
}
