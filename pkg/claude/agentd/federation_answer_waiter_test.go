package agentd

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

func TestFedAnswerWaiter_AcceptsOnlyTheBoundAnswer(t *testing.T) {
	w := newFedAnswerWaiter[proto.RouteAnswerPayload]("inst_bob", "env-open")

	assert.False(t, w.offer("inst_mallory", "env-open", proto.RouteAnswerPayload{Reason: "wrong peer"}))
	assert.False(t, w.offer("inst_bob", "env-other", proto.RouteAnswerPayload{Reason: "wrong request"}))
	assert.False(t, w.offer("inst_bob", "", proto.RouteAnswerPayload{Reason: "unbound"}))
	assert.True(t, w.offer("inst_bob", "env-open", proto.RouteAnswerPayload{OK: true}))
	assert.False(t, w.offer("inst_bob", "env-open", proto.RouteAnswerPayload{Reason: "duplicate"}),
		"a waiter holds one answer and never blocks the receive loop")
	require.True(t, (<-w.answer).OK)

	var missing fedAnswerWaiter[proto.RouteAnswerPayload]
	assert.False(t, missing.offer("inst_bob", "env-open", proto.RouteAnswerPayload{}),
		"an answer for a stream nobody waits on is dropped")
}

// TestHandleRouteAnswer_IgnoresUnboundAnswers drives the route answer handler
// with answers that name a pending stream but do not belong to its open. Each
// is dropped and the open keeps waiting, and the real answer still lands. An
// answer from the wrong instance used to abort the open.
func TestHandleRouteAnswer_IgnoresUnboundAnswers(t *testing.T) {
	rt := &fedRuntime{}
	sid := proto.NewEnvelopeID()
	w := newFedAnswerWaiter[proto.RouteAnswerPayload]("inst_bob", "env-open")
	rt.mu.Lock()
	rt.routesLocked().waiters[sid] = w
	rt.mu.Unlock()

	answer := func(from, inReplyTo string, a proto.RouteAnswerPayload) {
		t.Helper()
		raw, err := json.Marshal(a)
		require.NoError(t, err)
		env := &proto.Envelope{ID: proto.NewEnvelopeID(), Kind: proto.KindRouteAnswer, InReplyTo: inReplyTo, Payload: raw}
		rt.handleRouteAnswer(&db.FederationPeer{InstanceID: from}, env)
	}
	answer("inst_mallory", "env-open", proto.RouteAnswerPayload{Stream: sid, Reason: "wrong instance"})
	answer("inst_bob", "env-other", proto.RouteAnswerPayload{Stream: sid, Reason: "wrong request"})
	answer("inst_bob", "env-open", proto.RouteAnswerPayload{Stream: "not a stream id", Reason: "bad stream"})
	select {
	case a := <-w.answer:
		t.Fatalf("an unbound answer reached the open: %+v", a)
	default:
	}

	answer("inst_bob", "env-open", proto.RouteAnswerPayload{Stream: sid, OK: true})
	select {
	case a := <-w.answer:
		assert.True(t, a.OK)
	default:
		t.Fatal("the bound answer did not reach the open")
	}
}
