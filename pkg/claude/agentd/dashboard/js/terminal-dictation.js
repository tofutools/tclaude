// terminal-dictation.js -- the companion window's floating tools over a
// Terminals-tab pane (tcl-meljd0). An agent's pane talks to the agent: the
// draft goes as operator mail, exactly as in the companion, and the send
// phrase can be spoken. Any other pane has a program reading its input, where a
// mis-heard line could run a command, so its draft is only pasted to the
// prompt: an explicit key or click, no newline, and the Enter that runs it
// stays the operator's, in the terminal.

import { h } from 'preact';
import { useCallback, useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { agentBlockedOnOperator, companionMessageFits, composeCompanionMessage, viewContext } from './companion-model.js';
import { DraftField, FloatingTools, dictationSendPhrase, micTitle, postOperatorMessage, useAnnotation, useDictation } from './dictation-tools.js';
import { nodeFetch } from './node-api.js';
import { shellToast } from './shell-state.js';
import { dashboardState } from './snapshot-store.js';
import { harnessTakesMailMidTurn, voiceContinuous, voiceMarksDictated, voicePauses, voiceSilenceSeconds } from './voice-config.js';

const html = htm.bind(h);

// PaneDictation's last few options are for a chat tab, which floats over any view
// and whose body goes when its window shows another tab. viewTab names the view it
// is over, and quietSpeech stops any reply being read out when the operator starts
// to say something. keep holds what must outlive the body: { drafts, dictated,
// viewOff }. With keep the draft field also lasts through a lost terminal, since
// mail doesn't need one, and failures go to the page, which outlives the pane.

// paneTalksToAgent reports whether a draft from this pane goes to an agent as
// mail. A shell session has an agent id too, but nobody to read mail. A local
// pane whose harness is unknown stays a terminal, which never submits by
// itself; a peer's agent is not in this node's roster, so its pane decides.
const paneRow = (target, snapshot) => (snapshot?.agents || []).find((agent) => agent.agent_id === target?.agent || agent.conv_id === target?.agent);

export function paneTalksToAgent(pane, target, snapshot) {
  if (!target || snapshot?.collab_enabled !== true) return false;
  const row = paneRow(target, snapshot);
  const harness = pane.seed.harness || row?.state?.harness || '';
  if (harness === 'shell') return false;
  return !!(harness || pane.seed.remote);
}

// paneContinuous is continuous mode's settings for an agent's pane, null where
// it can't run. It needs this node's roster, to see the agent ask for its
// operator, so a peer's agent gets one message at a time, as does a harness
// that isn't proven to take mail mid-turn.
export function paneContinuous(pane, target, snapshot, name) {
  const mode = voiceContinuous(snapshot);
  const row = paneRow(target, snapshot);
  if (!mode || !row || pane.seed.remote) return null;
  if (!harnessTakesMailMidTurn(snapshot, pane.seed.harness || row.state?.harness)) return null;
  const agent = { agentId: row.agent_id, convId: row.conv_id, online: true, status: row.state?.status };
  return { ...mode, held: () => (agentBlockedOnOperator(snapshot, agent) ? `${name} is waiting on you, so continuous mode stopped. Your words stay in the draft.` : '') };
}

// terminalDraftText is a draft as one line of terminal input. Control
// characters, line breaks above all, would act rather than read: outside
// bracketed paste a newline runs what came before it.
export function terminalDraftText(text) {
  return String(text || '').replace(/[\u0000-\u001f\u007f-\u009f]+/g, ' ').trim();
}

export function PaneDictation({
  pane, active, solo, status, canType, target, snapshot, actions, win = globalThis.window, notify = shellToast, children = null, keep = null, dictationImpl = null, viewTab = () => 'terminals', quietSpeech = () => {}, fetchImpl = null,
}) {
  const [draft, setDraftText] = useState(() => keep?.drafts.get(pane.key) || '');
  useEffect(() => {
    if (draft) keep?.drafts.set(pane.key, draft);
    else keep?.drafts.delete(pane.key);
  }, [draft]);
  const [sending, setSending] = useState(false);
  const [fieldFocused, setFieldFocused] = useState(false);
  const fieldRef = useCallback((element) => { if (!element) setFieldFocused(false); }, []);
  const [withContext, setContext] = useState(() => !keep?.viewOff.has(pane.key));
  const setWithContext = (on) => {
    setContext(on);
    if (on) keep?.viewOff.delete(pane.key);
    else keep?.viewOff.add(pane.key);
  };
  // A detached window has no toast host, so it says it on the terminal.
  const [notice, setNotice] = useState('');
  const report = solo && !keep ? setNotice : notify;
  const talks = paneTalksToAgent(pane, target, snapshot?.value);
  const connected = String(status || '').startsWith('connected');
  const usable = (connected || (keep && talks)) && (talks || canType);
  const shown = usable && active && (solo || dashboardState.activeTab.value === 'terminals');
  const name = pane.label || 'this agent';
  const viewNow = () => viewContext({ activeTab: viewTab(), pathname: win?.location?.pathname, search: win?.location?.search });
  const agentFetch = () => fetchImpl || nodeFetch(target.node || '');
  const shownRef = useRef(false);
  shownRef.current = shown;
  const toTerminal = () => {
    setFieldFocused(false);
    if (shownRef.current) actions.activatePane(pane.key);
  };
  const dictation = useDictation({
    win, targetId: shown ? pane.key : '', draft, notify: report, dictationImpl, dictated: keep?.dictated,
    sendPhrase: talks ? dictationSendPhrase(snapshot?.value) : '',
    silenceSend: talks ? voiceSilenceSeconds(snapshot?.value) : 0,
    pauses: talks ? voicePauses(snapshot?.value) : null,
    mark: voiceMarksDictated(snapshot?.value),
    continuous: talks ? paneContinuous(pane, target, snapshot?.value, name) : null,
    wakeHost: () => (shownRef.current ? win?.document?.getElementById(pane.id) : null),
    setDraft: (_, text) => setDraftText(text),
    send: (text) => send(null, text),
    onStart: () => {
      setNotice('');
      win?.speechSynthesis?.cancel?.();
      quietSpeech();
    },
    onRelease: toTerminal,
  });
  const annotation = useAnnotation({ win, notify: report, viewNow });

  // Whatever was typed while this went out stays.
  function settle(text) {
    // The body may have gone while this was on its way, and a kept draft that was
    // delivered must not come back with it.
    if (keep?.drafts.get(pane.key) === text) keep.drafts.delete(pane.key);
    setDraftText((current) => (current === text ? '' : current));
    const focused = win?.document?.activeElement;
    if (!focused || focused === win.document.body || focused.matches?.('.companion-draft, .companion-input button[type="submit"]')) toTerminal();
  }

  async function mailAgent(text) {
    const context = withContext ? viewNow() : '';
    const message = dictation.outgoing(text);
    if (!companionMessageFits(message, context)) {
      report('That message is too long to send. Shorten it and try again.', true);
      return false;
    }
    setSending(true);
    try {
      await postOperatorMessage(agentFetch(), { to: target.agent, body: composeCompanionMessage(message, context) });
      settle(text);
      return true;
    } catch (error) {
      report(`Message to ${name} failed: ${error?.message || error}`, true);
      return false;
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
    if (sending || !text.trim()) return false;
    // An open mic would bring the sent text back. Enter or the button sends
    // what the draft holds; a failed send leaves it there.
    if (event) dictation.abort();
    if (talks) return mailAgent(text);
    pasteToTerminal(text);
    return true;
  }

  // A drawing in progress outlives a blip, and keeps its Cancel. The terminal's
  // own tools stay for a pane that takes no drafts, such as a watch-only view.
  if (!shown) {
    return html`${children && html`<div class="companion-floating-anchor">
      <div class="companion-floating" role="group" aria-label="Terminal tools">${children}</div>
    </div>`}${annotation.overlay || null}`;
  }
  const showField = draft || dictation.listening || fieldFocused;
  return html`
    <div class="companion-floating-anchor">
      <${FloatingTools} dictation=${dictation}
        micLabel=${talks ? `Dictate a message to ${name}` : 'Dictate a draft for this terminal'}
        micTitle=${talks ? micTitle(dictation.sendPhrase)
          : 'Dictate a draft. Nothing reaches the terminal until you press Enter here or Paste, and it lands at the prompt unrun. Uses the browser\'s speech recognition; in Chrome the audio is recognised by Google.'}
        withContext=${talks ? withContext : null} onContext=${setWithContext}
        contextNote=${talks && (snapshot?.value?.steward_access?.capture_off || []).includes(target.agent)
          ? `${name} can't capture your screen: that permission is off for it. The attached view says where you are, not what is on screen.` : ''}
        annotateLabel=${`Mark up this page for ${name}`}
        onAnnotate=${talks ? () => annotation.start({ agentId: target.agent, name, fetchImpl: agentFetch(), remote: !!target.node }) : null}>${children}<//>
      ${notice && html`<button type="button" class="companion-pane-notice" role="alert" title="Dismiss" onClick=${() => setNotice('')}>${notice}</button>`}
    </div>
    ${showField && html`<${DraftField} dictation=${dictation} draft=${draft} sending=${sending} fieldRef=${fieldRef}
      label=${talks ? `Message ${name}` : 'Draft for this terminal'}
      submitLabel=${talks ? 'Send' : 'Paste'}
      submitTitle=${talks ? null : 'Paste at the prompt, without a newline: press Enter in the terminal to run it'}
      onSubmit=${send} onEdit=${(text) => { quietSpeech(); setDraftText(text); }}
      onFocus=${() => setFieldFocused(true)} onBlur=${() => setFieldFocused(false)} />`}
    ${annotation.overlay}`;
}
