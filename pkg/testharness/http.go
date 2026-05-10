//go:build rewire

package testharness

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// JSONRequest builds an httptest request with a JSON body. Caller
// owns peer-context setup (testharness deliberately does not import
// agentd; doing so would create a cycle with flow tests in package
// agentd that import this harness).
func JSONRequest(t *testing.T, method, path string, body any) *http.Request {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(b)
	}
	r := httptest.NewRequest(method, path, reader)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}

// Serve drives handler with r, returning the recorder. Tiny wrapper —
// scenarios use it for symmetry with JSONRequest.
func Serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr
}

// DecodeJSON unmarshals rec.Body into into, fatal on failure with the
// raw body in the message so debugging stays painless.
func DecodeJSON(t *testing.T, rec *httptest.ResponseRecorder, into any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
	}
}
