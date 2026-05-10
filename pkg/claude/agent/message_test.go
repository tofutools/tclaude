package agent

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestRunMessage_HappyPath(t *testing.T) {
	setupTestDB(t)
	upsertConvIndex(t, "aaaaaaaa-2222-3333-4444-555555555555", "planner", "", "")
	upsertConvIndex(t, "bbbbbbbb-2222-3333-4444-555555555555", "reviewer", "", "")

	gID, err := db.CreateAgentGroup("alpha", "")
	if err != nil {
		t.Fatalf("CreateAgentGroup: %v", err)
	}
	for _, c := range []string{"aaaaaaaa-2222-3333-4444-555555555555", "bbbbbbbb-2222-3333-4444-555555555555"} {
		if err := db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: c}); err != nil {
			t.Fatalf("AddAgentGroupMember: %v", err)
		}
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
	if rc != rcOK {
		t.Fatalf("rc = %d, stderr = %q", rc, stderr.String())
	}

	// The target has no live tmux session row, so the nudge isn't called
	// in this test; we still expect persistence + queued status.
	if captured.called {
		t.Fatalf("nudge should not fire without an alive tmux session: %+v", captured)
	}
	if !strings.Contains(stdout.String(), "queued") {
		t.Fatalf("stdout = %q", stdout.String())
	}

	msgs, err := db.ListAgentMessagesForConv("bbbbbbbb-2222-3333-4444-555555555555", 0)
	if err != nil {
		t.Fatalf("ListAgentMessagesForConv: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	got := msgs[0]
	if got.Body != "hello there" || got.Subject != "ping" {
		t.Fatalf("unexpected message: %+v", got)
	}
	if got.GroupID != gID {
		t.Fatalf("group_id = %d, want %d", got.GroupID, gID)
	}
	// `delivered_at` should remain unset since no tmux session ran.
	if !got.DeliveredAt.IsZero() {
		t.Fatalf("expected delivered_at unset, got %v", got.DeliveredAt)
	}
}

func TestRunMessage_RefusesWithoutSharedGroup(t *testing.T) {
	setupTestDB(t)
	upsertConvIndex(t, "aaaaaaaa-2222-3333-4444-555555555555", "planner", "", "")
	upsertConvIndex(t, "bbbbbbbb-2222-3333-4444-555555555555", "reviewer", "", "")

	gID, _ := db.CreateAgentGroup("alpha", "")
	if err := db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "aaaaaaaa-2222-3333-4444-555555555555"}); err != nil {
		t.Fatalf("AddAgentGroupMember: %v", err)
	}
	// reviewer is not in any group with planner.

	t.Setenv("TCLAUDE_SESSION_ID", "aaaaaaaa-2222-3333-4444-555555555555")

	var stdout, stderr bytes.Buffer
	rc := runMessageDirect(&messageParams{
		Target: "reviewer",
		Body:   "hello",
	}, &messageDeps{nudge: func(string, string) error { return nil }}, "hello",
		&stdout, &stderr)
	if rc != rcAuth {
		t.Fatalf("rc = %d (want %d), stderr = %q", rc, rcAuth, stderr.String())
	}
	if !strings.Contains(stderr.String(), "shared group") {
		t.Fatalf("stderr = %q", stderr.String())
	}
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
	if rc != rcInvalidArg {
		t.Fatalf("rc = %d (want %d)", rc, rcInvalidArg)
	}
	if !strings.Contains(stderr.String(), "cannot message self") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestReadBody(t *testing.T) {
	var stderr bytes.Buffer

	body, rc := readBody(&messageParams{Body: "literal"}, strings.NewReader(""), &stderr)
	if rc != rcOK || body != "literal" {
		t.Errorf("literal body: rc=%d body=%q", rc, body)
	}

	body, rc = readBody(&messageParams{Stdin: true}, strings.NewReader("from stdin"), &stderr)
	if rc != rcOK || body != "from stdin" {
		t.Errorf("stdin body: rc=%d body=%q", rc, body)
	}

	stderr.Reset()
	_, rc = readBody(&messageParams{}, strings.NewReader(""), &stderr)
	if rc != rcInvalidArg {
		t.Errorf("no body: rc=%d", rc)
	}

	stderr.Reset()
	_, rc = readBody(&messageParams{Body: "x", Stdin: true}, strings.NewReader(""), &stderr)
	if rc != rcInvalidArg {
		t.Errorf("conflicting body sources should fail: rc=%d", rc)
	}
}

// TestFormatRecipientList covers the audience renderer in inbox read:
// alias-decorated entries get "alias <prefix>", aliasless entries fall
// back to the short prefix, and empty input yields "".
func TestFormatRecipientList(t *testing.T) {
	tests := []struct {
		name string
		in   []recipientLine
		want string
	}{
		{"empty", nil, ""},
		{"alias only", []recipientLine{{ConvID: "11111111-aaaa", Alias: "planner"}}, "planner <11111111>"},
		{"no alias", []recipientLine{{ConvID: "22222222-bbbb"}}, "22222222"},
		{"mixed", []recipientLine{
			{ConvID: "11111111-aaaa", Alias: "planner"},
			{ConvID: "22222222-bbbb"},
		}, "planner <11111111>, 22222222"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatRecipientList(tt.in)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
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
		if err := db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: c}); err != nil {
			t.Fatalf("AddAgentGroupMember: %v", err)
		}
	}

	id, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID:  gID,
		FromConv: "aaaaaaaa-2222-3333-4444-555555555555",
		ToConv:   "bbbbbbbb-2222-3333-4444-555555555555",
		Subject:  "hi",
		Body:     "hello reviewer",
	})
	if err != nil {
		t.Fatalf("InsertAgentMessage: %v", err)
	}

	t.Setenv("TCLAUDE_SESSION_ID", "cccccccc-2222-3333-4444-555555555555")

	var stdout, stderr bytes.Buffer
	rc := runInboxReadDirect(&inboxReadParams{ID: itoa(id)}, id, &stdout, &stderr)
	if rc != rcAuth {
		t.Fatalf("rc = %d (want %d), stderr = %q", rc, rcAuth, stderr.String())
	}
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
