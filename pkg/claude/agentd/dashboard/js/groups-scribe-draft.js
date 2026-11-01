// groups-scribe-draft.js -- "Draft steps" in Groups › Processes (tcl-m23mb2).
// The operator's words go to a background process scribe on one stored
// template, with no terminal; its live reply shows in the group body and the
// draft it saves opens in the inline step list for review.
//
// Nothing here starts on its own. A scribe costs a model run, so it starts
// only from the operator's click, after they approved the same grant as every
// other process scribe. That grant can save process templates and cannot
// start a run, and nothing runs until the operator presses Start.

import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { markQuickStart, scribeDraftAsked, scribeDraftName, scribeDraftRequest } from './groups-quickstart.js';
import { handOffProcessDraft } from './process-draft-handoff.js';
import { dashPrefs } from './prefs.js';
import {
  PROCESS_SCRIBE_CHAT_SUBJECT, PROCESS_SCRIBE_GRANT_META, PROCESS_SCRIBE_NAME, PROCESS_SCRIBE_PROMPT_MAX, PROCESS_SCRIBE_SLUGS,
  processScribeBrief, processScribeChatLive, processScribeChatMessage, processScribeChatTranscript, processScribeDraftLabel, processScribeGrantBody, processScribeHandoff, processScribeTaskRef,
} from './process-scribe.js';

const html = htm.bind(h);

export const SCRIBE_DRAFT_POLL_MS = 2000;
const LEFT_DRAFTS_PREF = 'tclaude.dash.groupsProcesses.leftScribeDrafts';
const LEFT_DRAFTS_MAX = 50;
const REVIEWED_DRAFTS_PREF = 'tclaude.dash.groupsProcesses.reviewedScribeDrafts';

function leftDrafts() {
  try {
    const stored = JSON.parse(dashPrefs.getItem(LEFT_DRAFTS_PREF) || '[]');
    return Array.isArray(stored) ? stored : [];
  } catch { return []; }
}

function setLeftDrafts(ids) {
  try { dashPrefs.setItem(LEFT_DRAFTS_PREF, JSON.stringify(ids.slice(-LEFT_DRAFTS_MAX))); } catch { /* best effort */ }
}

// leaveScribeDraft remembers that the operator is done with a draft's panel
// (they reviewed it or hid it), so it does not come back on a reload while its
// scribe stays up. reopenScribeDraft undoes that.
export function leaveScribeDraft(templateId) {
  setLeftDrafts([...leftDrafts().filter((id) => id !== templateId), templateId]);
}

export function reopenScribeDraft(templateId) {
  setLeftDrafts(leftDrafts().filter((id) => id !== templateId));
}

function reviewedDrafts() {
  try {
    const stored = JSON.parse(dashPrefs.getItem(REVIEWED_DRAFTS_PREF) || '[]');
    return Array.isArray(stored) ? stored.filter(Array.isArray) : [];
  } catch { return []; }
}

// noteDraftOpened remembers the version the operator has seen of a process
// while any scribe is up, so Start does not ask about that version again.
export function noteDraftOpened(scribes, templateId, view) {
  const ref = String(view?.currentRef || '');
  if (!ref || !scribes.some((scribe) => scribe.online)) return;
  const kept = reviewedDrafts().filter(([id]) => id !== templateId);
  try {
    dashPrefs.setItem(REVIEWED_DRAFTS_PREF, JSON.stringify([...kept, [templateId, ref]].slice(-LEFT_DRAFTS_MAX)));
  } catch { /* best effort */ }
}

// scribeStartWarning decides whether a Start from the group's ▶ strip asks
// first: only while a scribe is at work on the process. That is the process's
// own scribe in a turn, or a head saved by a live scribe (a scribe can save any
// template, so scope alone does not find it) or by any agent while the
// process's own scribe is up, at a version the operator has not opened. With no
// scribe up, or an idle one and nothing new, it answers null and a rerun stays
// one click. `ref` is the version asked about, for expectedRef. It reads only
// when Start is pressed; a read that fails asks rather than assumes.
export async function scribeStartWarning(api, scribes, templateId) {
  const online = scribes.filter((scribe) => scribe.online);
  if (!online.length) return null;
  let head;
  try {
    head = await api.getTemplateHead(templateId);
  } catch {
    return { ref: '', reason: 'Could not check the current version.' };
  }
  const ref = String(head?.currentRef || '');
  const own = online.find((scribe) => scribe.scope?.id === templateId);
  if (own?.status && own.status !== 'idle') return { ref, reason: 'The scribe is still working.' };
  const actor = String(head?.actor || '');
  const scribeSaved = online.some((scribe) => actor === `agent:${scribe.agentId}`) || (!!own && actor.startsWith('agent:'));
  const reviewed = reviewedDrafts().find(([id]) => id === templateId)?.[1];
  return scribeSaved && ref !== reviewed
    ? { ref, reason: 'A scribe saved a version you have not opened here.' } : null;
}

// DraftStartWarning is the question Start asks while scribeStartWarning answers.
export function DraftStartWarning({ warning, busy = false, openDisabled = false, onOpen, onStartAnyway }) {
  return html`<span class="gq-confirm" role="alert" data-start-draft-warning>
    This draft isn't reviewed: open it, or start anyway? <span class="gq-dim">${warning.reason}</span>
    <button type="button" class="gq-btn" data-start-draft-open disabled=${busy || openDisabled} onClick=${onOpen}>open it</button>
    <button type="button" class="gq-link" data-start-draft-anyway disabled=${busy} onClick=${onStartAnyway}>start anyway</button>
  </span>`;
}

// scribeDraftsOf are the drafts in `processes` with a live scribe still on
// them, rebuilt from the snapshot's scribes: only a scribe started by Draft
// steps carries the draft label. What the panel knew at the start and the
// snapshot does not (the first version, the operator's words) is left empty
// for it to work out.
export function scribeDraftsOf(scribes, processes) {
  return processes.flatMap((process) => {
    const scribe = scribes.find((candidate) => candidate.online && candidate.scope?.id === process.templateId
      && candidate.taskLabel === processScribeDraftLabel(process.templateId));
    if (!scribe) return [];
    return [{
      templateId: process.templateId, name: process.name, sourceHash: '', request: '',
      scribe: { agentId: scribe.agentId, convId: scribe.convId, name: scribe.name },
    }];
  });
}

// restorableScribeDraft is the draft whose panel a group brings back by itself.
export function restorableScribeDraft(scribes, processes) {
  const left = leftDrafts();
  return scribeDraftsOf(scribes, processes).find((draft) => !left.includes(draft.templateId)) || null;
}

// draftWithScribe asks the operator to approve the scribe's grant and only
// then stores a near-empty template, saved to the group (pinned, not listed
// in the library), and summons a background scribe on it. The scribe is
// summoned for a stored template, which is why the draft is stored first. The
// request goes as operator mail, not in the brief, because that is what the
// chat route reads as an open turn: its live tail, and when the turn ends. A
// declined approval stores and starts nothing and answers null. A failed
// start deletes the empty draft again, so nothing is left behind unlisted.
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
      brief: processScribeBrief(handoff), task_ref_url: task.url, task_ref_label: processScribeDraftLabel(templateId), background: true,
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
      view: { ...view, template: { ...view.template, id: templateId }, currentRef, sourceHash, semanticHash: String(body?.semanticHash || '') },
    };
  } catch (cause) {
    if (agentId) await api.stopAgent(agentId).catch(() => {});
    await api.deleteTemplate(templateId).catch(() => api.unpinProcess(group.name, templateId).catch(() => {}));
    throw cause;
  }
}

// draftSteps counts what the scribe saved, for the ready line.
function draftSteps(view) {
  return Object.values(view?.template?.nodes || {}).filter((node) => node?.performer).length;
}

// scribeSaved says a stored version is the scribe's work rather than the empty
// draft it started from. A re-attached panel does not have that first version,
// so it goes by whether anything was added to the empty graph.
function scribeSaved(draft, view) {
  if (draft.sourceHash) return String(view.sourceHash || '') !== draft.sourceHash;
  return Object.values(view?.template?.nodes || {}).some((node) => node?.type !== 'start' && node?.type !== 'end');
}

// useScribeDraft polls the scribe's conversation and the draft template.
// `ready` is the newest version the scribe saved.
function useScribeDraft(api, draft) {
  const [state, setState] = useState({ messages: [], live: null, ready: null, error: '', loaded: false });
  useEffect(() => {
    let stopped = false;
    let timer = null;
    const poll = async () => {
      const [chat, stored] = await Promise.allSettled([api.scribeChat(draft.scribe.agentId), api.getTemplateHead(draft.templateId)]);
      if (stopped) return;
      setState((prev) => {
        const read = chat.status === 'fulfilled' ? chat.value : null;
        const view = stored.status === 'fulfilled' ? stored.value : null;
        const failed = chat.status === 'rejected' ? chat.reason : null;
        return {
          messages: read ? processScribeChatTranscript(read) : prev.messages,
          live: read ? processScribeChatLive(read) : prev.live,
          ready: view && scribeSaved(draft, view) ? view : prev.ready,
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

// scribeFinished says the newest saved draft is the scribe's finished work:
// it is not in a turn, and its last word came after that save. A reply from
// before the save does not count: a scribe's welcome turn can end after the
// request was mailed, and the turn that does the work then looks closed.
// Review waits for this, because a scribe still writing would change the head
// under the step list and Start runs the head. A missing time is not finished:
// stopping the scribe still opens the draft.
export function scribeFinished({ messages, live, ready }) {
  const last = messages.at(-1);
  if (!ready || live || last?.role !== 'scribe') return false;
  return Date.parse(last.at) >= Date.parse(ready.authoredAt);
}

// ScribeDraft is the group body while a scribe drafts: the request, its live
// reply and a way to answer it, then the draft. The operator opens the
// finished draft, so its closing message stays readable and answerable; a
// scribe that has not finished is stopped first. Reviewing starts nothing.
export function ScribeDraft({ group, draft, api, wizard = false, notify, onReview, onClose }) {
  const { messages, live, ready, error, loaded } = useScribeDraft(api, draft);
  const [reply, setReply] = useState('');
  const [busy, setBusy] = useState('');
  const [failure, setFailure] = useState('');
  // The first human message is the request with its group context; the panel
  // shows the operator's own words instead.
  const request = messages.findIndex((message) => message.role === 'human');
  const log = messages.filter((message, index) => index !== request);
  const asked = draft.request || scribeDraftAsked(messages[request]?.body);
  const last = messages.at(-1);
  const working = busy === 'send' || !!live || last?.role === 'human' || !loaded;
  const finished = scribeFinished({ messages, live, ready });
  // Reads the head again: the poll may be a beat behind the last save.
  const review = async () => onReview(await api.getTemplate(draft.templateId));
  const openReview = async () => {
    if (busy) return;
    setBusy('review');
    setFailure('');
    try {
      await review();
    } catch (cause) {
      setFailure(`Could not open the draft: ${cause.message}`);
      setBusy('');
    }
  };
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
  // what the scribe saved so far opens for review.
  const stop = async () => {
    if (busy) return;
    setBusy('stop');
    setFailure('');
    try {
      await api.stopAgent(draft.scribe.agentId);
      notify(`Stopped the scribe. ${draft.name} stays in ${group.name}'s processes.`);
      if (ready) await review();
      else onClose();
    } catch (cause) {
      setFailure(`Could not stop the scribe: ${cause.message}`);
      setBusy('');
    }
  };
  const graph = () => handOffProcessDraft({ id: draft.templateId, name: draft.name, view: ready || draft.view });
  return html`<section class="gq-draft" aria-label=${`Draft of ${draft.name}`} data-scribe-draft=${draft.templateId}>
    <div class="gq-start-head">
      <h3>${wizard ? `A scribe sets down ${draft.name}` : `A scribe is drafting ${draft.name}`}</h3>
      <span class="gq-tag" title="It holds process.templates.read and process.templates.manage: it can save process templates, and it cannot start a run">background · ${draft.scribe.name}</span>
    </div>
    ${asked && html`<p class="gq-draft-ask" data-scribe-request>${asked}</p>`}
    <div class="gq-draft-log" aria-live="polite">
      ${log.map((message) => html`<div key=${message.id} class=${`gq-draft-msg is-${message.role}`} data-scribe-msg=${message.role}>${message.body}</div>`)}
      ${(live?.items || []).map((item, index) => html`<div key=${index} class=${item.kind === 'tool' ? 'gq-draft-tool' : 'gq-draft-msg is-scribe is-live'} data-scribe-live=${item.kind}>${item.text}</div>`)}
      ${working && html`<p class="gq-draft-working" role="status" title=${live?.unavailable || ''}><span class="psd-dot" aria-hidden="true"></span>scribe is working…</p>`}
    </div>
    ${ready && html`<p class="gq-draft-ready" data-scribe-ready>${finished
    ? `Draft saved: ${draftSteps(ready)} ${draftSteps(ready) === 1 ? 'step' : 'steps'}. Nothing runs until you press Start.`
    : `${draftSteps(ready)} ${draftSteps(ready) === 1 ? 'step' : 'steps'} saved so far. Stop the scribe to review them now.`}</p>`}
    <form class="gq-draft-reply" onSubmit=${send}>
      <input type="text" aria-label="Answer the scribe" data-scribe-reply maxlength=${PROCESS_SCRIBE_PROMPT_MAX} value=${reply}
        placeholder="Answer the scribe, or ask for a change" onInput=${(event) => setReply(event.currentTarget.value)} />
      <button type="submit" class="gq-btn" data-scribe-send disabled=${!reply.trim() || !!busy}>${busy === 'send' ? 'sending…' : 'Send'}</button>
    </form>
    <div class="gq-foot">
      <button type="button" class="gq-btn primary" data-scribe-review disabled=${!finished || !!busy} title=${finished ? 'Open the saved draft in the step list' : 'The scribe has not finished a draft yet'}
        onClick=${openReview}>Review steps</button>
      <button type="button" class="gq-btn" data-scribe-graph onClick=${graph} title="Continue with this scribe beside the graph editor">⤢ open in graph editor</button>
      <button type="button" class="gq-link" data-scribe-stop disabled=${!!busy} onClick=${stop} title="Stop the scribe; the draft stays in the group, and what it saved opens for review">${busy === 'stop' ? 'stopping…' : 'stop scribe'}</button>
      <button type="button" class="gq-link" data-scribe-hide disabled=${!!busy} onClick=${onClose} title="Hide this panel and leave the scribe working; ▶ on the process brings it back">hide</button>
    </div>
    ${(failure || error) && html`<div class="gq-error" role="alert">${failure || error}</div>`}
  </section>`;
}
