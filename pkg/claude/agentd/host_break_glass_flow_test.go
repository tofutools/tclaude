package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestHostBreakGlassOperatorGrantExecRevoke(t *testing.T) {
	f := newFlow(t)
	const conv = "glass-aaaa-bbbb-cccc-0001"
	f.HaveConvWithTitle(conv, "glass-agent")
	f.HaveEnrolledAgent(conv)
	grantBody := map[string]any{"target": conv, "duration": "30m", "reason": "test"}
	agentGrant := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/break-glass/grant", grantBody), conv))
	require.Equal(t, 403, agentGrant.Code, agentGrant.Body.String())
	sudo := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/sudo", map[string]any{"slugs": []string{"host.break-glass"}}), conv))
	require.Equal(t, 403, sudo.Code, sudo.Body.String())
	require.Contains(t, sudo.Body.String(), "not_requestable")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.Mux.ServeHTTP(w, agentd.AsAgentPeer(r, conv)) }))
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/break-glass/exec"
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.Error(t, err)
	require.Nil(t, conn)
	require.Equal(t, 403, response.StatusCode)
	require.NoError(t, response.Body.Close())
	grant := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/break-glass/grant", grantBody)))
	require.Equal(t, 200, grant.Code, grant.Body.String())
	conn, response, err = websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, response.Body.Close())
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	require.NoError(t, conn.WriteJSON(map[string]any{"shell": "printf hello; printf problem >&2; exit 7"}))
	var stdout, stderr string
	for {
		var frame struct {
			Stream   string `json:"stream"`
			Data     []byte `json:"data"`
			ExitCode *int   `json:"exit_code"`
			Error    string `json:"error"`
		}
		require.NoError(t, conn.ReadJSON(&frame))
		require.Empty(t, frame.Error)
		if frame.Stream == "stdout" {
			stdout += string(frame.Data)
		}
		if frame.Stream == "stderr" {
			stderr += string(frame.Data)
		}
		if frame.Stream == "exit" {
			require.NotNil(t, frame.ExitCode)
			require.Equal(t, 7, *frame.ExitCode)
			break
		}
	}
	require.Equal(t, "hello", stdout)
	require.Equal(t, "problem", stderr)
	revoked := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/break-glass/revoke", map[string]any{"target": conv})))
	require.Equal(t, 200, revoked.Code, revoked.Body.String())
	rows, err := db.ListActiveSudoGrants(conv)
	require.NoError(t, err)
	require.Empty(t, rows)
}
