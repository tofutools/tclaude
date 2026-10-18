import { computed, signal } from '@preact/signals';
import { createRequestLifecycle } from './request-lifecycle.js';

export const HUMAN_MAILBOX_ID = 'human';

export function messageDeleteEndpoint(folderID) {
  return folderID === HUMAN_MAILBOX_ID ? '/api/human-messages/delete' : '/api/mailbox/delete';
}

// nextMessagesAttention picks the item the Messages-tab badge is asking the
// operator to handle. Access requests outrank notifications because they block
// an agent in real time. Both snapshot lists have a defined order: pending
// access requests are oldest-first, while human notifications are
// newest-first, so scan the latter backwards to find the oldest unread one.
// The index is into the snapshot's bounded list (unread + recent); the page
// holding the row is resolved server-side via /api/mailbox?focus=.
export function nextMessagesAttention(snapshot) {
  const requests = Array.isArray(snapshot?.access_requests) ? snapshot.access_requests : [];
  const access = requests.find((request) => !request.status || request.status === 'pending');
  if (access) return { kind: 'access', id: String(access.id) };

  const messages = Array.isArray(snapshot?.messages) ? snapshot.messages : [];
  for (let index = messages.length - 1; index >= 0; index--) {
    if (!messages[index].read) {
      return { kind: 'notification', id: Number(messages[index].id), index };
    }
  }
  return null;
}

// prepareMessagesAttention resets state whose meaning depends on the current
// filter. Checked message IDs may span pages, so retaining them while an
// attention jump clears the filter could make a later bulk action touch rows
// the operator selected under a different message universe.
export function prepareMessagesAttention(data) {
  data.messageQuery = '';
  data.selectedMsgs.clear();
}

// Messages has several coordinated Sets plus server-paged data. Keep the
// mutable working set private to the feature and publish immutable snapshots
// to Preact through one revision signal; every action completes its related
// mutations before touching the revision, so components never observe a
// half-applied selection/page transition.
export function createMailState(initial) {
  const data = {
    ...initial,
    mailboxes: initial.mailboxes || [],
    messages: initial.messages || [],
    selectedMsgs: initial.selectedMsgs || new Set(),
    selectedBoxes: initial.selectedBoxes || new Set(),
  };
  const revision = signal(0);
  const touch = () => { revision.value += 1; };
  const mailboxPayload = signal(null);
  const messagePayload = signal(null);
  const mailboxRequest = createRequestLifecycle({
    payload: mailboxPayload,
    retainPayloadOnRefresh: true,
    retainPayloadOnError: true,
    onCommit: (payload) => {
      data.mailboxes = payload.mailboxes || [];
      // The roster is server-paged: these describe the whole filtered agent
      // list and the open folder, not just the page in mailboxes.
      data.agentsTotal = payload.agents_total || 0;
      data.agentsUnread = payload.agents_unread || 0;
      data.selectedMailbox = payload.selected_mailbox || null;
      data.selectedValid = payload.selected_valid !== false;
      data.selectedFor = payload.selected ?? null;
      touch();
    },
  });
  const messageRequest = createRequestLifecycle({
    payload: messagePayload,
    retainPayloadOnRefresh: true,
    retainPayloadOnError: false,
    onCommit: (payload) => {
      data.messages = payload.messages || [];
      if (typeof payload.page === 'number') data.page = payload.page;
      if (typeof payload.page_size === 'number') data.pageSize = payload.page_size;
      data.total = payload.total || 0;
      data.totalUnfiltered = payload.total_unfiltered || 0;
      touch();
    },
  });
  const view = computed(() => {
    revision.value;
    return {
      ...data,
      selectedMsgs: new Set(data.selectedMsgs),
      selectedBoxes: new Set(data.selectedBoxes),
      mailboxRequest: mailboxRequest.request.value,
      messageRequest: messageRequest.request.value,
    };
  });
  return Object.freeze({ data, revision, view, touch, mailboxRequest, messageRequest });
}
