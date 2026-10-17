package agent

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestBreakGlassCLIStreamsAndPropagatesExitCode(t *testing.T) {
	requests := make(chan map[string]any, 1)
	serveCompletionSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/break-glass/exec", r.URL.Path)
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		var spec map[string]any
		if conn.ReadJSON(&spec) != nil {
			return
		}
		requests <- spec
		_ = conn.WriteJSON(breakGlassFrame{Stream: "stdout", Data: []byte("out\x00put")})
		_ = conn.WriteJSON(breakGlassFrame{Stream: "stderr", Data: []byte("problem")})
		code := 7
		_ = conn.WriteJSON(breakGlassFrame{Stream: "exit", ExitCode: &code})
	}))
	var stdout, stderr bytes.Buffer
	code := runBreakGlass(context.Background(), &breakGlassParams{Args: []string{"printf", "literal $HOME"}, Cwd: "/srv/project", Env: []string{"LANG=C"}, Reason: "host diagnosis", Timeout: "2m"}, strings.NewReader(""), &stdout, &stderr)
	require.Equal(t, 7, code)
	require.Equal(t, "out\x00put", stdout.String())
	require.Equal(t, "problem", stderr.String())
	spec := <-requests
	require.Equal(t, "/srv/project", spec["cwd"])
	require.Equal(t, "host diagnosis", spec["reason"])
	require.Equal(t, []any{"printf", "literal $HOME"}, spec["args"])
}
