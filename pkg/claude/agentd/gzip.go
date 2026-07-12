package agentd

import (
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
)

// withGzip compresses a response when the client accepts gzip. It is applied
// to the large dashboard snapshot route, where remote dashboards benefit most,
// while leaving websocket/streaming routes on their native ResponseWriter.
func withGzip(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")
		if !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			next(w, r)
			return
		}

		zw := gzip.NewWriter(w)
		defer func() { _ = zw.Close() }()
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Del("Content-Length")
		next(&gzipResponseWriter{ResponseWriter: w, Writer: zw}, r)
	}
}

type gzipResponseWriter struct {
	http.ResponseWriter
	Writer *gzip.Writer
}

func (w *gzipResponseWriter) Write(p []byte) (int, error) { return w.Writer.Write(p) }

// acceptsGzip honours an explicit q=0 refusal rather than treating any raw
// substring match as support.
func acceptsGzip(header string) bool {
	for _, item := range strings.Split(header, ",") {
		parts := strings.Split(strings.TrimSpace(item), ";")
		if !strings.EqualFold(strings.TrimSpace(parts[0]), "gzip") {
			continue
		}
		quality := 1.0
		for _, param := range parts[1:] {
			key, value, ok := strings.Cut(strings.TrimSpace(param), "=")
			if ok && strings.EqualFold(strings.TrimSpace(key), "q") {
				if q, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
					quality = q
				}
			}
		}
		return quality > 0
	}
	return false
}
