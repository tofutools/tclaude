package agentd

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gitlabapi_redirect_test.go drives real redirects through both GitLab
// transports. Go's own redirect rule keeps Authorization for the same
// hostname on any port or scheme, and for subdomains; the token must reach
// only the instance's exact origin.

type redirectSeen struct {
	mu   sync.Mutex
	hits []string // "host auth" per non-redirect request
}

func (s *redirectSeen) record(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hits = append(s.hits, r.Host+" "+r.Header.Get("Authorization"))
}

func (s *redirectSeen) list() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.hits...)
}

// redirectWorld starts two TLS servers and points glHTTPClient at them
// through a transport that dials by PORT, so any hostname reaches them: the
// instance is gitlab.test on server A's port.
func redirectWorld(t *testing.T) (base string, portB string, seen *redirectSeen) {
	t.Helper()
	seen = &redirectSeen{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if to := r.URL.Query().Get("to"); to != "" {
			http.Redirect(w, r, to, http.StatusFound)
			return
		}
		seen.record(r)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	a := httptest.NewTLSServer(handler)
	t.Cleanup(a.Close)
	b := httptest.NewTLSServer(handler)
	t.Cleanup(b.Close)
	_, portA, err := net.SplitHostPort(a.Listener.Addr().String())
	require.NoError(t, err)
	_, portB, err = net.SplitHostPort(b.Listener.Addr().String())
	require.NoError(t, err)

	prev := glHTTPClient.Transport
	glHTTPClient.Transport = &http.Transport{
		// Test certificates do not name these hosts; the redirect policy
		// is what is under test, not certificate validation.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test-only
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
		},
	}
	t.Cleanup(func() { glHTTPClient.Transport = prev })
	return "https://gitlab.test:" + portA, portB, seen
}

func TestGitLabRedirects_KeepTheTokenOnTheInstancesExactOrigin(t *testing.T) {
	base, portB, seen := redirectWorld(t)
	portA := base[len("https://gitlab.test:"):]
	get := func(to string) (glAPIResult, error) {
		return doGitLabRequest(context.Background(), base, "glpat-redirect", glAPIRequest{
			Path: "redirect", Query: url.Values{"to": {to}},
		})
	}

	cases := []struct {
		name, to, wantHit string
	}{
		{"the same origin keeps it", base + "/api/v4/next", "gitlab.test:" + portA + " Bearer glpat-redirect"},
		{"the same origin spelled with a different case keeps it", "https://GITLAB.test:" + portA + "/next", "GITLAB.test:" + portA + " Bearer glpat-redirect"},
		{"another port on the same host drops it", "https://gitlab.test:" + portB + "/object", "gitlab.test:" + portB + " "},
		{"a subdomain drops it", "https://storage.gitlab.test:" + portA + "/object", "storage.gitlab.test:" + portA + " "},
		{"an unrelated host drops it", "https://objects.example.test:" + portB + "/object", "objects.example.test:" + portB + " "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(seen.list())
			res, err := get(tc.to)
			require.NoError(t, err)
			assert.Equal(t, http.StatusOK, res.Status)
			hits := seen.list()[before:]
			require.Len(t, hits, 1)
			assert.Equal(t, tc.wantHit, hits[0])
		})
	}

	t.Run("plain http is refused, not followed", func(t *testing.T) {
		before := len(seen.list())
		_, err := get("http://gitlab.test:" + portA + "/object")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "non-https")
		assert.Empty(t, seen.list()[before:], "nothing was requested over http")
	})

	t.Run("the streaming transport follows the same policy", func(t *testing.T) {
		before := len(seen.list())
		var out bytes.Buffer
		res, err := streamGitLabResponse(context.Background(), base, "glpat-redirect", glAPIRequest{
			Path: "redirect", Query: url.Values{"to": {"https://gitlab.test:" + portB + "/artifact"}},
		}, &out, 1<<20)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, res.Status)
		assert.Equal(t, `{"ok":true}`, out.String())
		hits := seen.list()[before:]
		require.Len(t, hits, 1)
		assert.Equal(t, "gitlab.test:"+portB+" ", hits[0], "an artifact handed to object storage goes there without the token")
	})
}

func TestGLCheckRedirect_BoundsTheChain(t *testing.T) {
	first, err := http.NewRequest(http.MethodGet, "https://gitlab.test/api/v4/x", nil)
	require.NoError(t, err)
	via := make([]*http.Request, glMaxRedirects)
	for i := range via {
		via[i] = first
	}
	next, err := http.NewRequest(http.MethodGet, "https://gitlab.test/api/v4/y", nil)
	require.NoError(t, err)
	assert.Error(t, glCheckRedirect(next, via))
	assert.NoError(t, glCheckRedirect(next, via[:1]))

	https443, err := http.NewRequest(http.MethodGet, "https://gitlab.test:443/api/v4/y", nil)
	require.NoError(t, err)
	https443.Header.Set("Authorization", "Bearer t")
	require.NoError(t, glCheckRedirect(https443, via[:1]))
	assert.Equal(t, "Bearer t", https443.Header.Get("Authorization"), ":443 is the same origin as a portless base")
}
