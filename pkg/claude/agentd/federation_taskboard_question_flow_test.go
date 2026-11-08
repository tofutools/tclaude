package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// An agent here asked a linked node's operator a question: the answer mail
// from that node is let in once, for the question and the agent recorded,
// without a message.direct grant. A forged or untagged answer, one for
// another agent, an expired record and a replay are refused as any
// unexported mail is.
func TestFederation_TaskboardAnswerToPeerAsker(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	const alice = "fedq-alice-bbbb-cccc-000000000001"
	const carol = "fedq-carol-bbbb-cccc-000000000002"
	f.HaveGroup("private")
	f.HaveConvWithTitle(alice, "alice-agent")
	f.HaveMember("private", alice)
	f.HaveConvWithTitle(carol, "carol-agent")
	f.HaveMember("private", carol)
	aliceAgent, err := db.AgentIDForConv(alice)
	require.NoError(t, err)
	carolAgent, err := db.AgentIDForConv(carol)
	require.NoError(t, err)

	now := time.Now()
	require.NoError(t, db.RecordFederationPeerQuestion(p.id.ID(), "tk_0000aaaa", aliceAgent, now, now.Add(time.Hour)))
	require.NoError(t, db.RecordFederationPeerQuestion(p.id.ID(), "tk_0000cccc", aliceAgent, now.Add(-2*time.Hour), now.Add(-time.Hour)))

	answer := func(to, task string) *proto.Envelope {
		env := p.envelope(proto.KindMail, proto.Endpoint{Agent: to},
			proto.MailPayload{Subject: "[taskboard] answered: crew · " + task, Body: "Picked: Amber", TaskboardAnswer: task})
		p.send(env)
		return env
	}
	refused := func(env *proto.Envelope, what string) {
		t.Helper()
		ack := fedAckFor(t, p, env.ID)
		assert.Equal(t, proto.AckRefused, ack.Status, what)
		assert.Equal(t, "not_exported", ack.Code, what)
	}

	// Untagged mail, a question never asked, another agent and an expired
	// record are all refused.
	plain := p.envelope(proto.KindMail, proto.Endpoint{Agent: aliceAgent}, proto.MailPayload{Body: "no tag"})
	p.send(plain)
	refused(plain, "untagged mail")
	refused(answer(aliceAgent, "tk_0000bbbb"), "a forged answer to a question never asked")
	refused(answer(carolAgent, "tk_0000aaaa"), "the answer to another agent")
	refused(answer(aliceAgent, "tk_0000cccc"), "an expired record")

	// A full inbox refuses the recorded answer for now (the sender
	// retries), and keeps the record: the retry, once there is room, is let
	// in.
	require.NoError(t, db.GrantAgentPermissionWithScope(carol, agentd.PermMessageDirect, "", "test"))
	for i := range agentd.RegularAgentMessageQueueLimitForTest {
		rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/messages",
			map[string]any{"to": "alice-agent", "body": "filler"}), carol))
		require.Equalf(t, http.StatusOK, rec.Code, "filler %d: %s", i, rec.Body.String())
	}
	ok := answer(aliceAgent, "tk_0000aaaa")
	full := fedAckFor(t, p, ok.ID)
	require.Equal(t, proto.AckRefused, full.Status)
	require.Equal(t, "queue_full", full.Code)
	queued, err := db.ListInboxForActor(alice, aliceAgent, 50)
	require.NoError(t, err)
	for _, m := range queued {
		require.NoError(t, db.MarkAgentMessageRead(m.ID))
	}
	p.mu.Lock()
	p.got = nil
	p.mu.Unlock()
	p.send(ok)
	assert.Equal(t, proto.AckAccepted, fedAckFor(t, p, ok.ID).Status, "the retry finds the record")
	msgs, err := db.ListInboxForActor(alice, aliceAgent, 50)
	require.NoError(t, err)
	var got string
	for _, m := range msgs {
		if strings.Contains(m.Subject, "answered: crew · tk_0000aaaa") {
			got = m.Body
		}
	}
	assert.Contains(t, got, "Picked: Amber")

	// Single-use: a replay in a new envelope is refused.
	refused(answer(aliceAgent, "tk_0000aaaa"), "a replayed answer")
}

// A question asked by a linked node's agent is answered here: the answer
// goes back to that agent as federation mail, tagged with the task.
func TestFederation_TaskboardAnswerMailedToPeerAsker(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	g := f.HaveGroup("crew")
	const bob = "agt_bobremote0000000000000000"
	q := &db.GroupTaskQuestion{Options: []db.GroupTaskQuestionOption{{Label: "Amber"}, {Label: "Teal"}}}
	require.NoError(t, db.NormalizeGroupTaskQuestion(q))
	q.AskedVia = "peer:" + p.id.ID() + "/agent:" + bob
	task, err := db.CreateGroupTask(db.NewGroupTask{GroupID: g.ID, Title: "Lane colour?", Stage: db.GroupTaskStageNext,
		Actor: "peer: agent bob-agent (" + bob + ") of bob", AssigneeID: db.OperatorAssignee, Question: q})
	require.NoError(t, err)

	rec := fedHuman(t, f, http.MethodPost, "/v1/taskboard/tasks/"+task.ID+"/answer", map[string]any{"picked": []string{"teal"}, "text": "dimmer"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var mail proto.MailPayload
	var to proto.Endpoint
	fedEventually(t, "the answer mailed to the peer's asker", func() bool {
		for _, env := range p.envelopes(proto.KindMail) {
			var mp proto.MailPayload
			if env.DecodePayload(&mp) == nil && mp.TaskboardAnswer == task.ID {
				mail, to = mp, env.To
				return true
			}
		}
		return false
	})
	assert.Equal(t, bob, to.Agent)
	assert.Contains(t, mail.Subject, "answered: crew · "+task.ID)
	assert.Contains(t, mail.Body, "Picked: Teal")
	assert.Contains(t, mail.Body, "Said: dimmer")
}

// An agent's question on a linked node's board, through its peer view, is
// recorded for the answer to come back: only a create that asks a question
// and that the peer accepted, for that agent.
func TestFederation_TaskboardPeerQuestionRecorded(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	agentd.ResetAgentPeerViewLimitsForTest()
	t.Cleanup(agentd.ResetAgentPeerViewLimitsForTest)
	inst := p.id.ID()
	const caller = "019fe740-43a4-7023-b8ae-0000000a7e01"
	f.HaveEnrolledAgent(caller)
	callerAgent, err := db.AgentIDForConv(caller)
	require.NoError(t, err)
	p.send(p.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{PeerViewAgents: true, Groups: []proto.CatalogGroup{{Name: "lab"}}}))
	fedEventually(t, "catalog stored", func() bool { c, _, _ := db.GetFederationCatalog(inst); return c != "" })
	require.NoError(t, db.GrantAgentPermissionWithScope(caller, agentd.PermTaskboardWrite, `{"peer":["`+inst+`/lab"]}`, "test"))

	opened := 0
	create := func(body map[string]any, status int, reply string) {
		t.Helper()
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			done <- testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost,
				"/v1/federation/peer/bob/v1/groups/lab/taskboard/tasks", body), caller))
		}()
		conn := acceptOutboundPeerView(t, p, opened)
		opened++
		readPeerViewWire(t, conn)
		writePeerViewWire(t, conn, peerViewWire{Status: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(reply)})
		rec := <-done
		require.Equal(t, status, rec.Code, rec.Body.String())
		_ = conn.Close()
	}
	create(map[string]any{"title": "Lane colour?", "question": map[string]any{}}, http.StatusCreated, `{"id":"tk_0000eeee","title":"Lane colour?"}`)
	create(map[string]any{"title": "plain"}, http.StatusCreated, `{"id":"tk_0000ffff","title":"plain"}`)
	create(map[string]any{"title": "refused?", "question": map[string]any{}}, http.StatusBadRequest, `{"id":"tk_00001111","error":"no"}`)

	for task, want := range map[string]string{"tk_0000eeee": proto.AckAccepted, "tk_0000ffff": proto.AckRefused, "tk_00001111": proto.AckRefused} {
		env := p.envelope(proto.KindMail, proto.Endpoint{Agent: callerAgent}, proto.MailPayload{Body: "answer", TaskboardAnswer: task})
		p.send(env)
		assert.Equal(t, want, fedAckFor(t, p, env.ID).Status, task)
	}
}

// A question on a task (tk_8b2c86f7) across nodes: an agent here asks Q2
// on a linked node's task through its peer view, and the answer mail,
// tagged "<task>#2", is let in; the task's own tag is not.
func TestFederation_TaskboardPeerQuestionOnTaskRecorded(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	agentd.ResetAgentPeerViewLimitsForTest()
	t.Cleanup(agentd.ResetAgentPeerViewLimitsForTest)
	inst := p.id.ID()
	const caller = "019fe740-43a4-7023-b8ae-0000000a7e02"
	f.HaveEnrolledAgent(caller)
	callerAgent, err := db.AgentIDForConv(caller)
	require.NoError(t, err)
	p.send(p.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{PeerViewAgents: true, Groups: []proto.CatalogGroup{{Name: "lab"}}}))
	fedEventually(t, "catalog stored", func() bool { c, _, _ := db.GetFederationCatalog(inst); return c != "" })
	require.NoError(t, db.GrantAgentPermissionWithScope(caller, agentd.PermTaskboardWrite, `{"peer":["`+inst+`"]}`, "test"))

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost,
			"/v1/federation/peer/bob/v1/taskboard/tasks/tk_0000abcd/questions", map[string]any{"title": "Which day?"}), caller))
	}()
	conn := acceptOutboundPeerView(t, p, 0)
	readPeerViewWire(t, conn)
	writePeerViewWire(t, conn, peerViewWire{Status: http.StatusCreated, Header: http.Header{"Content-Type": {"application/json"}},
		Body: []byte(`{"task":{"id":"tk_0000abcd","title":"Release"},"question":{"n":2,"title":"Which day?"}}`)})
	rec := <-done
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	_ = conn.Close()

	for task, want := range map[string]string{"tk_0000abcd": proto.AckRefused, "tk_0000abcd#2": proto.AckAccepted} {
		env := p.envelope(proto.KindMail, proto.Endpoint{Agent: callerAgent}, proto.MailPayload{Body: "answer", TaskboardAnswer: task})
		p.send(env)
		assert.Equal(t, want, fedAckFor(t, p, env.ID).Status, task)
	}
}

// Q2 on a task here, asked by a linked node's agent, is answered: the
// answer goes back tagged "<task>#2".
func TestFederation_TaskboardAnswerOnTaskMailedToPeerAsker(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	g := f.HaveGroup("crew")
	const bob = "agt_bobremote0000000000000001"
	task, err := db.CreateGroupTask(db.NewGroupTask{GroupID: g.ID, Title: "Release", Stage: db.GroupTaskStageNext, Actor: "human:operator"})
	require.NoError(t, err)
	for _, title := range []string{"Which day?", "Who announces?"} {
		_, _, err := db.AskGroupTaskQuestion(task.ID, &db.GroupTaskQuestion{Title: title,
			AskedBy: "peer: agent bob-agent (" + bob + ") of bob", AskedVia: "peer:" + p.id.ID() + "/agent:" + bob})
		require.NoError(t, err)
	}
	rec := fedHuman(t, f, http.MethodPost, "/v1/taskboard/tasks/"+task.ID+"/questions/2/answer", map[string]any{"text": "the lead"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var mail proto.MailPayload
	fedEventually(t, "the answer mailed to the peer's asker", func() bool {
		for _, env := range p.envelopes(proto.KindMail) {
			var mp proto.MailPayload
			if env.DecodePayload(&mp) == nil && mp.TaskboardAnswer == task.ID+"#2" {
				mail = mp
				return true
			}
		}
		return false
	})
	assert.Contains(t, mail.Subject, "answered Q2: crew · "+task.ID)
	assert.Contains(t, mail.Body, "Said: the lead")
}
