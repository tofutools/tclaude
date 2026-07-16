package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestOneShotCLIQueueFullIsActionableAndNonZero(t *testing.T) {
	prev := DaemonRequestImpl
	t.Cleanup(func() { DaemonRequestImpl = prev })
	const hint = "target message backlog is full (10/10 unprocessed regular messages); no message was queued. Wait for the target to process or read pending messages, then retry"
	DaemonRequestImpl = func(_, _ string, _, _ any, _ DaemonOpts) error {
		return &DaemonError{Status: http.StatusTooManyRequests, Code: "queue_full", Msg: hint}
	}

	for _, run := range []struct {
		name string
		call func(stdout, stderr *bytes.Buffer) int
	}{
		{name: "message", call: func(stdout, stderr *bytes.Buffer) int {
			return runMessageDaemon(&messageParams{Target: "worker"}, "hello", stdout, stderr)
		}},
		{name: "reply", call: func(stdout, stderr *bytes.Buffer) int {
			return runReplyDaemon(42, "", "hello", stdout, stderr)
		}},
	} {
		t.Run(run.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			rc := run.call(&stdout, &stderr)
			assert.Equal(t, rcIOFailure, rc)
			assert.Empty(t, stdout.String())
			assert.Contains(t, stderr.String(), hint)
		})
	}
}

func TestMessageCLIGroupQueueFullReportsRecipientAndFails(t *testing.T) {
	prev := DaemonRequestImpl
	t.Cleanup(func() { DaemonRequestImpl = prev })
	DaemonRequestImpl = func(_, _ string, _, out any, _ DaemonOpts) error {
		return json.Unmarshal([]byte(`{
			"via_group":"team",
			"recipients":[
				{"conv_id":"full-target","agent_id":"agt_111111111111","title":"full","pending":10,"limit":10,"queue_full":true,"error":"target message backlog is full (10/10 unprocessed regular messages); no message was queued. Wait for the target to process or read pending messages, then retry"},
				{"conv_id":"free-target","agent_id":"agt_222222222222","title":"free","message_id":9,"queued":true,"pending":1}
			]
		}`), out)
	}

	var stdout, stderr bytes.Buffer
	rc := runMessageDaemon(&messageParams{Target: "group:team"}, "hello", &stdout, &stderr)
	assert.Equal(t, rcOK, rc, "partial fan-out remains successful")
	assert.Contains(t, stderr.String(), "Warning: 1 recipient(s) were not queued")
	assert.Contains(t, stdout.String(), "2 recipients (1 saved to inbox, 1 failed)")
	assert.Contains(t, stdout.String(), "not queued: target message backlog is full")
	assert.Contains(t, stdout.String(), "then retry")
	assert.Contains(t, stdout.String(), "saved to target inbox")
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

func TestReadBody(t *testing.T) {
	tests := []struct {
		name       string
		params     messageParams
		stdin      string
		wantBody   string
		wantRC     int
		wantErrSub string
	}{
		{name: "positional", params: messageParams{Text: "positional text"}, wantBody: "positional text", wantRC: rcOK},
		{name: "body flag", params: messageParams{Body: "flag text"}, wantBody: "flag text", wantRC: rcOK},
		{name: "stdin", params: messageParams{Stdin: true}, stdin: "from stdin", wantBody: "from stdin", wantRC: rcOK},
		{name: "missing", wantRC: rcInvalidArg, wantErrSub: "--body"},
		{
			name:       "positional and body flag conflict",
			params:     messageParams{Text: "positional", Body: "flag"},
			wantRC:     rcInvalidArg,
			wantErrSub: "only one",
		},
		{
			name:       "body flag and stdin conflict",
			params:     messageParams{Body: "flag", Stdin: true},
			wantRC:     rcInvalidArg,
			wantErrSub: "only one",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			body, rc := readBody(&tt.params, true, strings.NewReader(tt.stdin), &stderr)
			assert.Equal(t, tt.wantRC, rc)
			assert.Equal(t, tt.wantBody, body)
			if tt.wantErrSub != "" {
				assert.Contains(t, stderr.String(), tt.wantErrSub)
			}
		})
	}
}

func TestMessageCmdSupportsBodyFlag(t *testing.T) {
	cmd := messageCmd()
	flag := cmd.Flags().Lookup("body")
	require.NotNil(t, flag)
	assert.Equal(t, "string", flag.Value.Type())
	assert.Contains(t, cmd.UseLine(), "[text]")
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
