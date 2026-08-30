package agentd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// TestGroupTermWS_ResolvesGroupDefaultDir covers the pre-upgrade resolution of
// GET /api/group-term-ws/{group} — the server side of the group ⚙ menu's "open
// web terminal" item, which opens a shell in the group's default directory
// (agent_groups.default_cwd):
//
//   - an unknown group 404s with a resolve error;
//   - a group with NO default_cwd 404s with the specific "no default working
//     directory" message (the menu item is gated on default_cwd client-side, so
//     this is the belt-and-suspenders server guard for a dir cleared between
//     render and click);
//   - a group WITH a default_cwd clears the resolve and reaches the WebSocket
//     upgrade — which fails on a plain (non-WS) httptest request, so the status
//     is anything but the 404 we guard against and the body never carries the
//     no-default-dir message.
func TestGroupTermWS_ResolvesGroupDefaultDir(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	// Unknown group → 404 resolve error.
	rec := httptest.NewRecorder()
	handleDashboardGroupTermWS(rec, dashboardRequest(http.MethodGet, "/api/group-term-ws/ghost", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown group: status %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "resolve group") {
		t.Errorf("unknown group body = %q, want a resolve-group error", rec.Body.String())
	}

	// A group with no default_cwd → 404 with the specific message.
	if _, err := db.CreateAgentGroup("nodir", ""); err != nil {
		t.Fatalf("CreateAgentGroup(nodir): %v", err)
	}
	rec = httptest.NewRecorder()
	handleDashboardGroupTermWS(rec, dashboardRequest(http.MethodGet, "/api/group-term-ws/nodir", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("no-default-dir group: status %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no default working directory") {
		t.Errorf("no-default-dir body = %q, want the no-default-dir error", rec.Body.String())
	}

	// A group WITH a default dir clears the resolve and reaches the WS upgrade.
	if _, err := db.CreateAgentGroup("withdir", ""); err != nil {
		t.Fatalf("CreateAgentGroup(withdir): %v", err)
	}
	if _, err := db.SetAgentGroupDefaultCwd("withdir", "/work/withdir"); err != nil {
		t.Fatalf("SetAgentGroupDefaultCwd: %v", err)
	}
	rec = httptest.NewRecorder()
	handleDashboardGroupTermWS(rec, dashboardRequest(http.MethodGet, "/api/group-term-ws/withdir", ""))
	if rec.Code == http.StatusNotFound {
		t.Fatalf("group with default dir should get past resolve, got 404; body=%s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "no default working directory") {
		t.Errorf("group with default dir must not report a missing dir; body=%s", rec.Body.String())
	}
}

// TestGroupTermSessionName_StableAndDistinct guards the tmux session name
// backing a group web terminal: it is deterministic per group name (so
// re-opening attaches back to the same shell via `tmux new-session -A`) and
// carries a prefix that can collide with neither a real agent session (the
// human-chosen label) nor a per-agent term session (termSessionName).
func TestGroupTermSessionName_StableAndDistinct(t *testing.T) {
	a := groupTermSessionName("alpha")
	if a != groupTermSessionName("alpha") {
		t.Errorf("group term session name not stable for the same group")
	}
	if a == groupTermSessionName("beta") {
		t.Errorf("distinct groups must get distinct session names")
	}
	if !strings.HasPrefix(a, "tclaude-groupterm-") {
		t.Errorf("session name = %q, want a tclaude-groupterm- prefix", a)
	}
	// The two namespaces must not overlap: a per-agent term session
	// (termSessionName) must never fall inside the group-term prefix, or a
	// group and an agent could fight over the same tmux session. "tclaude-term-"
	// is not under "tclaude-groupterm-", so this holds by construction — assert
	// it so a future prefix edit that broke the separation would fail here.
	if strings.HasPrefix(termSessionName("alpha", "current"), "tclaude-groupterm-") {
		t.Errorf("per-agent term session name must not fall in the group-term namespace")
	}
}

func TestGroupTermWSIncludesGroupEnvironmentWithoutGrowingTmuxCommand(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	w := testharness.New(t)
	previousTmux := clcommon.Default
	clcommon.Default = w.Tmux
	t.Cleanup(func() { clcommon.Default = previousTmux })
	t.Setenv("TMUX", "")
	t.Setenv(session.ResourceDelegationDirEnv, "")

	if _, err := db.CreateAgentGroup("squad", ""); err != nil {
		t.Fatalf("CreateAgentGroup: %v", err)
	}
	if _, err := db.SetAgentGroupDefaultCwd("squad", "/work/squad"); err != nil {
		t.Fatalf("SetAgentGroupDefaultCwd: %v", err)
	}
	if _, err := db.SetAgentGroupEnvironment("squad", []sandboxpolicy.EnvironmentEntry{
		{Name: "PLAIN", Value: "yes"},
		{Name: "BIG", Value: strings.Repeat("x", sandboxpolicy.MaxEnvironmentValue)},
	}); err != nil {
		t.Fatalf("SetAgentGroupEnvironment: %v", err)
	}

	command := captureTermCommand(t, handleDashboardGroupTermWS, "/api/group-term-ws/squad")
	if strings.Contains(command, "PLAIN=yes") || strings.Contains(command, strings.Repeat("x", 64)) {
		t.Fatalf("group environment leaked into tmux argv: %s", command)
	}
	if len(command) > 2048 {
		t.Fatalf("group terminal command grew with environment (%d bytes): %s", len(command), command)
	}
	if !strings.Contains(command, "launch-scripts") {
		t.Fatalf("group terminal command does not use a private launch script: %s", command)
	}
}

func TestGroupTerminalBootstrapExportsLiteralEnvironment(t *testing.T) {
	t.Setenv("SHELL", "/bin/zsh")
	got := groupTerminalBootstrap([]sandboxpolicy.EnvironmentEntry{
		{Name: "PLAIN", Value: "yes"},
		{Name: "LITERAL", Value: "spaces $(touch nope); `echo nope` 'quoted'"},
	})
	for _, want := range []string{
		"export PLAIN=yes\n",
		"export LITERAL='spaces $(touch nope); `echo nope` '\\''quoted'\\'''\n",
		"exec /bin/zsh",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("group terminal bootstrap missing %q:\n%s", want, got)
		}
	}
}
