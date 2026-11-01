// groups-scribe-draft.js -- "Draft steps" in Groups › Processes (tcl-m23mb2).
// The operator's words go to a background process scribe, scoped to one
// stored template, with no terminal; its live reply shows in the group body
// and the draft it saves opens in the inline step list for review.
//
// Nothing here starts on its own. A scribe costs a model run, so it starts
// only from the operator's click, after they approved the same grant as every
// other process scribe. It writes template versions and nothing else, and
// nothing runs until the operator presses Start in the editor.

import { h } from 'preact';
import { useEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { markQuickStart, scribeDraftName, scribeDraftRequest } from './groups-quickstart.js';
import { handOffProcessDraft } from './process-draft-handoff.js';
import {
  PROCESS_SCRIBE_CHAT_SUBJECT, PROCESS_SCRIBE_GRANT_META, PROCESS_SCRIBE_NAME, PROCESS_SCRIBE_PROMPT_MAX, PROCESS_SCRIBE_SLUGS,
  processScribeBrief, processScribeChatLive, processScribeChatMessage, processScribeChatTranscript, processScribeGrantBody, processScribeHandoff, processScribeScopeLabel, processScribeTaskRef,
} from './process-scribe.js';

const html = htm.bind(h);

export const SCRIBE_DRAFT_POLL_MS = 2000;

// draftWithScribe asks the operator to approve the scribe's grant and only
// then stores a near-empty template, saved to the group (pinned, not listed
// in the library), and summons a background scribe on it. The scribe is
// scoped to a stored template, which is why the draft is stored first. The
// request goes as operator mail, not in the brief, because that is what the
// chat route reads as an open turn: its live tail, and when the turn ends. A
// declined approval stores and starts nothing and answers null. A failed
// start takes the empty draft out of the group again.
export async function draftWithScribe(api, { group, text, roster = [], confirm }) {
  const name = scribeDraftName(text);
  const approved = await confirm({
    title: 'Grant process-scribe access?',
    body: processScribeGrantBody(`a new draft process, "${name}"`),
    meta: PROCESS_SCRIBE_GRANT_META,
    okLabel: 'Grant & draft',
  });
  if (!approved) return null;
  // Loaded on demand, as the Processes tab does: the graph model is not part
  // of the Groups tab's startup.
  const { blankEditView } = await import('./process-edit-model.js');
  const view = blankEditView(name);
  const body = await api.createTemplate(view, { listed: false, pinGroup: group.name });
  const templateId = String(body?.id || '');
  if (!templateId) throw new Error('the template was not created');
  const sourceHash = String(body?.sourceHash || '');
  const currentRef = String(body?.ref || '');
  let agentId = '';
  try {
    const handoff = processScribeHandoff({ kind: 'template', id: templateId, currentRef, sourceHash });
    const task = processScribeTaskRef(handoff);
    const scribe = await api.summonScribe({
      name: PROCESS_SCRIBE_NAME, slugs: PROCESS_SCRIBE_SLUGS, exclusive: true, scope: handoff.scope,
      brief: processScribeBrief(handoff), task_ref_url: task.url, task_ref_label: task.label, background: true,
    });
    agentId = String(scribe?.agent_id || '');
    const convId = String(scribe?.conv_id || '');
    if (!agentId || !convId) throw new Error('the scribe did not start');
    await api.sendScribeMessage(convId, {
      subject: PROCESS_SCRIBE_CHAT_SUBJECT,
      body: processScribeChatMessage({ prompt: scribeDraftRequest(text, group.name, roster) }),
    });
    markQuickStart();
    return {
      templateId, name, sourceHash, request: String(text || '').trim(),
      scribe: { agentId, convId, name: String(scribe?.name || PROCESS_SCRIBE_NAME) },
      scopeLabel: processScribeScopeLabel(handoff.scope),
      view: { ...view, template: { ...view.template, id: templateId }, currentRef, sourceHash, semanticHash: String(body?.semanticHash || '') },
    };
  } catch (cause) {
    if (agentId) await api.stopAgent(agentId).catch(() => {});
    await api.unpinProcess(group.name, templateId).catch(() => {});
    throw cause;
  }
}

// draftSteps counts what the scribe saved, for the ready line.
function draftSteps(view) {
  return Object.values(view?.template?.nodes || {}).filter((node) => node?.performer).length;
}

// useScribeDraft polls the scribe's conversation and the draft template.
// `ready` is the newest version the scribe saved; `idle` says it is not in a
// turn, so a saved version is its finished work, not a step along the way.
function useScribeDraft(api, draft) {
  const [state, setState] = useState({ messages: [], live: null, ready: null, error: '', loaded: false });
  useEffect(() => {
    let stopped = false;
    let timer = null;
    const poll = async () => {
      const [chat, stored] = await Promise.allSettled([api.scribeChat(draft.scribe.agentId), api.getTemplate(draft.templateId)]);
      if (stopped) return;
      setState((prev) => {
        const read = chat.status === 'fulfilled' ? chat.value : null;
        const view = stored.status === 'fulfilled' ? stored.value : null;
        const failed = chat.status === 'rejected' ? chat.reason : null;
        return {
          messages: read ? processScribeChatTranscript(read) : prev.messages,
          live: read ? processScribeChatLive(read) : prev.live,
          ready: view && String(view.sourceHash || '') !== draft.sourceHash ? view : prev.ready,
          error: failed ? failed.message || String(failed) : '',
          loaded: prev.loaded || !!read,
        };
      });
      timer = setTimeout(poll, SCRIBE_DRAFT_POLL_MS);
    };
    void poll();
    return () => { stopped = true; clearTimeout(timer); };
  }, [api, draft.scribe.agentId, draft.templateId, draft.sourceHash]);
  return state;
}

// ScribeDraft is the group body while a scribe drafts: the request, its live
// reply and a way to answer it, then the draft. A finished draft opens for
// review by itself; while the scribe is still working, Review opens the
// version saved so far. Reviewing starts nothing.
export function ScribeDraft({ group, draft, api, wizard = false, notify, onReview, onClose }) {
  const { messages, live, ready, error, loaded } = useScribeDraft(api, draft);
  const [reply, setReply] = useState('');
  const [busy, setBusy] = useState('');
  const [failure, setFailure] = useState('');
  const opened = useRef(false);
  // The first human message is the request with its group context; the panel
  // shows the operator's own words instead.
  const request = messages.findIndex((message) => message.role === 'human');
  const log = messages.filter((message, index) => index !== request);
  const last = messages.at(-1);
  const working = busy === 'send' || !!live || last?.role === 'human' || !loaded;
  const review = (view) => {
    opened.current = true;
    onReview(view);
  };
  useEffect(() => {
    if (ready && !working && !opened.current) review(ready);
  }, [ready, working]);
  const send = async (event) => {
    event.preventDefault();
    if (!reply.trim() || busy) return;
    setBusy('send');
    setFailure('');
    try {
      await api.sendScribeMessage(draft.scribe.convId, { subject: PROCESS_SCRIBE_CHAT_SUBJECT, body: processScribeChatMessage({ prompt: reply }) });
      setReply('');
    } catch (cause) {
      setFailure(`Could not send: ${cause.message}`);
    } finally {
      setBusy('');
    }
  };
  // Stopping keeps the draft: it stays one of the group's processes, and
  // whatever the scribe saved so far is there to edit or remove.
  const stop = async () => {
    if (busy) return;
    setBusy('stop');
    setFailure('');
    try {
      await api.stopAgent(draft.scribe.agentId);
      notify(`Stopped the scribe. ${draft.name} stays in ${group.name}'s processes.`);
      onClose();
    } catch (cause) {
      setFailure(`Could not stop the scribe: ${cause.message}`);
      setBusy('');
    }
  };
  const graph = () => handOffProcessDraft({ id: draft.templateId, name: draft.name, view: ready || draft.view });
  return html`<section class="gq-draft" aria-label=${`Draft of ${draft.name}`} data-scribe-draft=${draft.templateId}>
    <div class="gq-start-head">
      <h3>${wizard ? `A scribe sets down ${draft.name}` : `A scribe is drafting ${draft.name}`}</h3>
      <span class="gq-tag" title=${`Scoped to ${draft.scopeLabel}. It can save versions of this draft and cannot start it.`}>background · ${draft.scribe.name}</span>
    </div>
    <p class="gq-draft-ask" data-scribe-request>${draft.request}</p>
    <div class="gq-draft-log" aria-live="polite">
      ${log.map((message) => html`<div key=${message.id} class=${`gq-draft-msg is-${message.role}`} data-scribe-msg=${message.role}>${message.body}</div>`)}
      ${(live?.items || []).map((item, index) => html`<div key=${index} class=${item.kind === 'tool' ? 'gq-draft-tool' : 'gq-draft-msg is-scribe is-live'} data-scribe-live=${item.kind}>${item.text}</div>`)}
      ${working && html`<p class="gq-draft-working" role="status" title=${live?.unavailable || ''}><span class="psd-dot" aria-hidden="true"></span>scribe is working…</p>`}
    </div>
    ${ready && html`<p class="gq-draft-ready" data-scribe-ready>Draft saved: ${draftSteps(ready)} ${draftSteps(ready) === 1 ? 'step' : 'steps'}. Nothing runs until you press Start.</p>`}
    <form class="gq-draft-reply" onSubmit=${send}>
      <input type="text" aria-label="Answer the scribe" data-scribe-reply maxlength=${PROCESS_SCRIBE_PROMPT_MAX} value=${reply}
        placeholder="Answer the scribe, or ask for a change" onInput=${(event) => setReply(event.currentTarget.value)} />
      <button type="submit" class="gq-btn" data-scribe-send disabled=${!reply.trim() || !!busy}>${busy === 'send' ? 'sending…' : 'Send'}</button>
    </form>
    <div class="gq-foot">
      <button type="button" class="gq-btn primary" data-scribe-review disabled=${!ready} title=${ready ? 'Open the saved draft in the step list' : 'The scribe has not saved a draft yet'}
        onClick=${() => review(ready)}>Review steps</button>
      <button type="button" class="gq-btn" data-scribe-graph onClick=${graph} title="Continue with this scribe beside the graph editor">⤢ open in graph editor</button>
      <button type="button" class="gq-link" data-scribe-stop disabled=${!!busy} onClick=${stop} title="Stop the scribe; the draft stays in the group">${busy === 'stop' ? 'stopping…' : 'stop scribe'}</button>
    </div>
    ${(failure || error) && html`<div class="gq-error" role="alert">${failure || error}</div>`}
  </section>`;
}
