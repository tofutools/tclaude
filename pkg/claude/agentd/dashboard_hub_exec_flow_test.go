package agentd_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestDashboardHubExecTwoLocksOutputAuditAndRevocation(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	handler := agentd.BuildDashboardHandlerForTest()
	call := func(method, tail string, body any) *httptest.ResponseRecorder {
		return testharness.Serve(handler, testharness.JSONRequest(t, method, "/api/federation/hub/"+tail, body))
	}
	must := func(method, tail string, body any) map[string]any {
		t.Helper()
		rec := call(method, tail, body)
		require.Equal(t, 200, rec.Code, rec.Body.String())
		var out map[string]any
		testharness.DecodeJSON(t, rec, &out)
		return out
	}
	token, err := fh.store.PrepareAdminClaim(false, time.Now())
	require.NoError(t, err)
	call("GET", "status", nil)
	must("GET", "status", nil)
	must("POST", "claim", map[string]any{"token": token})
	local := must("GET", "status", nil)["instance"].(string)
	initial := must("GET", "run", nil)
	require.Equal(t, false, initial["can_exec"])
	require.Equal(t, false, initial["accept_remote_scripts"])
	require.Equal(t, 403, call("POST", "run", map[string]any{"script": "printf denied"}).Code)
	require.NoError(t, fh.store.GrantExec(local))
	require.Equal(t, 409, call("POST", "run", map[string]any{"script": "printf denied"}).Code)
	config := filepath.Join(filepath.Dir(fh.store.ClaimPath()), "hub-config.json")
	require.NoError(t, os.WriteFile(config, []byte(`{"accept_remote_scripts":true}`), 0600))
	status := must("GET", "run", nil)
	require.Equal(t, true, status["can_exec"])
	require.Equal(t, true, status["accept_remote_scripts"])
	require.Equal(t, "config", status["switch_source"])
	require.NotEmpty(t, status["service_user"])
	require.Equal(t, 400, call("PATCH", "settings", map[string]any{"overrides": map[string]any{"accept_remote_scripts": 0}}).Code)
	if os.Geteuid() == 0 {
		require.Equal(t, 403, call("POST", "run", map[string]any{"script": "printf root-refused"}).Code)
		return
	}
	// Poll the same production job surface the dashboard uses. Output is available
	// before the process completes, and the full script is durably audited first.
	script := "printf hub-secret-output; printf err-secret >&2; sleep 30"
	job := must("POST", "run", map[string]any{"script": script, "timeout_seconds": 60})
	id := job["id"].(string)
	var live map[string]any
	require.Eventually(t, func() bool {
		live = must("GET", "run/jobs/"+id, nil)
		return live["stdout_tail"] == "hub-secret-output"
	}, 3*time.Second, 10*time.Millisecond)
	require.Equal(t, "running", live["state"])
	require.NotNil(t, live["duration_ms"])
	chunk := must("GET", "run/jobs/"+id+"/logs?stream=stdout&offset=0", nil)
	var decoded struct {
		Data []byte `json:"data"`
	}
	raw, _ := json.Marshal(chunk)
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.Equal(t, []byte("hub-secret-output"), decoded.Data)
	require.Equal(t, false, chunk["eof"])
	audit := must("GET", "audit?max_entries=200", nil)
	raw, _ = json.Marshal(audit)
	require.Contains(t, string(raw), "hub-secret-output")
	logs := must("GET", "logs?max_entries=200", nil)
	raw, _ = json.Marshal(logs)
	require.NotContains(t, string(raw), "hub-secret-output")
	// A second admin can inspect metadata/audit but cannot read output or script.
	peer := fh.peer.id.ID()
	must("POST", "admins", map[string]any{"instance": peer, "capabilities": []string{"hub.logs.read"}})
	rpc := func(op string, p any) map[string]any {
		t.Helper()
		r, e := fh.peer.cl.AdminCall(context.Background(), op, p)
		require.NoError(t, e)
		if r.Code == "admin_generation" {
			r, e = fh.peer.cl.AdminCall(context.Background(), op, p)
			require.NoError(t, e)
		}
		require.Equal(t, 200, r.Status, r.Error)
		var out map[string]any
		require.NoError(t, json.Unmarshal(r.Body, &out))
		return out
	}
	meta := rpc("run.job", map[string]any{"job_id": id})
	require.NotContains(t, meta, "stdout_tail")
	require.NotContains(t, meta, "stderr_tail")
	require.NotContains(t, meta, "duration_ms")
	require.Equal(t, "running", meta["state"])
	redacted := rpc("audit", map[string]any{"max_entries": 200})
	raw, _ = json.Marshal(redacted)
	require.NotContains(t, string(raw), "hub-secret-output")
	require.Contains(t, string(raw), `"redacted":true`)
	result, err := fh.peer.cl.AdminCall(context.Background(), "run.logs", map[string]any{"job_id": id, "stream": "stdout"})
	require.NoError(t, err)
	require.Equal(t, 403, result.Status)
	require.Equal(t, "hub_exec_required", result.Code)
	// Changing the HOST config cancels an already-running process group.
	require.NoError(t, os.WriteFile(config, []byte(`{"accept_remote_scripts":false}`), 0600))
	require.Eventually(t, func() bool { job = must("GET", "run/jobs/"+id, nil); return job["state"] == "cancelled" }, 4*time.Second, 20*time.Millisecond)
	require.Equal(t, float64(130), job["exit_code"])
	audit = must("GET", "audit?max_entries=200", nil)
	raw, _ = json.Marshal(audit)
	require.Contains(t, string(raw), `"phase":"result"`)
	// Removing the capability alone also blocks new runs, regardless of switch.
	require.NoError(t, os.WriteFile(config, []byte(`{"accept_remote_scripts":true}`), 0600))
	must("POST", "admins", map[string]any{"instance": local, "capabilities": []string{"hub.admins.manage", "hub.logs.read"}})
	require.Equal(t, 403, call("POST", "run", map[string]any{"script": "printf denied"}).Code)
	require.Equal(t, 400, call("GET", "run/jobs/"+id+"/logs?offset=-1", nil).Code)
	require.Equal(t, 404, call("GET", "run/jobs/"+strings.Repeat("0", 32), nil).Code)
	rec := fedHuman(t, fh.f, "GET", "/v1/federation/hub/run", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
}
func TestDashboardHubExecRoutesAreLocalHumanOnly(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	handler := agentd.BuildDashboardHandlerForTest()
	id := strings.Repeat("0", 32)
	for _, route := range []struct{ method, tail string }{{"GET", "run"}, {"POST", "run"}, {"GET", "run/jobs/" + id}, {"GET", "run/jobs/" + id + "/logs"}, {"GET", "audit"}} {
		req := testharness.JSONRequest(t, route.method, "/api/federation/hub/"+route.tail, map[string]any{})
		rec := testharness.Serve(agentd.PeerViewHandler(fh.peer.id.ID()), req)
		require.Equal(t, 403, rec.Code, route.tail)
		req = testharness.JSONRequest(t, route.method, "/v1/federation/hub/"+route.tail, map[string]any{})
		rec = testharness.Serve(fh.f.Mux, agentd.AsAgentPeer(req, "hub-agent"))
		require.Equal(t, 403, rec.Code, route.tail)
	}
	req := testharness.JSONRequest(t, "POST", "/api/federation/hub/run", map[string]any{"script": "printf nope"})
	req.Header.Set("Origin", "https://evil.invalid")
	rec := testharness.Serve(handler, req)
	require.Equal(t, 403, rec.Code)
}
