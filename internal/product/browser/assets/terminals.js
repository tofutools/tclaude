'use strict';

// A browser tab owns an attachment to one exact execution. Switching tabs never
// changes that execution, opens another attachment, or stops its workload.
class TerminalWorkspace {
  constructor({requestID}) {
    this.requestID = requestID;
    this.entries = new Map();
    this.selected = null;
    this.sequence = 0;
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

  open(execution, label) {
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
      entry = {id: execution.id, label, tab, panel, socket: null, terminal: null,
        state: 'disconnected', canResize: false, columns: 80, rows: 24};
      tab.onclick = () => this.select(entry);
      this.entries.set(entry.id, entry);
      this.tabs.append(tab);
      this.panels.append(panel);
    }
    this.select(entry);
    this.connect(entry);
  }

  select(entry, focus = true) {
    this.selected = entry;
    for (const candidate of this.entries.values()) {
      const active = candidate === entry;
      candidate.panel.hidden = !active;
      candidate.tab.setAttribute('aria-selected', String(active));
      candidate.tab.tabIndex = active ? 0 : -1;
    }
    this.size.elements.columns.value = entry?.columns ?? 80;
    this.size.elements.rows.value = entry?.rows ?? 24;
    this.renderStatus();
    if (focus) entry?.terminal?.focus();
  }

  connect(entry) {
    if (!entry || entry.socket) return;
    if (!entry.terminal) {
      entry.terminal = new Terminal({cols: entry.columns, rows: entry.rows,
        convertEol: false, theme: {background: '#0f1419', foreground: '#d4d4d4'}});
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
    if (this.selected === entry) {
      this.select(entries[index + 1] || entries[index - 1] || null);
    }
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
    this.resizeButton.disabled = !connected || !entry.canResize;
    const states = {
      connecting: 'Connecting', connected: 'Attached', disconnected: 'Disconnected',
      unavailable: 'Attachment unavailable', 'invalid-response': 'Invalid terminal response',
    };
    this.status.textContent = entry
      ? `${states[entry.state]} · ${entry.label} · ${entry.id}. Disconnecting or closing a tab leaves the workload running.`
      : 'No attachment open. Choose Attach on a running agent or workspace shell.';
  }
}
