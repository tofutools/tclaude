package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

const codexAppServerRelayMaxMessageBytes = 16 << 20

// codexAppServerRelayCmd exposes one shared policy/auth forwarding boundary on
// a generation-private Unix socket for agentd and a daemon-minted numeric
// loopback WebSocket for the TUI. Managed-profile launches also repair the
// remote TUI's lossy legacy sandbox projection at this protocol boundary; see
// rewriteCodexAppServerClientMessage.
func codexAppServerRelayCmd() *cobra.Command {
	var socketPath, listenURL, upstream, permissionProfile string
	cmd := &cobra.Command{
		Use:    "codex-app-server-relay",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runCodexAppServerRelay(
				context.Background(), socketPath, listenURL, upstream, permissionProfile)
		},
	}
	cmd.Flags().StringVar(&socketPath, "socket", "", "Unix listener path")
	cmd.Flags().StringVar(&listenURL, "listen", "", "numeric loopback WebSocket listener for the remote TUI")
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
	return runCodexAppServerRelay(ctx, socketPath, "", upstream, "")
}

// RunCodexAppServerProfileRelay exposes the enforcing relay for the opt-in
// real-Codex compatibility test. Production supplies the same profile through
// the hidden subprocess command.
func RunCodexAppServerProfileRelay(
	ctx context.Context,
	socketPath string,
	listenURL string,
	upstream string,
	permissionProfile string,
) error {
	return runCodexAppServerRelay(ctx, socketPath, listenURL, upstream, permissionProfile)
}

func runCodexAppServerRelay(
	ctx context.Context,
	socketPath string,
	listenURL string,
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
	unixListener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen on Codex app-server relay socket: %w", err)
	}
	defer func() {
		_ = unixListener.Close()
		_ = os.Remove(socketPath)
	}()
	if err := os.Chmod(socketPath, 0o600); err != nil {
		return fmt.Errorf("secure Codex app-server relay socket: %w", err)
	}
	listeners := []net.Listener{unixListener}
	if strings.TrimSpace(listenURL) != "" {
		listenAddress, err := codexAppServerRelayListenAddress(listenURL)
		if err != nil {
			return err
		}
		tuiListener, err := net.Listen("tcp4", listenAddress)
		if err != nil {
			return fmt.Errorf("listen on Codex TUI relay endpoint: %w", err)
		}
		defer tuiListener.Close()
		listeners = append(listeners, tuiListener)
	}
	return serveCodexAppServerRelay(ctx, listeners, upstream, permissionProfile)
}

func codexAppServerRelayListenAddress(listenURL string) (string, error) {
	endpoint, err := url.Parse(listenURL)
	if err != nil || endpoint.Scheme != "ws" || endpoint.Hostname() != "127.0.0.1" ||
		endpoint.User != nil || endpoint.Path != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return "", errors.New("codex TUI relay listener must be numeric IPv4 loopback WebSocket")
	}
	port, err := strconv.Atoi(endpoint.Port())
	if err != nil || port < 1 || port > 65535 {
		return "", errors.New("codex TUI relay listener must have a valid port")
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), nil
}

func serveCodexAppServerRelay(
	ctx context.Context,
	listeners []net.Listener,
	upstream string,
	permissionProfile string,
) error {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		relayCodexAppServerWebSocket(w, r, upstream, permissionProfile)
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 15 * time.Second}
	shutdownDone := make(chan struct{})
	defer close(shutdownDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = server.Close()
		case <-shutdownDone:
		}
	}()
	results := make(chan error, len(listeners))
	for _, listener := range listeners {
		go func() { results <- server.Serve(listener) }()
	}
	var firstErr error
	for range listeners {
		err := <-results
		if ctx.Err() == nil && !errors.Is(err, http.ErrServerClosed) && firstErr == nil {
			firstErr = err
			_ = server.Close()
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	return firstErr
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
		status := http.StatusBadGateway
		if response != nil && response.Body != nil {
			status = response.StatusCode
			_ = response.Body.Close()
		}
		// The native app-server remains the authentication authority. Preserve
		// its rejection status without reflecting an upstream body that may
		// disclose implementation details.
		http.Error(w, http.StatusText(status), status)
		return
	}
	defer func() { _ = server.Close() }()

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
	defer func() { _ = client.Close() }()
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
			if messageType == websocket.TextMessage && permissionProfile != "" {
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
// permission-bearing fields. A turn with no permissions field inherits the
// corrected thread profile and must pass through unchanged: tclaude's typed
// app-server client intentionally does not opt into experimentalApi, so adding
// the experimental turn/start.permissions field to that connection would make
// Codex reject an otherwise stable request.
func rewriteCodexAppServerClientMessage(payload []byte, permissionProfile string) ([]byte, error) {
	var request struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(payload, &request); err != nil || request.Method == "" {
		return payload, nil
	}
	switch request.Method {
	case "thread/start", "thread/resume":
	case "turn/start", "thread/settings/update":
	default:
		return payload, nil
	}

	if len(request.Params) == 0 || string(request.Params) == "null" {
		return nil, fmt.Errorf("decode Codex app-server %s params: object required", request.Method)
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(request.Params, &params); err != nil {
		return nil, fmt.Errorf("decode Codex app-server %s params: %w", request.Method, err)
	}
	if params == nil {
		return nil, fmt.Errorf("decode Codex app-server %s params: object required", request.Method)
	}
	_, hasSandbox := params["sandbox"]
	_, hasSandboxPolicy := params["sandboxPolicy"]
	_, hasPermissions := params["permissions"]
	if (request.Method == "turn/start" || request.Method == "thread/settings/update") &&
		!hasSandbox && !hasSandboxPolicy && !hasPermissions {
		return payload, nil
	}
	profileJSON, err := json.Marshal(permissionProfile)
	if err != nil {
		return nil, err
	}
	// Strip every legacy spelling on every permission-bearing method. This is
	// deliberately broader than the fields Codex 0.147's typed clients emit:
	// retaining an unexpected legacy spelling alongside the named profile
	// would leave precedence to the upstream decoder.
	delete(params, "sandbox")
	delete(params, "sandboxPolicy")
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
