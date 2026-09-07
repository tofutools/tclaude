'use strict';
// Adapts the legacy group/profile/spawn NAME + value rows to typed v2 maps.
// Values remain literal; the host environment is never read into this editor.
class LaunchEnvironment {
 constructor(value={}, {inherited={}}={}) {
  this.host=document.createElement('div');this.host.className='launch-environment';
  this.rows=Object.entries(value||{}).map(([name,value])=>({name,value}));
  this.inherited=inherited;this.render();
 }
 render(){
  this.host.replaceChildren();
  for(const row of this.rows){
   const line=document.createElement('div');line.className='launch-environment-row';
   const name=document.createElement('input');name.placeholder='NAME';name.setAttribute('aria-label','Environment name');name.value=row.name;
   const value=document.createElement('textarea');value.rows=1;value.placeholder='Literal value';value.setAttribute('aria-label','Environment value');value.value=row.value;
   name.oninput=()=>{row.name=name.value;this.preview()};value.oninput=()=>{row.value=value.value;this.preview()};
   const remove=document.createElement('button');remove.type='button';remove.textContent='Remove';remove.onclick=()=>{this.rows=this.rows.filter(r=>r!==row);this.render()};
   line.append(name,value,remove);this.host.append(line);
  }
  const add=document.createElement('button');add.type='button';add.textContent='Add variable';add.onclick=()=>{this.rows.push({name:'',value:''});this.render();this.host.querySelector('.launch-environment-row:last-of-type input')?.focus()};
  this.host.append(add);
  this.effective=document.createElement('pre');this.effective.className='environment-effective';this.host.append(this.effective);this.preview();
 }
 read(){
  const out=Object.create(null);
  for(const row of this.rows){
   const name=row.name.trim();if(!name&&!row.value)continue;
   if(!/^[A-Za-z_][A-Za-z0-9_]{0,127}$/.test(name))throw new Error('Environment names must be ASCII identifiers starting with a letter or underscore.');
   if(Object.hasOwn(out,name))throw new Error('Duplicate environment name: '+name);
   out[name]=row.value;
  }
  LaunchEnvironment.validate(out);return out;
 }
 static validate(values){
  const entries=Object.entries(values),encoder=new TextEncoder();let total=0;
  if(entries.length>128)throw new Error('Environment has more than 128 entries.');
  for(const [name,value] of entries){
   if(['HOME','PATH','SHELL','TMPDIR','TMP','TEMP','ENV','BASH_ENV','TMUX','TMUX_PANE','CLAUDE_CONFIG_DIR'].includes(name)||/^(TCLAUDE_|CLAUDE_CODE_|CODEX_|COPILOT_|OPENCODE_|XDG_|LD_|DYLD_)/.test(name))throw new Error('Environment name is reserved for runtime control: '+name);
   const bytes=encoder.encode(value).length;
   if(bytes>16384||value.includes('\0')||!value.isWellFormed())throw new Error('Invalid environment value for '+name);
   total+=encoder.encode(name).length+bytes;
  }
  if(total>65536)throw new Error('Environment exceeds 65536 bytes.');
 }
 preview(){
  try{const values={...this.inherited,...this.read()};this.effective.textContent=Object.keys(values).length?'Effective configured environment (literal values):\n'+Object.keys(values).sort().map(k=>k+' = '+JSON.stringify(values[k])).join('\n'):'No configured environment variables.'}
  catch(e){this.effective.textContent=e.message}
 }
}

class LaunchEnvironmentSets {
 constructor(values=[]){this.host=document.createElement('div');this.editors=(values||[]).map(value=>new LaunchEnvironment(value));this.render()}
 render(){this.host.replaceChildren();const help=document.createElement('p');help.textContent='No sets allows only an empty configured environment. Each listed set permits exactly those names and literal values; add an empty set to also permit no variables.';this.host.append(help);for(const editor of this.editors){const group=document.createElement('fieldset'),legend=document.createElement('legend'),remove=document.createElement('button');legend.textContent='Allowed exact environment';remove.type='button';remove.textContent='Remove set';remove.onclick=()=>{this.editors=this.editors.filter(e=>e!==editor);this.render()};group.append(legend,editor.host,remove);this.host.append(group)}const add=document.createElement('button');add.type='button';add.textContent='Add allowed environment';add.onclick=()=>{this.editors.push(new LaunchEnvironment());this.render()};this.host.append(add)}
 read(){return this.editors.map(editor=>editor.read())}
}
