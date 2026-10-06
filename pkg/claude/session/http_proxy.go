package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

const HTTPProxyEnvPrefix = "TCLAUDE_HTTP_PROXY_"

// WrapHTTPProxyCommand puts the bridge inside the workload's namespace, so
// ordinary clients can use its loopback URLs even with isolated networking.
// This runs after the launch gate has bound the pane's generation.
func WrapHTTPProxyCommand(sessionID, command string) string {
	cfg, err := config.Load()
	if err != nil || !cfg.HTTPProxyConfigured() {
		return command
	}
	executable, err := os.Executable()
	if err != nil {
		return command
	}
	return clcommon.ShellQuoteArg(executable) + " session http-proxy-exec --session-id " + clcommon.ShellQuoteArg(sessionID) + " --command " + clcommon.ShellQuoteArg(command)
}

func httpProxyExecCmd() *cobra.Command {
	var sessionID, command string
	cmd := &cobra.Command{Use: "http-proxy-exec", Hidden: true, Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			code, err := runHTTPProxyExec(sessionID, command)
			if err != nil {
				return err
			}
			if code != 0 {
				os.Exit(code)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&sessionID, "session-id", "", "generation-bound launch row")
	cmd.Flags().StringVar(&command, "command", "", "workload shell command")
	_ = cmd.MarkFlagRequired("session-id")
	_ = cmd.MarkFlagRequired("command")
	return cmd
}

var newHTTPProxyDaemonClient = httpProxyDaemonClient

func httpProxyDaemonClient() *http.Client {
	return &http.Client{Timeout: 75 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var last error
			for _, path := range agentipc.ClientSocketPaths() {
				conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
				if err == nil {
					return conn, nil
				}
				last = err
			}
			if last == nil {
				last = errors.New("no daemon socket configured")
			}
			return nil, last
		},
	}, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
}

func httpProxyNames(ctx context.Context, client *http.Client, sessionID string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://tclaude/v1/http/environment", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(agentipc.SessionClaimHeader, sessionID)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("daemon refused HTTP proxy discovery (status %d)", resp.StatusCode)
	}
	var data struct {
		Names []string `json:"names"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&data)
	return data.Names, err
}

// newHTTPProxyBridge binds each unguessable local URL to one instance. Requests
// cross the Unix socket with the bridge's kernel identity and a verified pane
// claim; neither a caller's name nor HTTP headers can select another agent.
func newHTTPProxyBridge(client *http.Client, sessionID string, names []string) (http.Handler, map[string]string, error) {
	instances := map[string]string{}
	suffixes := map[string]string{}
	for _, name := range names {
		if name == "" || strings.ContainsAny(name, "=\x00") {
			return nil, nil, errors.New("HTTP proxy name is not usable in an environment variable")
		}
		var secret [32]byte
		if _, err := rand.Read(secret[:]); err != nil {
			return nil, nil, err
		}
		capability := hex.EncodeToString(secret[:])
		instances[capability] = name
		suffixes[HTTPProxyEnvPrefix+name] = "/" + capability + "/"
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Use EscapedPath so encoded separators/traversal survive to the daemon's
		// confinement validator. ServeMux cleaning must never erase them first.
		cap, path, ok := strings.Cut(strings.TrimPrefix(r.URL.EscapedPath(), "/"), "/")
		name, found := instances[cap]
		if !ok || !found || r.URL.IsAbs() {
			http.Error(w, "unknown HTTP proxy endpoint", http.StatusNotFound)
			return
		}
		target := "http://tclaude/v1/http/proxy/" + url.PathEscape(name) + "/" + path
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, target, http.MaxBytesReader(w, r.Body, 4*1024*1024))
		if err != nil {
			http.Error(w, "invalid HTTP request", 400)
			return
		}
		req.Header = r.Header.Clone()
		req.Header.Set(agentipc.SessionClaimHeader, sessionID)
		// Never disclose the local capability through a browser Referer.
		req.Header.Del("Referer")
		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, "HTTP gateway unavailable; upstream outcome may be unknown", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for name, values := range resp.Header {
			w.Header()[name] = values
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, io.LimitReader(resp.Body, 4*1024*1024+1))
	})
	return handler, suffixes, nil
}

func runHTTPProxyExec(sessionID, command string) (int, error) {
	client := newHTTPProxyDaemonClient()
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	names, err := httpProxyNames(ctx, client, sessionID)
	cancel()
	if err != nil {
		return 0, err
	}
	handler, entries, err := newHTTPProxyBridge(client, sessionID, names)
	if err != nil {
		return 0, err
	}
	environ := []string{}
	for _, pair := range os.Environ() {
		if !strings.HasPrefix(pair, HTTPProxyEnvPrefix) {
			environ = append(environ, pair)
		}
	}
	var server *http.Server
	if len(entries) > 0 {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return 0, err
		}
		defer listener.Close()
		base := "http://" + listener.Addr().String()
		for name, path := range entries {
			environ = append(environ, name+"="+base+path)
		}
		server = &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 65 * time.Second, WriteTimeout: 65 * time.Second, IdleTimeout: 30 * time.Second}
		defer func() { _ = server.Close() }()
		go func() { _ = server.Serve(listener) }()
	}
	child := exec.Command(clcommon.BootstrapShellPath(), "-c", command)
	child.Env = environ
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Keep the terminal's foreground process group: the workload must retain
	// stdin and receive terminal resize/interrupt signals directly from tmux.
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	if err := child.Start(); err != nil {
		return 0, err
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	for {
		select {
		case sig := <-signals:
			// Terminal interrupts already reach the workload's shared process group.
			if sig != syscall.SIGINT {
				_ = child.Process.Signal(sig)
			}
		case err := <-done:
			if err == nil {
				return 0, nil
			}
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
					return 128 + int(status.Signal()), nil
				}
				return exit.ExitCode(), nil
			}
			return 0, err
		}
	}
}

// LaunchResumedTmuxSession establishes a fresh pane generation before a
// resumed HTTP gateway bootstrap discovers its permissions. Without a gate,
// the resumed command could race its parent writing the new session row.
func LaunchResumedTmuxSession(sessionID, tmuxSession, cwd, command string, markers ...string) error {
	cfg, err := config.Load()
	if err != nil || !cfg.HTTPProxyConfigured() {
		return LaunchDetachedTmuxSession(tmuxSession, cwd, command, markers...)
	}
	rows, err := db.FindSessionsByConvID(sessionID)
	if err != nil || len(rows) == 0 {
		return fmt.Errorf("HTTP proxy resume requires a recorded session")
	}
	latest := rows[0]
	state := fromRow(latest)
	state.ID, state.ConvID, state.TmuxSession, state.Cwd = sessionID, sessionID, tmuxSession, cwd
	state.PID, state.Status = 0, StatusIdle
	state.Created, state.Updated = time.Now(), time.Now()
	generation := newExitLaunchGeneration(sessionID, tmuxSession)
	guard, err := newExitLaunchGuard(sessionID, tmuxSession, generation)
	if err != nil {
		return err
	}
	defer guard.abort()
	if err := SaveSessionStateForLaunch(state, generation, db.SessionExitGatePending); err != nil {
		return err
	}
	if err := LaunchDetachedTmuxSession(tmuxSession, cwd, guard.wrap(command), markers...); err != nil {
		state.Status = StatusExited
		_ = SaveSessionState(state)
		return err
	}
	guard.armPaneHook()
	guard.bind()
	state.PID = ParsePIDFromTmux(tmuxSession)
	if err := SaveSessionState(state); err != nil {
		_ = clcommon.TmuxCommand("kill-session", "-t", clcommon.ExactTarget(tmuxSession)).Run()
		return err
	}
	if err := guard.release(); err != nil {
		_ = clcommon.TmuxCommand("kill-session", "-t", clcommon.ExactTarget(tmuxSession)).Run()
		return err
	}
	return nil
}
