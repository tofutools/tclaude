package agentd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// tmux always parses OSC 8 out of pane output and keeps the target URL in its
// grid, but it re-emits the sequence only to a client whose terminal advertises
// the hyperlink capability. Neither the web terminal's terminfo entry nor tmux's
// own detection (xterm.js answers no XTVERSION query) supplies it, so every test
// here guards the difference between a clickable link and dead label text.

// The env-var form exists for the PTY sites that reach tmux through `tclaude
// session attach`, which builds the tmux argv itself and so cannot be handed a
// flag. Scoping the assignment to the command — rather than to the PTY's whole
// environment — keeps an interactive browser terminal from handing a
// process-wide copy to anything the operator starts inside it.
func TestWebTerminalAttachCmdScopesFeatureRequestToTheCommand(t *testing.T) {
	got := webTerminalAttachCmd("exec tclaude session attach 'worker'")
	want := clcommon.TmuxClientFeaturesEnv + "=" + clcommon.TmuxHyperlinksFeature +
		" " + session.WebTerminalAttachEnv + "=1 exec tclaude session attach 'worker'"
	if got != want {
		t.Fatalf("webTerminalAttachCmd()\n got: %s\nwant: %s", got, want)
	}
	if strings.HasPrefix(got, "exec ") {
		t.Error("the assignment must precede the command, or the shell exports nothing")
	}
}

// captureTermCommand runs a dashboard PTY route far enough to build its shell
// command and returns it. The WebSocket upgrade fails on a plain httptest
// request, which is after the command is built and handed to the hook.
func captureTermCommand(t *testing.T, handler http.HandlerFunc, path string) string {
	t.Helper()
	var command string
	prev := termWSTestHook
	termWSTestHook = &TermWSHook{
		RewriteCommand: func(gotCommand, gotSession string) (string, string) {
			command = gotCommand
			return gotCommand, gotSession
		},
	}
	t.Cleanup(func() { termWSTestHook = prev })

	rec := httptest.NewRecorder()
	handler(rec, dashboardRequest(http.MethodGet, path, ""))
	if command == "" {
		t.Fatalf("%s never built a PTY command; status=%d body=%s", path, rec.Code, rec.Body.String())
	}
	return command
}

// assertHyperlinkFlagsPrecedeCommand pins placement as well as presence: tmux
// reads client flags only BEFORE the command word, so a `-T` sitting after
// `new-session` would be parsed as one of that command's own options.
func assertHyperlinkFlagsPrecedeCommand(t *testing.T, command, tmuxCommandWord string) {
	t.Helper()
	flagAt := strings.Index(command, "-T "+clcommon.TmuxHyperlinksFeature)
	if flagAt < 0 {
		t.Fatalf("command does not request tmux hyperlink passthrough: %s", command)
	}
	wordAt := strings.Index(command, tmuxCommandWord)
	if wordAt < 0 {
		t.Fatalf("command lost its %q verb: %s", tmuxCommandWord, command)
	}
	if flagAt > wordAt {
		t.Fatalf("client flags must precede %q: %s", tmuxCommandWord, command)
	}
}

// The conv-addressed routes need a fully resolvable agent, so they are covered
// in the flow harness: see dashboard_term_hyperlinks_flow_test.go.
func TestGroupTermWSRequestsHyperlinks(t *testing.T) {
	setupTestDB(t)
	// This test stops after the PTY command is built; it does not exercise a
	// live tmux server. Use the same private simulator as the flow tests so
	// the external-runtime guard cannot turn command construction into an
	// environment-dependent 503. The hyperlink assertion remains the exact
	// production command assertion below.
	w := testharness.New(t)
	previousTmux := clcommon.Default
	clcommon.Default = w.Tmux
	t.Cleanup(func() { clcommon.Default = previousTmux })
	t.Setenv("TMUX", "")
	t.Setenv(session.ResourceDelegationDirEnv, "")
	withDashboardAuth(t)
	if _, err := db.CreateAgentGroup("squad", ""); err != nil {
		t.Fatalf("CreateAgentGroup: %v", err)
	}
	if _, err := db.SetAgentGroupDefaultCwd("squad", "/work/squad"); err != nil {
		t.Fatalf("SetAgentGroupDefaultCwd: %v", err)
	}

	command := captureTermCommand(t, handleDashboardGroupTermWS, "/api/group-term-ws/squad")
	assertHyperlinkFlagsPrecedeCommand(t, command, "new-session")
}
