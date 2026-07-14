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
// Returning the notification's snapshot index also lets the paged mail client
// jump straight to the page that contains it.
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

// adjacentAttentionPages returns the two places a snapshot-derived target can
// move during the race before its mailbox page loads. Newer insertions push it
// to the next page; deletion of newer rows pulls it to the previous page.
export function adjacentAttentionPages(page, totalPages) {
  const pages = [];
  if (page < totalPages) pages.push(page + 1);
  if (page > 1) pages.push(page - 1);
  return pages;
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
    prevGenIds: initial.prevGenIds || new Set(),
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
    onCommit: (payload) => { data.mailboxes = payload.mailboxes || []; touch(); },
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
      prevGenIds: new Set(data.prevGenIds),
      mailboxRequest: mailboxRequest.request.value,
      messageRequest: messageRequest.request.value,
    };
  });
  return Object.freeze({ data, revision, view, touch, mailboxRequest, messageRequest });
}
