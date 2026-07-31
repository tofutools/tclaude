//go:build linux

package session

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/sandboxproxy"
)

// The decision log is the running proxy's only account of what it carried and
// what it refused. These tests are ordinary unit tests: they drive the proxy
// server over a plain loopback listener with no sandbox, because what is under
// test is the record the audit hook writes, not the floor beneath it.

const (
	decisionLogSecret       = "s3cr3t-must-never-be-logged"
	decisionLogProxyAuth    = "Basic dXNlcjpzM2NyM3QtbXVzdC1uZXZlci1iZS1sb2dnZWQ="
	decisionLogAllowedHost  = "allowed.decision.test"
	decisionLogRefusedHost  = "refused.decision.test"
	decisionLogDialDeadline = 5 * time.Second
)

// TestProxyNetworkDecisionLogRecordsBothOutcomes is the observability contract
// the harness-cooperation smokes rest on. A refusal leaves no other trace — the
// client sees a status it may swallow, and an empty-namespace floor produces no
// packet to capture — so if the refused destination is not in this record, the
// claim "every undeclared origin was refused" is an inference rather than an
// observation.
func TestProxyNetworkDecisionLogRecordsBothOutcomes(t *testing.T) {
	log := captureProxyDecisionLog(t)
	endpoint := startDecisionLogProxy(t)

	// An authorized name whose upstream does not exist: the decision is still
	// reported, because the verdict is reached before the dial.
	status, err := decisionLogHTTPConnect(endpoint,
		net.JoinHostPort(decisionLogAllowedHost, "443"), "")
	require.NoError(t, err)
	assert.NotEqual(t, 403, status,
		"an authorized destination must not be refused by policy")

	status, err = decisionLogHTTPConnect(endpoint,
		net.JoinHostPort(decisionLogRefusedHost, "443"), "")
	require.NoError(t, err)
	require.Equal(t, 403, status)

	records := log.lines()
	assert.Truef(t, decisionLogHas(records, decisionLogAllowedHost, "allowed"),
		"the carried destination must be observable; log:\n%s",
		strings.Join(records, "\n"))
	assert.Truef(t, decisionLogHas(records, decisionLogRefusedHost, "not_authorized"),
		"the REFUSED destination must be observable, or a refusal is only inferred; log:\n%s",
		strings.Join(records, "\n"))
}

// TestProxyNetworkDecisionLogCarriesNoCredentials pins the closed set of fields
// the record is built from.
//
// The property holds by construction today — the hook is handed an evaluated
// Target, which has no field for a header or a URL — but "by construction" is
// exactly the kind of guarantee a later field addition erases silently. Both
// carriages are exercised with credentials in every place a client can put
// them: a Proxy-Authorization header on CONNECT, and userinfo plus a header on
// an absolute-form plain HTTP request.
func TestProxyNetworkDecisionLogCarriesNoCredentials(t *testing.T) {
	log := captureProxyDecisionLog(t)
	endpoint := startDecisionLogProxy(t)

	_, err := decisionLogHTTPConnect(endpoint,
		net.JoinHostPort(decisionLogRefusedHost, "443"),
		"Proxy-Authorization: "+decisionLogProxyAuth+"\r\n")
	require.NoError(t, err)

	// Absolute-form plain HTTP, with the credential in the URL as well as in a
	// header. req.URL.User never reaches the evaluated Target, and this asserts
	// that it never reaches the log either.
	require.NoError(t, decisionLogAbsoluteForm(endpoint, fmt.Sprintf(
		"GET http://user:%s@%s/private?token=%s HTTP/1.1\r\n"+
			"Host: %s\r\nProxy-Authorization: %s\r\nConnection: close\r\n\r\n",
		decisionLogSecret, decisionLogRefusedHost, decisionLogSecret,
		decisionLogRefusedHost, decisionLogProxyAuth)))

	// And over the SOCKS5 carriage, which has its own handshake.
	_, err = decisionLogSOCKS5Connect(endpoint, decisionLogRefusedHost, 443)
	require.NoError(t, err)

	records := strings.Join(log.lines(), "\n")
	require.NotEmpty(t, records, "the proxy must have recorded its decisions")
	require.Contains(t, records, decisionLogRefusedHost,
		"the destination itself IS logged; without this the assertions below are vacuous")
	assert.NotContains(t, records, decisionLogSecret,
		"no credential may reach the decision log")
	assert.NotContains(t, records, decisionLogProxyAuth,
		"the Proxy-Authorization header may not reach the decision log")
	assert.NotContains(t, records, "/private",
		"the request path may not reach the decision log")
	assert.NotContains(t, records, "token=",
		"the query string may not reach the decision log")
}

// decisionLogHas reports whether some record names this host with this verdict.
func decisionLogHas(records []string, host, verdict string) bool {
	for _, record := range records {
		if strings.Contains(record, ProxyNetworkDecisionMessage) &&
			strings.Contains(record, host) &&
			strings.Contains(record, verdict) {
			return true
		}
	}
	return false
}

type decisionLogCapture struct {
	mu      sync.Mutex
	builder strings.Builder
}

func (c *decisionLogCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.builder.Write(p)
}

func (c *decisionLogCapture) lines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Split(strings.TrimSpace(c.builder.String()), "\n")
}

// captureProxyDecisionLog installs a debug-level handler for the duration of
// one test. Debug is required rather than incidental: the decision record is
// emitted at that level on purpose, so a test that captured only the default
// level would see nothing and pass vacuously.
func captureProxyDecisionLog(t *testing.T) *decisionLogCapture {
	t.Helper()
	capture := &decisionLogCapture{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(capture, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return capture
}

// startDecisionLogProxy serves the production audit hooks on a loopback
// listener under a policy that authorizes exactly one name.
func startDecisionLogProxy(t *testing.T) string {
	t.Helper()
	rules := sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{
			{Host: decisionLogAllowedHost, Ports: []int{443}},
		},
	}
	server, err := sandboxproxy.New(sandboxproxy.Config{
		Rules:      rules,
		OnDecision: logProxyNetworkDecision,
		OnError:    logProxyNetworkError,
	})
	require.NoError(t, err)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return listener.Addr().String()
}

func decisionLogHTTPConnect(
	endpoint, target, extraHeaders string,
) (int, error) {
	conn, err := net.DialTimeout("tcp", endpoint, decisionLogDialDeadline)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(decisionLogDialDeadline)); err != nil {
		return 0, err
	}
	if _, err := fmt.Fprintf(conn,
		"CONNECT %s HTTP/1.1\r\nHost: %s\r\n%s\r\n",
		target, target, extraHeaders); err != nil {
		return 0, err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 2 {
		return 0, fmt.Errorf("malformed proxy status %q", line)
	}
	var status int
	if _, err := fmt.Sscanf(fields[1], "%d", &status); err != nil {
		return 0, err
	}
	return status, nil
}

func decisionLogAbsoluteForm(endpoint, request string) error {
	conn, err := net.DialTimeout("tcp", endpoint, decisionLogDialDeadline)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(decisionLogDialDeadline)); err != nil {
		return err
	}
	if _, err := conn.Write([]byte(request)); err != nil {
		return err
	}
	_, err = bufio.NewReader(conn).ReadString('\n')
	return err
}

func decisionLogSOCKS5Connect(endpoint, host string, port int) (byte, error) {
	conn, err := net.DialTimeout("tcp", endpoint, decisionLogDialDeadline)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(decisionLogDialDeadline)); err != nil {
		return 0, err
	}
	if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
		return 0, err
	}
	reader := bufio.NewReader(conn)
	greeting := make([]byte, 2)
	if _, err := readFullFromReader(reader, greeting); err != nil {
		return 0, err
	}
	body := []byte{5, 1, 0, 3, byte(len(host))}
	body = append(body, host...)
	body = binary.BigEndian.AppendUint16(body, uint16(port))
	if _, err := conn.Write(body); err != nil {
		return 0, err
	}
	header := make([]byte, 4)
	if _, err := readFullFromReader(reader, header); err != nil {
		return 0, err
	}
	return header[1], nil
}

func readFullFromReader(reader *bufio.Reader, buffer []byte) (int, error) {
	read := 0
	for read < len(buffer) {
		n, err := reader.Read(buffer[read:])
		read += n
		if err != nil {
			return read, err
		}
	}
	return read, nil
}
