class TerminalDownloads {
 constructor({host,workspace,el,button}){
  Object.assign(this,{host,workspace,el,button});this.binding=null;this.generation=0;this.controller=null;this.url=null;
  this.path=el('input');this.path.setAttribute('aria-label','Download execution file path');this.path.placeholder='Relative path or absolute path inside this execution’s working directory';
  this.status=el('p');this.status.setAttribute('role','status');this.target=el('p');this.download=button('Download execution file',()=>this.read(this.path.value));this.download.onclick=async()=>{try{await this.read(this.path.value)}catch(error){this.status.textContent=error.message}finally{this.update()}};
  host.replaceChildren(el('summary','Download an execution file'),this.target,el('p','Explicitly download a regular file (up to 32 MiB) inside the active execution’s working directory. Ctrl/⌘-click an embedded file link to download; other paths fail visibly. No terminal input is sent.','muted'),this.path,this.download,this.status);
  const prior=workspace.onStatus;workspace.onStatus=()=>{prior?.();this.update()};workspace.onLink=(entry,event,text)=>this.link(entry,event,text);document.addEventListener('workspace-signout',()=>this.clear());window.addEventListener('pagehide',()=>this.clear());this.update();
 }
 current(){const e=this.workspace.selected;return e?.state==='connected'&&e.socket?.readyState===WebSocket.OPEN?e:null}
 same(binding){const e=this.current();return !!e&&binding?.id===e.id&&binding.socket===e.socket}
 update(){if(this.binding&&!this.same(this.binding))this.clearState();const e=this.current();this.target.textContent=e?`File source: ${e.label} · ${e.id}`:'Connect and select an execution first.';this.path.disabled=!e;this.download.disabled=!e||!!this.controller}
 clearState(){this.generation++;this.controller?.abort();this.controller=null;this.binding=null;this.path.value='';this.status.textContent='';if(this.url){URL.revokeObjectURL(this.url);this.url=null}}
 clear(){this.clearState();this.update()}
 parse(raw){
  if(typeof raw!=='string'||/[\x00-\x1f\x7f]/.test(raw))return null;
  if(raw.startsWith('/')&&!raw.startsWith('//'))return {kind:'file',value:raw};
  try{const u=new URL(raw);if(u.username||u.password)return null;if(['http:','https:'].includes(u.protocol))return{kind:'http',value:u.href};if(u.protocol==='file:'&&(!u.hostname||u.hostname==='localhost')&&!u.search&&!u.hash){const path=decodeURIComponent(u.pathname);if(path.startsWith('/')&&!/[\x00-\x1f\x7f]/.test(path))return{kind:'file',value:path}}}catch{}return null;
 }
 link(entry,event,raw){
  if(this.current()!==entry)return;const link=this.parse(raw);if(!link){this.status.textContent='Blocked unsupported terminal link.';this.host.open=true;return}
  if(!event?.ctrlKey&&!event?.metaKey){this.status.textContent=`Ctrl/⌘-click to ${link.kind==='file'?'download':'open'}: ${link.value}`;this.host.open=true;return}
  event.preventDefault?.();if(link.kind==='http'){window.open(link.value,'_blank','noopener,noreferrer');return}
  this.host.open=true;this.path.value=link.value;this.read(link.value).catch(error=>{this.status.textContent=error.message});
 }
 async read(path){
  const entry=this.current();if(!entry)throw new Error('Connect and select an execution first.');if(this.controller)throw new Error('A file download is already pending.');if(!path||/[\x00-\x1f\x7f]/.test(path))throw new Error('Enter a valid file path.');
  if(this.url){URL.revokeObjectURL(this.url);this.url=null}
  const binding={id:entry.id,socket:entry.socket},generation=++this.generation,controller=new AbortController();this.binding=binding;this.controller=controller;this.update();
  try{
   const query=new URLSearchParams({execution_id:entry.id,path});const response=await fetch('/v2/execution-files?'+query,{credentials:'same-origin',cache:'no-store',signal:controller.signal});
   if(!response.ok){let code='';try{code=(await response.json()).code}catch{};throw new Error(code==='forbidden'?'Current authority does not allow file reads for this execution.':code==='unsupported'?'File downloads are not supported by this backend.':'File unavailable: check the path, execution state, directory boundary and 32 MiB limit.')}
   const length=response.headers.get('Content-Length'),declared=Number(length);if(length===null||!Number.isSafeInteger(declared)||declared<0||declared>32*1024*1024)throw new Error('Invalid download size.');
   const reader=response.body.getReader(),chunks=[];let size=0;try{for(;;){const {done,value}=await reader.read();if(done)break;size+=value.byteLength;if(size>declared||size>32*1024*1024)throw new Error('Download exceeded its size bound.');chunks.push(value)}}finally{await reader.cancel().catch(()=>{})}
   if(size!==declared)throw new Error('Incomplete download.');if(generation!==this.generation||!this.same(binding))return;
   const url=URL.createObjectURL(new Blob(chunks,{type:'application/octet-stream'}));this.url=url;const a=this.el('a');a.href=url;a.download=path.split('/').pop()||'download';a.click();this.status.textContent=`Downloaded ${path} · ${size} bytes from ${entry.label} · ${entry.id}. No input sent.`;setTimeout(()=>{URL.revokeObjectURL(url);if(this.url===url)this.url=null},1000);
  }catch(error){if(generation===this.generation&&error.name!=='AbortError')this.status.textContent=error.message}
  finally{if(generation===this.generation){this.controller=null;this.update()}}
 }
}
