'use strict';
class MessageWorkspace {
 constructor({host,el,button,api,refresh,card}){
  Object.assign(this,{host,el,button,api,refresh,card});this.closed=new Set();this.limit=50;
  const controls=el('form',undefined,'toolbar');controls.onsubmit=e=>e.preventDefault();
  this.search=el('input');this.search.placeholder='Search messages';this.search.setAttribute('aria-label','Search messages');
  this.audience=el('select');this.audience.setAttribute('aria-label','Message audience');
  for(const[value,label]of [['all','All visible messages'],['operator','Operator inbox'],['unread','Operator unread']]){const option=el('option',label);option.value=value;this.audience.append(option)}
  this.agent=el('select');this.agent.setAttribute('aria-label','Message agent');
  this.attachments=el('input');this.attachments.type='checkbox';this.attachments.setAttribute('aria-label','With attachments');const attachmentLabel=el('label','With attachments ');attachmentLabel.prepend(this.attachments);
  controls.append(this.search,this.audience,this.agent,attachmentLabel);
  for(const field of [this.search,this.audience,this.agent,this.attachments])field.oninput=()=>{this.limit=50;this.draw()};
  this.count=el('p');this.report=el('p');this.report.role='status';this.list=el('div');
  this.read=el('button','Mark matching operator messages read');this.read.type='button';this.read.onclick=()=>this.markRead().catch(error=>{this.report.textContent+=' Refresh failed: '+error.message});
  host.replaceChildren(controls,this.count,this.read,this.report,this.list);
 }
 update(snapshot){this.snapshot=snapshot;const prior=this.agent.value;this.agent.replaceChildren();
  const options=[['','Any agent'],...(snapshot.agents||[]).map(a=>[a.ID,a.Name])];for(const[value,label]of options){const option=this.el('option',label);option.value=value;this.agent.append(option)}if(options.some(([id])=>id===prior))this.agent.value=prior;this.draw();
 }
 draw(){if(!this.snapshot)return;const messages=this.snapshot.messages||[],query=this.search.value.trim().toLowerCase(),names=new Map((this.snapshot.agents||[]).map(a=>[a.ID,a.Name]));
  this.matches=messages.filter(m=>{
   const operator=(m.Recipients||[]).filter(r=>r.AddressKind==='operator');
   return(!query||[m.Subject,m.Body,m.Sender.AgentID,m.Sender.Kind,names.get(m.Sender.AgentID),...(m.Recipients||[]).map(r=>names.get(r.AgentID)),...(m.Attachments||[]).map(a=>a.Filename)].join(' ').toLowerCase().includes(query))&&
    (this.audience.value==='all'||operator.some(r=>this.audience.value==='operator'||!r.ReadAt))&&
    (!this.agent.value||m.Sender.AgentID===this.agent.value||(m.Recipients||[]).some(r=>r.AgentID===this.agent.value))&&(!this.attachments.checked||m.Attachments?.length);
  });
  const matching=new Set(this.matches.map(m=>m.ID)),threads=new Map();
  for(const m of messages){const id=m.ThreadID||m.ID;if(!threads.has(id))threads.set(id,[]);threads.get(id).push(m)}
  const latest=items=>items.reduce((value,m)=>Math.max(value,Date.parse(m.CreatedAt)||0),0);
  const visible=[...threads].filter(([,items])=>items.some(m=>matching.has(m.ID))).sort((a,b)=>latest(b[1])-latest(a[1]));
  this.count.textContent=`${this.matches.length} matching messages · ${visible.length} threads. Threads include available reply context.`;
  this.read.disabled=this.busy||!this.matches.some(m=>(m.Recipients||[]).some(r=>r.AddressKind==='operator'&&!r.ReadAt));this.list.replaceChildren();
  for(const[id,items]of visible.slice(0,this.limit)){
   items.sort((a,b)=>Date.parse(a.CreatedAt)-Date.parse(b.CreatedAt)||a.ID.localeCompare(b.ID));
   const details=this.el('details',undefined,'message-thread');details.dataset.thread=id;details.open=!this.closed.has(id);
   const summary=this.el('summary',`${items[0].Subject||'Message thread'} · ${items.length} messages`);details.append(summary);
   details.ontoggle=()=>{if(!details.isConnected)return;details.open?this.closed.delete(id):this.closed.add(id)};
   details.append(this.button('Export thread text',()=>this.exportThread(id,items)));
   for(const m of items){const card=this.card(m);card.dataset.message=m.ID;if(!matching.has(m.ID))card.prepend(this.el('p','Thread context','muted'));details.append(card)}this.list.append(details);
  }
  if(visible.length>this.limit)this.list.append(this.button('Show more threads',()=>{this.limit+=50;this.draw()}));
  if(!visible.length)this.list.append(this.el('p',messages.length?'No messages match these filters.':'No messages.'));
 }
 async markRead(){if(this.busy)return;const targets=this.matches.filter(m=>(m.Recipients||[]).some(r=>r.AddressKind==='operator'&&!r.ReadAt));this.busy=true;this.draw();const failures=[];
  try{for(const m of targets){try{await this.api('/v2/messages/'+encodeURIComponent(m.ID)+'/read',{request_id:'read_'+crypto.randomUUID(),operator:true})}catch(error){failures.push(`${m.Subject||m.ID}: ${error.message}`)}}
   this.report.textContent=`Marked ${targets.length-failures.length} messages read.${failures.length?' Failed: '+failures.join('; '):''}`;
  }finally{this.busy=false;await this.refresh();this.draw()}
 }
 exportThread(id,items){const text=items.map(m=>`${m.Subject||'Message'}\n${m.Sender.AgentID||m.Sender.Kind} · ${m.CreatedAt}\nTo/CC: ${(m.Recipients||[]).map(r=>`${r.Audience||'to'} ${r.AddressKind==='operator'?'Operator':r.AgentID}`).join(', ')}\n\n${m.Body}\n\nAttachments: ${(m.Attachments||[]).map(a=>`${a.Filename} (${a.Size} bytes, SHA256 ${a.SHA256})`).join(', ')||'none'}`).join('\n\n---\n\n');const url=URL.createObjectURL(new Blob([text],{type:'text/plain;charset=utf-8'}));const link=this.el('a');link.href=url;link.download=`thread-${id}.txt`;link.click();setTimeout(()=>URL.revokeObjectURL(url),1000);}
}
