package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

const codexAppServerRelayMaxMessageBytes = 16 << 20

// codexAppServerRelayCmd exposes Codex's authenticated loopback WebSocket over
// the generation-private Unix socket. Managed-profile launches also repair the
// remote TUI's lossy legacy sandbox projection at this protocol boundary; see
// rewriteCodexAppServerClientMessage.
func codexAppServerRelayCmd() *cobra.Command {
	var socketPath, upstream, permissionProfile string
	cmd := &cobra.Command{
		Use:    "codex-app-server-relay",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runCodexAppServerRelay(
				context.Background(), socketPath, upstream, permissionProfile)
		},
	}
	cmd.Flags().StringVar(&socketPath, "socket", "", "Unix listener path")
	cmd.Flags().StringVar(&upstream, "upstream", "", "loopback TCP upstream")
	cmd.Flags().StringVar(&permissionProfile, "permission-profile", "",
		"managed permission profile enforced for remote thread settings")
	_ = cmd.MarkFlagRequired("socket")
	_ = cmd.MarkFlagRequired("upstream")
	return cmd
}

func codexAppServerTokenConsumeCmd() *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:    "codex-app-server-token-consume",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			token, err := consumeCodexAppServerToken(path)
			if err != nil {
				return err
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), token)
			return err
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "one-shot capability file")
	_ = cmd.MarkFlagRequired("path")
	return cmd
}

func consumeCodexAppServerToken(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("codex app-server capability handoff must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() || info.Mode()&os.ModeSymlink != 0 ||
		!info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > 1024 {
		return "", errors.New("codex app-server capability handoff is not an owned private file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) {
		return "", errors.New("codex app-server capability handoff changed while opening")
	}
	if err := os.Remove(path); err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(file, 1025))
	if err != nil {
		return "", err
	}
	if len(data) > 1024 {
		return "", errors.New("codex app-server capability handoff is too large")
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", errors.New("codex app-server capability handoff is empty")
	}
	return token, nil
}

// RunCodexAppServerRelay exposes the relay for the real-Codex compatibility
// test. Production calls it only through the hidden subprocess command.
func RunCodexAppServerRelay(ctx context.Context, socketPath, upstream string) error {
	return runCodexAppServerRelay(ctx, socketPath, upstream, "")
}

func runCodexAppServerRelay(
	ctx context.Context,
	socketPath string,
	upstream string,
	permissionProfile string,
) error {
	if !filepath.IsAbs(socketPath) {
		return errors.New("codex app-server relay socket must be absolute")
	}
	host, _, err := net.SplitHostPort(upstream)
	if err != nil {
		return fmt.Errorf("parse Codex app-server relay upstream: %w", err)
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil || !ip.IsLoopback() {
		return errors.New("codex app-server relay upstream must be numeric loopback")
	}
	permissionProfile, err = harness.ValidateCodexProfileName(permissionProfile)
	if err != nil {
		return err
	}
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale Codex app-server relay socket: %w", err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen on Codex app-server relay socket: %w", err)
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	}()
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	if err := os.Chmod(socketPath, 0o600); err != nil {
		return fmt.Errorf("secure Codex app-server relay socket: %w", err)
	}
	if permissionProfile != "" {
		return serveCodexAppServerProfileRelay(
			ctx, listener, upstream, permissionProfile)
	}
	for {
		client, acceptErr := listener.Accept()
		if acceptErr != nil {
			if ctx.Err() != nil {
				return nil
			}
			return acceptErr
		}
		go relayCodexAppServerConnection(client, upstream)
	}
}

func serveCodexAppServerProfileRelay(
	ctx context.Context,
	listener net.Listener,
	upstream string,
	permissionProfile string,
) error {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		relayCodexAppServerWebSocket(w, r, upstream, permissionProfile)
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 15 * time.Second}
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	err := server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) && ctx.Err() != nil {
		return nil
	}
	return err
}

func relayCodexAppServerWebSocket(
	w http.ResponseWriter,
	r *http.Request,
	upstream string,
	permissionProfile string,
) {
	upstreamHeaders := http.Header{}
	// Codex remains the authentication authority. The relay transports the
	// opaque capability exactly once and never logs or retains it.
	if authorization := r.Header.Get("Authorization"); authorization != "" {
		upstreamHeaders.Set("Authorization", authorization)
	}
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	dialer.Subprotocols = websocket.Subprotocols(r)
	server, response, err := dialer.Dial("ws://"+upstream+r.URL.RequestURI(), upstreamHeaders)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		http.Error(w, "Codex app-server upstream unavailable", http.StatusBadGateway)
		return
	}
	defer server.Close()

	upgrader := websocket.Upgrader{
		CheckOrigin: func(*http.Request) bool { return true },
	}
	if protocol := server.Subprotocol(); protocol != "" {
		upgrader.Subprotocols = []string{protocol}
	}
	client, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer client.Close()
	client.SetReadLimit(codexAppServerRelayMaxMessageBytes)
	server.SetReadLimit(codexAppServerRelayMaxMessageBytes)

	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			messageType, payload, readErr := client.ReadMessage()
			if readErr != nil {
				return
			}
			if messageType == websocket.TextMessage {
				payload, readErr = rewriteCodexAppServerClientMessage(payload, permissionProfile)
				if readErr != nil {
					return
				}
			}
			if writeErr := server.WriteMessage(messageType, payload); writeErr != nil {
				return
			}
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			messageType, payload, readErr := server.ReadMessage()
			if readErr != nil {
				return
			}
			if writeErr := client.WriteMessage(messageType, payload); writeErr != nil {
				return
			}
		}
	}()
	<-done
	_ = client.Close()
	_ = server.Close()
	<-done
}

// rewriteCodexAppServerClientMessage preserves the full daemon-minted managed
// profile across Codex 0.147's remote-TUI seam. That TUI deliberately omits a
// named profile on thread/start and thread/resume, then sends a legacy
// read-only/workspace-write sandbox value instead. App-server treats that
// value as a thread override, discarding the startup default_permissions table
// (including the single agentd Unix-socket allow entry). Translate only the
// permission-bearing fields. Codex 0.147 reconstructs the model-tool sandbox
// at turn/start, so the managed profile must be restated there even when the
// typed agentd request carries no legacy sandboxPolicy field; otherwise the
// server falls back to its restricted network floor and drops the Unix-socket
// allowlist.
func rewriteCodexAppServerClientMessage(payload []byte, permissionProfile string) ([]byte, error) {
	var request struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(payload, &request); err != nil || request.Method == "" {
		return payload, nil
	}
	var legacyField string
	switch request.Method {
	case "thread/start", "thread/resume":
		legacyField = "sandbox"
	case "turn/start", "thread/settings/update":
		legacyField = "sandboxPolicy"
	default:
		return payload, nil
	}

	var params map[string]json.RawMessage
	if err := json.Unmarshal(request.Params, &params); err != nil {
		return nil, fmt.Errorf("decode Codex app-server %s params: %w", request.Method, err)
	}
	_, hasLegacy := params[legacyField]
	_, hasPermissions := params["permissions"]
	if request.Method == "thread/settings/update" && !hasLegacy && !hasPermissions {
		return payload, nil
	}
	profileJSON, err := json.Marshal(permissionProfile)
	if err != nil {
		return nil, err
	}
	delete(params, legacyField)
	params["permissions"] = profileJSON
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("encode Codex app-server %s params: %w", request.Method, err)
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, err
	}
	envelope["params"] = paramsJSON
	rewritten, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("encode Codex app-server %s request: %w", request.Method, err)
	}
	return rewritten, nil
}

func relayCodexAppServerConnection(client net.Conn, upstream string) {
	defer client.Close()
	server, err := net.Dial("tcp", upstream)
	if err != nil {
		return
	}
	defer server.Close()
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(server, client)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, server)
		done <- struct{}{}
	}()
	<-done
	_ = client.Close()
	_ = server.Close()
	<-done
}
