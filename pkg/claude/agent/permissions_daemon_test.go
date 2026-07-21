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

// permLsHarness points HOME at an empty temp store (so any local DB read
// would come back empty), stubs the daemon transport with serve, and
// flags whether the conversation-index rescan fired. TCL-611: the
// `permissions ls` path must be entirely daemon-backed, so a rescan here
// is itself the bug — it is what produced the warning storm and the raw
// db.sqlite path in the reported output.
func permLsHarness(t *testing.T, serve func(path string) (int, any)) *bool {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	db.ResetForTest()

	rescanned := false
	prevRefresh := refreshAllProjects
	refreshAllProjects = func() { rescanned = true }

	prevAvail := DaemonAvailableImpl
	prevReq := DaemonRequestImpl
	DaemonAvailableImpl = func() bool { return true }
	DaemonRequestImpl = func(_, path string, _, out any, _ DaemonOpts) error {
		status, body := serve(path)
		raw, err := json.Marshal(body)
		require.NoError(t, err, "marshal stub response")
		if status >= 400 {
			var envelope struct {
				Error string `json:"error"`
				Code  string `json:"code"`
			}
			_ = json.Unmarshal(raw, &envelope)
			return &DaemonError{Status: status, Code: envelope.Code, Msg: envelope.Error, Raw: raw}
		}
		return json.Unmarshal(raw, out)
	}

	t.Cleanup(func() {
		refreshAllProjects = prevRefresh
		DaemonAvailableImpl = prevAvail
		DaemonRequestImpl = prevReq
	})
	return &rescanned
}

// A stale selector must produce ONE concise line from the daemon's typed
// not_found — no local rescan, no warning storm, no private path.
func TestPermissionsLs_StaleSelectorStaysDaemonSide(t *testing.T) {
	rescanned := permLsHarness(t, func(path string) (int, any) {
		assert.Contains(t, path, "/v1/permissions?target=", "targeted read goes to the daemon")
		return http.StatusNotFound, map[string]string{
			"code":  "not_found",
			"error": `no conversation or agent matches "sandbox-test-codex"`,
		}
	})

	var stdout, stderr bytes.Buffer
	rc := runPermissionsLs(&permissionsLsParams{Target: "sandbox-test-codex"}, &stdout, &stderr)

	assert.Equal(t, rcNotFound, rc, "stale selector exits not-found")
	assert.False(t, *rescanned, "a miss must NOT trigger a local conversation-index rescan")
	assert.Empty(t, stdout.String(), "nothing rendered on a miss")
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	assert.Len(t, lines, 1, "exactly one error line; got:\n%s", stderr.String())
	assert.Contains(t, stderr.String(), "sandbox-test-codex", "names the selector that missed")
	assert.NotContains(t, stderr.String(), "db.sqlite", "no private DB path in agent-facing output")
}

// An ambiguous selector renders the daemon-supplied candidates — the CLI
// cannot look them up itself.
func TestPermissionsLs_AmbiguousRendersDaemonCandidates(t *testing.T) {
	rescanned := permLsHarness(t, func(string) (int, any) {
		return http.StatusConflict, map[string]any{
			"code":  "ambiguous",
			"error": `selector "twin" matches multiple conversations`,
			"candidates": []map[string]string{
				{"conv_id": "11112222-3333-4444-5555-666677778888", "title": "twin-a",
					"agent_id": "agt_032fdfcfbb0578a5a1cf6493db7264fb"},
				{"conv_id": "99998888-7777-6666-5555-444433332222", "title": "twin-b"},
			},
		}
	})

	var stdout, stderr bytes.Buffer
	rc := runPermissionsLs(&permissionsLsParams{Target: "twin"}, &stdout, &stderr)

	assert.Equal(t, rcAmbiguous, rc, "ambiguous selector exits with the ambiguity code")
	assert.False(t, *rescanned, "ambiguity must NOT trigger a local rescan")
	out := stderr.String()
	assert.Contains(t, out, "matches 2 conversations", "states the candidate count")
	assert.Contains(t, out, "twin-a", "lists the first candidate by its daemon-supplied title")
	assert.Contains(t, out, "twin-b", "lists the second candidate")
	assert.Contains(t, out, "agt_032fdfcf", "leads a candidate with its stable agent_id")
	assert.Contains(t, out, "99998888", "falls back to the conv prefix without an agent_id")
}

// The untargeted roster decorates keys from the daemon's `titles`
// projection. With an empty local store, a title in the output can only
// have come over the wire.
func TestPermissionsLs_RosterTitlesComeFromDaemon(t *testing.T) {
	const conv = "11112222-3333-4444-5555-666677778888"
	rescanned := permLsHarness(t, func(path string) (int, any) {
		assert.Equal(t, "/v1/permissions", path, "untargeted read hits the plain endpoint")
		return http.StatusOK, permissionsState{
			Defaults:  []string{"self.rename"},
			Overrides: map[string]map[string]string{conv: {"groups.spawn": "grant"}},
			AgentIDs:  map[string]string{conv: "agt_032fdfcfbb0578a5a1cf6493db7264fb"},
			Titles:    map[string]string{conv: "sandbox-lead"},
		}
	})

	var stdout, stderr bytes.Buffer
	rc := runPermissionsLs(&permissionsLsParams{}, &stdout, &stderr)

	require.Equal(t, rcOK, rc, "stderr=%s", stderr.String())
	assert.False(t, *rescanned, "roster rendering must not rescan conversations")
	assert.Contains(t, stdout.String(), "sandbox-lead", "roster renders the daemon-supplied title")
	assert.Contains(t, stdout.String(), "agt_032fdfcf", "roster leads with the stable agent_id")
}
