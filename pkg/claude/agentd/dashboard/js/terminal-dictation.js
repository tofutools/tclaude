// terminal-dictation.js -- the overlay chat windows' floating tools over a
// Terminals-tab pane (tcl-meljd0). An agent's pane talks to the agent: the
// draft goes as operator mail, exactly as in an overlay window, and the send
// phrase can be spoken. Any other pane has a program reading its input, where a
// mis-heard line could run a command, so its draft is only pasted to the
// prompt: an explicit key or click, no newline, and the Enter that runs it
// stays the operator's, in the terminal.

import { h } from 'preact';
import { useCallback, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { companionMessageFits, composeCompanionMessage, viewContext } from './companion-model.js';
import { DraftField, FloatingTools, dictationSendPhrase, micTitle, postOperatorMessage, useAnnotation, useDictation } from './dictation-tools.js';
import { nodeFetch } from './node-api.js';
import { shellToast } from './shell-state.js';
import { dashboardState } from './snapshot-store.js';

const html = htm.bind(h);

// paneTalksToAgent reports whether a draft from this pane goes to an agent as
// mail. A shell session has an agent id too, but nobody to read mail. A local
// pane whose harness is unknown stays a terminal, which never submits by
// itself; a peer's agent is not in this node's roster, so its pane decides.
export function paneTalksToAgent(pane, target, snapshot) {
  if (!target || snapshot?.collab_enabled !== true) return false;
  const row = (snapshot.agents || []).find((agent) => agent.agent_id === target.agent || agent.conv_id === target.agent);
  const harness = pane.seed.harness || row?.state?.harness || '';
  if (harness === 'shell') return false;
  return !!(harness || pane.seed.remote);
}

// terminalDraftText is a draft as one line of terminal input. Control
// characters, line breaks above all, would act rather than read: outside
// bracketed paste a newline runs what came before it.
export function terminalDraftText(text) {
  return String(text || '').replace(/[\u0000-\u001f\u007f-\u009f]+/g, ' ').trim();
}

export function PaneDictation({
  pane, active, solo, status, canType, target, snapshot, actions, win = globalThis.window, notify = shellToast,
}) {
  const [draft, setDraftText] = useState('');
  const [sending, setSending] = useState(false);
  const [fieldFocused, setFieldFocused] = useState(false);
  const fieldRef = useCallback((element) => { if (!element) setFieldFocused(false); }, []);
  const [withContext, setWithContext] = useState(true);
  // A detached window has no toast host, so it says it on the terminal.
  const [notice, setNotice] = useState('');
  const report = solo ? setNotice : notify;
  const talks = paneTalksToAgent(pane, target, snapshot?.value);
  const connected = String(status || '').startsWith('connected');
  const usable = connected && (talks || canType);
  const shown = usable && active && (solo || dashboardState.activeTab.value === 'terminals');
  const name = pane.label || 'this agent';
  const viewNow = () => viewContext({ activeTab: 'terminals', pathname: win?.location?.pathname, search: win?.location?.search });
  const agentFetch = () => nodeFetch(target.node || '');
  const shownRef = useRef(false);
  shownRef.current = shown;
  const toTerminal = () => {
    setFieldFocused(false);
    if (shownRef.current) actions.activatePane(pane.key);
  };
  const dictation = useDictation({
    win, targetId: shown ? pane.key : '', draft, notify: report,
    sendPhrase: talks ? dictationSendPhrase(snapshot?.value) : '',
    setDraft: (_, text) => setDraftText(text),
    send: (text) => send(null, text),
    onStart: () => {
      setNotice('');
      win?.speechSynthesis?.cancel?.();
    },
    onRelease: toTerminal,
  });
  const annotation = useAnnotation({ win, notify: report, viewNow });

  // Whatever was typed while this went out stays.
  function settle(text) {
    setDraftText((current) => (current === text ? '' : current));
    const focused = win?.document?.activeElement;
    if (!focused || focused === win.document.body || focused.matches?.('.companion-draft, .companion-input button[type="submit"]')) toTerminal();
  }

  async function mailAgent(text) {
    const context = withContext ? viewNow() : '';
    if (!companionMessageFits(text, context)) {
      report('That message is too long to send. Shorten it and try again.', true);
      return;
    }
    setSending(true);
    try {
      await postOperatorMessage(agentFetch(), { to: target.agent, body: composeCompanionMessage(text, context) });
      settle(text);
    } catch (error) {
      report(`Message to ${name} failed: ${error?.message || error}`, true);
    } finally {
      setSending(false);
    }
  }

  function pasteToTerminal(text) {
    if (actions.widgetFor(pane.id)?.paste(terminalDraftText(text))) settle(text);
    else report('This terminal is not taking input, so the draft stays here.', true);
  }

  async function send(event, text = draft) {
    event?.preventDefault?.();
    if (sending || !text.trim()) return;
    // An open mic would bring the sent text back. Enter or the button sends
    // what the draft holds; a failed send leaves it there.
    if (event) dictation.abort();
    if (talks) await mailAgent(text);
    else pasteToTerminal(text);
  }

  // A drawing in progress outlives a blip, and keeps its Cancel.
  if (!shown) return annotation.overlay || null;
  const showField = draft || dictation.listening || fieldFocused;
  return html`
    <div class="companion-floating-anchor">
      <${FloatingTools} dictation=${dictation}
        micLabel=${talks ? `Dictate a message to ${name}` : 'Dictate a draft for this terminal'}
        micTitle=${talks ? micTitle(dictation.sendPhrase)
          : 'Dictate a draft. Nothing reaches the terminal until you press Enter here or Paste, and it lands at the prompt unrun. Uses the browser\'s speech recognition; in Chrome the audio is recognised by Google.'}
        withContext=${talks ? withContext : null} onContext=${setWithContext}
        annotateLabel=${`Mark up this page for ${name}`}
        onAnnotate=${talks ? () => annotation.start({ agentId: target.agent, name, fetchImpl: agentFetch() }) : null} />
      ${notice && html`<button type="button" class="companion-pane-notice" role="alert" title="Dismiss" onClick=${() => setNotice('')}>${notice}</button>`}
    </div>
    ${showField && html`<${DraftField} dictation=${dictation} draft=${draft} sending=${sending} fieldRef=${fieldRef}
      label=${talks ? `Message ${name}` : 'Draft for this terminal'}
      submitLabel=${talks ? 'Send' : 'Paste'}
      submitTitle=${talks ? null : 'Paste at the prompt, without a newline: press Enter in the terminal to run it'}
      onSubmit=${send} onEdit=${setDraftText}
      onFocus=${() => setFieldFocused(true)} onBlur=${() => setFieldFocused(false)} />`}
    ${annotation.overlay}`;
}
