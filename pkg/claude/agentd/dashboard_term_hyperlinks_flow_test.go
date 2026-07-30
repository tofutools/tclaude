package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// tmux keeps an OSC 8 link's target in its grid and re-emits the sequence only
// to a client whose terminal advertises the hyperlink capability, which neither
// the web terminal's terminfo entry nor tmux's own detection supplies. These
// cover the two conv-addressed browser terminal routes: without the opt-in the
// dashboard shows a harness's labelled links as dead text.

// captureWebTermCommand drives a dashboard PTY route far enough to build its
// shell command. The WebSocket upgrade fails on a plain httptest request, which
// happens after the command is built and handed to the hook.
func captureWebTermCommand(t *testing.T, path string) string {
	t.Helper()
	var command string
	t.Cleanup(agentd.SetTermWSHookForTest(&agentd.TermWSHook{
		RewriteCommand: func(gotCommand, gotSession string) (string, string) {
			command = gotCommand
			return gotCommand, gotSession
		},
	}))
	rec := testharness.Serve(agentd.BuildDashboardHandlerForTest(),
		httptest.NewRequest(http.MethodGet, path, nil))
	require.NotEmpty(t, command, "%s built no PTY command; status=%d body=%s",
		path, rec.Code, rec.Body.String())
	return command
}

// The per-agent shell terminal forks tmux directly, so it can spell the flag
// itself — before the command word, which is the only place tmux reads client
// flags.
func TestWebTermWSRequestsHyperlinkPassthrough(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("dev")
	spawned := f.Spawn("dev", "worker")
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	command := captureWebTermCommand(t, "/api/term-ws/"+spawned.ConvID+"?which=start")

	flagAt := strings.Index(command, "-T "+clcommon.TmuxHyperlinksFeature)
	require.GreaterOrEqual(t, flagAt, 0, "no hyperlink opt-in in %q", command)
	assert.Less(t, flagAt, strings.Index(command, "new-session"),
		"client flags must precede the tmux command word: %q", command)
}

// "Open window" is the dashboard's console on a live agent — the terminal that
// actually shows Claude Code's and Codex's output, so the one that matters most
// here. It reaches tmux through `tclaude session attach`, which builds the tmux
// argv itself, so the request crosses that hop as an assignment scoped to the
// command rather than as PTY-wide environment an operator's shell would inherit.
func TestWebOpenWindowWSRequestsHyperlinkPassthrough(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("dev")
	spawned := f.Spawn("dev", "worker")
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	command := captureWebTermCommand(t, "/api/open-window-ws/"+spawned.ConvID)

	assert.True(t,
		strings.HasPrefix(command, clcommon.TmuxClientFeaturesEnv+"="+clcommon.TmuxHyperlinksFeature+" "),
		"the assignment must lead the command so the shell exports it: %q", command)
	assert.Contains(t, command, "session attach",
		"still the ordinary attach wrapper, only with the feature request: %q", command)
}
