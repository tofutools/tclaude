package agentd

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// tmux always parses OSC 8 out of pane output and keeps the target URL in its
// grid, but it re-emits the sequence only to a client whose terminal advertises
// the hyperlink capability. Neither the web terminal's terminfo entry nor tmux's
// own detection (xterm.js answers no XTVERSION query) supplies it, so every one
// of these tests is guarding the difference between a clickable link and dead
// label text in the browser.

func TestWebTerminalTmuxFlagsRequestHyperlinks(t *testing.T) {
	got := webTerminalTmuxFlags()
	if got != "-T "+clcommon.TmuxHyperlinksFeature {
		t.Fatalf("webTerminalTmuxFlags() = %q, want the hyperlink feature opt-in", got)
	}
}

// The PTY sites that reach tmux through `tclaude session attach` cannot carry a
// tmux flag in their command string — the wrapper builds the tmux argv itself —
// so the request has to cross that process boundary in the environment.
func TestWebTerminalPTYEnvCarriesTerminalContract(t *testing.T) {
	env := webTerminalPTYEnv()
	for _, want := range []string{
		"TERM=xterm-256color",
		clcommon.TmuxClientFeaturesEnv + "=" + clcommon.TmuxHyperlinksFeature,
	} {
		if !slices.Contains(env, want) {
			t.Errorf("web terminal PTY env missing %q", want)
		}
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
// reads client flags only BEFORE the command word, so `-T` after `new-session`
// or `attach-session` would be parsed as one of that command's options and fail
// the client outright — a blank terminal, not merely one without links.
func assertHyperlinkFlagsPrecedeCommand(t *testing.T, command, tmuxCommandWord string) {
	t.Helper()
	flags := "-T " + clcommon.TmuxHyperlinksFeature
	flagAt := strings.Index(command, flags)
	if flagAt < 0 {
		t.Fatalf("command does not request tmux hyperlink passthrough: %s", command)
	}
	wordAt := strings.Index(command, tmuxCommandWord)
	if wordAt < 0 {
		t.Fatalf("command lost its %q verb: %s", tmuxCommandWord, command)
	}
	if flagAt > wordAt {
		t.Fatalf("client flags must precede %q or tmux fails the client: %s", tmuxCommandWord, command)
	}
}

func TestGroupTermWSRequestsHyperlinks(t *testing.T) {
	setupTestDB(t)
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
