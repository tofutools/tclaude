package agentd

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWithGzip(t *testing.T) {
	const body = `{"snapshot":"` + "highly compressible dashboard payload " + `"}`
	handler := withGzip(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, strings.Repeat(body, 100))
	})

	req := httptest.NewRequest(http.MethodGet, "/api/snapshot", nil)
	req.Header.Set("Accept-Encoding", "br, gzip")
	rec := httptest.NewRecorder()
	handler(rec, req)

	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(decoded), strings.Repeat(body, 100); got != want {
		t.Fatalf("decoded body mismatch: got %d bytes, want %d", len(got), len(want))
	}
	if rec.Body.Len() >= len(decoded) {
		t.Fatalf("gzip did not reduce response: compressed=%d plain=%d", rec.Body.Len(), len(decoded))
	}
}

func TestWithGzipHonoursQualityZero(t *testing.T) {
	handler := withGzip(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "plain") })
	req := httptest.NewRequest(http.MethodGet, "/api/snapshot", nil)
	req.Header.Set("Accept-Encoding", "gzip;q=0, br")
	rec := httptest.NewRecorder()
	handler(rec, req)
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want empty", got)
	}
	if got := rec.Body.String(); got != "plain" {
		t.Fatalf("body = %q, want plain", got)
	}
}
