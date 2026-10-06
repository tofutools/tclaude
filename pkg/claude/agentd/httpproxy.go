package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"golang.org/x/net/http/httpguts"
)

const maxHTTPProxyBytes = 4 * 1024 * 1024

// Bodies use JSON's base64 encoding for []byte, preserving arbitrary binary data.
type httpProxyRequest struct {
	Name    string            `json:"name"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    []byte            `json:"body,omitempty"`
}
type httpProxyResponse struct {
	Status  int         `json:"status"`
	Headers http.Header `json:"headers"`
	Body    []byte      `json:"body"`
}

// httpProxyURL appends a relative path to the configured base. Never resolve a
// caller URL against it: absolute references and dot segments could spend the
// operator's credential outside the service or API prefix they selected.
func httpProxyURL(base, path string) (*url.URL, error) {
	b, err := url.Parse(base)
	if err != nil || b == nil || (b.Scheme != "http" && b.Scheme != "https") || b.Host == "" || b.User != nil || b.RawQuery != "" || b.Fragment != "" || b.Opaque != "" {
		return nil, fmt.Errorf("proxy base URL must be HTTP(S), without credentials, query or fragment")
	}
	p, err := url.Parse(path)
	if err != nil || p == nil || p.IsAbs() || p.Host != "" || p.User != nil || p.Fragment != "" || p.Opaque != "" || strings.HasPrefix(path, "//") {
		return nil, fmt.Errorf("path must be relative to the configured service")
	}
	// Reject ambiguous forms, including encoded separators and double-encoding,
	// which upstream routers might normalize differently from net/url.
	for _, part := range strings.Split(p.Path, "/") {
		if part == "." || part == ".." || strings.ContainsAny(part, "\\%") {
			return nil, fmt.Errorf("path contains an ambiguous or traversing segment")
		}
	}
	if strings.Contains(strings.ToLower(p.EscapedPath()), "%2f") || strings.Contains(strings.ToLower(p.EscapedPath()), "%5c") {
		return nil, fmt.Errorf("path contains an encoded separator")
	}
	b.Path = strings.TrimRight(b.Path, "/") + "/" + strings.TrimLeft(p.Path, "/")
	b.RawPath = ""
	b.RawQuery = p.RawQuery
	return b, nil
}

func httpProxyHeaderAllowed(name string) bool {
	if !httpguts.ValidHeaderFieldName(name) {
		return false
	}
	switch strings.ToLower(name) {
	case "host", "connection", "proxy-connection", "proxy-authorization", "proxy-authenticate", "keep-alive", "te", "trailer", "transfer-encoding", "upgrade", "content-length":
		return false
	}
	return true
}

func handleHTTPProxyRequest(w http.ResponseWriter, r *http.Request) {
	var body httpProxyRequest
	r.Body = http.MaxBytesReader(w, r.Body, 6*1024*1024)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid HTTP proxy request")
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_arg", "expected one HTTP proxy request")
		return
	}
	if _, ok := requirePermission(w, r, PermHTTP, ActionContext{HTTPProxy: body.Name}); !ok {
		return
	}
	cfg, err := config.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "could not load HTTP proxy configuration")
		return
	}
	var policy config.HTTPProxyConfig
	var found bool
	if cfg.Agent != nil {
		policy, found = cfg.Agent.HTTPProxies[body.Name]
	}
	if !found {
		writeError(w, http.StatusNotFound, "http_proxy_not_configured", "named HTTP proxy is not configured")
		return
	}
	setAuditTargetLabel(r, body.Name)
	target, err := httpProxyURL(policy.URL, body.Path)
	if err != nil || !httpProxyHeaderAllowed(policy.Header) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid proxy URL, relative path or configured header")
		return
	}
	if body.Method == "" {
		body.Method = http.MethodGet
	}
	// CONNECT and TRACE have transport/credential reflection semantics rather
	// than ordinary service API semantics.
	if !httpguts.ValidHeaderFieldName(body.Method) || strings.EqualFold(body.Method, "CONNECT") || strings.EqualFold(body.Method, "TRACE") || len(body.Body) > maxHTTPProxyBytes {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid method or body exceeds 4 MiB")
		return
	}
	for name, value := range body.Headers {
		if !httpProxyHeaderAllowed(name) || !httpguts.ValidHeaderFieldValue(value) {
			writeError(w, http.StatusBadRequest, "invalid_arg", "invalid or reserved request header")
			return
		}
	}
	value := policy.HeaderValue
	if policy.HeaderValueFile != "" {
		path := policy.HeaderValueFile
		if strings.HasPrefix(path, "~/") {
			home, homeErr := os.UserHomeDir()
			if homeErr != nil {
				writeError(w, 500, "io", "could not resolve header file")
				return
			}
			path = filepath.Join(home, path[2:])
		}
		f, openErr := os.Open(path)
		if openErr != nil {
			writeError(w, 503, "http_proxy_credential", "could not read configured header file")
			return
		}
		data, readErr := io.ReadAll(io.LimitReader(f, 16385))
		_ = f.Close()
		if readErr != nil || len(data) > 16384 {
			writeError(w, 503, "http_proxy_credential", "could not read configured header file")
			return
		}
		value = strings.TrimSpace(string(data))
	}
	if value == "" || !httpguts.ValidHeaderFieldValue(value) {
		writeError(w, 503, "http_proxy_credential", "configured header value is empty or invalid")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, body.Method, target.String(), bytes.NewReader(body.Body))
	if err != nil {
		writeError(w, 400, "invalid_arg", "could not construct request")
		return
	}
	for name, value := range body.Headers {
		req.Header.Set(name, value)
	}
	req.Header.Set(policy.Header, value)
	// Dedicated transport: do not send service credentials through an ambient
	// HTTP_PROXY or use cookies carried over from another request.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		// Transport errors can include the full URL; never expose policy or secrets.
		writeError(w, 502, "http_proxy_upstream", "HTTP proxy request failed; upstream outcome may be unknown")
		return
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPProxyBytes+1))
	if err != nil || len(data) > maxHTTPProxyBytes {
		writeError(w, 502, "http_proxy_response", "upstream response unreadable or exceeds 4 MiB; request may have succeeded")
		return
	}
	writeJSON(w, http.StatusOK, httpProxyResponse{Status: resp.StatusCode, Headers: resp.Header, Body: data})
}
