package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)


// SocketPath is the well-known location for the tclaude agentd Unix socket.
// Mirrors agentd.SocketPath but lives here to avoid an import cycle —
// agentd already depends on agent for shared helpers, so agent can't depend
// on agentd in turn.
func SocketPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".tclaude", "agentd.sock")
}

var (
	clientOnce sync.Once
	cachedHTTP *http.Client
)

// httpClient returns a singleton http.Client that dials the agentd Unix
// socket. The hostname in URLs is ignored — we always go through the
// fixed socket path.
func httpClient() *http.Client {
	clientOnce.Do(func() {
		sock := SocketPath()
		cachedHTTP = &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sock)
				},
			},
		}
	})
	return cachedHTTP
}

// DaemonAvailable returns true if a tclaude agentd is reachable on the
// well-known socket. CLI commands route through the daemon; if it's not
// running, they exit with a clear error pointing the user at
// `tclaude agentd serve`. The direct-DB code paths still exist for
// tests, but production CLI invocations always go through the daemon.
func DaemonAvailable() bool {
	sock := SocketPath()
	if sock == "" {
		return false
	}
	if _, err := os.Stat(sock); err != nil {
		return false
	}
	conn, err := net.DialTimeout("unix", sock, 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// daemonRequiredMsg is the user-facing error when the CLI is invoked but
// the daemon isn't reachable. Centralised so the wording is consistent.
const daemonRequiredMsg = "tclaude agentd is not running.\n" +
	"Start it from a non-sandboxed shell with: tclaude agentd serve"

// RequireDaemonOrExit writes a clear "daemon not running" message to
// stderr and returns rcIOFailure if the daemon isn't reachable. CLI
// entry points use this as a precondition so we never silently fall
// back to direct DB writes — that path is for tests only and lets
// sandboxed agents bypass the daemon's auth gating if the socket
// happens to be down.
func RequireDaemonOrExit(stderr io.Writer) int {
	if DaemonAvailable() {
		return rcOK
	}
	fmt.Fprintln(stderr, "Error: "+daemonRequiredMsg)
	return rcIOFailure
}


// DaemonError represents a non-2xx response from the daemon. Callers can
// inspect Code to map back to CLI exit codes.
type DaemonError struct {
	Status int
	Code   string
	Msg    string
	Raw    []byte // when the body wasn't a structured error
}

func (e *DaemonError) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	return fmt.Sprintf("agentd returned %d", e.Status)
}

// DaemonGet performs a GET against the daemon and decodes the JSON body
// into out. Pass nil for out to ignore the response body.
func DaemonGet(path string, out any) error {
	return daemonReq(http.MethodGet, path, nil, out)
}

// DaemonPost performs a POST with a JSON body.
func DaemonPost(path string, in, out any) error {
	return daemonReq(http.MethodPost, path, in, out)
}

// DaemonDelete performs a DELETE.
func DaemonDelete(path string, out any) error {
	return daemonReq(http.MethodDelete, path, nil, out)
}

// DaemonPatch performs a PATCH with a JSON body.
func DaemonPatch(path string, in, out any) error {
	return daemonReq(http.MethodPatch, path, in, out)
}

func daemonReq(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(buf)
	}
	url := "http://_" + path
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		_ = json.Unmarshal(raw, &e)
		return &DaemonError{Status: resp.StatusCode, Code: e.Code, Msg: e.Error, Raw: raw}
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// MapDaemonErrorToRC converts a DaemonError's code into the CLI's rc*
// exit codes. Unknown codes fall back to rcIOFailure so the user always
// sees a non-zero exit on failure.
func MapDaemonErrorToRC(err error) int {
	de, ok := err.(*DaemonError)
	if !ok {
		return rcIOFailure
	}
	switch de.Code {
	case "not_found":
		return rcNotFound
	case "ambiguous":
		return rcAmbiguous
	case "invalid_arg":
		return rcInvalidArg
	case "auth":
		return rcAuth
	default:
		return rcIOFailure
	}
}
