class AttentionWorkspace {
 constructor({host,api,el,button,refresh,select}){
  Object.assign(this,{host,api,el,button,refresh,select});this.seen=new Set();this.notices=new Set();this.ready=false;this.desktop=false;this.generation=0;this.messages=[];this.decisions=[];
  this.summary=el('summary','Attention');this.list=el('div');this.status=el('p','Automatic refresh starts after sign-in.');this.status.setAttribute('role','status');this.toggle=button('Enable desktop notifications',()=>this.enableDesktop());this.toggle.setAttribute('aria-pressed','false');
  host.replaceChildren(this.summary,this.toggle,this.status,this.list);
  document.addEventListener('visibilitychange',()=>{if(this.running&&!document.hidden)this.poll()});
 }
 async enableDesktop(){
  if(this.desktop){this.desktop=false;this.toggle.textContent='Enable desktop notifications';this.toggle.setAttribute('aria-pressed','false');return}
  if(!('Notification' in window)){this.status.textContent='Desktop notifications are not supported by this browser.';return}
  const generation=this.generation;const permission=await Notification.requestPermission();if(generation!==this.generation)return;
  this.desktop=permission==='granted';this.toggle.textContent=this.desktop?'Disable desktop notifications':'Enable desktop notifications';this.toggle.setAttribute('aria-pressed',String(this.desktop));this.status.textContent=this.desktop?'Desktop notifications enabled for new operator messages in this window.':'Desktop notification permission was not granted. Attention remains available here.';
 }
 update(snapshot){
  const unread=(snapshot.messages||[]).filter(m=>(m.Recipients||[]).some(r=>r.AddressKind==='operator'&&!r.ReadAt));
  if(this.ready&&this.desktop)for(const m of unread)if(!this.seen.has(m.ID)){
   try{const n=new Notification('tclaude: new message',{body:(m.Subject||m.Body||'New operator message').slice(0,160),tag:'tclaude-'+m.ID});this.notices.add(n);n.onclose=()=>this.notices.delete(n);n.onclick=()=>{window.focus();this.select('messages').catch(e=>this.error(e));n.close()}}catch(e){this.status.textContent='Desktop notification unavailable; the message remains in Attention.'}
  }
  for(const m of snapshot.messages||[])this.seen.add(m.ID);this.ready=true;this.messages=unread;this.render();
 }
 render(){
  const {el,button}=this,now=Date.now();const pending=this.decisions.filter(d=>!d.expires||new Date(d.expires).getTime()>now);
  this.summary.textContent=`Attention · ${this.messages.length} unread · ${pending.length} decisions`;this.summary.setAttribute('aria-label',this.summary.textContent);this.list.replaceChildren();
  for(const m of this.messages.slice(0,10)){const row=el('div',undefined,'row');row.append(el('span',m.Subject||m.Body?.slice(0,120)||m.ID),button('Open messages',()=>this.select('messages')));this.list.append(row)}
  for(const d of pending.slice(0,10)){const row=el('div',undefined,'row');row.append(el('span',d.text),button('Open decisions',()=>this.select('decisions')));this.list.append(row)}
  if(!this.messages.length&&!pending.length)this.list.append(el('p','No unread operator messages or current decisions.'));
  if(this.messages.length>10||pending.length>10)this.list.append(el('p','Showing the first ten of each kind. Open the workspace to inspect all items.'));
 }
 error(e){this.status.textContent=`Automatic refresh failed: ${e.message||e}. Use Refresh to retry.`}
 async poll(){
  if(!this.running||this.busy||(document.hidden&&!this.desktop))return;this.busy=true;const generation=this.generation;
  try{
   await this.refresh({automatic:true});if(generation!==this.generation)return;
   const [work,access]=await Promise.all([this.api('/v2/decisions'),this.api('/v2/access-requests?pending_only=true')]);if(generation!==this.generation)return;
   this.decisions=[...(work||[]).filter(x=>x.Window.State==='open').map(x=>({id:x.Window.ID,text:x.Window.Question||x.Window.ID,expires:x.Window.ExpiresAt})),...(access.requests||[]).filter(x=>x.decision.state==='open').map(x=>({id:x.decision.decision_id,text:`Access: ${x.request.action} · ${x.request.requester.agent_id||x.request.requester.execution_id}`,expires:x.decision.expires_at}))];
   this.render();this.status.textContent=`Checked ${new Date().toLocaleTimeString()} · automatic refresh every 10 seconds while visible${this.desktop?' or notifications are enabled':''}.`;
  }catch(e){if(generation===this.generation)this.error(e)}finally{this.busy=false}
 }
 start(){if(this.running)return;this.running=true;this.poll();this.timer=setInterval(()=>this.poll(),10000)}
 stop(){this.running=false;this.generation++;clearInterval(this.timer)}
 clear(){this.stop();for(const n of this.notices)n.close();this.notices.clear();this.desktop=false;this.ready=false;this.seen.clear();this.messages=[];this.decisions=[];this.render();this.status.textContent='Signed out';this.toggle.textContent='Enable desktop notifications';this.toggle.setAttribute('aria-pressed','false')}
}
