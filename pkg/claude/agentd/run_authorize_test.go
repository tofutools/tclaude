package agentd

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func runAuthorizeRequestWithPeer(body string, p *peer) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/run/authorize", bytes.NewBufferString(body))
	return r.WithContext(context.WithValue(r.Context(), peerKey{}, p))
}

func TestRunAuthorizeUsesScopedSandboxProfilePermission(t *testing.T) {
	setupTestDB(t)
	_, _, err := db.EnsureAgentForConv("runner", "test")
	require.NoError(t, err)
	_, err = db.CreateSandboxProfile(&db.SandboxProfile{Name: "allowed"})
	require.NoError(t, err)
	_, err = db.CreateSandboxProfile(&db.SandboxProfile{Name: "other"})
	require.NoError(t, err)
	require.NoError(t, db.GrantAgentPermissionWithScope("runner", PermAgentRun,
		`{"sandbox_profile":["allowed"]}`, "test"))
	p := &peer{PID: 999, HasClaudeAncestor: true, ConvID: "runner"}

	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"sandbox_impl":"tclaude-layer","sandbox_profile":"allowed"}`, http.StatusOK},
		{`{"sandbox_impl":"tclaude-layer","sandbox_profile":"other"}`, http.StatusForbidden},
		{`{"sandbox_impl":"harness-builtin"}`, http.StatusForbidden},
	} {
		w := httptest.NewRecorder()
		handleRunAuthorize(w, runAuthorizeRequestWithPeer(tc.body, p))
		require.Equal(t, tc.want, w.Code, "body=%s response=%s", tc.body, w.Body.String())
		if tc.want == http.StatusOK {
			require.Contains(t, w.Body.String(), `"snapshot"`)
		}
	}
	require.NoError(t, db.SetGlobalSandboxProfile("allowed"))
	w := httptest.NewRecorder()
	handleRunAuthorize(w, runAuthorizeRequestWithPeer(`{"sandbox_impl":"tclaude-layer"}`, p))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

func TestRunAuthorizeDeniesAgentWithoutPermission(t *testing.T) {
	setupTestDB(t)
	_, _, err := db.EnsureAgentForConv("runner", "test")
	require.NoError(t, err)
	w := httptest.NewRecorder()
	handleRunAuthorize(w, runAuthorizeRequestWithPeer(`{"sandbox_impl":"harness-builtin"}`,
		&peer{PID: 999, HasClaudeAncestor: true, ConvID: "runner"}))
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Contains(t, w.Body.String(), PermAgentRun)
}
