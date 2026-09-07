class UsageWorkspace {
 constructor({host,api,el,button,getSnapshot,setTarget}){
  Object.assign(this,{host,api,el,button,getSnapshot,setTarget});this.generation=0;this.records=[];
  const label=(text,node)=>{const l=el('label',text);l.append(node);return l};
  this.target=el('select');this.target.setAttribute('aria-label','Usage target');this.target.onchange=()=>{if(this.target.value){const target=JSON.parse(this.target.value);setTarget(target);this.show(target).catch(e=>this.error(e))}};
  this.after=el('input');this.after.type='datetime-local';this.after.setAttribute('aria-label','Usage observed from');this.before=el('input');this.before.type='datetime-local';this.before.setAttribute('aria-label','Usage observed before');
  const form=el('form',undefined,'toolbar'),apply=el('button','Apply observed-time range');apply.type='submit';form.append(label('Target',this.target),label('Observed from (local)',this.after),label('Observed before (local)',this.before),apply);form.onsubmit=e=>{e.preventDefault();this.load().catch(e=>this.error(e))};
  this.unit=el('select');this.unit.setAttribute('aria-label','Usage chart unit');this.unit.onchange=()=>this.render();this.status=el('p');this.status.setAttribute('role','status');this.content=el('div');
  host.replaceChildren(form,el('p','These are source readings, which may be cumulative. Observed-time filters do not calculate consumption during the interval. Conversation-attributed readings remain conversation-wide even when an execution is selected. Missing usage is not zero usage.','muted'),this.unit,this.status,this.content);
 }
 error(e){this.status.textContent=e.message||String(e)}
 clear(){this.generation++;this.records=[];this.activeTarget=null;this.content.replaceChildren();this.target.replaceChildren();this.status.textContent='Signed out'}
 async show(target){
  const {el}=this,snapshot=this.getSnapshot(),choices=new Map();
  for(const c of snapshot.conversations||[])choices.set(JSON.stringify({ConversationID:c.ID}),`Conversation: ${c.ID}`);
  for(const e of snapshot.executions||[]){const a=(snapshot.agents||[]).find(a=>a.ID===e.agent_id);choices.set(JSON.stringify({ExecutionID:e.id}),`Execution: ${a?.Name||e.id} · ${e.id}`);if(e.conversation_id)choices.set(JSON.stringify({ConversationID:e.conversation_id}),`Conversation: ${e.conversation_id}`)}
  if(target&&!choices.has(JSON.stringify(target)))choices.set(JSON.stringify(target),`Selected: ${Object.values(target).join(' · ')}`);
  this.target.replaceChildren();const empty=el('option','Choose a recorded target');empty.value='';this.target.append(empty);for(const [value,text] of choices){const o=el('option',text);o.value=value;this.target.append(o)}
  this.activeTarget=target?{...target}:null;this.target.value=target?JSON.stringify(target):'';await this.load();
 }
 async load(){
  const generation=++this.generation;this.records=[];this.content.replaceChildren();this.next='';if(!this.activeTarget){this.status.textContent='Choose a recorded target, or use Select target for a historical conversation ID.';return}
  const filter={Target:{...this.activeTarget},Limit:25};for(const [input,key] of [[this.after,'After'],[this.before,'Before']])if(input.value){const d=new Date(input.value);if(!Number.isFinite(d.getTime()))throw new Error('Enter a valid observed-time range.');filter[key]=d.toISOString()}
  if(filter.After&&filter.Before&&filter.After>=filter.Before)throw new Error('Observed before must be later than observed from.');this.filter=filter;this.status.textContent='Loading usage…';await this.page(generation,'');
 }
 async page(generation,cursor){
  const result=await this.api('/v2/usage/query',{filter:{...this.filter,Cursor:cursor}});if(generation!==this.generation)return;
  const seen=new Set(this.records.map(r=>r.ID));for(const r of result.Observations||[])if(!seen.has(r.ID)){this.records.push({...r,Counters:(r.Counters||[]).map(c=>({Unit:c.Unit,Value:c.ExactValue??String(c.Value)}))});seen.add(r.ID)}this.next=result.NextCursor||'';
  const selected=this.unit.value,units=[...new Set(this.records.flatMap(r=>(r.Counters||[]).map(c=>c.Unit)))];this.unit.replaceChildren();for(const u of units){const o=this.el('option',u.replaceAll('_',' '));o.value=u;this.unit.append(o)}if(units.includes(selected))this.unit.value=selected;this.render();
 }
 render(){
  const {el,button}=this;this.content.replaceChildren();const n=this.records.length,complete=this.records.filter(r=>r.Coverage?.Counters==='complete').length;this.status.textContent=`${n} recorded readings loaded · ${complete} with complete counter coverage${this.next?' · more available':''}`;
  const selectedTarget={...this.activeTarget},generation=this.generation;
  const canRefresh=(this.getSnapshot().executions||[]).some(e=>selectedTarget.ExecutionID?e.id===selectedTarget.ExecutionID:e.conversation_id===selectedTarget.ConversationID);
  if(canRefresh)this.content.append(button('Refresh native usage',async()=>{this.status.textContent='Refreshing native usage…';try{await this.api('/v2/usage/refresh',{target:selectedTarget});if(generation===this.generation)await this.load()}catch(e){if(generation===this.generation)this.error(e);throw e}}));
  if(!canRefresh)this.content.append(el('p','Native refresh is unavailable: this historical conversation has no recorded execution. Its imported readings remain available.'));
  if(!n)this.content.append(el('p','No recorded observations in this observed-time range. Missing usage is not zero usage.'));
  const values=this.records.map(r=>({r,c:(r.Counters||[]).find(c=>c.Unit===this.unit.value)})).filter(x=>x.c);
  if(values.length){const chart=el('figure'),title=el('figcaption',`${this.unit.value.replaceAll('_',' ')} by source observation (not consumption per interval)`);chart.append(title);const max=values.reduce((max,x)=>Math.max(max,Number(x.c.Value)),1);for(const {r,c} of values){const row=el('div',undefined,'usage-reading');row.append(el('span',`${new Date(r.ObservedAt).toLocaleString()} · ${r.Source}: ${c.Value}`));const meter=el('meter');meter.min=0;meter.max=max;meter.value=c.Value;meter.setAttribute('aria-label',`${r.ID} ${c.Unit}`);row.append(meter);chart.append(row)}this.content.append(chart)}
  for(const r of this.records){const card=el('article',undefined,'card');card.dataset.usage=r.ID;card.append(el('strong',`${r.Harness} · ${r.Attribution.Precision}`),el('p',`${r.ID} · ${r.Source} · observed ${r.ObservedAt}`,'muted'),el('p',`Conversation ${r.Attribution.ConversationID||'none'}${r.Attribution.ExecutionID?' · execution '+r.Attribution.ExecutionID:''}`));for(const c of r.Counters||[])card.append(el('p',`${c.Unit.replaceAll('_',' ')}: ${c.Value}`));if(r.Cost)card.append(el('p',`${r.Cost.Amount} ${r.Cost.Currency} · ${r.Cost.Kind.replaceAll('_',' ')}`));card.append(el('p',`Counters: ${r.Coverage.Counters}; cost: ${r.Coverage.Cost}`));if(r.Coverage.Reason)card.append(el('p',r.Coverage.Reason));if(r.Historical)card.append(el('p',`Historical record · ${r.Provenance||'provenance unavailable'}`));this.content.append(card)}
  if(n)this.content.append(button('Export loaded usage JSON',()=>{const url=URL.createObjectURL(new Blob([JSON.stringify({target:selectedTarget,observed_time_filter:this.filter,more_available:!!this.next,observations:this.records},null,2)],{type:'application/json'})),a=el('a');a.href=url;a.download='usage-observations.json';a.click();setTimeout(()=>URL.revokeObjectURL(url),1000)}));
  if(this.next){const cursor=this.next;this.content.append(button('More observations',()=>this.page(generation,cursor)))}
 }
}
