package sandboxproxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RefusalHeader names the machine-readable verdict on an HTTP refusal, so a
// cooperating client can distinguish a policy refusal from an origin's own 403.
const RefusalHeader = "X-Tclaude-Network-Refusal"

// hopByHopHeaders are dropped when forwarding an absolute-form request: they
// describe the hop to the proxy, not the request to the origin.
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Proxy-Connection",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// serveHTTP carries the HTTP proxy protocol: CONNECT tunnels for HTTPS and any
// other TCP a client tunnels, plus absolute-form plain HTTP.
//
// There is deliberately no TLS interception and no generated CA. Policy is
// enforced on the host the client requested, which is exactly the identity a
// CONNECT states, so no trust store anywhere needs to change.
func (s *Server) serveHTTP(conn net.Conn, reader *bufio.Reader) {
	client := &bufferedConn{Conn: conn, reader: reader}
	for {
		_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
		req, err := http.ReadRequest(reader)
		if err != nil {
			return
		}
		if req.Method == http.MethodConnect {
			s.serveHTTPConnect(client, req)
			return
		}
		if !s.serveHTTPAbsoluteForm(conn, req) {
			return
		}
	}
}

func (s *Server) serveHTTPConnect(conn *bufferedConn, req *http.Request) {
	target, err := parseHTTPAuthority(req.Host, 0)
	if err != nil {
		s.reportError(CarriageHTTP, err)
		writeHTTPStatus(conn, http.StatusBadRequest,
			"tclaude filtering proxy could not read the CONNECT target.")
		return
	}
	upstream, decision, dialErr := s.connect(s.baseCtx, CarriageHTTP, target)
	if !decision.Allowed() {
		writeHTTPRefusal(conn, decision)
		return
	}
	if dialErr != nil {
		writeHTTPStatus(conn, http.StatusBadGateway,
			"tclaude filtering proxy could not reach "+target.String()+".")
		return
	}
	defer func() { _ = upstream.Close() }()
	if _, err := io.WriteString(conn,
		"HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	// An established tunnel carries arbitrary traffic for as long as both ends
	// want it; the handshake deadline must not cut it.
	_ = conn.SetDeadline(time.Time{})
	pipe(conn, upstream)
}

// serveHTTPAbsoluteForm handles `GET http://host/path HTTP/1.1`. It reports
// whether the proxy connection may carry another request.
func (s *Server) serveHTTPAbsoluteForm(conn net.Conn, req *http.Request) bool {
	if req.URL == nil || !req.URL.IsAbs() {
		writeHTTPStatus(conn, http.StatusBadRequest,
			"tclaude filtering proxy requires an absolute-form request URI or CONNECT.")
		return false
	}
	if !strings.EqualFold(req.URL.Scheme, "http") {
		writeHTTPStatus(conn, http.StatusBadRequest,
			"tclaude filtering proxy carries "+req.URL.Scheme+
				" only through CONNECT; use a CONNECT tunnel for this scheme.")
		return false
	}
	target, err := parseHTTPAuthority(req.URL.Host, 80)
	if err != nil {
		s.reportError(CarriageHTTP, err)
		writeHTTPStatus(conn, http.StatusBadRequest,
			"tclaude filtering proxy could not read the request target.")
		return false
	}
	upstream, decision, dialErr := s.connect(s.baseCtx, CarriageHTTP, target)
	if !decision.Allowed() {
		writeHTTPRefusal(conn, decision)
		return false
	}
	if dialErr != nil {
		writeHTTPStatus(conn, http.StatusBadGateway,
			"tclaude filtering proxy could not reach "+target.String()+".")
		return false
	}
	defer func() { _ = upstream.Close() }()
	return s.forwardHTTP(conn, upstream, req, target)
}

// forwardHTTP relays one origin-form request over the already-authorized
// connection and copies the response back.
func (s *Server) forwardHTTP(
	conn net.Conn,
	upstream net.Conn,
	req *http.Request,
	target Target,
) bool {
	outbound := req.Clone(req.Context())
	outbound.RequestURI = ""
	// The Host field is deliberately not overridden. For an absolute-form
	// request Go sets req.Host from the request-line authority, which is
	// exactly the authority policy evaluated, so a client cannot use a
	// conflicting Host header to select a vhost that was never evaluated.
	// One upstream connection carries exactly one forwarded request here, so
	// ask the origin to close it rather than leaving a pooled connection this
	// proxy does not own.
	outbound.Close = true
	_ = upstream.SetDeadline(time.Now().Add(handshakeTimeout))
	if err := outbound.Write(upstream); err != nil {
		s.reportError(CarriageHTTP, err)
		writeHTTPStatus(conn, http.StatusBadGateway,
			"tclaude filtering proxy could not forward the request.")
		return false
	}
	_ = upstream.SetDeadline(time.Time{})
	resp, err := http.ReadResponse(bufio.NewReader(upstream), outbound)
	if err != nil {
		s.reportError(CarriageHTTP, err)
		writeHTTPStatus(conn, http.StatusBadGateway,
			"tclaude filtering proxy could not read the upstream response.")
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	_ = conn.SetDeadline(time.Time{})
	if err := resp.Write(conn); err != nil {
		return false
	}
	// req.Close reflects the client's own Connection header; honor it, and
	// otherwise allow another request on the proxy connection.
	return !req.Close
}

// parseHTTPAuthority reads a host[:port] authority. defaultPort applies when
// the authority omits one; a zero defaultPort makes the port mandatory, which
// is what CONNECT requires.
func parseHTTPAuthority(authority string, defaultPort int) (Target, error) {
	authority = strings.TrimSpace(authority)
	if authority == "" {
		return Target{}, fmt.Errorf("proxy request has no authority")
	}
	host, portText, err := net.SplitHostPort(authority)
	if err != nil {
		if defaultPort == 0 {
			return Target{}, fmt.Errorf("proxy authority %q has no port", authority)
		}
		return ParseTarget(strings.Trim(authority, "[]"), defaultPort)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return Target{}, fmt.Errorf("proxy authority %q has an invalid port", authority)
	}
	return ParseTarget(host, port)
}

// writeHTTPRefusal renders a refusal the harness itself can read. A silent
// packet drop cannot explain itself; this is the one component that can.
func writeHTTPRefusal(conn net.Conn, decision Decision) {
	body := decision.Detail
	if body == "" {
		body = "tclaude filtering proxy refused this destination."
	}
	writeHTTPResponse(conn, http.StatusForbidden, body,
		RefusalHeader+": "+string(decision.Verdict)+"\r\n")
}

func writeHTTPStatus(conn net.Conn, status int, body string) {
	writeHTTPResponse(conn, status, body, "")
}

func writeHTTPResponse(
	conn net.Conn,
	status int,
	body string,
	extraHeaders string,
) {
	body += "\n"
	head := "HTTP/1.1 " + strconv.Itoa(status) + " " +
		http.StatusText(status) + "\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Length: " + strconv.Itoa(len(body)) + "\r\n" +
		extraHeaders +
		"Connection: close\r\n\r\n"
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	_, _ = io.WriteString(conn, head+body)
}
