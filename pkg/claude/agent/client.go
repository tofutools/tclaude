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
	"strconv"
	"strings"
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
		cachedHTTP = newUnixSocketClient(10 * time.Second)
	})
	return cachedHTTP
}

// httpClientWithTimeout builds a fresh http.Client with the given
// timeout. Used by requests that need longer-than-default timeouts
// (e.g. AskHuman-bearing requests waiting for a popup decision).
func httpClientWithTimeout(timeout time.Duration) *http.Client {
	return newUnixSocketClient(timeout)
}

func newUnixSocketClient(timeout time.Duration) *http.Client {
	sock := SocketPath()
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}
}

// DaemonAvailableImpl is the indirection point for DaemonAvailable so
// CLI flow-tests can stand in a stub without binding a real Unix
// socket. Production code paths use realDaemonAvailable. Tests
// reassign with t.Cleanup to restore.
var DaemonAvailableImpl = realDaemonAvailable

// DaemonAvailable returns true if a tclaude agentd is reachable on the
// well-known socket. CLI commands route through the daemon; if it's not
// running, they exit with a clear error pointing the user at
// `tclaude agentd serve`. The direct-DB code paths still exist for
// tests, but production CLI invocations always go through the daemon.
func DaemonAvailable() bool { return DaemonAvailableImpl() }

func realDaemonAvailable() bool {
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

// ParseAskHuman normalises a --ask-human flag value into a duration.
// Accepts: "" (no popup), bare integers (seconds), or Go duration
// strings ("30s", "2m"). Caps at 300s to match the daemon. Returns
// (0, error) on malformed input.
func ParseAskHuman(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, nil
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		if d > 300*time.Second {
			d = 300 * time.Second
		}
		return d, nil
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		if n > 300 {
			n = 300
		}
		return time.Duration(n) * time.Second, nil
	}
	return 0, fmt.Errorf("invalid --ask-human value %q (use seconds like 30 or duration like 30s)", v)
}

// DaemonOpts configures one daemon request. Default-zero is fine.
type DaemonOpts struct {
	// AskHuman, when > 0, sends the X-Tclaude-Ask-Human header and
	// extends the per-request timeout so the daemon has room to wait
	// for a human-approval popup decision. Capped daemon-side at 300s.
	AskHuman time.Duration
	// TargetConv, when non-empty, sends the X-Tclaude-Target-Conv
	// header so endpoints that support the operator view (today:
	// /v1/inbox and /v1/messages/{id}) act on that conv-id instead of
	// the caller's own. Resolved daemon-side via agent.ResolveSelector,
	// so titles / prefixes work too.
	TargetConv string
}

// DaemonGet performs a GET against the daemon and decodes the JSON body
// into out. Pass nil for out to ignore the response body.
func DaemonGet(path string, out any) error {
	return daemonReq(http.MethodGet, path, nil, out, DaemonOpts{})
}

// DaemonPost performs a POST with a JSON body.
func DaemonPost(path string, in, out any) error {
	return daemonReq(http.MethodPost, path, in, out, DaemonOpts{})
}

// DaemonDelete performs a DELETE.
func DaemonDelete(path string, out any) error {
	return daemonReq(http.MethodDelete, path, nil, out, DaemonOpts{})
}

// DaemonPatch performs a PATCH with a JSON body.
func DaemonPatch(path string, in, out any) error {
	return daemonReq(http.MethodPatch, path, in, out, DaemonOpts{})
}

// DaemonRequestImpl is the indirection point for DaemonRequest so CLI
// flow-tests can capture the body a `tclaude agent <verb>` invocation
// would have sent without standing up a real daemon. Production code
// paths use the default (the real Unix-socket transport in
// daemonReq). Tests reassign with t.Cleanup to restore.
var DaemonRequestImpl = daemonReq

// DaemonRequest is the variadic-opts form. Use this from CLI commands
// that need to attach AskHuman.
func DaemonRequest(method, path string, in, out any, opts DaemonOpts) error {
	return DaemonRequestImpl(method, path, in, out, opts)
}

func daemonReq(method, path string, in, out any, opts DaemonOpts) error {
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
	client := httpClient()
	if opts.AskHuman > 0 {
		req.Header.Set("X-Tclaude-Ask-Human", opts.AskHuman.String())
		// The default client has a short timeout; popup approvals can
		// take up to 300s. Use a per-request client whose timeout is
		// generous enough to outlive the daemon's wait.
		client = httpClientWithTimeout(opts.AskHuman + 30*time.Second)
	}
	if opts.TargetConv != "" {
		req.Header.Set("X-Tclaude-Target-Conv", opts.TargetConv)
	}
	resp, err := client.Do(req)
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
