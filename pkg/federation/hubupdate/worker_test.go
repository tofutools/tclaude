package hubupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestPrivateGuardianWorkerGenerationAndReadiness(t *testing.T) {
	b, err := NewBridge(testutil.CanonicalTempDir(t))
	require.NoError(t, err)
	defer b.Close()
	old, _, err := b.NewWorker()
	require.NoError(t, err)
	current, ready, err := b.NewWorker()
	require.NoError(t, err)
	value := Ready{HubID: "hub-id", Version: "v1.0.0", Address: "127.0.0.1:1234"}
	require.Error(t, (Client(old)).Ready(context.Background(), value))
	require.NoError(t, (Client(current)).Ready(context.Background(), value))
	require.Equal(t, value, <-ready)
	// Without a current private token neither ready nor control is reachable.
	for _, path := range []string{"/ready", "/control"} {
		raw, _ := json.Marshal(value)
		r := httptest.NewRequest("POST", path, bytes.NewReader(raw))
		w := httptest.NewRecorder()
		b.ServeHTTP(w, r)
		require.Equal(t, http.StatusForbidden, w.Code)
	}
}
func TestWorkerArgumentsPreserveHostSettings(t *testing.T) {
	require.Equal(t, []string{"serve", "--supervised", "--db", "/host/hub.sqlite", "--accept-remote-scripts=false", "--supervised=false", "--guardian-worker=true"}, WorkerArgs([]string{"serve", "--supervised", "--db", "/host/hub.sqlite", "--accept-remote-scripts=false"}))
}

// This subprocess exercises the real private config/lifetime descriptors and
// listener health probe without relying on an installed release binary.
func TestGuardianChildHelper(t *testing.T) {
	if os.Getenv("TCLAUDE_TEST_GUARDIAN_CHILD") != "1" {
		return
	}
	config, err := ReadWorkerConfig()
	if err != nil {
		os.Exit(10)
	}
	life := os.NewFile(4, "lifetime")
	defer life.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		os.Exit(11)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "hub_id": "test-hub", "version": "v1.0.0"})
	}), ReadHeaderTimeout: time.Second}
	go server.Serve(listener)
	if err = (Client(config)).Ready(context.Background(), Ready{HubID: "test-hub", Version: "v1.0.0", Address: listener.Addr().String()}); err != nil {
		os.Exit(12)
	}
	_, _ = io.Copy(io.Discard, life)
	_ = server.Close()
}
func TestGuardianRealChildReadinessAndStop(t *testing.T) {
	t.Setenv("TCLAUDE_TEST_GUARDIAN_CHILD", "1")
	b, err := NewBridge(testutil.CanonicalTempDir(t))
	require.NoError(t, err)
	defer b.Close()
	exe, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	child, err := Launch(ctx, b, exe, []string{"-test.run=^TestGuardianChildHelper$"}, "test-hub", false)
	require.NoError(t, err)
	defer child.Stop(ctx)
	require.NoError(t, child.Healthy(ctx, "v1.0.0"))
	require.NoError(t, child.Stop(ctx))
	select {
	case <-child.Done():
	case <-ctx.Done():
		t.Fatal("serving child did not stop")
	}
}
