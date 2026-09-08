'use strict';

class RosterWorkspace {
  constructor({host, api, el, button, edit, refresh}) {
    Object.assign(this,{host,api,el,button,edit,refresh}); this.selected=new Set(); this.results=[]; this.busy=false;
    const controls=el('form',undefined,'toolbar'); controls.setAttribute('aria-label','Roster filters');
    this.query=el('input');this.query.placeholder='Search agents';this.query.setAttribute('aria-label','Search agents');
    this.harness=el('select');this.harness.setAttribute('aria-label','Harness filter');
    this.state=el('select');this.state.setAttribute('aria-label','State filter');
    this.group=el('select');this.group.setAttribute('aria-label','Group filter');
    this.sort=el('select');this.sort.setAttribute('aria-label','Sort agents');
    this.options(this.state,[['','All states'],['offline','Offline'],['running','Running'],['reserved','Reserved'],['prepared','Prepared'],['released','Released'],['unknown','Unknown'],['exited','Exited'],['failed','Failed'],['retired','Retired']]);
    this.options(this.sort,[['name','Name'],['harness','Harness'],['model','Model'],['newest','Newest']]);
    controls.append(this.query,this.harness,this.state,this.group,this.sort);
    controls.onsubmit=e=>e.preventDefault();for(const control of controls.children)control.addEventListener('input',()=>this.draw());
    this.actions=el('div',undefined,'toolbar');this.count=el('span');
    this.actions.append(button('Select visible',()=>{this.selected=new Set(this.visible.map(a=>a.ID));this.draw()}),button('Clear selection',()=>{this.selected.clear();this.draw()}),this.count);
    for(const action of ['start','stop','retire','reactivate'])this.actions.append(button(action[0].toUpperCase()+action.slice(1)+' selected',()=>this.confirm(action)));
    this.report=el('div');this.report.setAttribute('role','status');this.list=el('div');
    host.replaceChildren(controls,this.actions,this.report,this.list);
  }
  options(select,options){const prior=select.value;select.replaceChildren();for(const[value,label]of options){const o=this.el('option',label);o.value=value;select.append(o)}if(options.some(([value])=>value===prior))select.value=prior}
  update(snapshot,row,order=[]){this.snapshot=snapshot;this.row=row;const rank=new Map(order.map((id,i)=>[id,i]));this.groups=[...(snapshot.groups||[])].sort((a,b)=>(rank.get(a.ID)??Number.MAX_SAFE_INTEGER)-(rank.get(b.ID)??Number.MAX_SAFE_INTEGER));const agents=snapshot.agents||[];
    this.options(this.harness,[['','All harnesses'],...[...new Set(agents.map(a=>a.Desired.Harness))].sort().map(h=>[h,h])]);
    this.options(this.group,[['','All groups'],['__ungrouped','Ungrouped'],...this.groups.map(g=>[g.ID,g.Name+' · '+g.ID])]);
    const ids=new Set(agents.map(a=>a.ID));for(const id of this.selected)if(!ids.has(id))this.selected.delete(id);this.draw();
  }
  draw(){if(!this.snapshot)return;const{el,button}=this,agents=this.snapshot.agents||[],groups=this.groups||[],grouped=new Set(groups.flatMap(g=>g.Members||[]));
    const state=a=>a.Lifecycle==='retired'?'retired':(this.snapshot.executions||[]).find(e=>e.id===a.PrimaryExecutionID)?.state||'offline';
    const group=this.group.value,search=this.query.value.trim().toLowerCase(),members=groups.find(g=>g.ID===group)?.Members||[];
    this.visible=agents.filter(a=>(!search||[a.Name,a.ID,a.Labels?.Role,a.Labels?.Description,a.TaskReference,a.Desired.Harness,a.Desired.Model].join(' ').toLowerCase().includes(search))&&(!this.harness.value||a.Desired.Harness===this.harness.value)&&(!this.state.value||state(a)===this.state.value)&&(!group||(group==='__ungrouped'?!grouped.has(a.ID):members.includes(a.ID))));
    const field=a=>this.sort.value==='harness'?a.Desired.Harness:this.sort.value==='model'?a.Desired.Model:a.Name;
    this.visible.sort((a,b)=>(this.sort.value==='newest'?Date.parse(b.CreatedAt)-Date.parse(a.CreatedAt):field(a).localeCompare(field(b)))||a.ID.localeCompare(b.ID));
    this.list.replaceChildren();
    const ungrouped={ID:'__ungrouped',Name:'Ungrouped',Members:agents.filter(a=>!grouped.has(a.ID)).map(a=>a.ID)},all=[...groups,ungrouped],byID=new Map(groups.map(g=>[g.ID,g])),keep=new Set(),cards=new Map();
    for(const g of all){if(group&&g.ID!==group)continue;const shown=this.visible.some(a=>(g.Members||[]).includes(a.ID));if(shown||(!search&&!this.harness.value&&!this.state.value&&g.ID!=='__ungrouped'))keep.add(g.ID)}
    for(const id of [...keep]){let parent=byID.get(id)?.ParentGroupID;const seen=new Set([id]);while(parent&&byID.has(parent)&&!seen.has(parent)){seen.add(parent);keep.add(parent);parent=byID.get(parent).ParentGroupID}}
    for(const g of all){if(!keep.has(g.ID))continue;const card=el('div',undefined,'group');card.dataset.groupId=g.ID;card.append(el('h2',g.Name));appendGroupDetails(card,g.Details||{},el);if(g.ID!=='__ungrouped')card.append(el('code',g.ID));
      for(const a of (group&&g.ID!==group?[]:this.visible.filter(a=>(g.Members||[]).includes(a.ID)))){const row=this.row(a),check=el('input');check.type='checkbox';check.checked=this.selected.has(a.ID);check.disabled=this.busy;check.setAttribute('aria-label','Select '+a.Name);check.onchange=()=>{check.checked?this.selected.add(a.ID):this.selected.delete(a.ID);this.draw()};row.prepend(check);card.append(row)}cards.set(g.ID,card);
    }
    for(const g of all){const card=cards.get(g.ID);if(!card)continue;const parent=cards.get(g.ParentGroupID);if(parent&&parent!==card){let children=parent.querySelector(':scope > .roster-children');if(!children){children=el('div',undefined,'roster-children');children.style.marginInlineStart='1rem';parent.append(children)}children.append(card)}else this.list.append(card)}
    if(!this.visible.length)this.list.append(el('p',agents.length?'No agents match these filters.':'No agents yet. Create an agent to save its configuration before starting work.'));
    this.count.textContent=`${this.visible.length} visible · ${this.selected.size} selected`;
    for(const b of this.actions.querySelectorAll('button'))b.disabled=this.busy;
    this.report.replaceChildren();
    for(const r of this.results)this.report.append(el('p',`${r.name} · ${r.action}: ${r.status}${r.detail?' · '+r.detail:''}`));
    if(!this.busy&&this.results.some(r=>r.status==='Failed'))this.report.append(button('Retry failed requests',()=>this.run(this.results.filter(r=>r.status==='Failed'))));
  }
  confirm(action){if(this.busy)return;const targets=(this.snapshot.agents||[]).filter(a=>this.selected.has(a.ID));if(!targets.length)throw new Error('Select at least one agent.');
    this.edit(`${action} selected agents`,[{name:'confirm',label:`Selected agents: ${targets.map(a=>a.Name).join(', ')}. Type ${action.toUpperCase()} to confirm.`,required:true},...(action==='retire'?[{name:'reason',label:'Retirement reason',multiline:true}]:[])],async form=>{
      if(form.confirm!==action.toUpperCase())throw new Error('Confirmation does not match.');
      this.results=targets.map((a,index)=>{const id=form.requestID+'_'+index;let path,body;
        if(action==='start'){path='/v2/launch';body={request_id:id,target:{agent:{agent_id:a.ID,expected_revision:a.Revision}}}}
        else if(action==='stop'){path='/v2/stop';body={request_id:id,execution_id:a.PrimaryExecutionID,force:false}}
        else {path='/v2/agents/'+encodeURIComponent(a.ID)+'/'+action;body={expected_revision:a.Revision,...(action==='retire'?{reason:form.reason}:{})}}
        return{name:a.Name,action,path,body,status:'Pending'};
      });await this.run(this.results);
    });
  }
  async run(records){if(this.busy)return;this.busy=true;this.draw();
    try{for(const r of records){r.status='Pending';r.detail='';this.draw();try{
      if(r.action==='stop'&&!r.body.execution_id)throw new Error('No selected primary execution to stop.');
      const result=await this.api(r.path,r.body),state=result?.operation?.state;
      r.status=!state||state==='succeeded'?'Accepted':['refused','failed'].includes(state)?'Failed':state==='uncertain'?'Uncertain':'Pending';
      r.detail=[state,result?.operation?.detail,result?.execution?.state||result?.Lifecycle].filter(Boolean).join(' · ');
    }catch(e){r.status='Failed';r.detail=e.message}this.draw()}}
    finally{this.busy=false;await this.refresh();this.draw()}
  }
}
