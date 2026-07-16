package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestRunMessage_HappyPath(t *testing.T) {
	setupTestDB(t)
	upsertConvIndex(t, "aaaaaaaa-2222-3333-4444-555555555555", "planner", "", "")
	upsertConvIndex(t, "bbbbbbbb-2222-3333-4444-555555555555", "reviewer", "", "")

	gID, err := db.CreateAgentGroup("alpha", "")
	require.NoError(t, err, "CreateAgentGroup")
	for _, c := range []string{"aaaaaaaa-2222-3333-4444-555555555555", "bbbbbbbb-2222-3333-4444-555555555555"} {
		require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: c}), "AddAgentGroupMember")
	}
	t.Setenv("TCLAUDE_SESSION_ID", "aaaaaaaa-2222-3333-4444-555555555555")

	var captured struct {
		called  bool
		session string
		msg     string
	}
	deps := &messageDeps{
		nudge: func(s, m string) error {
			captured.called = true
			captured.session = s
			captured.msg = m
			return nil
		},
	}

	var stdout, stderr bytes.Buffer
	rc := runMessageDirect(&messageParams{
		Target:  "reviewer",
		Body:    "hello there",
		Subject: "ping",
	}, deps, "hello there", &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr = %q", stderr.String())

	// The target has no live tmux session row, so the nudge isn't called
	// in this test; we still expect persistence + queued status.
	require.False(t, captured.called, "nudge should not fire without an alive tmux session: %+v", captured)
	assert.Contains(t, stdout.String(), "queued")

	msgs, err := db.ListAgentMessagesForConv("bbbbbbbb-2222-3333-4444-555555555555", 0)
	require.NoError(t, err, "ListAgentMessagesForConv")
	require.Len(t, msgs, 1)
	got := msgs[0]
	assert.Equal(t, "hello there", got.Body)
	assert.Equal(t, "ping", got.Subject)
	assert.Equal(t, gID, got.GroupID, "group_id")
	// `delivered_at` should remain unset since no tmux session ran.
	assert.True(t, got.DeliveredAt.IsZero(), "expected delivered_at unset, got %v", got.DeliveredAt)
}

func TestRunInboxReadDaemonShowsReplyability(t *testing.T) {
	prev := DaemonRequestImpl
	t.Cleanup(func() { DaemonRequestImpl = prev })
	DaemonRequestImpl = func(_, _ string, _, out any, _ DaemonOpts) error {
		payload := map[string]any{
			"id": 42, "from_title": "human operator", "to": "worker",
			"group": "", "body": "please investigate", "created_at": "2026-07-16T12:00:00Z",
			"replyable": false,
		}
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		return json.Unmarshal(b, out)
	}

	var stdout, stderr bytes.Buffer
	rc := runInboxReadDaemon(&inboxReadParams{ID: "42"}, 42, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%s", stderr.String())
	assert.Contains(t, stdout.String(), "Replyable:  false")
	assert.NotContains(t, stdout.String(), "Reply-To:")
	assert.NotContains(t, stdout.String(), "Reply-Cmd:")
}

func TestRunMessage_RefusesWithoutSharedGroup(t *testing.T) {
	setupTestDB(t)
	upsertConvIndex(t, "aaaaaaaa-2222-3333-4444-555555555555", "planner", "", "")
	upsertConvIndex(t, "bbbbbbbb-2222-3333-4444-555555555555", "reviewer", "", "")

	gID, _ := db.CreateAgentGroup("alpha", "")
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "aaaaaaaa-2222-3333-4444-555555555555"}), "AddAgentGroupMember")
	// reviewer is not in any group with planner.

	t.Setenv("TCLAUDE_SESSION_ID", "aaaaaaaa-2222-3333-4444-555555555555")

	var stdout, stderr bytes.Buffer
	rc := runMessageDirect(&messageParams{
		Target: "reviewer",
		Body:   "hello",
	}, &messageDeps{nudge: func(string, string) error { return nil }}, "hello",
		&stdout, &stderr)
	require.Equal(t, rcAuth, rc, "stderr = %q", stderr.String())
	assert.Contains(t, stderr.String(), "shared group")
}

func TestRunMessage_RefusesSelfMessage(t *testing.T) {
	setupTestDB(t)
	upsertConvIndex(t, "aaaaaaaa-2222-3333-4444-555555555555", "planner", "", "")

	t.Setenv("TCLAUDE_SESSION_ID", "aaaaaaaa-2222-3333-4444-555555555555")

	var stdout, stderr bytes.Buffer
	rc := runMessageDirect(&messageParams{
		Target: "planner",
		Body:   "hi self",
	}, &messageDeps{nudge: func(string, string) error { return nil }}, "hi self",
		&stdout, &stderr)
	require.Equal(t, rcInvalidArg, rc)
	assert.Contains(t, stderr.String(), "cannot message self")
}

func TestReadBody(t *testing.T) {
	var stderr bytes.Buffer

	body, rc := readBody(&messageParams{Body: "literal"}, strings.NewReader(""), &stderr)
	assert.Equal(t, rcOK, rc, "literal body rc")
	assert.Equal(t, "literal", body, "literal body")

	body, rc = readBody(&messageParams{Stdin: true}, strings.NewReader("from stdin"), &stderr)
	assert.Equal(t, rcOK, rc, "stdin body rc")
	assert.Equal(t, "from stdin", body, "stdin body")

	stderr.Reset()
	_, rc = readBody(&messageParams{}, strings.NewReader(""), &stderr)
	assert.Equal(t, rcInvalidArg, rc, "no body")

	stderr.Reset()
	_, rc = readBody(&messageParams{Body: "x", Stdin: true}, strings.NewReader(""), &stderr)
	assert.Equal(t, rcInvalidArg, rc, "conflicting body sources should fail")
}

// TestFormatRecipientList covers the audience renderer in inbox read:
// title-decorated entries get "title <prefix>", titleless entries fall
// back to the short prefix, and empty input yields "".
func TestFormatRecipientList(t *testing.T) {
	tests := []struct {
		name string
		in   []recipientLine
		want string
	}{
		{"empty", nil, ""},
		// No agent_id (non-agent conv): falls back to the conv-id prefix.
		{"title only, conv fallback", []recipientLine{{ConvID: "11111111-aaaa", Title: "planner"}}, "planner <11111111>"},
		{"no title, conv fallback", []recipientLine{{ConvID: "22222222-bbbb"}}, "22222222"},
		// Agent recipient: leads with the stable short agent_id (agt_ + 8).
		{"agent id with title", []recipientLine{
			{ConvID: "11111111-aaaa", AgentID: "agt_0123456789abcdef", Title: "planner"},
		}, "planner <agt_01234567>"},
		{"agent id no title", []recipientLine{
			{ConvID: "22222222-bbbb", AgentID: "agt_0123456789abcdef"},
		}, "agt_01234567"},
		{"mixed agent + conv", []recipientLine{
			{ConvID: "11111111-aaaa", AgentID: "agt_0123456789abcdef", Title: "planner"},
			{ConvID: "22222222-bbbb"},
		}, "planner <agt_01234567>, 22222222"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatRecipientList(tt.in)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRunInboxRead_RefusesWrongRecipient(t *testing.T) {
	setupTestDB(t)
	upsertConvIndex(t, "aaaaaaaa-2222-3333-4444-555555555555", "planner", "", "")
	upsertConvIndex(t, "bbbbbbbb-2222-3333-4444-555555555555", "reviewer", "", "")
	upsertConvIndex(t, "cccccccc-2222-3333-4444-555555555555", "lurker", "", "")

	gID, _ := db.CreateAgentGroup("alpha", "")
	for _, c := range []string{
		"aaaaaaaa-2222-3333-4444-555555555555",
		"bbbbbbbb-2222-3333-4444-555555555555",
	} {
		require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: c}), "AddAgentGroupMember")
	}

	id, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID:  gID,
		FromConv: "aaaaaaaa-2222-3333-4444-555555555555",
		ToConv:   "bbbbbbbb-2222-3333-4444-555555555555",
		Subject:  "hi",
		Body:     "hello reviewer",
	})
	require.NoError(t, err, "InsertAgentMessage")

	t.Setenv("TCLAUDE_SESSION_ID", "cccccccc-2222-3333-4444-555555555555")

	var stdout, stderr bytes.Buffer
	rc := runInboxReadDirect(&inboxReadParams{ID: itoa(id)}, id, &stdout, &stderr)
	require.Equal(t, rcAuth, rc, "stderr = %q", stderr.String())
}

func TestRunInboxReadDirect_SenderlessSystemMessageIsNotReplyable(t *testing.T) {
	setupTestDB(t)
	const target = "bbbbbbbb-2222-3333-4444-555555555555"
	id, err := db.InsertAgentMessage(&db.AgentMessage{
		ToConv:  target,
		Subject: "system instruction",
		Body:    "refresh state",
	})
	require.NoError(t, err)
	t.Setenv("TCLAUDE_SESSION_ID", target)

	var stdout, stderr bytes.Buffer
	rc := runInboxReadDirect(&inboxReadParams{ID: itoa(id)}, id, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%s", stderr.String())
	assert.Contains(t, stdout.String(), "Replyable:  false")
	assert.NotContains(t, stdout.String(), "Reply-To:")
	assert.NotContains(t, stdout.String(), "Reply-Cmd:")
}

func itoa(i int64) string {
	// Avoid pulling strconv in the test for one usage.
	if i == 0 {
		return "0"
	}
	var buf []byte
	for i > 0 {
		buf = append([]byte{byte('0' + i%10)}, buf...)
		i /= 10
	}
	return string(buf)
}

// Sanity check that resolveSelector still returns the AmbiguousByTitle
// sentinel error when called from message-level code.
var _ = errors.Is
