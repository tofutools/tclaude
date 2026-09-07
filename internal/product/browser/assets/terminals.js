'use strict';

// A browser tab owns an attachment to one exact execution. Switching tabs never
// changes that execution, opens another attachment, or stops its workload.
class TerminalWorkspace {
  constructor({requestID}) {
    this.requestID = requestID;
    this.entries = new Map();
    this.theme = TERMINAL_THEME;
    document.addEventListener('terminal-theme',event=>{this.theme=terminalThemeFor(event.detail.wizard,event.detail.enabled);for(const entry of this.entries.values())if(entry.terminal)entry.terminal.options.theme={...this.theme};});
    this.selected = null;
    this.sequence = 0;
    this.layout = 'tabs';
    this.restoring = false;
    this.layoutControl = document.getElementById('terminal-layout');
    this.layoutControl.onchange = () => { this.layout = this.layoutControl.value; this.select(this.selected, false); };
    document.getElementById('terminal-left').onclick = () => this.move(-1);
    document.getElementById('terminal-right').onclick = () => this.move(1);
    this.popOutButton = document.getElementById('terminal-popout');
    this.popOutButton.onclick = () => this.popOut();
    this.tabs = document.getElementById('terminal-tabs');
    this.panels = document.getElementById('terminal');
    this.status = document.getElementById('terminal-status');
    this.size = document.getElementById('terminal-size');
    this.disconnectButton = document.getElementById('close-terminal');
    this.reconnectButton = document.getElementById('reconnect-terminal');
    this.removeButton = document.getElementById('remove-terminal');
    this.resizeButton = document.getElementById('resize-terminal');
    this.disconnectButton.onclick = () => this.disconnect(this.selected);
    this.reconnectButton.onclick = () => this.connect(this.selected);
    this.removeButton.onclick = () => this.remove(this.selected);
    this.size.onsubmit = event => {
      event.preventDefault();
      this.resize();
    };
    this.tabs.addEventListener('keydown', event => {
      const entries = [...this.entries.values()];
      const index = entries.findIndex(entry => entry.tab === event.target);
      if (index < 0 || !['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return;
      event.preventDefault();
      const next = event.key === 'Home' ? 0 : event.key === 'End' ? entries.length - 1
        : (index + (event.key === 'ArrowLeft' ? -1 : 1) + entries.length) % entries.length;
      this.select(entries[next], false);
      entries[next].tab.focus();
    });
    this.renderStatus();
  }

  open(execution, label, connect = true) {
    let entry = this.entries.get(execution.id);
    if (!entry) {
      const id = `terminal-pane-${++this.sequence}`;
      const tab = document.createElement('button');
      tab.type = 'button';
      tab.id = `${id}-tab`;
      tab.role = 'tab';
      tab.dataset.execution = execution.id;
      tab.textContent = label;
      tab.title = `${label} · ${execution.id}`;
      tab.setAttribute('aria-controls', id);
      const panel = document.createElement('div');
      panel.id = id;
      panel.className = 'terminal-panel';
      panel.role = 'tabpanel';
      panel.setAttribute('aria-labelledby', tab.id);
      panel.textContent = 'Disconnected. Select this pane and reconnect to attach.';
      panel.addEventListener('pointerdown', () => this.select(entry, false));
      entry = {id: execution.id, label, tab, panel, socket: null, terminal: null,
        state: 'disconnected', canResize: false, columns: 80, rows: 24};
      tab.onclick = () => this.select(entry);
      this.entries.set(entry.id, entry);
      this.tabs.append(tab);
      this.panels.append(panel);
    }
    this.select(entry);
    if (connect) this.connect(entry);
    this.persist();
  }

  select(entry, focus = true) {
    this.selected = entry;
    for (const candidate of this.entries.values()) {
      const active = candidate === entry;
      candidate.panel.hidden = this.layout === 'tabs' && !active;
      candidate.panel.classList.toggle('selected', active);
      candidate.tab.setAttribute('aria-selected', String(active));
      candidate.tab.tabIndex = active ? 0 : -1;
    }
    this.panels.dataset.layout = this.layout;
    this.layoutControl.value = this.layout;
    this.persist();
    this.size.elements.columns.value = entry?.columns ?? 80;
    this.size.elements.rows.value = entry?.rows ?? 24;
    this.renderStatus();
    if (focus) entry?.terminal?.focus();
  }

  connect(entry) {
    if (!entry || entry.socket) return;
    if (!entry.terminal) {
      entry.terminal = new Terminal({cols: entry.columns, rows: entry.rows,
        convertEol: false, theme: {...this.theme}});
      entry.panel.replaceChildren();
      entry.terminal.open(entry.panel);
      entry.terminal.onData(data => {
        if (entry.socket?.readyState === WebSocket.OPEN) {
          entry.socket.send(new TextEncoder().encode(data));
        }
      });
    }
    const url = new URL('/v2/attach', location.href);
    url.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
    url.searchParams.set('execution_id', entry.id);
    url.searchParams.set('request_id', this.requestID());
    const socket = new WebSocket(url, 'tclaude.terminal.v1');
    socket.binaryType = 'arraybuffer';
    entry.socket = socket;
    entry.state = 'connecting';
    entry.canResize = false;
    this.renderStatus();
    if (this.selected === entry) entry.terminal.focus();
    socket.onopen = () => {
      if (entry.socket !== socket) return;
      entry.state = 'connected';
      this.renderStatus();
    };
    socket.onmessage = event => {
      // A reconnect may finish before the previous connection's callbacks.
      if (entry.socket !== socket) return;
      if (typeof event.data !== 'string') {
        // Hidden terminals keep consuming output into their own scrollback.
        entry.terminal.write(new Uint8Array(event.data));
        return;
      }
      try {
        const control = JSON.parse(event.data);
        if (control.type !== 'capabilities' || typeof control.resize !== 'boolean') {
          throw new Error('Invalid terminal capabilities');
        }
        entry.canResize = control.resize;
        this.onAttached?.(entry);
        if (control.resize) {
          socket.send(JSON.stringify({type: 'resize', columns: entry.columns, rows: entry.rows}));
        }
        this.renderStatus();
      } catch {
        this.disconnect(entry, 'invalid-response');
      }
    };
    socket.onerror = () => {
      if (entry.socket === socket) this.disconnect(entry, 'unavailable');
    };
    socket.onclose = () => {
      if (entry.socket !== socket) return;
      entry.socket = null;
      entry.canResize = false;
      entry.state = 'disconnected';
      this.renderStatus();
    };
  }

  disconnect(entry, state = 'disconnected') {
    if (!entry) return;
    const socket = entry.socket;
    entry.socket = null;
    entry.canResize = false;
    entry.state = state;
    socket?.close();
    this.renderStatus();
  }

  remove(entry) {
    if (!entry) return;
    const entries = [...this.entries.values()];
    const index = entries.indexOf(entry);
    this.disconnect(entry);
    this.entries.delete(entry.id);
    entry.terminal?.dispose();
    entry.tab.remove();
    entry.panel.remove();
    this.persist();
    if (this.selected === entry) {
      this.select(entries[index + 1] || entries[index - 1] || null);
    }
  }

  popOut() {
    const entry=this.selected;if(!entry)return;
    const nonce=crypto.randomUUID(),url=new URL(location.pathname,location.origin);
    url.searchParams.set('terminal',entry.id);url.hash=new URLSearchParams({handoff:nonce}).toString();
    const child=window.open(url,'_blank','popup,width=1100,height=760');
    if(!child){this.status.textContent='Pop-out was blocked. Allow pop-ups to move this attachment.';return}
    const receive=event=>{
      if(event.origin!==location.origin||event.source!==child||event.data?.type!=='terminal-attached'||event.data.nonce!==nonce||event.data.executionID!==entry.id)return;
      cleanup();if(this.entries.get(entry.id)===entry)this.disconnect(entry);
    };
    const cleanup=()=>{window.removeEventListener('message',receive);clearTimeout(timer)};
    const timer=setTimeout(cleanup,30000);window.addEventListener('message',receive);
  }

  move(delta) {
    const entries = [...this.entries.values()], index = entries.indexOf(this.selected), next = index + delta;
    if(index < 0 || next < 0 || next >= entries.length) return;
    [entries[index], entries[next]] = [entries[next], entries[index]];
    this.entries = new Map(entries.map(entry => [entry.id, entry]));
    for(const entry of entries){this.tabs.append(entry.tab);this.panels.append(entry.panel)}
    this.persist();
  }

  persist() {
    if(this.restoring) return;
    try { sessionStorage.setItem('terminal-layout-v1', JSON.stringify({layout:this.layout, selected:this.selected?.id,
      entries:[...this.entries.values()].map(e=>({id:e.id,columns:e.columns,rows:e.rows}))})); } catch { /* Storage may be disabled; live attachments still work. */ }
  }

  restore(executions, agents) {
    let saved;try{saved=JSON.parse(sessionStorage.getItem('terminal-layout-v1'))}catch{return}
    if(!saved || !Array.isArray(saved.entries)) return;
    this.restoring = true;
    try {
      this.layout = saved.layout === 'split' ? 'split' : 'tabs';
      for(const item of saved.entries.slice(0,16)){
        const execution=executions.find(e=>e.id===item?.id);if(!execution)continue;
        const agent=agents.find(a=>a.PrimaryExecutionID===execution.id);
        this.open(execution,agent?.Name||execution.id,false);
        const entry=this.entries.get(execution.id);
        if(Number.isInteger(item.columns)&&item.columns>=1&&item.columns<=1000)entry.columns=item.columns;
        if(Number.isInteger(item.rows)&&item.rows>=1&&item.rows<=1000)entry.rows=item.rows;
      }
      this.select(this.entries.get(saved.selected)||this.selected,false);
    } finally {this.restoring=false;this.persist()}
  }

  suspend() {
    this.persist();
    for (const entry of this.entries.values()) this.disconnect(entry);
  }

  closeAll() {
    for (const entry of [...this.entries.values()]) this.remove(entry);
  }

  resize() {
    const entry = this.selected;
    if (!entry?.canResize || entry.socket?.readyState !== WebSocket.OPEN) return;
    const columns = Number(this.size.elements.columns.value);
    const rows = Number(this.size.elements.rows.value);
    if (![columns, rows].every(value => Number.isInteger(value) && value >= 1 && value <= 1000)) return;
    entry.socket.send(JSON.stringify({type: 'resize', columns, rows}));
    entry.terminal.resize(columns, rows);
    entry.columns = columns;
    entry.rows = rows;
    this.persist();
  }

  renderStatus() {
    const entry = this.selected;
    for (const candidate of this.entries.values()) {
      candidate.tab.dataset.connection = candidate.state;
      candidate.tab.setAttribute('aria-label', `${candidate.label} · ${candidate.state}`);
    }
    const connected = entry?.state === 'connected';
    this.disconnectButton.disabled = !entry?.socket;
    this.reconnectButton.disabled = !entry || !!entry.socket;
    this.removeButton.disabled = !entry;
    this.popOutButton.disabled = !entry;
    this.resizeButton.disabled = !connected || !entry.canResize;
    const states = {
      connecting: 'Connecting', connected: 'Attached', disconnected: 'Disconnected',
      unavailable: 'Attachment unavailable', 'invalid-response': 'Invalid terminal response',
    };
    this.status.textContent = entry
      ? `${states[entry.state]} · ${entry.label} · ${entry.id}. Disconnecting or closing a tab leaves the workload running.`
      : 'No attachment open. Choose Attach on a running agent or workspace shell.';
    this.onStatus?.();
  }
}
