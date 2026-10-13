package pifixture

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDrive stands in for agentd's POST /v1/whoami/pi-drive: each poll
// carries the extension's acks and is answered with the next scripted batch.
type fakeDrive struct {
	mu      sync.Mutex
	batches [][]map[string]any
	acks    []map[string]any
	polls   int
	claims  []string
}

func (d *fakeDrive) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/whoami/pi-drive" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	var body struct {
		Acks []map[string]any `json:"acks"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	d.mu.Lock()
	d.polls++
	d.claims = append(d.claims, r.Header.Get("X-Tclaude-Session-ID")+"|"+r.Header.Get("X-Tclaude-Agent-Hint"))
	d.acks = append(d.acks, body.Acks...)
	var batch []map[string]any
	if len(d.batches) > 0 {
		batch, d.batches = d.batches[0], d.batches[1:]
	}
	d.mu.Unlock()
	if batch == nil {
		// Nothing queued: hold the poll briefly, as the daemon does.
		select {
		case <-r.Context().Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"drive": true, "requests": batch})
}

func (d *fakeDrive) push(batch ...map[string]any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.batches = append(d.batches, batch)
}

func (d *fakeDrive) ackFor(id float64) map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, a := range d.acks {
		if a["id"] == id {
			return a
		}
	}
	return nil
}

func startFakeDrive(t *testing.T, d *fakeDrive) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pidrive")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "agentd.sock")
	listener, err := net.Listen("unix", sock)
	require.NoError(t, err)
	srv := &http.Server{Handler: http.HandlerFunc(d.serve)}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

// The drive end to end against the real pi: a message becomes a turn, a
// rename names the session, a compaction runs and its follow-up is sent after
// it, and exit shuts pi down. Each operation is acknowledged on the next poll.
func TestPiExtensionDriveRunsDaemonRequests(t *testing.T) {
	w := NewWorld(t)
	ext := materializedExtension(t)
	drive := &fakeDrive{}
	w.ExtraEnv = []string{"TCLAUDE_AGENTD_SOCKET=" + startFakeDrive(t, drive), "TCLAUDE_SESSION_ID=spwn-drive"}
	id := uuid.NewString()
	rpc := w.StartRPC(w.Project, "--session-id", id, "--extension", ext, "--model", MockModel)

	drive.push(
		map[string]any{"id": 1, "op": "message", "text": "hello over DRIVE", "deliver": "steer"},
		map[string]any{"id": 2, "op": "rename", "name": "drive-named"},
	)
	rpc.AwaitEvent("agent_settled")
	assert.Contains(t, w.Mock.LastMessages(), "hello over DRIVE")

	drive.push(map[string]any{"id": 3, "op": "compact", "text": "AFTER compaction"})
	rpc.AwaitEvent("compaction_end")
	rpc.AwaitEvent("agent_settled")
	assert.Contains(t, w.Mock.LastMessages(), "AFTER compaction")

	require.Eventually(t, func() bool { return drive.ackFor(3) != nil }, 30*time.Second, 50*time.Millisecond)
	for _, id := range []float64{1, 2, 3} {
		assert.Equal(t, true, drive.ackFor(id)["ok"], "ack %v: %v", id, drive.ackFor(id))
	}
	// Every poll carries the session claim the daemon needs to place a
	// launch inside tclaude's sandbox.
	drive.mu.Lock()
	for _, claim := range drive.claims {
		assert.Equal(t, "spwn-drive|1", claim)
	}
	drive.mu.Unlock()
	state := rpc.Send(map[string]any{"type": "get_state"})
	assert.Contains(t, mustJSON(state), "drive-named")

	drive.push(map[string]any{"id": 4, "op": "exit"})
	done := make(chan error, 1)
	go func() { done <- rpc.cmd.Wait() }()
	select {
	case <-done:
		// RPC mode exits 143 on SIGTERM; the TUI exits 0.
	case <-time.After(30 * time.Second):
		t.Fatal("pi did not exit on the drive's exit request")
	}

	session, err := filepath.Glob(filepath.Join(w.AgentDir, "sessions", "*", "*"+id+"*.jsonl"))
	require.NoError(t, err)
	require.Len(t, session, 1)
	raw, err := os.ReadFile(session[0])
	require.NoError(t, err)
	assert.True(t, strings.Contains(string(raw), "drive-named"), "session file lacks the name")
}

// A launch without the drive is told so and stops polling: it asks once more
// after 30s (in case its posture was not yet recorded), then never again.
func TestPiExtensionDriveStopsWhenDaemonSaysNoDrive(t *testing.T) {
	w := NewWorld(t)
	ext := materializedExtension(t)
	var mu sync.Mutex
	polls := 0
	dir, err := os.MkdirTemp("", "pidrive")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "agentd.sock")
	listener, err := net.Listen("unix", sock)
	require.NoError(t, err)
	srv := &http.Server{Handler: http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		polls++
		mu.Unlock()
		_ = json.NewEncoder(rw).Encode(map[string]any{"drive": false})
	})}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })
	w.ExtraEnv = []string{"TCLAUDE_AGENTD_SOCKET=" + sock}
	rpc := w.StartRPC(w.Project, "--session-id", uuid.NewString(), "--extension", ext, "--model", MockModel)
	require.Equal(t, true, rpc.Send(map[string]any{"type": "prompt", "message": "hi"})["success"])
	rpc.AwaitEvent("agent_settled")
	time.Sleep(time.Second)
	mu.Lock()
	assert.Equal(t, 1, polls)
	mu.Unlock()
	rpc.Close()
}
