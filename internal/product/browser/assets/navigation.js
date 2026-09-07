'use strict';
class WorkspaceNavigation {
 constructor({select,report}){
  this.select=select;this.report=report;
  this.dialog=document.createElement('dialog');this.dialog.id='command-picker';
  const title=document.createElement('h2');title.textContent='Go to…';this.dialog.append(title);
  this.input=document.createElement('input');this.input.setAttribute('aria-label','Search commands');this.input.placeholder='Search workspaces and actions';this.input.autocomplete='off';this.dialog.append(this.input);
  this.results=document.createElement('div');this.results.className='command-results';this.dialog.append(this.results);
  const close=document.createElement('button');close.type='button';close.textContent='Close';close.onclick=()=>this.dialog.close();this.dialog.append(close);document.body.append(this.dialog);
  this.input.oninput=()=>this.draw();this.input.onkeydown=event=>{if(event.key==='ArrowDown'){event.preventDefault();this.results.querySelector('button')?.focus()}else if(event.key==='Enter'){event.preventDefault();this.results.querySelector('button')?.click()}};
  this.results.onkeydown=event=>{const buttons=[...this.results.querySelectorAll('button')],index=buttons.indexOf(document.activeElement);if(event.key==='ArrowDown'||event.key==='ArrowUp'){event.preventDefault();buttons[(index+(event.key==='ArrowDown'?1:buttons.length-1))%buttons.length]?.focus()}};
  this.dialog.onclose=()=>{if(this.previousFocus?.isConnected)this.previousFocus.focus()};
  document.getElementById('open-commands').onclick=()=>this.open();
  window.addEventListener('popstate',()=>{this.select(document.body.classList.contains('terminal-window')?'terminals':this.initialTab(false)).catch(report)});
  document.addEventListener('keydown',event=>{
   if(event.repeat||document.body.classList.contains('terminal-window')||document.querySelector('dialog[open]')||(event.target instanceof Element&&event.target.closest('.xterm,.terminal-panel')))return;
   if((event.ctrlKey||event.metaKey)&&event.code==='KeyK'){event.preventDefault();this.open();return}
   if(event.altKey&&!event.ctrlKey&&!event.metaKey&&!event.shiftKey&&/^Digit[1-9]$/.test(event.code)&&!(event.target instanceof Element&&event.target.closest('input,textarea,select,[contenteditable=true]'))){const tab=this.tabs()[Number(event.code.slice(-1))-1];if(tab){event.preventDefault();tab.click()}}
  });
 }
 tabs(){return [...document.querySelectorAll('nav [data-tab]')].filter(n=>!n.hidden)}
 initialTab(useSaved=true){const explicit=new URLSearchParams(location.search).get('tab');let saved;try{saved=sessionStorage.getItem('workspace-tab-v1')}catch{};const wanted=explicit||(useSaved?saved:null);return this.tabs().some(t=>t.dataset.tab===wanted)?wanted:'groups'}
 record(tab,replace=false){if(document.body.classList.contains('terminal-window'))return;if(!this.tabs().some(t=>t.dataset.tab===tab))return;try{sessionStorage.setItem('workspace-tab-v1',tab)}catch{};const url=new URL(location.href);if(url.searchParams.get('tab')===tab)return;url.searchParams.set('tab',tab);history[replace?'replaceState':'pushState'](history.state,'',url)}
 open(){if(document.body.classList.contains('terminal-window')||document.querySelector('dialog[open]')||document.querySelector('main').inert)return;this.previousFocus=document.activeElement;this.input.value='';this.draw();this.dialog.showModal();this.input.focus()}
 draw(){const query=this.input.value.trim().toLowerCase();const commands=this.tabs().map(tab=>({label:tab.textContent,run:()=>tab.click()}));
  for(const id of ['new-agent','new-group','compose','create-checkout','register-workspace']){const target=document.getElementById(id);if(target)commands.push({label:target.textContent,run:()=>target.click()})}
  this.results.replaceChildren();for(const command of commands.filter(c=>c.label.toLowerCase().includes(query))){const button=document.createElement('button');button.type='button';button.textContent=command.label;button.onclick=()=>{this.dialog.close();command.run()};this.results.append(button)}
  if(!this.results.childNodes.length){const empty=document.createElement('p');empty.textContent='No matching commands';this.results.append(empty)}
 }
}
