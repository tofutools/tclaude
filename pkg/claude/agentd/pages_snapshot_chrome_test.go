package agentd

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A single-page app in the small: a loading curtain, then its data arrives
// slowly and the app paints.
const slowAppHTML = `<!doctype html><html><head><title>loading</title></head><body>
<div id="curtain">Loading…</div>
<script>
fetch('/data').then(function(r){ return r.json(); }).then(function(d){
  document.getElementById('curtain').remove();
  var ul = document.createElement('ul');
  d.items.forEach(function(x){ var li = document.createElement('li'); li.textContent = x; ul.appendChild(li); });
  document.body.appendChild(ul);
  document.title = 'ready';
});
</script></body></html>`

func slowApp(t *testing.T, delay time.Duration) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/data":
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"items":["alpha","beta","gamma"]}`))
		default:
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(slowAppHTML))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The real headless Chrome, so opt-in: TCLAUDE_SNAPSHOT_CHROME=1 (with
// Chrome on PATH or TCLAUDE_CHROME).
func chromeForTest(t *testing.T) {
	t.Helper()
	if os.Getenv("TCLAUDE_SNAPSHOT_CHROME") == "" {
		t.Skip("set TCLAUDE_SNAPSHOT_CHROME=1 to run against the host's headless Chrome")
	}
	if _, err := findChrome(); err != nil {
		t.Skip(err)
	}
}

func TestChromeDefaultSnapshotWaitsForASlowAppToPaint(t *testing.T) {
	chromeForTest(t)
	srv := slowApp(t, 1500*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), pageSnapshotTimeout)
	defer cancel()
	png, settle, err := renderPageSnapshot(ctx, pageSnapshotJob{URL: srv.URL + "/", Origin: srv.URL, Width: 800, Height: 600})
	require.NoError(t, err)
	assert.True(t, bytes.HasPrefix(png, []byte("\x89PNG")))
	assert.True(t, settle.Settled, "%+v", settle)
	assert.GreaterOrEqual(t, settle.WaitedMS, int64(1500), "the wait outlasts the request the app is waiting on")
}

func TestChromeProbesFollowASlowAppFromCurtainToPainted(t *testing.T) {
	chromeForTest(t)
	srv := slowApp(t, 1200*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), pageSnapshotTimeout)
	defer cancel()
	b, err := openPageSnapshotChrome(ctx, pageSnapshotJob{URL: srv.URL + "/", Origin: srv.URL, Width: 800, Height: 600})
	require.NoError(t, err)
	defer b.Close()

	var titles []string
	var sawLoading bool
	var track pageChangeTracker
	start := time.Now()
	deadline := start.Add(10 * time.Second)
	for time.Now().Before(deadline) {
		p, err := b.Probe(ctx, true)
		require.NoError(t, err)
		at := time.Since(start)
		changed := track.observe(p, at)
		t.Logf("%5dms loaded=%v net=%d/%d dom=%+d text=%s(%d) layout=%s px=%s title=%q changed=%v note=%q",
			at.Milliseconds(), p.Loaded, p.InFlight, p.Requests, p.Mutations, p.TextHash, p.TextLen, p.Layout, p.Pixels, p.Title, changed, p.Note)
		if p.Title == "loading" && p.InFlight > 0 {
			sawLoading = true
		}
		if len(titles) == 0 || titles[len(titles)-1] != p.Title {
			titles = append(titles, p.Title)
		}
		if p.Title == "ready" && track.still(at) >= time.Second {
			break
		}
		time.Sleep(200 * time.Millisecond) // the probe cadence, not a wait for a condition
	}
	assert.True(t, sawLoading, "a probe saw the curtain with the data request in flight")
	assert.Equal(t, "ready", titles[len(titles)-1], "titles seen: %v", titles)
	frame, err := b.Frame(ctx, 0.25)
	require.NoError(t, err)
	assert.True(t, bytes.HasPrefix(frame, []byte("\x89PNG")))
	png, err := b.Final(ctx)
	require.NoError(t, err)
	assert.Greater(t, len(png), len(frame), "the final capture is full size")
}

// What a page starts beyond itself (a popup, a worker, a service worker) is
// held to the same filter as the page: it never reaches another host.
func TestChromeKeepsAPagesWorkersAndPopupsOnItsOrigin(t *testing.T) {
	chromeForTest(t)
	var hits atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		t.Logf("elsewhere was reached: %s", r.URL.Path)
	}))
	t.Cleanup(elsewhere.Close)
	reached := make(chan string, 8)
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sw.js", "/worker.js":
			w.Header().Set("Content-Type", "text/javascript")
			_, _ = fmt.Fprintf(w, "fetch(%q).catch(function(){}); fetch(%q);", elsewhere.URL+r.URL.Path, strings.TrimSuffix(r.URL.Path, ".js")+"-ran")
		case "/sw-ran", "/worker-ran":
			reached <- r.URL.Path
		default:
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprintf(w, `<!doctype html><title>x</title><body>page<script>
try { window.open(%[1]q); } catch (e) {}
new Worker('/worker.js');
navigator.serviceWorker && navigator.serviceWorker.register('/sw.js').catch(function(){});
</script></body>`, elsewhere.URL+"/popup")
		}
	}))
	t.Cleanup(page.Close)
	ctx, cancel := context.WithTimeout(context.Background(), pageSnapshotTimeout)
	defer cancel()
	b, err := openPageSnapshotChrome(ctx, pageSnapshotJob{URL: page.URL + "/", Origin: page.URL, Width: 800, Height: 600})
	require.NoError(t, err)
	defer b.Close()
	// A worker runs (and reaches the page's own origin)...
	select {
	case p := <-reached:
		t.Logf("%s reached the page", p)
	case <-time.After(10 * time.Second):
		t.Fatal("the worker did not run")
	}
	// ...and every request they or a popup made elsewhere was refused.
	_, err = b.Final(ctx)
	require.NoError(t, err)
	assert.Zero(t, hits.Load(), "nothing the page started reached another host")
}
