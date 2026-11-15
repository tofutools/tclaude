package agentd

import (
	"bytes"
	"compress/gzip"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The dashboard is ~700 no-build ES modules, stylesheets and vendor runtimes.
// Over a tunnel (cloudflared) every fetch is a real round trip, so a reload
// that refetches all of them and a cold load that sends them uncompressed are
// both slow. Two remedies, both keyed on the embedded tree's content hash
// (dashboardAssetsVersion):
//
//   - Versioned URLs. Pages reference assets under /static/v-<hash>/…, served
//     immutable: a reload takes them from the browser cache. A module's
//     relative imports resolve against its own URL, so the whole graph
//     inherits the prefix with no change to the JS; the few absolute
//     references (the HTML pages, the import maps) are rewritten when the
//     page is built. An agentd upgrade changes the hash and so every URL, so
//     a browser never runs stale JS. The bare /static/… path still works,
//     uncached (no-store), for anything that names it directly.
//   - Compression. Text assets are gzipped once, on first request, and served
//     to every client that accepts gzip.

// dashboardStaticBase is the versioned asset prefix the served pages use.
var dashboardStaticBase = "/static/v-" + dashboardAssetsVersion + "/"

// dashboardStaticImmutable is the Cache-Control of a current versioned asset.
// private, not public: the assets sit behind the dashboard's auth, so a shared
// cache (a CDN in front of a tunnel) must not keep and hand them out.
const dashboardStaticImmutable = "private, max-age=31536000, immutable"

// versionStaticRefs rewrites a page's quoted absolute "/static/ references
// (attributes and the import map) to the versioned prefix.
func versionStaticRefs(html []byte) []byte {
	return bytes.ReplaceAll(html, []byte(`"/static/`), []byte(`"`+dashboardStaticBase))
}

// splitStaticVersion splits a /static/ request path (prefix already stripped)
// into its version and asset path: "v-abc/js/x.js" is ("abc", "js/x.js", true);
// "js/x.js" is unversioned.
func splitStaticVersion(p string) (version, asset string, versioned bool) {
	if !strings.HasPrefix(p, "v-") {
		return "", p, false
	}
	version, asset, ok := strings.Cut(p[2:], "/")
	if !ok {
		return "", p, false
	}
	return version, asset, true
}

// handleDashboardStatic serves the dashboard's static assets — stylesheets,
// ES modules, vendor runtimes and images — from the embedded dashboard/
// directory, behind the same session-cookie gate as /api/*.
//
// /static/v-<current hash>/… is immutable. Any other path is no-store: the
// bare /static/… (an embed.FS has no modtime, so http.FileServerFS emits no
// validators to revalidate with) and a version this binary does not serve —
// a page built by the previous agentd, which reloads itself on its next poll
// (js/assets-version.js) and meanwhile gets the current files.
func handleDashboardStatic() http.Handler {
	files := http.FileServerFS(dashboardAssetsFS)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := dashboardStaticNow()
		authed := checkDashboardAuth(w, r)
		authDone := dashboardStaticNow()
		defer func() { warnSlowDashboardStatic(r.URL.Path, start, authDone, dashboardStaticNow()) }()
		if !authed {
			return
		}
		rest, ok := strings.CutPrefix(r.URL.Path, "/static/")
		// No directory listings — only the named asset files are served.
		if !ok || rest == "" || strings.HasSuffix(rest, "/") {
			http.NotFound(w, r)
			return
		}
		version, asset, versioned := splitStaticVersion(rest)
		if versioned && version == dashboardAssetsVersion {
			w.Header().Set("Cache-Control", dashboardStaticImmutable)
		} else {
			w.Header().Set("Cache-Control", "no-store")
		}
		if serveDashboardStaticGzip(w, r, asset) {
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/" + asset
		r2.URL.RawPath = ""
		files.ServeHTTP(w, r2)
	})
}

// dashboardStaticNow is the static handler's clock (a seam for tests).
var dashboardStaticNow = time.Now

// dashboardStaticSlow is how long a static request may take inside agentd
// before it is logged. The boot graph is ~300 parallel module requests, so
// one slow response holds the whole boot. No warning for a request that was
// slow end to end means it was held before it reached agentd (a tunnel, a
// CDN). "serve" includes writing the response, so a slow reader shows there.
const dashboardStaticSlow = time.Second

func warnSlowDashboardStatic(path string, start, authDone, end time.Time) {
	if end.Sub(start) < dashboardStaticSlow {
		return
	}
	slog.Warn("dashboard static: slow request", "path", path,
		"total", end.Sub(start), "auth", authDone.Sub(start), "serve", end.Sub(authDone))
}

// dashboardCompressible lists the asset types worth gzipping: text. Images,
// fonts and the like are already compressed.
var dashboardCompressible = map[string]bool{
	".js": true, ".mjs": true, ".css": true, ".svg": true, ".html": true, ".json": true, ".map": true,
}

// dashboardGzipMinSize skips files too small for gzip to pay for its header.
const dashboardGzipMinSize = 1024

type dashboardGzipEntry struct {
	once sync.Once
	data []byte // nil: serve the file as is
}

var dashboardGzipCache sync.Map // asset path → *dashboardGzipEntry

// dashboardGzipped returns the gzip of an embedded asset, computed once, or
// nil when the asset is missing, not text, small, or does not shrink.
func dashboardGzipped(asset string) []byte {
	if !dashboardCompressible[path.Ext(asset)] || !fs.ValidPath(asset) {
		return nil
	}
	// Only real files get a cache entry, so requests for made-up paths cannot
	// grow the cache.
	if info, err := fs.Stat(dashboardAssetsFS, asset); err != nil || info.IsDir() {
		return nil
	}
	v, _ := dashboardGzipCache.LoadOrStore(asset, &dashboardGzipEntry{})
	entry := v.(*dashboardGzipEntry)
	entry.once.Do(func() {
		raw, err := fs.ReadFile(dashboardAssetsFS, asset)
		if err != nil || len(raw) < dashboardGzipMinSize {
			return
		}
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		_, _ = zw.Write(raw)
		if zw.Close() != nil || buf.Len() >= len(raw)*9/10 {
			return
		}
		entry.data = buf.Bytes()
	})
	return entry.data
}

// serveDashboardStaticGzip writes the gzipped asset when the client accepts
// gzip and it is worth it, and reports whether it did. A Range request takes
// the plain path: ranges address the identity bytes.
func serveDashboardStaticGzip(w http.ResponseWriter, r *http.Request, asset string) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if r.Header.Get("Range") != "" {
		return false
	}
	// Vary on every compressible asset, whichever encoding this client gets,
	// so a cache never hands a gzip body to a client that did not ask for it.
	if dashboardCompressible[path.Ext(asset)] {
		w.Header().Add("Vary", "Accept-Encoding")
	}
	if !acceptsGzip(r.Header.Get("Accept-Encoding")) {
		return false
	}
	data := dashboardGzipped(asset)
	if data == nil {
		return false
	}
	h := w.Header()
	if ct := mime.TypeByExtension(path.Ext(asset)); ct != "" {
		h.Set("Content-Type", ct)
	}
	h.Set("Content-Encoding", "gzip")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
	return true
}
