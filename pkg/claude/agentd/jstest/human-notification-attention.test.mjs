import test from 'node:test';
import assert from 'node:assert/strict';
import {
  groupHasUnreadHumanNotifications,
  hasUnreadHumanNotifications,
  humanNotificationSenderQuery,
  memberUnreadHumanCount,
} from '../dashboard/js/human-notification-attention.js';

const alice = { agent_id: 'agt_alice', conv_id: 'conv-alice-now' };
const bob = { agent_id: 'agt_bob', conv_id: 'conv-bob' };

test('member unread count prefers stable agent identity across generations', () => {
  const snapshot = {
    messages: [
      { from_agent: 'agt_alice', from_conv: 'conv-alice-old', read: false },
      { from_agent: 'agt_alice', from_conv: 'conv-alice-now', read: true },
      { from_agent: 'agt_bob', from_conv: 'conv-bob', read: false },
    ],
  };
  assert.equal(memberUnreadHumanCount(alice, snapshot), 1);
  assert.equal(memberUnreadHumanCount(bob, snapshot), 1);
  assert.equal(groupHasUnreadHumanNotifications([alice], snapshot), true);
  assert.equal(groupHasUnreadHumanNotifications([{ agent_id: 'agt_none' }], snapshot), false);
});

test('legacy notifications fall back to the sending conversation id', () => {
  const snapshot = {
    messages: [{ from_agent: '', from_conv: 'conv-alice-now', read: false }],
  };
  assert.equal(memberUnreadHumanCount(alice, snapshot), 1);
});

test('global attention follows the unread count with message fallback', () => {
  assert.equal(hasUnreadHumanNotifications({ messages_unread: 2, messages: [] }), true);
  assert.equal(hasUnreadHumanNotifications({
    messages_unread: 0,
    messages: [{ from_agent: 'agt_alice', read: false }],
  }), true);
  assert.equal(hasUnreadHumanNotifications({
    messages_unread: 0,
    messages: [{ from_agent: 'agt_alice', read: true }],
  }), false);
});

test('deep-link query uses stable agent id then conversation fallback', () => {
  assert.equal(humanNotificationSenderQuery(alice), 'agt_alice');
  assert.equal(humanNotificationSenderQuery({ conv_id: 'conv-legacy' }), 'conv-legacy');
});
