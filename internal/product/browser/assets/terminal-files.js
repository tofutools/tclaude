class TerminalFiles {
 constructor({host,workspace,tools,api,el,button,requestID}) {
  Object.assign(this,{host,workspace,tools,api,el,button,requestID});this.generation=0;this.binding=null;this.intent=null;this.receipt=null;this.busy=false;
  this.target=el('p');this.status=el('p');this.status.setAttribute('role','status');this.pick=el('input');this.pick.type='file';this.pick.setAttribute('aria-label','Choose terminal upload');
  this.pick.onchange=()=>{this.choose(this.pick.files?.[0]);this.pick.value=''};
  this.upload=button('Upload to selected execution',()=>this.stage());this.insert=button('Insert uploaded path into draft',()=>this.insertPath());this.upload.onclick=async()=>{try{await this.stage()}catch(error){this.status.textContent=error.message}finally{this.update()}};
  this.drop=el('div','Drop one file or paste an image here to select it. Upload remains explicit.','terminal-file-drop');this.drop.tabIndex=0;this.drop.setAttribute('aria-label','Terminal upload drop zone');
  this.drop.ondragover=e=>{e.preventDefault()};this.drop.ondrop=e=>{e.preventDefault();if(e.dataTransfer.files.length===1)this.choose(e.dataTransfer.files[0]);else this.status.textContent='Select one file at a time.'};
  this.drop.onpaste=e=>{const files=[...(e.clipboardData?.files||[])];if(files.length){e.preventDefault();if(files.length===1)this.choose(files[0]);else this.status.textContent='Select one image at a time.'}};
  host.replaceChildren(el('summary','Upload a file to this execution'),this.target,el('p','Files are staged privately for this execution (up to 8 MiB each). Uploading does not send terminal input. Publication may finish after disconnection; a repeated request does not publish twice. Receipts do not guarantee future file availability.','muted'),this.pick,this.drop,this.upload,this.insert,this.status);
  const prior=workspace.onStatus;workspace.onStatus=()=>{prior?.();this.update()};document.addEventListener('workspace-signout',()=>this.clear());window.addEventListener('pagehide',()=>this.clear());this.update();
 }
 current(){const e=this.workspace.selected;return e?.state==='connected'&&e.socket?.readyState===WebSocket.OPEN&&e.canStageFile?e:null}
 same(binding){const e=this.current();return !!e&&binding?.id===e.id&&binding.socket===e.socket}
 update(){
  if(this.binding&&!this.same(this.binding))this.clearState();
  const e=this.current();this.target.textContent=e?`Upload target: ${e.label} · ${e.id}`:'Select a connected pane whose provider supports file staging.';
  this.pick.disabled=!e||this.busy;this.upload.disabled=!e||!this.intent||this.busy;this.insert.disabled=!e||!this.receipt||this.busy;
 }
 clearState(){this.generation++;this.binding=null;this.intent=null;this.receipt=null;this.busy=false;this.status.textContent='';this.pick.value=''}
 clear(){this.clearState();this.update()}
 choose(file){
  if(!file)return;const e=this.current();if(!e){this.status.textContent='This pane does not support file staging.';return}if(this.busy){this.status.textContent='Wait for this upload before choosing another file.';return}
  if(file.size<1||file.size>8*1024*1024){this.clear();this.status.textContent='Choose a nonempty file of at most 8 MiB.';return}
  if(!file.name||new TextEncoder().encode(file.name).length>240||/[\x00-\x1f\x7f/\\]/.test(file.name)){this.clear();this.status.textContent='The filename is not supported.';return}
  this.clearState();this.binding={id:e.id,socket:e.socket};this.intent={file,request:this.requestID()};this.status.textContent=`Selected ${file.name} (${file.size} bytes). Nothing uploaded or sent.`;this.update();
 }
 async stage(){
  if(this.busy||!this.intent||!this.same(this.binding))throw new Error('Choose a file for the current connected pane first.');
  const generation=this.generation,intent=this.intent,binding=this.binding;this.busy=true;this.update();
  try{
   const bytes=new Uint8Array(await intent.file.arrayBuffer());if(generation!==this.generation||!this.same(binding))return;
   let binary='';for(let offset=0;offset<bytes.length;offset+=32768)binary+=String.fromCharCode(...bytes.subarray(offset,offset+32768));
   const result=await this.api('/v2/terminal-files',{request_id:intent.request,execution_id:binding.id,filename:intent.file.name,content:btoa(binary)});
   if(generation!==this.generation||!this.same(binding))return;
   if(result.operation?.state==='succeeded'&&result.file?.ExecutionID===binding.id&&result.file.NativePath){this.receipt=result.file;this.status.textContent=`Uploaded ${result.file.Filename} · ${result.file.Size} bytes · SHA-256 ${result.file.SHA256}. Path: ${result.file.NativePath}. No input sent.`}
   else {this.receipt=null;this.status.textContent=`Upload ${result.operation?.state||'unknown'} · ${result.operation?.id||''} · ${result.operation?.result_code||'unknown'}. No path will be inserted. Retrying this request only reads its recorded outcome.`}
  }catch(error){if(generation===this.generation)this.status.textContent=`${error.message} Retry uses the same upload request; no new publication is automatic.`}
  finally{if(generation===this.generation){this.busy=false;this.update()}}
 }
 insertPath(){
  if(!this.receipt||!this.same(this.binding))throw new Error('Upload a file for this exact attachment first.');
  const path=this.receipt.NativePath,quoted="'"+path.replaceAll("'","'\\''")+"'",text=this.tools.draft.value+(this.tools.draft.value?' ':'')+quoted;
  this.tools.validate(text);this.tools.bind();this.tools.draft.value=text;this.tools.host.open=true;this.status.textContent='Path inserted into the draft. Review it and explicitly Send draft when ready; no input has been sent.';
 }
}
