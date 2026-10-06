package session

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
)

type httpProxyTestTransport func(*http.Request) (*http.Response, error)

func (f httpProxyTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestHTTPProxyLaunchChild is the workload spawned by the bootstrap test. It
// uses the injected URL with a normal net/http client, without any tclaude API.
func TestHTTPProxyLaunchChild(t *testing.T) {
	if os.Getenv("TCLAUDE_GATEWAY_TEST_CHILD") != "1" {
		return
	}
	if os.Getenv(HTTPProxyEnvPrefix+"billing") != "" {
		fmt.Fprintln(os.Stderr, "inherited denied proxy survived")
		os.Exit(1)
	}
	base := os.Getenv(HTTPProxyEnvPrefix + "inventory")
	if base == "" {
		fmt.Fprintln(os.Stderr, "missing injected URL")
		os.Exit(1)
	}
	resp, err := http.Post(base+"items?key=one", "application/octet-stream", bytes.NewReader([]byte{0, 255, 10}))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != 422 || !bytes.Equal(data, []byte{0, 255, 10}) {
		os.Exit(1)
	}
}

func TestHTTPProxyBootstrapInjectsWorkingURLs(t *testing.T) {
	t.Setenv("TCLAUDE_GATEWAY_TEST_CHILD", "1")
	t.Setenv(HTTPProxyEnvPrefix+"billing", "http://stale.invalid/")
	calls := 0
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "launch-row", r.Header.Get(agentipc.SessionClaimHeader))
		if r.URL.Path == "/v1/http/environment" {
			_, _ = io.WriteString(w, `{"names":["inventory"]}`)
			return
		}
		calls++
		assert.Equal(t, "/v1/http/proxy/inventory/items", r.URL.Path)
		assert.Equal(t, "key=one", r.URL.RawQuery)
		assert.Equal(t, "POST", r.Method)
		data, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		w.WriteHeader(422)
		_, _ = w.Write(data)
	}))
	defer daemon.Close()
	target, err := url.Parse(daemon.URL)
	require.NoError(t, err)
	previous := newHTTPProxyDaemonClient
	t.Cleanup(func() { newHTTPProxyDaemonClient = previous })
	newHTTPProxyDaemonClient = func() *http.Client {
		return &http.Client{Transport: httpProxyTestTransport(func(r *http.Request) (*http.Response, error) {
			clone := r.Clone(r.Context())
			copyURL := *r.URL
			clone.URL = &copyURL
			clone.URL.Scheme, clone.URL.Host = target.Scheme, target.Host
			return daemon.Client().Transport.RoundTrip(clone)
		})}
	}
	code, err := runHTTPProxyExec("launch-row", clcommon.ShellQuoteArg(os.Args[0])+" -test.run '^TestHTTPProxyLaunchChild$'")
	require.NoError(t, err)
	assert.Zero(t, code)
	assert.Equal(t, 1, calls)
}

func TestHTTPProxyBridgeRejectsForeignCapabilitiesAndPreservesEscaping(t *testing.T) {
	client := &http.Client{Transport: httpProxyTestTransport(func(r *http.Request) (*http.Response, error) {
		assert.Equal(t, "/v1/http/proxy/inventory/foo%2fbar", r.URL.EscapedPath())
		assert.Equal(t, "launch-row", r.Header.Get(agentipc.SessionClaimHeader))
		assert.Empty(t, r.Header.Get("Referer"))
		return &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("invalid path"))}, nil
	})}
	handler, entries, err := newHTTPProxyBridge(client, "launch-row", []string{"inventory"})
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/unknown/items", nil))
	assert.Equal(t, 404, rec.Code)
	req := httptest.NewRequest("GET", entries[HTTPProxyEnvPrefix+"inventory"]+"foo%2fbar", nil)
	req.Header.Set("Referer", "http://localhost/secret/")
	req.Header.Set(agentipc.SessionClaimHeader, "foreign")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, 400, rec.Code)
	// Discovery returns only names and does not follow HTTP redirects.
	_, err = httpProxyNames(context.Background(), &http.Client{Transport: httpProxyTestTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader("denied"))}, nil
	})}, "launch-row")
	assert.Error(t, err)
}
