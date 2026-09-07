'use strict';
// Program revisions are authoring material. Saving never executes a command.
class ProgramProfilesWorkspace {
 constructor({host,api,el,button,edit,requestID}){Object.assign(this,{host,api,el,button,edit,requestID});this.generation=0}
 clear(){this.generation++;this.host.replaceChildren()}
 async load(){const generation=++this.generation,profiles=await this.api('/v2/program-profiles');if(generation!==this.generation)return;const {el,button}=this;this.host.replaceChildren(el('h2','Program configurations'),el('p','Define literal commands for process tasks. Saving does not execute them; a process pins the selected revision. Execution still requires current workspace authority.'),button('New program configuration',()=>this.open()));for(const profile of profiles||[]){const card=el('article',undefined,'card');card.append(el('h3',profile.Name),el('code',profile.ID),el('p',`Revision ${profile.Revision} · ${profile.HeadRevisionID}`));card.append(button('Edit program configuration',async()=>{const value=await this.api('/v2/program-profiles/'+encodeURIComponent(profile.ID));if(generation===this.generation&&card.isConnected)this.open(value)}));this.host.append(card)}}
 open(record){const p=record?.Profile,r=record?.Revision||{},id=p?.ID||this.requestID();
  this.edit(p?'Edit program configuration':'New program configuration',[
   {name:'name',label:'Program name',value:p?.Name||''},
   {name:'executable',label:'Executable (literal path or command)',value:r.Executable||''},
   {name:'arguments',label:'Argument prefix (JSON string array; never shell parsed)',value:JSON.stringify(r.ArgumentPrefix||[],null,2),multiline:true},
   {name:'environment',label:'Program environment (JSON object of literal strings)',value:JSON.stringify(r.Environment||{},null,2),multiline:true},
   {name:'directory',label:'Working directory (blank uses the selected workspace)',value:r.WorkingDirectory||'',required:false},
   {name:'sandbox',label:'Program sandbox',value:r.Sandbox||'',options:[{value:'',label:'Choose confinement explicitly'},'read_only','workspace_write','unconfined']},
   {name:'timeout',label:'Command timeout seconds',value:(r.Timeout||60000000000)/1e9},
   {name:'output',label:'Output limit bytes',value:r.OutputLimitBytes||1048576},
   {name:'authority',label:'Required effects (typed JSON; workspace execution is required)',value:JSON.stringify(r.EffectAuthority||[{Action:'program.execute',Resource:{Kind:'workspace'}}],null,2),multiline:true}
  ],async f=>{const args=JSON.parse(f.arguments),environment=JSON.parse(f.environment),authority=JSON.parse(f.authority),timeout=Number(f.timeout)*1e9,output=Number(f.output);
   if(!Array.isArray(args)||!args.every(v=>typeof v==='string'))throw new Error('Arguments must be a JSON array of strings.');
   if(!environment||Array.isArray(environment)||typeof environment!=='object'||!Object.values(environment).every(v=>typeof v==='string'))throw new Error('Environment must be a JSON object of strings.');
   if(!Array.isArray(authority)||!authority.length)throw new Error('Required effects must be a non-empty JSON array.');
   if(!Number.isSafeInteger(timeout)||timeout<=0||!Number.isSafeInteger(output)||output<=0)throw new Error('Timeout and output limit must be positive and exactly representable.');
   await this.api('/v2/program-profiles',{request_id:f.requestID,id,name:f.name,expected_revision:p?.Revision||0,executable:f.executable,argument_prefix:args,environment,working_directory:f.directory,sandbox:f.sandbox,timeout,output_limit_bytes:output,effect_authority:authority});await this.load();
  },{skipUnchanged:!!p});
  document.getElementById('editor-fields').append(this.el('p','Commands run on the host only when explicitly admitted as process work. Saving grants no authority. The bundled program host currently supports unconfined execution only; other requested modes are refused, never silently weakened. Shell syntax is literal unless you explicitly choose a shell executable. Existing processes keep their pinned revision.'));
 }
}
