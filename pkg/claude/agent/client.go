package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	tcommon "github.com/tofutools/tclaude/pkg/common"
)

// SocketPath is the well-known location for the tclaude agentd Unix socket.
// Mirrors agentd.SocketPath but lives here to avoid an import cycle —
// agentd already depends on agent for shared helpers, so agent can't depend
// on agentd in turn.
func SocketPath() string {
	return agentipc.ClientSocketPath()
}

var (
	clientOnce sync.Once
	cachedHTTP *http.Client
)

// httpClient returns a singleton http.Client that dials agentd over its Unix
// socket. The hostname in URLs is ignored; current clients prefer the
// canonical path and can fall back to the legacy path during migration.
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
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				var lastErr error
				for _, sock := range agentipc.ClientSocketPaths() {
					conn, err := d.DialContext(ctx, "unix", sock)
					if err == nil {
						return conn, nil
					}
					lastErr = err
				}
				return nil, lastErr
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
	for _, sock := range agentipc.ClientSocketPaths() {
		if sock == "" {
			continue
		}
		if _, err := os.Stat(sock); err != nil {
			continue
		}
		conn, err := net.DialTimeout("unix", sock, 250*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return true
		}
	}
	return false
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
	if waitForDaemon(stderr, DaemonAvailable, defaultRetryPolicy()) {
		return rcOK
	}
	fmt.Fprintln(stderr, "Error: "+daemonRequiredMsg)
	return rcIOFailure
}

var (
	connectionRetryBackoffs = []time.Duration{
		time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
	}
	serverRetryBackoffs = []time.Duration{
		time.Second,
		2 * time.Second,
	}
)

type daemonRetryPolicy struct {
	connectionBackoffs []time.Duration
	serverBackoffs     []time.Duration
	sleep              func(context.Context, time.Duration) error
	retryMutations     bool
}

func defaultRetryPolicy() daemonRetryPolicy {
	return daemonRetryPolicy{
		connectionBackoffs: connectionRetryBackoffs,
		serverBackoffs:     serverRetryBackoffs,
		sleep:              sleepContext,
		retryMutations:     true,
	}
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func waitForDaemon(stderr io.Writer, available func() bool, policy daemonRetryPolicy) bool {
	if available() {
		return true
	}
	for i, delay := range policy.connectionBackoffs {
		fmt.Fprintf(stderr, "agentd is unavailable; retrying in %s (%d/%d)\n",
			delay, i+1, len(policy.connectionBackoffs))
		if err := policy.sleep(context.Background(), delay); err != nil {
			return false
		}
		if available() {
			return true
		}
	}
	return false
}

// DaemonError represents a non-2xx response from the daemon. Callers can
// inspect Code to map back to CLI exit codes.
type DaemonError struct {
	Status int
	Code   string
	Msg    string
	Raw    []byte // the full response body (always set on a >= 400 reply)
}

func (e *DaemonError) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	return fmt.Sprintf("agentd returned %d", e.Status)
}

// IsDangling reports whether this error carries the dangling-agent-entry
// signal (HTTP 409 + {"dangling":true}) the retire endpoint emits when
// the target enrollment has no resolvable conversation. Callers turn it
// into actionable guidance toward `tclaude agent delete` instead of the
// generic resolve error.
func (e *DaemonError) IsDangling() bool {
	if e == nil || e.Status != http.StatusConflict || len(e.Raw) == 0 {
		return false
	}
	var body struct {
		Dangling bool `json:"dangling"`
	}
	if err := json.Unmarshal(e.Raw, &body); err != nil {
		return false
	}
	return body.Dangling
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
	// Timeout, when > 0, overrides the default 10s client timeout for
	// this one request. Needed by slow endpoints — template
	// instantiation spawns a whole agent team and can run well past
	// 10s. Ignored when AskHuman is set (that path picks its own
	// generous timeout).
	Timeout time.Duration
	// RetryOutput receives retry notices. Nil writes to os.Stderr. This is
	// primarily useful to callers that already expose an injectable stderr.
	RetryOutput io.Writer
	// NoRetry disables both connection and HTTP 5xx retries. Use it for
	// best-effort probes on latency-sensitive paths such as shell completion;
	// normal agent commands should retain the default restart-tolerant policy.
	NoRetry bool
}

// DaemonGet performs a GET against the daemon and decodes the JSON body
// into out. Pass nil for out to ignore the response body.
//
// Routed through DaemonRequestImpl like DaemonRequest, so a CLI flow test
// can drive a read-only command against the real daemon mux without a
// live socket. Production behaviour is unchanged — the default impl is
// daemonReq.
func DaemonGet(path string, out any) error {
	return DaemonRequestImpl(http.MethodGet, path, nil, out, DaemonOpts{})
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

// HumanTokenHeader / HumanTokenEnvVar mirror agentd's humanTokenHeader /
// humanTokenEnvVar — the `agent` package cannot import `agentd` (import
// cycle), so the strings are duplicated here. Keep them in sync.
const (
	HumanTokenHeader     = "X-Tclaude-Human-Token"
	HumanTokenEnvVar     = "TCLAUDE_HUMAN_TOKEN"
	IdempotencyKeyHeader = "Idempotency-Key"
	RequestDigestHeader  = "X-Tclaude-Request-Digest"
)

// attachCallerIdentity adds the caller's non-peer-credential identity inputs
// to a daemon request. An explicit TCLAUDE_HUMAN_TOKEN wins; otherwise a
// non-agent-hinted process best-effort reads the persistent file fallback from
// tclaude's private data directory.
//
// Sandboxed agents cannot read that directory, so the fallback is a silent
// no-op for them. An unsandboxed agent may be able to read it, but such a
// process can already read all same-uid tclaude state; more importantly,
// agentd classifies a harness ancestor as an agent before considering this
// token, so attaching it can never promote an agent to the human operator.
//
// TCLAUDE_AGENT_HINT is deliberately advisory. It avoids a pointless private
// file read and lets agentd tailor identity-failure help, but agentd never
// trusts it for authorization.
func attachCallerIdentity(req *http.Request) {
	if hasAgentHint() {
		req.Header.Set(agentipc.AgentHintHeader, "1")
	}
	if tok := humanToken(); tok != "" {
		req.Header.Set(HumanTokenHeader, tok)
	}
}

func humanToken() string {
	if tok := strings.TrimSpace(os.Getenv(HumanTokenEnvVar)); tok != "" {
		return tok
	}
	if hasAgentHint() {
		return ""
	}
	path := tcommon.TclaudeStatePath("operator_token")
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func hasAgentHint() bool {
	return strings.TrimSpace(os.Getenv(agentipc.AgentHintEnvVar)) == "1"
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
	attachCallerIdentity(req)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := httpClient()
	switch {
	case opts.AskHuman > 0:
		req.Header.Set("X-Tclaude-Ask-Human", opts.AskHuman.String())
		// The default client has a short timeout; popup approvals can
		// take up to 300s. Use a per-request client whose timeout is
		// generous enough to outlive the daemon's wait.
		client = httpClientWithTimeout(opts.AskHuman + 30*time.Second)
	case opts.Timeout > 0:
		client = httpClientWithTimeout(opts.Timeout)
	}
	if opts.TargetConv != "" {
		req.Header.Set("X-Tclaude-Target-Conv", opts.TargetConv)
	}
	policy, err := retryPolicyForRequest(client, req, retryOutput(opts.RetryOutput))
	if err != nil {
		return err
	}
	if opts.NoRetry {
		policy.connectionBackoffs = nil
		policy.serverBackoffs = nil
	}
	resp, err := doDaemonRequest(client, req, retryOutput(opts.RetryOutput), policy)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := daemonResponseBytes(resp)
	if readErr != nil {
		return fmt.Errorf("read response body: %w", readErr)
	}
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

// daemonRawTimeout is the per-request timeout for the raw binary
// transfers (group export download / import upload). Generous: building
// an export or applying an import touches many .jsonl files and runs a
// multi-table transaction, well beyond the default JSON-call budget.
const daemonRawTimeout = 10 * time.Minute

// DaemonGetRawImpl is the indirection point for DaemonGetRaw, the same
// seam DaemonRequestImpl provides for the JSON transport: CLI tests
// reassign it (with t.Cleanup restoration) to serve canned daemon bytes
// without a real Unix-socket daemon.
var DaemonGetRawImpl = daemonGetRaw

// DaemonGetRaw performs a GET against the daemon and returns the raw
// response body plus its headers — used for downloads where the body
// must pass through unparsed: a group-export .zip, or a template export
// whose JSON the daemon owns the shape of. A >= 400 status is returned
// as a *DaemonError, its message decoded from the JSON error envelope
// the daemon writes on failure.
func DaemonGetRaw(path string) ([]byte, http.Header, error) {
	return DaemonGetRawImpl(path)
}

func daemonGetRaw(path string) ([]byte, http.Header, error) {
	req, err := http.NewRequest(http.MethodGet, "http://_"+path, nil)
	if err != nil {
		return nil, nil, err
	}
	attachCallerIdentity(req)
	resp, err := doDaemonRequest(httpClientWithTimeout(daemonRawTimeout), req, os.Stderr, defaultRetryPolicy())
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, readErr := daemonResponseBytes(resp)
	if readErr != nil {
		return nil, nil, fmt.Errorf("read response body: %w", readErr)
	}
	if resp.StatusCode >= 400 {
		return nil, nil, decodeDaemonError(resp.StatusCode, raw)
	}
	return raw, resp.Header, nil
}

// DaemonPostRaw performs a POST with a raw (non-JSON) request body —
// used to upload a group-export .zip — and decodes the JSON response
// into out (pass nil to ignore the response body).
func DaemonPostRaw(path, contentType string, body []byte, out any) error {
	return DaemonPostRawWithOptions(path, contentType, body, nil, out, DaemonOpts{})
}

// DaemonPostRawWithOptions is the configurable raw upload transport. Headers
// carry small metadata for binary endpoints; opts preserves the same ad-hoc
// human approval behavior as JSON commands.
func DaemonPostRawWithOptions(path, contentType string, body []byte, headers http.Header, out any, opts DaemonOpts) error {
	req, err := http.NewRequest(http.MethodPost, "http://_"+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	attachCallerIdentity(req)
	req.Header.Set("Content-Type", contentType)
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	client := httpClientWithTimeout(daemonRawTimeout)
	if opts.AskHuman > 0 {
		req.Header.Set("X-Tclaude-Ask-Human", opts.AskHuman.String())
		client = httpClientWithTimeout(max(daemonRawTimeout, opts.AskHuman+30*time.Second))
	}
	policy, err := retryPolicyForRequest(client, req, retryOutput(opts.RetryOutput))
	if err != nil {
		return err
	}
	resp, err := doDaemonRequest(client, req, retryOutput(opts.RetryOutput), policy)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, readErr := daemonResponseBytes(resp)
	if readErr != nil {
		return fmt.Errorf("read response body: %w", readErr)
	}
	if resp.StatusCode >= 400 {
		return decodeDaemonError(resp.StatusCode, raw)
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func retryOutput(out io.Writer) io.Writer {
	if out != nil {
		return out
	}
	return os.Stderr
}

// doDaemonRequest retries transient failures at the single shared agentd
// transport boundary. Connection failures get the full ~30 second backoff;
// HTTP 5xx replies get two retries. The budgets are independent so an agentd
// restart can move from 5xx shutdown responses to connection failures and
// still recover once the replacement daemon starts accepting requests.
func doDaemonRequest(client *http.Client, req *http.Request, stderr io.Writer, policy daemonRetryPolicy) (*http.Response, error) {
	if err := attachIdempotencyKey(req); err != nil {
		return nil, err
	}
	mutation := isMutatingRequest(req)
	canRetryConnection := !mutation || policy.retryMutations
	canRetry5xx := !mutation
	connectionRetries := 0
	serverRetries := 0
	for {
		attempt, err := replayableRequest(req)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(attempt)
		if err != nil {
			if !canRetryConnection || !isConnectionFailure(err) || connectionRetries >= len(policy.connectionBackoffs) {
				return nil, err
			}
			delay := policy.connectionBackoffs[connectionRetries]
			connectionRetries++
			fmt.Fprintf(stderr, "agentd connection failed: %v; retrying in %s (%d/%d)\n",
				err, delay, connectionRetries, len(policy.connectionBackoffs))
			if err := policy.sleep(req.Context(), delay); err != nil {
				return nil, err
			}
			continue
		}

		raw, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			if !canRetryConnection || !isConnectionFailure(readErr) || connectionRetries >= len(policy.connectionBackoffs) {
				return nil, fmt.Errorf("read agentd response: %w", readErr)
			}
			delay := policy.connectionBackoffs[connectionRetries]
			connectionRetries++
			fmt.Fprintf(stderr, "agentd connection failed while reading response: %v; retrying in %s (%d/%d)\n",
				readErr, delay, connectionRetries, len(policy.connectionBackoffs))
			if err := policy.sleep(req.Context(), delay); err != nil {
				return nil, err
			}
			continue
		}
		resp.Body = newBufferedDaemonBody(raw)
		resp.ContentLength = int64(len(raw))

		if resp.StatusCode < http.StatusInternalServerError || !canRetry5xx || serverRetries >= len(policy.serverBackoffs) {
			return resp, nil
		}
		delay := policy.serverBackoffs[serverRetries]
		serverRetries++
		fmt.Fprintf(stderr, "agentd returned HTTP %d; retrying in %s (%d/%d)\n",
			resp.StatusCode, delay, serverRetries, len(policy.serverBackoffs))
		if err := policy.sleep(req.Context(), delay); err != nil {
			return nil, err
		}
	}
}

func retryPolicyForRequest(client *http.Client, req *http.Request, stderr io.Writer) (daemonRetryPolicy, error) {
	policy := defaultRetryPolicy()
	if !isMutatingRequest(req) {
		return policy, nil
	}
	supported, err := daemonSupportsIdempotency(client, stderr)
	if err != nil {
		return policy, err
	}
	policy.retryMutations = supported
	return policy, nil
}

func daemonSupportsIdempotency(client *http.Client, stderr io.Writer) (bool, error) {
	req, err := http.NewRequest(http.MethodGet, "http://_/v1/info", nil)
	if err != nil {
		return false, err
	}
	attachCallerIdentity(req)
	resp, err := doDaemonRequest(client, req, stderr, defaultRetryPolicy())
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, readErr := daemonResponseBytes(resp)
	if readErr != nil {
		return false, fmt.Errorf("read agentd capabilities: %w", readErr)
	}
	if resp.StatusCode >= 400 {
		return false, decodeDaemonError(resp.StatusCode, raw)
	}
	var info struct {
		Idempotency string `json:"idempotency"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return false, fmt.Errorf("decode agentd capabilities: %w", err)
	}
	return info.Idempotency == "v1", nil
}

type bufferedDaemonBody struct {
	*bytes.Reader
	data []byte
}

func newBufferedDaemonBody(data []byte) *bufferedDaemonBody {
	return &bufferedDaemonBody{Reader: bytes.NewReader(data), data: data}
}

func (b *bufferedDaemonBody) Close() error { return nil }

// daemonResponseBytes transfers the retry loop's already-buffered response to
// callers without copying large raw downloads a second time.
func daemonResponseBytes(resp *http.Response) ([]byte, error) {
	if body, ok := resp.Body.(*bufferedDaemonBody); ok {
		return body.data, nil
	}
	return io.ReadAll(resp.Body)
}

func isMutatingRequest(req *http.Request) bool {
	switch req.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func attachIdempotencyKey(req *http.Request) error {
	if isMutatingRequest(req) {
		if req.Header.Get(IdempotencyKeyHeader) == "" {
			req.Header.Set(IdempotencyKeyHeader, uuid.NewString())
		}
		if req.Header.Get(RequestDigestHeader) != "" {
			return nil
		}
		h := sha256.New()
		_, _ = io.WriteString(h, req.Method+"\x00"+req.URL.RequestURI()+"\x00")
		if req.Body != nil {
			if req.GetBody == nil {
				return fmt.Errorf("agentd request body cannot be fingerprinted")
			}
			body, err := req.GetBody()
			if err != nil {
				return fmt.Errorf("fingerprint agentd request body: %w", err)
			}
			_, copyErr := io.Copy(h, body)
			_ = body.Close()
			if copyErr != nil {
				return fmt.Errorf("fingerprint agentd request body: %w", copyErr)
			}
		}
		req.Header.Set(RequestDigestHeader, fmt.Sprintf("%x", h.Sum(nil)))
	}
	return nil
}

func replayableRequest(req *http.Request) (*http.Request, error) {
	attempt := req.Clone(req.Context())
	if req.Body == nil {
		return attempt, nil
	}
	if req.GetBody == nil {
		return nil, fmt.Errorf("agentd request body cannot be replayed")
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, fmt.Errorf("replay agentd request body: %w", err)
	}
	attempt.Body = body
	return attempt, nil
}

func isConnectionFailure(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ENOENT) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		switch opErr.Op {
		case "dial", "read", "write":
			return true
		}
	}
	return false
}

// decodeDaemonError builds a *DaemonError from a failed response,
// pulling the message + code out of the daemon's JSON error envelope.
func decodeDaemonError(status int, raw []byte) error {
	var e struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	_ = json.Unmarshal(raw, &e)
	return &DaemonError{Status: status, Code: e.Code, Msg: e.Error, Raw: raw}
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
