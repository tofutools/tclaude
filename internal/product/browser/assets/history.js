class HistoryWorkspace {
 constructor({host,api,el,button,edit,startWork,selection}){
  Object.assign(this,{host,api,el,button,edit,startWork,selection});this.generation=0;this.pageSize=30;
  const label=(text,node)=>{const l=el('label',text);l.append(node);return l};
  this.query=el('input');this.query.name='query';this.query.setAttribute('aria-label','Search history');
  const select=(name,values)=>{const s=el('select');s.setAttribute('aria-label',name);for(const [value,text] of values){const o=el('option',text);o.value=value;s.append(o)}return s};
  this.harness=select('History harness',[['','All harnesses'],...['claude','codex','opencode','copilot'].map(x=>[x,x])]);
  this.archive=select('History archive',[['active','Active'],['archived','Archived'],['all','All conversations']]);
  this.sort=select('History order',[['recent','Recently changed'],['title','Title']]);
  const form=el('form',undefined,'toolbar'),submit=el('button','Search');submit.type='submit';form.append(label('Search',this.query),label('Harness',this.harness),label('Show',this.archive),label('Order',this.sort),submit);
  this.status=el('p');this.status.setAttribute('role','status');this.list=el('div');this.detail=el('article',undefined,'card');this.detail.hidden=true;
  host.replaceChildren(form,this.status,this.list,this.detail);form.onsubmit=e=>{e.preventDefault();this.load().catch(e=>this.error(e))};for(const control of [this.harness,this.archive,this.sort])control.onchange=()=>this.load().catch(e=>this.error(e));
 }
 clear(){this.generation++;this.readGeneration=(this.readGeneration||0)+1;this.entries=[];this.list.replaceChildren();this.detail.replaceChildren();this.detail.hidden=true;this.status.textContent='Signed out'}
 error(e){this.status.textContent=e.message||String(e)}
 async load(){
  const generation=++this.generation;this.status.textContent='Loading history…';this.list.replaceChildren();
  const request={query:this.query.value,harness:this.harness.value};if(this.archive.value!=='all')request.archived=this.archive.value==='archived';
  const result=await this.api('/v2/history/search',request);if(generation!==this.generation)return;
  this.entries=[...(result.Entries||[])];this.entries.sort(this.sort.value==='title'?(a,b)=>(a.Title||a.ConversationID).localeCompare(b.Title||b.ConversationID):(a,b)=>(b.ModifiedAt||'').localeCompare(a.ModifiedAt||''));this.visible=this.pageSize;this.coverage=result.Coverage;this.render();
 }
 coverageText(c){return `Metadata: ${c?.Metadata||'unknown'} · content: ${c?.Content||'unknown'}${c?.RefreshedAt?' · source observed '+c.RefreshedAt:''}`}
 render(){
  const {el,button}=this;this.status.textContent=`${this.entries.length} conversations · ${this.coverageText(this.coverage)}. Search covers catalogued metadata and previously indexed text.`;this.list.replaceChildren();
  for(const entry of this.entries.slice(0,this.visible)){
   const card=el('article',undefined,'card');card.dataset.conversation=entry.ConversationID;card.append(el('strong',entry.Title||entry.ConversationID),el('p',`${entry.ConversationID} · ${entry.Harness} · ${entry.Availability||'unknown'}${entry.Archived?' · archived':''}`),el('p',entry.WorkspaceHint||entry.WorkspaceID||'No workspace recorded','muted'),el('p',this.coverageText(entry.Coverage),'muted'));
   card.append(button('Read conversation',()=>this.read(entry)),button('Edit title',()=>this.metadata(entry,false)),button(entry.Archived?'Restore from archive':'Archive conversation',()=>this.metadata(entry,true)));this.list.append(card);
  }
  if(!this.entries.length)this.list.append(el('p','No matching catalogued histories. Incomplete source coverage is not proof that no history exists.'));
  if(this.visible<this.entries.length)this.list.append(button('Show more conversations',()=>{this.visible+=this.pageSize;this.render()}));
 }
 metadata(entry,toggle){return this.edit(toggle?(entry.Archived?'Restore conversation':'Archive conversation'):'History title',toggle?[{name:'confirm',label:'Conversation',options:[{value:entry.ConversationID,label:`${entry.Title||entry.ConversationID} · ${entry.ConversationID}`}]}]:[{name:'title',label:'Title',value:entry.Title||'',required:false}],async f=>{await this.api('/v2/history/metadata',{request_id:f.requestID,conversation_id:entry.ConversationID,expected_revision:entry.Revision,title:toggle?entry.Title:f.title,archived:toggle?!entry.Archived:!!entry.Archived});await this.load()})}
 async read(entry,point){
  const ticket=(this.readGeneration||0)+1;this.readGeneration=ticket;
  const read=await this.api('/v2/history/read',{selection:this.selection(entry,point)});if(ticket!==this.readGeneration)return;this.showRead(read);
 }
 partText(part){if(part.Omitted)return `[${part.Kind||'part'} omitted${part.MediaType?' · '+part.MediaType:''}]`;return part.Text||`[${part.Kind||'unsupported'}${part.MediaType?' · '+part.MediaType:''}]`}
 exportText(read){return [`${read.Entry.Title||read.Entry.ConversationID}\n${read.Entry.ConversationID}\n${this.coverageText(read.Coverage)}`,read.Point?`Selected ${read.Point.Kind} · ${read.Point.ID}`:'Selected persisted head',...(read.Turns||[]).map(t=>`${t.Role}\n${(t.Parts||[]).map(p=>this.partText(p)).join('\n')}`)].join('\n\n')}
 showRead(read){
  const {el,button}=this;this.detail.hidden=false;this.detail.replaceChildren();this.detail.dataset.conversation=read.Entry.ConversationID;
  this.detail.append(el('h2',read.Entry.Title||read.Entry.ConversationID),el('p',this.coverageText(read.Coverage)));
  const points=el('select');points.setAttribute('aria-label','History read point');for(const p of [{ID:'',Kind:'persisted head'},...(read.Points||[])]){const o=el('option',`${p.Kind}${p.ID?' · '+p.ID:''}`);o.value=p.ID;points.append(o)}points.value=read.Point?.ID||'';
  const search=el('input');search.setAttribute('aria-label','Find in conversation');search.placeholder='Find in this conversation';const roles=el('select');roles.setAttribute('aria-label','Conversation role');for(const role of ['',...new Set((read.Turns||[]).map(t=>t.Role))]){const o=el('option',role||'All roles');o.value=role;roles.append(o)}
  this.detail.append(points,button('Read selected point',()=>this.read(read.Entry,(read.Points||[]).find(p=>p.ID===points.value))),search,roles);
  const count=el('p'),turns=el('div');let visible=50;const draw=()=>{const matches=(read.Turns||[]).filter(t=>(!roles.value||t.Role===roles.value)&&(!search.value||JSON.stringify(t.Parts||[]).toLowerCase().includes(search.value.toLowerCase())));count.textContent=`${matches.length} matching turns of ${(read.Turns||[]).length}`;turns.replaceChildren();for(const t of matches.slice(0,visible)){const row=el('section',undefined,'history-turn');row.append(el('strong',t.Role));for(const p of t.Parts||[])row.append(el('pre',this.partText(p)));turns.append(row)}if(visible<matches.length)turns.append(button('Show more turns',()=>{visible+=50;draw()}))};search.oninput=roles.onchange=()=>{visible=50;draw()};draw();
  this.detail.append(count,turns,button('Export conversation text',()=>{const url=URL.createObjectURL(new Blob([this.exportText(read)],{type:'text/plain;charset=utf-8'}));const a=el('a');a.href=url;a.download=`conversation-${read.Entry.ConversationID}.txt`;a.click();setTimeout(()=>URL.revokeObjectURL(url),1000)}),button('Start work from this history',()=>this.startWork(read)),button('Close conversation',()=>{this.readGeneration++;this.detail.hidden=true;this.detail.replaceChildren()}));
 }
}
