'use strict';
// Reusable profiles and launch overrides share ordinary dialog fields. Empty
// profile values inherit; admitted agents still receive a complete configuration.
function profileOptionFields(options={},name=''){
 const fields=desiredFields({...options,name});
 for(const field of fields){
  if(field.name==='name')continue;
  field.required=false;
  if(field.name==='harness'){
   field.options=[{value:'',label:'Inherit harness'},...field.options];field.value=options.Harness||'';field.allowInheritedHarness=true;
  }
  if(field.name==='approval'||field.name==='sandbox'){
   field.options=[{value:'',label:'Inherit from defaults'},...field.options];field.value=options[field.name==='approval'?'Approval':'Sandbox']||'';
  }
  if(field.name==='auto_memory'){
   field.options=[{value:'',label:'Inherit auto-memory'},{value:'on',label:'On'},{value:'off',label:'Off'}];
   field.value=options.AutoMemory===undefined?'':options.AutoMemory?'on':'off';
  }
  if(field.name==='auto_review'){
   field.options=[{value:'',label:'Inherit approval reviewer'},{value:'on',label:'Automatic approval review'},{value:'off',label:'No automatic approval review'}];
   field.value=options.AutoReview===undefined?'':options.AutoReview?'on':'off';
  }
 }
 return fields;
}
function profileOptionsFromForm(form){
 validateConfigurationForm(form);
 const out={};
 for(const [name,key] of Object.entries({harness:'Harness',model:'Model',effort:'Effort',cwd:'WorkingDirectory',approval:'Approval',sandbox:'Sandbox',fast_mode:'FastMode',tool_governance:'ToolGovernance'}))if(form[name])out[key]=form[name];
 if(form.auto_memory)out.AutoMemory=form.auto_memory==='on';
 if(form.auto_review)out.AutoReview=form.auto_review==='on';
 if(form.host_sandbox)out.HostSandbox=form.host_sandbox;
 if(form.environment&&Object.keys(form.environment).length)out.Environment=form.environment;
 return out;
}
function profileLaunchFields(revision){
 return profileOptionFields({},'').filter(field=>field.name!=='name').map(field=>({...field,label:field.label+' · launch override'}));
}
function profileLaunchOverrides(form){const options=profileOptionsFromForm(form);return Object.keys(options).length?{configuration_overrides:options}:{}}

// An independent copy must contain explicit settings. Let the operator complete
// a partial profile in the existing configuration form; never invent a cwd.
function copyProfileConfiguration(revision){
 if(!revision.Options)return Promise.resolve(structuredClone(revision.Desired));
 return new Promise(resolve=>{
  let copied=null;
  edit('Complete copied configuration',desiredFields(revision.Options).filter(field=>field.name!=='name').map(field=>field.name==='model'?{...field,required:false}:field),form=>{copied=configuration(form)});
  document.getElementById('editor').addEventListener('close',()=>resolve(copied),{once:true});
 });
}
