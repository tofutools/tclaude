package copilotfixture_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness/copilotfixture"
)

// The capture seam's own behaviour, proven against a loopback echo server so
// the checks run offline with no credential and no live service.
//
// These exist because the pass-through mode is the shape that carries real
// traffic when an operator runs the credentialed evidence step, and the two
// properties it rests on are easy to break silently: that a tunnel is spliced
// blind but still ACCOUNTED for, and that a plain-HTTP proxy request is refused
// rather than served. The second one is the redaction guarantee — it is the
// only path by which a URL or a header could ever reach the process.

// echoServer is a loopback stand-in for a destination.
func echoServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return listener.Addr().String()
}

// connectThrough opens a tunnel and returns the proxy's status code.
func connectThrough(t *testing.T, proxy, authority string) (int, net.Conn) {
	t.Helper()
	conn, err := net.Dial("tcp", proxy)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	_, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", authority, authority)
	require.NoError(t, err)
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	require.NoError(t, err)
	return response.StatusCode, conn
}

func TestProxyCapturePassThroughTunnelsAndRecords(t *testing.T) {
	destination := echoServer(t)
	capture := copilotfixture.NewProxyCaptureWithOptions(t,
		copilotfixture.ProxyCaptureOptions{PassThrough: true})
	capture.SetPhase("turn")

	status, tunnel := connectThrough(t, capture.Endpoint(), destination)
	require.Equal(t, http.StatusOK, status)

	// The tunnel must genuinely carry bytes: a capture that recorded the
	// destination but broke the connection would make a credentialed run fail
	// in a way that looks like the destination refusing it.
	_, err := tunnel.Write([]byte("ping"))
	require.NoError(t, err)
	echoed := make([]byte, 4)
	_, err = io.ReadFull(tunnel, echoed)
	require.NoError(t, err)
	assert.Equal(t, "ping", string(echoed))

	host, port, err := net.SplitHostPort(destination)
	require.NoError(t, err)
	observations := capture.Observations()
	require.Len(t, observations, 1)
	assert.Equal(t, "turn", observations[0].Phase)
	assert.Equal(t, host, observations[0].Host)
	assert.Equal(t, port, fmt.Sprint(observations[0].Port),
		"the port is contract evidence and must survive into the observation")
	assert.Equal(t, 1, observations[0].Tunnels)
	assert.Equal(t, 1, observations[0].Dialed)
	assert.Equal(t, 0, observations[0].Refused)
}

func TestProxyCaptureRefusesPlainHTTPEvenPassingThrough(t *testing.T) {
	destination := echoServer(t)
	capture := copilotfixture.NewProxyCaptureWithOptions(t,
		copilotfixture.ProxyCaptureOptions{PassThrough: true})

	conn, err := net.Dial("tcp", capture.Endpoint())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = fmt.Fprintf(conn,
		"GET http://%s/secret?token=redacted HTTP/1.1\r\nHost: %s\r\n\r\n",
		destination, destination)
	require.NoError(t, err)
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadGateway, response.StatusCode,
		"a plain-HTTP proxy request must be refused: serving it would put a request "+
			"line and headers through this process, which is the one way payload could "+
			"reach a capture that otherwise only ever sees a CONNECT authority")

	observations := capture.Observations()
	require.Len(t, observations, 1)
	assert.Equal(t, 1, observations[0].Refused)
	assert.Equal(t, 0, observations[0].Dialed)
}

func TestProxyCaptureRefusingModeNeverDials(t *testing.T) {
	destination := echoServer(t)
	capture := copilotfixture.NewProxyCapture(t)

	status, _ := connectThrough(t, capture.Endpoint(), destination)
	assert.Equal(t, http.StatusBadGateway, status)

	observations := capture.Observations()
	require.Len(t, observations, 1)
	assert.Equal(t, 0, observations[0].Dialed,
		"the default capture must observe intent without ever connecting")
	assert.Equal(t, 1, observations[0].Refused)
}

func TestProxyCaptureWallRefusesUnlistedDestination(t *testing.T) {
	admitted := echoServer(t)
	excluded := echoServer(t)
	capture := copilotfixture.NewProxyCaptureWithOptions(t,
		copilotfixture.ProxyCaptureOptions{
			PassThrough:         true,
			AllowedDestinations: []string{admitted},
		})

	admittedStatus, _ := connectThrough(t, capture.Endpoint(), admitted)
	assert.Equal(t, http.StatusOK, admittedStatus)
	excludedStatus, _ := connectThrough(t, capture.Endpoint(), excluded)
	assert.Equal(t, http.StatusForbidden, excludedStatus,
		"a destination outside the wall must be refused without being dialed, or a "+
			"phase run this way is not the filtered launch it claims to model")

	byHost := map[string]copilotfixture.ProxyObservation{}
	for _, observation := range capture.Observations() {
		byHost[observation.Host+":"+fmt.Sprint(observation.Port)] = observation
	}
	require.Len(t, byHost, 2)
	assert.Equal(t, 1, byHost[admitted].Dialed)
	assert.Equal(t, 0, byHost[excluded].Dialed)
	assert.Equal(t, 1, byHost[excluded].Refused)
}

// TestProxyCaptureChainsThroughUpstreamProxy covers the case an operator whose
// own sandbox forces egress through a proxy actually runs in: the capture has
// to reach the destination THROUGH that proxy, or the credentialed evidence
// step observes a wall of refusals and misreads them as Copilot behaviour.
//
// The upstream here is a second capture, so the whole chain stays on loopback.
func TestProxyCaptureChainsThroughUpstreamProxy(t *testing.T) {
	destination := echoServer(t)
	upstream := copilotfixture.NewProxyCaptureWithOptions(t,
		copilotfixture.ProxyCaptureOptions{PassThrough: true})
	capture := copilotfixture.NewProxyCaptureWithOptions(t,
		copilotfixture.ProxyCaptureOptions{
			PassThrough:   true,
			UpstreamProxy: "http://" + upstream.Endpoint(),
		})

	status, tunnel := connectThrough(t, capture.Endpoint(), destination)
	require.Equal(t, http.StatusOK, status)
	_, err := tunnel.Write([]byte("ping"))
	require.NoError(t, err)
	echoed := make([]byte, 4)
	_, err = io.ReadFull(tunnel, echoed)
	require.NoError(t, err)
	assert.Equal(t, "ping", string(echoed))

	// Both proxies must have seen the SAME destination: a chain that recorded
	// the upstream proxy's own address as the destination would turn every
	// observation into the same host and prove nothing.
	host, _, err := net.SplitHostPort(destination)
	require.NoError(t, err)
	for name, observed := range map[string][]copilotfixture.ProxyObservation{
		"capture":  capture.Observations(),
		"upstream": upstream.Observations(),
	} {
		require.Len(t, observed, 1, "%s", name)
		assert.Equal(t, host, observed[0].Host, "%s", name)
		assert.Equal(t, 1, observed[0].Dialed, "%s", name)
	}
}
