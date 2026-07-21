package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// `tclaude agent reply` threads by inheriting the original subject as
// "Re: <subject>", and --subject overrides that. Both branches live in
// handleMessageReply; only the human-reply path's auto-prefix was covered
// before, so an override regression on the agent path would have been
// silent.
func TestMessageReply_SubjectDefaultsToReAndIsOverridable(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	const senderConv = "send-aaaa-bbbb-cccc-111111111111"
	const workerConv = "work-aaaa-bbbb-cccc-222222222222"
	f.HaveMember("alpha", senderConv)
	f.HaveMember("alpha", workerConv)

	// The original message carries a subject for the reply to inherit.
	rr := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodPost, "/v1/messages", map[string]any{
			"to":      workerConv,
			"subject": "deploy plan",
			"body":    "please review",
		}), senderConv)
	rec := testharness.Serve(f.Mux, rr)
	require.Equal(t, http.StatusOK, rec.Code, "send body=%s", rec.Body.String())

	inbox, err := db.ListAgentMessagesForConv(workerConv, 100)
	require.NoError(t, err, "ListAgentMessagesForConv(worker)")
	require.Len(t, inbox, 1, "worker should have the original message")
	orig := inbox[0]
	require.Equal(t, "deploy plan", orig.Subject)

	reply := func(body map[string]any) {
		t.Helper()
		path := "/v1/messages/" + itoa64(orig.ID) + "/reply"
		rr := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, path, body), workerConv)
		rec := testharness.Serve(f.Mux, rr)
		require.Equal(t, http.StatusOK, rec.Code, "reply body=%s", rec.Body.String())
	}

	// No subject → inherit as "Re: <original>".
	reply(map[string]any{"body": "looks good"})
	// Explicit subject → used verbatim, with no "Re: " prefix bolted on.
	reply(map[string]any{"body": "actually, a new thread", "subject": "rollback plan"})

	got, err := db.ListAgentMessagesForConv(senderConv, 100)
	require.NoError(t, err, "ListAgentMessagesForConv(sender)")
	require.Len(t, got, 2, "sender should have received both replies")

	subjects := map[string]string{} // body -> subject
	for _, m := range got {
		subjects[m.Body] = m.Subject
	}
	assert.Equal(t, "Re: deploy plan", subjects["looks good"],
		"a reply with no --subject inherits 'Re: <original>'")
	assert.Equal(t, "rollback plan", subjects["actually, a new thread"],
		"--subject overrides the generated 'Re: …' subject verbatim")
}
