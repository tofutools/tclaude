'use strict';
// Shared authoring vocabulary; provider support is shown by the preview below.
function launchApprovalChoices(){return ['supervised','automatic','deny','never','on-request','on-failure','untrusted','inherit','default','manual','plan','acceptEdits','auto','dontAsk','bypassPermissions','allow-tools','yolo','ask'];}
// A read-only adapter declaration. It never changes or submits authored settings.
function attachLaunchSupportPreview({host,api}) {
 const harness=host.querySelector('[name=harness]'),approval=host.querySelector('[name=approval]'),sandbox=host.querySelector('[name=sandbox]');
 if(!harness||!approval||!sandbox)return;
 const dialog=host.closest('dialog'),status=document.createElement('p');status.setAttribute('role','status');status.setAttribute('aria-label','Configured launch support');host.append(status);
 let generation=0,disposed=false,observer;
 const current=()=>!disposed&&host.isConnected&&(!dialog||dialog.open);
 const update=async()=>{
  const token=++generation,selected=harness.value;
  if(!current())return;
  if(!selected){status.textContent='Choose a harness to inspect configured launch policy support.';return}
  status.textContent='Reading configured launch policy support…';
  try{
   const support=await api('/v2/launch-support?harness='+encodeURIComponent(selected));
   if(token!==generation||!current())return;
   if(support.Harness!==selected)throw new Error('The returned support does not match the selected harness.');
   host.dispatchEvent(new CustomEvent('launch-policy-support',{detail:support}));
   if(!support.Configured){status.textContent=selected+': no provider is configured in this backend. You can save offline settings; launching requires a configured provider.';return}
   const hostSelection=host.querySelector('[name=host_sandbox]'),hasHostSandbox=hostSelection&&hostSelection.value&&!['none','defaults','retained:none','retained:defaults'].includes(hostSelection.value);
   const defaultSupport=['defaults','retained:defaults'].includes(hostSelection?.value)?'Global and group sandbox defaults are resolved at launch. ':'';
   const hostSupport=hasHostSandbox?(support.HostSandbox?'Host sandbox preparation is configured; the selected policy and host capabilities are checked at launch. ':'Unsupported host sandbox selection: this provider has no host sandbox preparation configured. '):'';
   if(!support.PolicyKnown){status.textContent=hostSupport+selected+': the configured provider does not publish policy support. Launch validation remains authoritative.';return}
   const approvals=support.ApprovalModes||[],sandboxes=support.SandboxModes||[],problems=[];
   if(approval.value&&!approvals.includes(approval.value))problems.push('selected approval '+approval.value);
   if(sandbox.value&&!sandboxes.includes(sandbox.value))problems.push('selected confinement '+sandbox.value);
   status.textContent=selected+' adapter supports approval: '+(approvals.join(', ')||'none')+'; confinement: '+(sandboxes.join(', ')||'none')+'. '+(problems.length?'Unsupported '+problems.join(' and ')+'. Change these settings before launch. ':(hasHostSandbox?'Selected native approval and confinement are supported by the adapter. ':'Selected policy is supported by the adapter. '))+hostSupport+defaultSupport+(support.ApprovalDescriptions?.[approval.value] ? support.ApprovalDescriptions[approval.value]+' ' : '')+(support.PreparedInitialInput?'Prepared initial input is supported. ':'Prepared initial input is not declared. ')+support.Basis;
  }catch(error){if(token===generation&&current())status.textContent='Launch support could not be read: '+error.message+'. Offline settings can still be saved; support is checked at launch.'}
 };
 const changed=e=>{if([harness,approval,sandbox].includes(e.target)||e.target.matches('[name=host_sandbox]'))update()};
 const dispose=()=>{disposed=true;generation++;observer?.disconnect();host.removeEventListener('change',changed);document.removeEventListener('workspace-signout',dispose);dialog?.removeEventListener('close',dispose)};
 host.addEventListener('change',changed);document.addEventListener('workspace-signout',dispose);dialog?.addEventListener('close',dispose,{once:true});
 // Template field forms are replaced within the same dialog. Stop their listeners
 // as soon as that form is detached, even when the editor itself remains open.
 observer=new MutationObserver(()=>{if(!host.isConnected){dispose();observer.disconnect()}});observer.observe(dialog||document.body,{childList:true,subtree:true});
 update();
 return dispose;
}

function launchToolGovernanceChoices(){return [{value:"",label:"Provider default"},...[["allow","Allow audited tools"],["ask","Ask before audited tools"],["deny","Deny audited tools"]].map(([value,label])=>({value,label}))];}

// Provider-specific controls follow an explicit harness change independently of
// the asynchronous, read-only capability preview.
function attachProviderSettingControl(host,name,provider,isEditable=()=>true){
 const harness=host.querySelector('[name=harness]'),tools=host.querySelector('[name='+name+']');
 if(!harness||!tools)return ()=>{};
 const sync=()=>{const supported=harness.value===provider||(!harness.value&&harness.dataset.allowInheritedHarness==='true');if(!supported)tools.value='';tools.disabled=!supported||!isEditable();};
 host.addEventListener('change',event=>{if(event.target===harness)sync()});sync();return sync;
}

function attachToolGovernanceControl(host,isEditable){return attachProviderSettingControl(host,'tool_governance','opencode',isEditable)}
function launchFastModeChoices(){return [{value:'',label:'Inherit Codex setting'},{value:'on',label:'On'},{value:'off',label:'Off'}]}
function attachFastModeControl(host,isEditable){return attachProviderSettingControl(host,'fast_mode','codex',isEditable)}

function launchAutoReviewChoices(){return [{value:'',label:'No automatic-review override'},{value:'on',label:'Automatic approval review'}]}
function attachAutoReviewControl(host,isEditable){return attachProviderSettingControl(host,'auto_review','codex',isEditable)}
function launchSettingValue(key,value){return key==='auto_review'||key==='auto_memory'||key==='peer_messaging'?(value===true||value==='on'?'on':(key==='auto_memory'||key==='peer_messaging')&&value===false?'off':''):(value||'')}

function autoReviewAuthorityField(bounds={}){return {name:'auto_review',label:'Allow Codex automatic approval review',type:'checkbox',required:false,value:!!bounds.AutoReview}}

function launchAutoMemoryChoices(){return [{value:'',label:'Use default (off)'},{value:'on',label:'On'},{value:'off',label:'Off'}]}
function attachAutoMemoryControl(host,isEditable){return attachProviderSettingControl(host,'auto_memory','claude',isEditable)}

function launchPeerMessagingChoices(){return [{value:'',label:'Use default (off)'},{value:'on',label:'On'},{value:'off',label:'Off'}]}
function attachPeerMessagingControl(host,isEditable){return attachProviderSettingControl(host,'peer_messaging','claude',isEditable)}
