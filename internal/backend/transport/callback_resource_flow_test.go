package transport

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers/nativeguidance"
)

// Exercise the actual private resource and generated hook command across Unix
// HTTP, including repeated recovery and an empty no-guidance response.
func TestProviderCallbackCommandAcrossUnixIngressAndRecovery(t *testing.T) {
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl unavailable")
	}
	root, err := os.MkdirTemp("/tmp", "callback-flow-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	socket := filepath.Join(root, "callback.sock")
	registry, err := NewCallbackRegistry(socket)
	require.NoError(t, err)
	defer registry.Close()
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	server := &http.Server{Handler: registry, ReadHeaderTimeout: time.Second}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	defer func() { _ = server.Close(); <-done }()
	resource, err := nativeguidance.PrepareCallback(filepath.Join(root, "private"))
	require.NoError(t, err)
	handler := func(body string) ports.NativeCallbackHandler {
		return rawCallbackFunc(func(ctx context.Context, raw ports.RawNativeCallback, sink ports.RawNativeCallbackResponder) error {
			if string(raw.Body) != `{"session_id":"exact"}` {
				return context.Canceled
			}
			response := ports.RawNativeCallbackResponse{StatusCode: http.StatusNoContent}
			if body != "" {
				response = ports.RawNativeCallbackResponse{StatusCode: 200, ContentType: "application/json", Body: []byte(body)}
			}
			_, err := sink.Respond(ctx, response)
			return err
		})
	}
	require.NoError(t, resource.Register(context.Background(), registry, "execution", 1, handler(`{"guidance":"first"}`)))
	command, err := resource.Command(curl)
	require.NoError(t, err)
	script, err := resource.WriteCommandScript(command)
	require.NoError(t, err)
	invoke := func() string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, script)
		cmd.Stdin = strings.NewReader(`{"session_id":"exact"}`)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
		return string(out)
	}
	require.JSONEq(t, `{"guidance":"first"}`, invoke())
	recovered, err := nativeguidance.RecoverCallback(filepath.Join(root, "private"), resource.Evidence())
	require.NoError(t, err)
	require.NoError(t, recovered.Register(context.Background(), registry, "execution", 1, handler("")))
	// Old registration cleanup must not erase the new proven registration.
	require.NoError(t, resource.CloseRegistration(context.Background()))
	require.Empty(t, invoke())
	require.NoError(t, recovered.Remove(context.Background()))
}
