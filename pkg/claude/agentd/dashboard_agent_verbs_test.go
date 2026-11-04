package agentd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDashboardAgentVerbs_MethodErrorsAreUnchanged pins every per-agent
// dashboard action's method gate to the exact 405 text the hand-written
// router answered with before the verb table. The expectations are spelled
// out here rather than read from the table, so a table edit cannot move them.
func TestDashboardAgentVerbs_MethodErrorsAreUnchanged(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)
	want := map[string]string{
		"status":            "GET only",
		"debug-export":      "GET only",
		"stop":              "POST only",
		"resume":            "POST only",
		"restart":           "POST only",
		"sandbox-restart":   "POST only",
		"fast-mode/enable":  "POST only",
		"fast-mode/disable": "POST only",
		"sandbox-impl":      "GET or POST only",
		"clone":             "POST only",
		"reincarnate":       "POST only",
		"compact":           "POST only",
		"rename":            "POST only",
		"worktree":          "GET only",
		"notify":            "POST only",
		"tags":              "POST only",
		"task":              "POST only",
		"remote-control":    "POST only",
		"export":            "POST only",
		"exports":           "GET or DELETE only",
	}
	for verb, text := range want {
		w := httptest.NewRecorder()
		handleDashboardAgentsAPI(w, dashboardRequest(http.MethodPatch, "/api/agents/nobody/"+verb, ""))
		assert.Equal(t, http.StatusMethodNotAllowed, w.Code, verb)
		assert.Equal(t, text+"\n", w.Body.String(), verb)
	}
	// Verbs that check their own method, and every table entry, are covered.
	for _, verb := range []string{"morph", "claude-subscription", "promote", "retire", "reinstate"} {
		want[verb] = ""
	}
	for verb := range dashboardAgentVerbs {
		_, ok := want[verb]
		assert.True(t, ok, "verb %q has no pinned method behaviour in this test", verb)
	}
	assert.Len(t, dashboardAgentVerbs, len(want))
}

// TestDashboardAgentVerbs_TwinsResolveWithTheSame404 drives each twin with
// its allowed method and an unknown agent: it reaches the shared resolve
// step and answers the router's old plain-text 404.
func TestDashboardAgentVerbs_TwinsResolveWithTheSame404(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)
	for verb, method := range map[string]string{
		"debug-export":   http.MethodGet,
		"resume":         http.MethodPost,
		"clone":          http.MethodPost,
		"reincarnate":    http.MethodPost,
		"compact":        http.MethodPost,
		"rename":         http.MethodPost,
		"notify":         http.MethodPost,
		"tags":           http.MethodPost,
		"task":           http.MethodPost,
		"remote-control": http.MethodPost,
	} {
		w := httptest.NewRecorder()
		handleDashboardAgentsAPI(w, dashboardRequest(method, "/api/agents/nobody/"+verb, ""))
		assert.Equal(t, http.StatusNotFound, w.Code, verb)
		assert.True(t, strings.HasPrefix(w.Body.String(), "resolve agent: "), "%s: %s", verb, w.Body.String())
	}
}

func TestDashboardAgentVerbs_UnknownSubpath(t *testing.T) {
	withDashboardAuth(t)
	w := httptest.NewRecorder()
	handleDashboardAgentsAPI(w, dashboardRequest(http.MethodPost, "/api/agents/nobody/fly", ""))
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, "unknown subpath /api/agents/{conv}/fly\n", w.Body.String())
}
