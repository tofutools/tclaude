package agentd_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
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

	// The recorded answer is let in.
	ok := answer(aliceAgent, "tk_0000aaaa")
	assert.Equal(t, proto.AckAccepted, fedAckFor(t, p, ok.ID).Status)
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
