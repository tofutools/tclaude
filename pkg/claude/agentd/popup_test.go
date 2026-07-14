package agentd

import (
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

type errorThenTailReadCloser struct {
	prefix     []byte
	tail       io.Reader
	readErr    error
	closeErr   error
	closeCalls int
}

func (r *errorThenTailReadCloser) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.prefix) > 0 {
		n := copy(p, r.prefix)
		r.prefix = r.prefix[n:]
		if len(r.prefix) == 0 {
			return n, r.readErr
		}
		return n, nil
	}
	return r.tail.Read(p)
}

func (r *errorThenTailReadCloser) Close() error {
	r.closeCalls++
	return r.closeErr
}

func TestSnapshotApprovalRequestBodyProcessRunRedactsParamsAndPreservesBody(t *testing.T) {
	const body = `{"templateRef":"deploy@sha256:abc","runId":"release-42","params":{"secret_name":"secret-value","token":"another-secret"}}`
	req, err := http.NewRequest(http.MethodPost, "/v1/process/runs", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	preview := snapshotApprovalRequestBody(req, PermProcessRunsCreate)
	for _, safe := range []string{"deploy@sha256:abc", "release-42", "[redacted: 2 parameter(s)]"} {
		if !strings.Contains(preview, safe) {
			t.Fatalf("preview %q does not contain safe context %q", preview, safe)
		}
	}
	for _, secret := range []string{"secret_name", "secret-value", "token", "another-secret"} {
		if strings.Contains(preview, secret) {
			t.Fatalf("preview %q contains runtime parameter material %q", preview, secret)
		}
	}
	got, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("restored body changed: got %q want %q", got, body)
	}
}

func TestSnapshotApprovalRequestBodyProcessRunMalformedAndOversizedFailClosed(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "malformed", body: `{"templateRef":"safe","params":{"secret_name":"sentinel-secret"`},
		{name: "oversized", body: `{"templateRef":"safe","params":{"secret_name":"` + strings.Repeat("x", maxProcessEditBody) + `sentinel-secret"}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, "/v1/process/runs", strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			preview := snapshotApprovalRequestBody(req, PermProcessRunsCreate)
			if preview != processRunApprovalPreviewUnavailable {
				t.Fatalf("preview = %q, want fail-closed marker", preview)
			}
			if strings.Contains(preview, "secret_name") || strings.Contains(preview, "sentinel-secret") {
				t.Fatalf("fail-closed preview contains submitted parameter material: %q", preview)
			}
			got, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.body {
				t.Fatalf("restored body length = %d, want %d", len(got), len(tc.body))
			}
		})
	}
}

func TestSnapshotApprovalRequestBodyProcessRunReplaysReadErrorBeforeUnreadTail(t *testing.T) {
	const (
		prefix = `{"templateRef":"deploy@sha256:abc","runId":"release-42",`
		tail   = `"params":{"secret_name":"sentinel-secret"}}`
	)
	readErr := errors.New("injected request body read error")
	closeErr := errors.New("injected request body close error")
	source := &errorThenTailReadCloser{
		prefix:   []byte(prefix),
		tail:     strings.NewReader(tail),
		readErr:  readErr,
		closeErr: closeErr,
	}
	req, err := http.NewRequest(http.MethodPost, "/v1/process/runs", source)
	if err != nil {
		t.Fatal(err)
	}

	preview := snapshotApprovalRequestBody(req, PermProcessRunsCreate)
	if preview != processRunApprovalPreviewUnavailable {
		t.Fatalf("preview = %q, want fail-closed marker", preview)
	}
	if strings.Contains(preview, "secret_name") || strings.Contains(preview, "sentinel-secret") {
		t.Fatalf("fail-closed preview contains submitted parameter material: %q", preview)
	}

	buf := make([]byte, len(prefix)+len(tail))
	n, err := req.Body.Read(buf)
	if n != len(prefix) || string(buf[:n]) != prefix {
		t.Fatalf("first replay read = (%q, %d), want prefix (%q, %d)", buf[:n], n, prefix, len(prefix))
	}
	if !errors.Is(err, readErr) {
		t.Fatalf("first replay error = %v, want %v", err, readErr)
	}
	rest, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("continued replay read: %v", err)
	}
	if string(rest) != tail {
		t.Fatalf("continued replay read = %q, want %q", rest, tail)
	}
	if err := req.Body.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("replay close error = %v, want %v", err, closeErr)
	}
	if source.closeCalls != 1 {
		t.Fatalf("source close calls = %d, want 1", source.closeCalls)
	}
}

func TestSnapshotRequestBodyAttachmentLeavesBinaryStreamUntouched(t *testing.T) {
	const binary = "a large-ish binary body \x00 that must survive"
	req, err := http.NewRequest(http.MethodPost, "/v1/notify-human/attachment", strings.NewReader(binary))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Tclaude-Notify-Metadata", base64.RawURLEncoding.EncodeToString(
		[]byte(`{"body":"report ready","name":"report.zip"}`)))
	preview := snapshotRequestBody(req)
	if !strings.Contains(preview, "report ready") || !strings.Contains(preview, "binary attachment omitted") {
		t.Fatalf("unexpected approval preview: %q", preview)
	}
	got, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != binary {
		t.Fatalf("binary stream changed: got %q want %q", got, binary)
	}
}

// TestEscapeForCmdExe pins the cmd.exe metachar escaping that makes
// --slop survive the cmd /c start "" URL path on WSL and native
// Windows. Without this, the `&slop=1` query parameter was lost (cmd
// treated `&` as a command separator), so the browser opened
// `http://…?init_token=X` and the slop theme never activated.
func TestEscapeForCmdExe(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"plain", "http://localhost:1234/?init_token=abc", "http://localhost:1234/?init_token=abc"},
		{"single ampersand", "?a=1&b=2", "?a=1^&b=2"},
		{
			"slop dashboard url",
			"http://localhost:1234/?init_token=abc123&slop=1",
			"http://localhost:1234/?init_token=abc123^&slop=1",
		},
		{"pre-existing caret", "x^y&z", "x^^y^&z"},
		{"all metachars", "&<>|^", "^&^<^>^|^^"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := escapeForCmdExe(tc.in)
			if got != tc.want {
				t.Fatalf("escapeForCmdExe(%q)\n got: %q\nwant: %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRequestHumanApproval_DefaultDoesNotOpenBrowserOrNotify(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ResetApprovalsForTest()
	requireConfigSave(t, &config.Config{
		Notifications: &config.NotificationConfig{Enabled: true},
	})

	var opened, notified atomic.Int32
	prevOpen := approvalBrowserOpener
	approvalBrowserOpener = func(string) error {
		opened.Add(1)
		return nil
	}
	t.Cleanup(func() { approvalBrowserOpener = prevOpen })
	prevNotify := accessRequestNotify
	accessRequestNotify = func(_, _, _, _, _ string) {
		notified.Add(1)
	}
	t.Cleanup(func() { accessRequestNotify = prevNotify })

	req := testApprovalRequest()
	done := make(chan bool, 1)
	go func() { done <- realRequestHumanApproval(req, "http://127.0.0.1:1234") }()
	req.decision <- outcomeDeny

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("approval waiter did not finish")
	}
	if got := opened.Load(); got != 0 {
		t.Fatalf("default config opened browser %d time(s), want 0", got)
	}
	if got := notified.Load(); got != 0 {
		t.Fatalf("default config sent access-request notification %d time(s), want 0", got)
	}
}

func TestRequestHumanApproval_OptInOpensBrowserAndNotifies(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ResetApprovalsForTest()
	requireConfigSave(t, &config.Config{
		Notifications: &config.NotificationConfig{Enabled: true},
		Agent: &config.AgentConfig{
			AccessRequestAutoOpenBrowser:    true,
			AccessRequestSystemNotification: true,
		},
	})

	var opened, notified atomic.Int32
	openedCh := make(chan struct{}, 1)
	prevOpen := approvalBrowserOpener
	approvalBrowserOpener = func(string) error {
		opened.Add(1)
		openedCh <- struct{}{}
		return nil
	}
	t.Cleanup(func() { approvalBrowserOpener = prevOpen })
	prevNotify := accessRequestNotify
	accessRequestNotify = func(_, _, _, _, _ string) {
		notified.Add(1)
	}
	t.Cleanup(func() { accessRequestNotify = prevNotify })

	req := testApprovalRequest()
	done := make(chan bool, 1)
	go func() { done <- realRequestHumanApproval(req, "http://127.0.0.1:1234") }()
	select {
	case <-openedCh:
	case <-time.After(time.Second):
		t.Fatal("approval waiter did not invoke the stubbed browser opener")
	}
	req.decision <- outcomeDeny

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("approval waiter did not finish")
	}
	if got := opened.Load(); got != 1 {
		t.Fatalf("opt-in config opened browser %d time(s), want 1", got)
	}
	if got := notified.Load(); got != 1 {
		t.Fatalf("opt-in config sent access-request notification %d time(s), want 1", got)
	}
}

func testApprovalRequest() *approvalRequest {
	return &approvalRequest{
		id:        newApprovalID(),
		perm:      "human.notify",
		convID:    "conv-access",
		convTitle: "access tester",
		path:      "POST /v1/notify-human",
		timeout:   time.Second,
		createdAt: time.Now(),
		decision:  make(chan approvalOutcome, 1),
		extend:    make(chan time.Duration, 1),
	}
}

func requireConfigSave(t *testing.T, cfg *config.Config) {
	t.Helper()
	if err := config.Save(cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
}
