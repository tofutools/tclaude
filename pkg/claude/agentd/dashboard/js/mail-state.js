import { computed, signal } from '@preact/signals';
import { createRequestLifecycle } from './request-lifecycle.js';

export function messageDeleteEndpoint(folderID) {
  return folderID === 'human' ? '/api/human-messages/delete' : '/api/mailbox/delete';
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
