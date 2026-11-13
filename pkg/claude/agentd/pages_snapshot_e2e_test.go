package agentd_test

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPageSnapshotWatchesARealSinglePageApp is the end-to-end check of
// tk_83873896 against the tclaude dashboard itself: start the dashboard
// sandbox (scripts/sandbox/ui.sh up), then
//
//	TCLAUDE_SNAPSHOT_E2E_UPSTREAM=http://127.0.0.1:PORT TCLAUDE_SNAPSHOT_E2E_OUT=DIR \
//	  TCLAUDE_CHROME=... go test ./pkg/claude/agentd/ -run TestPageSnapshotWatchesARealSinglePageApp -v
//
// The sandbox is shown as a live page through a proxy the test owns (the
// stage serves only the agent's own servers), and rendered by the host's
// real headless Chrome: the default snapshot, then a watch that captures by
// session once the page has been still for a while. DIR gets the frames
// and the PNGs.
func TestPageSnapshotWatchesARealSinglePageApp(t *testing.T) {
	upstream := os.Getenv("TCLAUDE_SNAPSHOT_E2E_UPSTREAM")
	if upstream == "" {
		t.Skip("set TCLAUDE_SNAPSHOT_E2E_UPSTREAM to a running dashboard sandbox (scripts/sandbox/ui.sh up)")
	}
	out := os.Getenv("TCLAUDE_SNAPSHOT_E2E_OUT")
	if out == "" {
		out = t.TempDir()
	}
	require.NoError(t, os.MkdirAll(out, 0o755))
	u, err := url.Parse(upstream)
	require.NoError(t, err)
	proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(u) }}
	srv := httptest.NewServer(proxy)
	t.Cleanup(srv.Close)
	port := srv.Listener.Addr().(*net.TCPAddr).Port

	e := newPageEnv(t)
	a := e.agent(t, "e2e", true)
	_, rec := e.show(t, a, ":"+strconv.Itoa(port), "dash", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// The default snapshot waits for the dashboard to go calm.
	rec = e.call(t, a, http.MethodPost, "/v1/pages/dash/snapshot", map[string]any{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var snap struct {
		PNG    string          `json:"png"`
		Settle json.RawMessage `json:"settle"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snap))
	t.Logf("default snapshot: settle %s", snap.Settle)
	assert.Contains(t, string(snap.Settle), `"settled":true`, "the dashboard's poll and clock do not keep it from going calm")
	require.NoError(t, os.WriteFile(filepath.Join(out, "default.png"), []byte(decodePNG(t, snap.PNG)), 0o644))

	// The watch: from the loading curtain to settled, then a capture by session.
	s := e.watch(t, a, "dash", map[string]any{"every_ms": 250, "for_ms": 20_000, "frame_every": 1, "frame_scale": 0.5})
	start := s.next()
	require.Equal(t, "start", start.Type)
	var titles, texts []string
	var captured bool
	for !captured {
		ev := s.next()
		require.Equal(t, "tick", ev.Type, "%+v", ev)
		if ev.Frame != "" {
			require.NoError(t, os.WriteFile(filepath.Join(out, fmt.Sprintf("frame-%03d.png", ev.N)), []byte(decodePNG(t, ev.Frame)), 0o644))
		}
		// As `page snapshot --watch` prints it.
		line := fmt.Sprintf("t=%.1fs #%d calm=%.1fs still=%.1fs changed=%s doc=%s net=%d/%d dom=+%d text=%s/%d els=%d layout=%s px=%s path=%s title=%q",
			float64(ev.ElapsedMS)/1000, ev.N, float64(ev.CalmMS)/1000, float64(ev.StillMS)/1000, strings.Join(ev.Changed, ","), ev.Ready,
			ev.InFlight-ev.LongLived, ev.Requests, ev.Mutations, ev.TextHash, ev.TextLen, ev.Elements, ev.Layout, ev.PixelHash, ev.Path, ev.Title)
		if ev.CalmMS == 0 && ev.Unsettled != "" {
			line += " (not calm: " + ev.Unsettled + ")"
		}
		t.Log(line)
		if len(texts) == 0 || texts[len(texts)-1] != ev.TextHash {
			texts = append(texts, ev.TextHash)
		}
		if len(titles) == 0 || titles[len(titles)-1] != ev.Title {
			titles = append(titles, ev.Title)
		}
		if ev.Loaded && ev.CalmMS >= 1500 {
			rec := e.call(t, a, http.MethodPost, "/v1/pages/dash/snapshot/watch/"+start.Session+"/capture", map[string]any{})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var got struct {
				PNG     string `json:"png"`
				WatchMS int64  `json:"watch_ms"`
				StillMS int64  `json:"still_ms"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
			require.NoError(t, os.WriteFile(filepath.Join(out, "final.png"), []byte(decodePNG(t, got.PNG)), 0o644))
			t.Logf("captured at t=%.2fs, still for %.2fs", float64(got.WatchMS)/1000, float64(got.StillMS)/1000)
			captured = true
		}
	}
	t.Logf("titles seen: %q; distinct visible texts in order: %d", titles, len(texts))
	assert.Greater(t, len(texts), 1, "the watch saw the page change before it went still")
	t.Logf("frames and PNGs in %s", out)
}
