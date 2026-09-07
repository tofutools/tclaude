'use strict';
class AttachmentPreview {
 constructor({api,el,button}){
  Object.assign(this,{api,el});this.generation=0;this.url=null;
  this.dialog=el('dialog');this.dialog.id='attachment-preview';this.dialog.setAttribute('aria-label','Attachment preview');
  this.title=el('h2');this.detail=el('p');this.content=el('div');this.status=el('p');this.status.setAttribute('role','status');
  this.dialog.append(this.title,this.detail,this.content,this.status,button('Close preview',()=>this.clear()));document.body.append(this.dialog);
  this.dialog.addEventListener('close',()=>{if(!this.dialog.open)this.reset()});
  window.addEventListener('pagehide',()=>this.clear());document.addEventListener('workspace-signout',()=>this.clear());
 }
 kind(attachment){const type=(attachment.MediaType||'').split(';')[0].trim().toLowerCase();return ['image/png','image/jpeg','image/gif','image/webp'].includes(type)?type:type.startsWith('text/')||type==='application/json'?'text':''}
 reset(){this.generation++;if(this.url)URL.revokeObjectURL(this.url);this.url=null;this.content.replaceChildren();this.title.textContent='';this.detail.textContent='';this.status.textContent=''}
 clear(){this.reset();if(this.dialog.open)this.dialog.close()}
 imageMatches(type,b){const ascii=(at,n)=>String.fromCharCode(...b.slice(at,at+n));return type==='image/png'?[137,80,78,71,13,10,26,10].every((v,i)=>b[i]===v):type==='image/jpeg'?b[0]===255&&b[1]===216&&b[2]===255:type==='image/gif'?['GIF87a','GIF89a'].includes(ascii(0,6)):ascii(0,4)==='RIFF'&&ascii(8,4)==='WEBP'}
 async open(attachment){
  this.clear();const generation=++this.generation,type=this.kind(attachment);this.title.textContent=attachment.Filename;this.detail.textContent=`${attachment.MediaType} · ${attachment.Size} bytes · ${attachment.ID}`;this.status.textContent='Loading attachment…';this.dialog.showModal();
  try{
   if(!type)throw new Error('Preview is unavailable for this file type. Use Download instead.');
   if(attachment.Size>5*1024*1024)throw new Error('Preview is limited to 5 MiB. Use Download instead.');
   const result=await this.api('/v2/attachments/'+encodeURIComponent(attachment.ID));if(generation!==this.generation)return;
   if(result.Attachment.ID!==attachment.ID||result.Attachment.SHA256!==attachment.SHA256)throw new Error('Attachment identity changed. Refresh the message before trying again.');
   const bytes=Uint8Array.from(atob(result.Content),c=>c.charCodeAt(0));if(bytes.length>5*1024*1024||bytes.length!==attachment.Size)throw new Error('Attachment size does not match its metadata.');
   if(type==='text'){
    const limit=64*1024,truncated=bytes.length>limit,decoder=new TextDecoder('utf-8',{fatal:true});const text=decoder.decode(bytes.subarray(0,limit),{stream:truncated});this.content.append(this.el('pre',text));this.status.textContent=truncated?'Showing the first 64 KiB. Download for the complete file.':'Complete UTF-8 text preview.';
   }else{
    if(!this.imageMatches(type,bytes))throw new Error('Image content does not match its declared type. Use Download to inspect it separately.');
    this.url=URL.createObjectURL(new Blob([bytes],{type}));const image=this.el('img');image.alt=attachment.Filename;image.className='attachment-preview-image';image.onload=()=>{if(generation===this.generation)this.status.textContent=`${image.naturalWidth} × ${image.naturalHeight} pixels`};image.onerror=()=>{if(generation===this.generation)this.status.textContent='This image could not be decoded. Use Download instead.'};image.src=this.url;this.content.append(image);this.status.textContent='Decoding image…';
   }
  }catch(e){if(generation===this.generation)this.status.textContent=e.message||String(e)}
 }
}
