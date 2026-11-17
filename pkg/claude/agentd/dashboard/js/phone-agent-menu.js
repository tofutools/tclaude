// phone-agent-menu.js -- one agent's menu on a phone: the sheet a touch and
// hold on an agent opens (phone-hold.js), and the row's ⋯ too, wherever an
// agent is listed (Agents, Now's rows and cards, a terminal's ⋯).
//
//   quick    four tiles: Terminal (Power on when it is offline; Select text
//            from its own terminal's ⋯, phone-terminal-text.js), Message,
//            Interrupt (only while it is in a turn, and only where the daemon
//            knows how to stop one: Esc for Claude Code, the app-server's
//            interrupt for Codex on it; greyed otherwise, and a tap says why)
//            and Copy id
//   Session  Open in Claude app ↗ (or Turn on Remote Access & open,
//            claude-app.js), Compact…, Restart…, Shut down… (left out while
//            it is offline, with a line saying why)
//   Agent    Shell here (and Shell in its launch directory, when it was
//            started elsewhere: phone-open-shell.js), Rename…, Reincarnate…,
//            Grant sudo…, Copy name, Open PR #N ›
//   last     Retire…, in red
//
// The order never changes with the agent's state, so a thumb finds an entry
// where it was. "…" entries go through the desktop's row action handler and
// its own confirm; Interrupt goes without one (it stops the agent's turn, and
// the agent carries on from there). Clone, export, snapshot, sandbox,
// permissions, schedule, fast mode and the remote control switch stay on the
// desktop.
// It reads the agent live from the snapshot, so a state change while it is
// open shows, and an agent that goes away says so.

import { h } from 'preact';
import { diagnosticsEnabled, setDiagnosticsEnabled } from './terminal-diagnostics.js';
import { contextFigure } from './context-meter.js';
import htm from 'htm';
import { agentGroups, findRow } from './phone-agents-model.js';
import { SheetHead, StatePill } from './phone-ui.js';
import { harnessCanCompact, harnessCanRemoteControl } from './helpers.js';
import { claudeAppOffer, pageCanToggleRemote } from './claude-app.js';
import { agentShellEntries, openAgentShell } from './phone-open-shell.js';

const html = htm.bind(h);

// CLAUDE_BUSY are the statuses in which Esc stops a Claude Code agent's turn,
// the ones the daemon accepts (agent_slash.go claudeInterruptible): at work,
// or asking for approval or input mid-turn.
const CLAUDE_BUSY = new Set(['working', 'awaiting_permission', 'awaiting_input']);

// interruptWhy is '' when Interrupt can stop the row's turn, else why not. A
// Codex agent on the app-server drive is interrupted through its API while it
// works (runCodexInterrupt); a Claude Code agent gets Esc in its pane, from
// this node's own dashboard only (runClaudeInterrupt). Other harnesses wait
// until their interrupt key is verified.
export function interruptWhy(row) {
  if (!row.online) return 'Interrupt: it is not running.';
  const status = row.member?.state?.status || '';
  if (row.member?.state?.codex_app_server) {
    return row.variant === 'working' ? '' : 'Interrupt works only while it is at work.';
  }
  if (row.harness && row.harness !== 'claude') return `Interrupt is not available for ${row.harness} yet.`;
  if (globalThis.__tclaudeRemoteNode?.id) return 'Interrupt is not available from a peer\'s page yet.';
  return CLAUDE_BUSY.has(status) ? '' : 'Interrupt works only while it is working or asking.';
}

// menuModel is what the menu offers for a row in its group, as plain data:
// { quick: [...], session: [...] | null, agent: [...], retire, note }. Each
// entry is { key, label, sub?, disabled?, href? }. here: the menu is opened
// from the agent's own terminal, which it then does not offer.
export function menuModel(row, group, snapshot, { here = false, selectText = false, diagnostics = false } = {}) {
  const online = row.online;
  const quick = [
    online && here && selectText
      // In its own terminal, where Terminal would be the one open: Select.
      ? { key: 'select', icon: '⌶', label: 'Select text' }
      : online
        ? { key: 'terminal', icon: '⌨', label: 'Terminal', disabled: here, sub: here ? 'open' : '' }
      : { key: 'power', icon: '⏻', label: row.waking ? 'Waking…' : 'Power on', disabled: row.waking },
    { key: 'message', icon: '✉', label: 'Message' },
    // Greyed but tappable (aria-disabled), so a tap can say why.
    { key: 'interrupt', icon: '⏹', label: 'Interrupt', why: interruptWhy(row) },
    { key: 'copy-id', icon: '⧉', label: 'Copy id' },
  ];
  const app = claudeAppOffer(row.member?.state, {
    online, canRemote: harnessCanRemoteControl(snapshot, row.harness), canToggle: pageCanToggleRemote(snapshot),
  });
  const session = online ? [
    ...(app?.kind === 'open' ? [{ key: 'claude-app', label: app.label, href: app.url }] : []),
    ...(app?.kind === 'get' ? [{ key: 'claude-app', label: app.label, turnOn: app.turnOn, sub: 'The same session, in the Claude app' }] : []),
    ...(harnessCanCompact(snapshot, row.harness) ? [{ key: 'compact', label: 'Compact…', sub: 'Summarise its context and carry on' }] : []),
    { key: 'restart', label: 'Restart…' },
    { key: 'shutdown', label: 'Shut down…' },
  ] : null;
  const runsHere = !row.member?.external_steward;
  const agent = [
    ...agentShellEntries(snapshot, row, group?.group ? group.name : ''),
    ...(group?.group ? [{ key: 'rename', label: 'Rename…' }] : []),
    ...(online && runsHere ? [{ key: 'reincarnate', label: 'Reincarnate…', sub: 'A fresh successor with the same identity' }] : []),
    ...(row.member?.agent_id ? [{ key: 'grant', label: 'Grant sudo…' }] : []),
    { key: 'copy-name', label: 'Copy name' },
    // In its own terminal: the Diagnostics overlay, on this device.
    ...(here ? [{ key: 'diagnostics', label: diagnostics ? 'Hide diagnostics' : 'Diagnostics', sub: 'This terminal\'s frame rate and latency' }] : []),
    ...(row.pr?.url ? [{ key: 'pr', label: `Open PR #${row.pr.number} ›`, href: row.pr.url }] : []),
  ];
  return {
    quick, session, agent,
    retire: { key: 'retire', label: 'Retire…', sub: 'Back to a plain conversation' },
    note: online ? '' : 'Compact, Restart and Reincarnate need it running.',
  };
}

// AgentMenu is the sheet's body. actions are phone-agent-actions.js's.
export function AgentMenu({ state, order, actions, agentKey, groupKey = '', here = false, selectText = null, close }) {
  const snapshot = state.snapshot.value;
  const found = findRow(agentGroups(snapshot, order), agentKey, groupKey);
  if (!found) return html`<p class="ui-empty phone-empty">This agent is no longer listed.</p>`;
  const { row, group } = found;
  const model = menuModel(row, group, snapshot, { here, selectText: !!selectText, diagnostics: diagnosticsEnabled() });
  const run = (key) => () => {
    const tile = model.quick.find((q) => q.key === key);
    if (tile?.why) {
      actions.explain?.(tile.why);
      return;
    }
    close();
    const shell = model.agent.find((entry) => entry.key === key && entry.target);
    if (shell) {
      void (actions.openShell || openAgentShell)(shell, row.name);
      return;
    }
    const go = {
      terminal: () => actions.openTerminal(row),
      select: () => selectText?.(),
      power: () => actions.rowAction('dot-toggle', row),
      message: () => actions.message(row),
      interrupt: () => actions.rowAction('interrupt-agent', row),
      'copy-id': () => actions.copy(row.member?.agent_id || row.conv, `${row.name}'s id`),
      'claude-app': () => actions.openClaudeApp(row, !!model.session?.find((entry) => entry.key === 'claude-app')?.turnOn),
      compact: () => actions.rowAction('compact-agent', row),
      restart: () => actions.rowAction('restart', row),
      shutdown: () => actions.rowAction('dot-toggle', row),
      rename: () => actions.rename(row, group),
      reincarnate: () => actions.rowAction('reincarnate', row),
      grant: () => actions.grant(row),
      'copy-name': () => actions.copy(row.name, `${row.name}'s name`),
      diagnostics: () => setDiagnosticsEnabled(!diagnosticsEnabled()),
      retire: () => actions.rowAction('retire-agent', row),
    }[key];
    go?.();
  };
  const item = (entry, danger = false) => html`<li key=${entry.key}>${entry.href
    ? html`<a class="phone-menu-item phone-agent-menu-item" href=${entry.href} target="_blank" rel="noopener" onClick=${() => close()}>
        <span class="phone-menu-label">${entry.label}</span></a>`
    : html`<button data-ui-exempt type="button" class=${`phone-menu-item phone-agent-menu-item${danger ? ' phone-menu-danger' : ''}`}
        data-act=${entry.key} disabled=${!!entry.disabled} onClick=${run(entry.key)}>
        <span class="phone-agent-menu-text"><span>${entry.label}</span>${entry.sub && html`<small>${entry.sub}</small>`}</span></button>`}</li>`;
  return html`<div class="phone-agent-sheet">
    <div class="phone-agent-line"><h2 class="phone-agent-sheet-name">${row.name}</h2>
      <${StatePill} tone=${row.pill.tone} class="phone-agent-state">${row.pill.text}<//></div>
    <p class="phone-agent-sheet-meta">${[group.group ? group.name : 'No group', row.role, row.harness, contextFigure(row.member?.state)].filter(Boolean).join(' · ')}</p>
    ${row.detail && html`<p class="phone-agent-why">${row.detail}</p>`}
    ${row.branch && html`<p class="phone-agent-sheet-meta">⎇ ${row.branch}${row.pr?.ci ? ` · PR #${row.pr.number} checks ${row.pr.ci}` : ''}</p>`}
    ${row.cwd && html`<p class="phone-agent-sheet-meta phone-agent-cwd">${row.cwd}</p>`}
    <div class="phone-agent-quick" role="group" aria-label="Quick actions">
      ${model.quick.map((tile) => html`<button data-ui-exempt type="button" key=${tile.key} class="ui-card ui-inset phone-agent-tile" data-act=${tile.key}
        disabled=${!!tile.disabled} aria-disabled=${tile.why ? 'true' : null} title=${tile.why || null} onClick=${run(tile.key)}>
        <span class="phone-agent-tile-icon" aria-hidden="true">${tile.icon}</span><span>${tile.label}</span></button>`)}
    </div>
    ${model.session && html`<${SheetHead}>Session<//>
      <ul class="phone-menu phone-agent-menu">${model.session.map((entry) => item(entry))}</ul>`}
    <${SheetHead}>Agent<//>
    <ul class="phone-menu phone-agent-menu">${model.agent.map((entry) => item(entry))}</ul>
    <ul class="phone-menu phone-agent-menu phone-agent-menu-last">${item(model.retire, true)}</ul>
    ${model.note && html`<p class="phone-sheet-meta">${model.note}</p>`}
  </div>`;
}
