// Durations stay integer nanoseconds at the API boundary. Browser drafts use
// decimal strings only outside the safe integer range; never float seconds.
const maxDuration = 9223372036854775807n;
export function seconds(value) {
  const text=String(value||'0').trim();
  const match=/^(\d+)(?:\.(\d{1,9}))?$/.exec(text);
  if(!match)throw new Error('Seconds must be nonnegative decimal text with at most nine fractional digits.');
  const nanos=BigInt(match[1])*1000000000n+BigInt((match[2]||'').padEnd(9,'0'));
  if(nanos>maxDuration)throw new Error('Duration exceeds the supported signed 64-bit range.');
  return nanos<=9007199254740991n?Number(nanos):String(nanos);
}
export function durationSeconds(value) {
  if(typeof value==='number'&&!Number.isSafeInteger(value))throw new Error('Exact duration projection is missing. Reload this definition.');
  const nanos=BigInt(value||0), whole=nanos/1000000000n;
  const fraction=String(nanos%1000000000n).padStart(9,'0').replace(/0+$/,'');
  return String(whole)+(fraction?'.'+fraction:'');
}
export function visitDurations(nodes, visit) {
  const retry=(r,path)=>{if(r)for(const key of ['Backoff','AttemptBudget'])if(r[key]!==undefined)visit(r,key,path+'.'+key)};
  const decision=(d,path)=>{if(d&&d.ExpiresAfter!==undefined)visit(d,'ExpiresAfter',path+'.ExpiresAfter')};
  for(const [i,n] of (nodes||[]).entries()) {
    const path=String(i);retry(n.Retry,path+'.Retry');decision(n.Decision,path+'.Decision');
    if(n.Wait&&n.Wait.Duration!==undefined)visit(n.Wait,'Duration',path+'.Wait.Duration');
    if(n.Stages){
      retry(n.Stages.Plan?.Retry,path+'.Stages.Plan.Retry');
      retry(n.Stages.Review?.Retry,path+'.Stages.Review.Retry');
      decision(n.Stages.PlanApproval,path+'.Stages.PlanApproval');
      for(const [j,c] of (n.Stages.Checks||[]).entries())retry(c.Retry,path+'.Stages.Checks.'+j+'.Retry');
    }
  }
}
export function applyDurationProjection(nodes, exact) {
  visitDurations(nodes,(object,key,path)=>{if(exact&&Object.hasOwn(exact,path))object[key]=exact[path]});
  return nodes;
}
export function wireDurationNodes(nodes,{exporting=false}={}) {
  const copy=structuredClone(nodes);
  visitDurations(copy,(object,key)=>{
    if(typeof object[key]==='number'&&!Number.isSafeInteger(object[key]))throw new Error('Duration is not exactly representable. Reopen the saved definition or use an exact export.');
    if(typeof object[key]!=='string')return;
    if(!/^(0|[1-9][0-9]*)$/.test(object[key])||BigInt(object[key])>maxDuration)throw new Error('Invalid exact duration.');
    if(!exporting)object[key]=new globalThis.ExactJSONTools.ExactJSON(object[key]);
  });
  return copy;
}
export function wireDefinitionDraft(draft,options={}) {
  const wire=globalThis.ExactJSONTools.wireDefinitionDraft(draft,options);
  if(wire.Process)wire.Process={...wire.Process,Graph:{...wire.Process.Graph,Nodes:wireDurationNodes(wire.Process.Graph.Nodes,options)}};
  delete wire.ProcessDurationNS;
  return wire;
}
