package agentd

import (
	"compress/gzip"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// gzipWriterPool reuses gzip.Writer instances across requests. A fresh writer
// allocates the full deflate compressor state (~800 KB measured on the perf
// payload), and the gzipped routes sit on the dashboard's polling loop —
// /api/snapshot every 2s per open tab — so per-request allocation is steady
// GC pressure for no benefit.
var gzipWriterPool = sync.Pool{
	New: func() any { return gzip.NewWriter(io.Discard) },
}

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

		zw := gzipWriterPool.Get().(*gzip.Writer)
		zw.Reset(w)
		defer func() {
			_ = zw.Close()
			gzipWriterPool.Put(zw)
		}()
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
// substring match as support. Per RFC 9110 §12.5.3 a "*" wildcard matches
// codings not explicitly listed, so it counts as gzip support at q>0 — but an
// explicit gzip entry always wins over the wildcard, in either order.
func acceptsGzip(header string) bool {
	wildcard := false
	for _, item := range strings.Split(header, ",") {
		parts := strings.Split(strings.TrimSpace(item), ";")
		token := strings.TrimSpace(parts[0])
		isGzip := strings.EqualFold(token, "gzip")
		if !isGzip && token != "*" {
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
		if isGzip {
			return quality > 0
		}
		wildcard = quality > 0
	}
	return wildcard
}
