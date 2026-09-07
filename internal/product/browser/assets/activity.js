class ActivityWorkspace {
 constructor({host,api,el,button,getSnapshot,setTarget}){
  Object.assign(this,{host,api,el,button,getSnapshot,setTarget});this.generation=0;this.records=[];
  const label=(text,input)=>{const node=el('label',text);node.append(input);return node};
  this.target=el('select');this.target.setAttribute('aria-label','Activity target');this.target.onchange=()=>{const value=this.target.value?JSON.parse(this.target.value):null;setTarget(value);this.show(value).catch(e=>this.error(e))};
  this.kind=el('select');this.kind.setAttribute('aria-label','Activity kind');for(const [value,text] of [['','All kinds'],['operation','Operations'],['work_run','Work runs'],['work_evidence','Evidence'],['decision','Decisions'],['historical_audit','Historical audit']]){const option=el('option',text);option.value=value;this.kind.append(option)}
  this.after=el('input');this.after.type='datetime-local';this.after.setAttribute('aria-label','Activity from');this.before=el('input');this.before.type='datetime-local';this.before.setAttribute('aria-label','Activity before');
  const form=el('form',undefined,'toolbar'),apply=el('button','Apply activity filters');apply.type='submit';form.append(label('Target',this.target),label('Kind',this.kind),label('Started from (local)',this.after),label('Started before (local)',this.before),apply);form.onsubmit=e=>{e.preventDefault();this.load().catch(e=>this.error(e))};
  this.search=el('input');this.search.setAttribute('aria-label','Search loaded activity');this.search.placeholder='Search loaded records';this.search.oninput=()=>this.render();this.status=el('p');this.status.setAttribute('role','status');this.content=el('div');host.replaceChildren(form,el('p','Activity projects durable operations, work, evidence and decisions. Imported history retains its attribution; these records do not confer authority. Text search applies only to loaded pages.','muted'),this.search,this.status,this.content);
 }
 error(e){this.status.textContent=e.message||String(e)}
 clear(){this.generation++;this.records=[];this.activeTarget=null;this.target.replaceChildren();this.content.replaceChildren();this.search.value='';this.status.textContent='Signed out'}
 async show(target){
  const choices=new Map(),snapshot=this.getSnapshot(),add=(kind,id,label)=>{if(id)choices.set(JSON.stringify({[kind]:id}),label+' · '+id)};
  for(const a of snapshot.agents||[])add('AgentID',a.ID,'Agent: '+a.Name);
  for(const c of snapshot.conversations||[])add('ConversationID',c.ID,'Conversation');
  for(const e of snapshot.executions||[])add('ExecutionID',e.id,'Execution');
  for(const w of snapshot.work_runs||[])add('WorkRunID',w.run?.id,'Work run');
  if(target&&!choices.has(JSON.stringify(target)))choices.set(JSON.stringify(target),'Selected: '+Object.values(target).join(' · '));
  this.target.replaceChildren();const empty=this.el('option','Choose a recorded target');empty.value='';this.target.append(empty);for(const [value,text] of choices){const o=this.el('option',text);o.value=value;this.target.append(o)}this.activeTarget=target?{...target}:null;this.target.value=target?JSON.stringify(target):'';await this.load();
 }
 async load(){
  const generation=++this.generation;this.records=[];this.next='';this.content.replaceChildren();if(!this.activeTarget){this.status.textContent='Choose a recorded target, or select an exact target ID.';return}
  const filter={Target:{...this.activeTarget},Limit:25};if(this.kind.value)filter.Kinds=[this.kind.value];for(const [input,key] of [[this.after,'After'],[this.before,'Before']])if(input.value){const d=new Date(input.value);if(!Number.isFinite(d.getTime()))throw new Error('Enter valid activity dates.');filter[key]=d.toISOString()}if(filter.After&&filter.Before&&filter.After>=filter.Before)throw new Error('Activity before must be later than activity from.');this.filter=filter;this.status.textContent='Loading activity…';await this.page(generation,'');
 }
 async page(generation,cursor){
  const result=await this.api('/v2/activity/query',{filter:{...this.filter,Cursor:cursor}});if(generation!==this.generation)return;const seen=new Set(this.records.map(r=>r.Kind+':'+r.ID));for(const r of result.Records||[]){const key=r.Kind+':'+r.ID;if(!seen.has(key)){seen.add(key);this.records.push(r)}}this.next=result.NextCursor||'';this.render();
 }
 render(){
  const {el,button}=this,query=this.search.value.trim().toLowerCase(),shown=this.records.filter(r=>JSON.stringify(r).toLowerCase().includes(query));this.content.replaceChildren();this.status.textContent=`${shown.length} matching of ${this.records.length} loaded activity records${this.next?' · more available':''}`;
  for(const r of shown){const card=el('article',undefined,'card');card.dataset.activity=r.ID;card.append(el('strong',`${r.Kind.replaceAll('_',' ')} · ${r.Outcome}`),el('p',r.ID,'muted'),el('p',`Actor: ${r.Actor.Kind}${r.Actor.AgentID?' · '+r.Actor.AgentID:''}${r.Actor.ExecutionID?' · '+r.Actor.ExecutionID:''}${r.Actor.AutomationRun?' · '+r.Actor.AutomationRun:''}`),el('p',`Started ${new Date(r.StartedAt).toLocaleString()}${r.FinishedAt?' · finished '+new Date(r.FinishedAt).toLocaleString():''}`));for(const key of ['AgentID','ConversationID','ExecutionID','WorkRunID'])if(r[key])card.append(el('p',`${key}: ${r[key]}`));if(r.Reason)card.append(el('p',r.Reason));if(r.Historical)card.append(el('p',`Imported historical record · ${r.Provenance}`));this.content.append(card)}
  if(!shown.length)this.content.append(el('p',this.records.length?'No loaded records match the text search.':'No recorded activity for this target and started-time range.'));
  if(this.records.length)this.content.append(button('Export loaded activity JSON',()=>{const url=URL.createObjectURL(new Blob([JSON.stringify({filter:this.filter,more_available:!!this.next,records:this.records},null,2)],{type:'application/json'})),a=el('a');a.href=url;a.download='activity.json';a.click();setTimeout(()=>URL.revokeObjectURL(url),1000)}));
  if(this.next){const cursor=this.next,generation=this.generation;this.content.append(button('More activity',()=>this.page(generation,cursor)))}
 }
}
