package agentd

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// connectSSE opens an SSE connection against srv as the authenticated
// dashboard, returns the line reader plus a cancel func that closes
// the connection. The cookie + Origin combo matches the production
// checkDashboardAuth gate (see dashboardRequest in dashboard_edit_test.go).
func connectSSE(t *testing.T, srv *httptest.Server) (*bufio.Reader, context.CancelFunc, *http.Response) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events", nil)
	require.NoError(t, err, "build SSE request")
	req.Header.Set("Origin", popupBaseURL)
	req.AddCookie(&http.Cookie{Name: dashboardCookieName, Value: dashboardSessionToken})

	// httptest server's default client doesn't follow keep-alive in
	// a way that fights SSE; the trick is just to not let the
	// transport gulp the body — we read it line-by-line below.
	resp, err := srv.Client().Do(req)
	if err != nil {
		cancel()
		t.Fatalf("connect SSE: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		t.Fatalf("connect SSE got status %d", resp.StatusCode)
	}
	t.Cleanup(func() {
		cancel()
		resp.Body.Close()
	})
	return bufio.NewReader(resp.Body), cancel, resp
}

// readNextSSEEvent walks the byte stream until it sees an
// `event: <name>\ndata: <data>` record terminated by a blank line,
// or until `deadline` elapses. Comments (lines starting with `:`)
// are skipped — the handler emits one as a keepalive on connect
// and on heartbeat ticks. Returns event/data; either empty on
// timeout.
func readNextSSEEvent(rdr *bufio.Reader, deadline time.Duration) (event, data string) {
	type rec struct{ event, data string }
	ch := make(chan rec, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var ev, dt string
		for {
			line, err := rdr.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				if ev != "" || dt != "" {
					select {
					case ch <- rec{event: ev, data: dt}:
					default:
					}
					return
				}
				continue
			}
			if strings.HasPrefix(line, ":") {
				continue // comment / keepalive
			}
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				ev = v
				continue
			}
			if v, ok := strings.CutPrefix(line, "data: "); ok {
				dt = v
				continue
			}
		}
	}()
	select {
	case r := <-ch:
		return r.event, r.data
	case <-time.After(deadline):
		return "", ""
	}
}

// TestSSE_DeliversBroadcastEvent — the end-to-end happy path: connect
// to /api/events, fire one Publish() on the broadcaster, see an
// `event: snapshot` arrive. Pins the wire format that the dashboard
// JS already keys off (the addEventListener('snapshot', …) call).
func TestSSE_DeliversBroadcastEvent(t *testing.T) {
	withDashboardAuth(t)
	prevBcast := dashboardEvents
	b := newTestBroadcaster()
	dashboardEvents = b
	t.Cleanup(func() { dashboardEvents = prevBcast })

	mux := http.NewServeMux()
	mux.HandleFunc("/api/events", handleDashboardEvents)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	rdr, _, _ := connectSSE(t, srv)

	// Wait for the initial connect comment to land so we know the
	// subscription is registered before we publish. Without this a
	// Publish issued before Subscribe() ran on the server goroutine
	// would be lost (the broadcaster only fans to current subs).
	require.Eventually(t, func() bool {
		// Peek a few bytes — the ": connected\n\n" is the first
		// thing on the wire.
		_, err := rdr.Peek(1)
		return err == nil
	}, 500*time.Millisecond, 10*time.Millisecond, "SSE connect didn't open")
	// Drain the initial comment so the next read lands on the data
	// event we trigger below.
	_, _ = rdr.ReadString('\n') // ": connected"
	_, _ = rdr.ReadString('\n') // blank line terminator

	b.Publish()

	event, data := readNextSSEEvent(rdr, b.maxWait+200*time.Millisecond)
	require.Equal(t, "snapshot", event,
		"expected event: snapshot (got %q, data %q)", event, data)
	require.NotEmpty(t, data, "data field must carry the timestamp the JS may eyeball")
}

// TestSSE_AuthRequired — an unauthenticated connect must 403 just
// like every other dashboard /api route. Pins the cookie + Origin
// gate; a regression here would expose the event stream to drive-by
// browser tabs.
func TestSSE_AuthRequired(t *testing.T) {
	withDashboardAuth(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/events", handleDashboardEvents)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/events", nil)
	require.NoError(t, err)
	// no cookie, no origin → must 403
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode,
		"unauthenticated SSE must 403")
}

// TestSSE_DebouncedFanout — a burst of Publishes on the broadcaster
// should arrive at the SSE subscriber as ONE event, not N. The full
// debounce + transport path, end to end. This is the visible
// behaviour the user actually cares about: a Claude turn fires a
// flurry of hook-callback writes, the dashboard sees one nudge.
func TestSSE_DebouncedFanout(t *testing.T) {
	withDashboardAuth(t)
	prevBcast := dashboardEvents
	b := newTestBroadcaster()
	dashboardEvents = b
	t.Cleanup(func() { dashboardEvents = prevBcast })

	mux := http.NewServeMux()
	mux.HandleFunc("/api/events", handleDashboardEvents)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	rdr, _, _ := connectSSE(t, srv)
	_, _ = rdr.ReadString('\n') // ": connected"
	_, _ = rdr.ReadString('\n') // blank

	// 20 Publishes inside the quiet window — should collapse to 1.
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			b.Publish()
		})
	}
	wg.Wait()

	event, _ := readNextSSEEvent(rdr, b.maxWait+200*time.Millisecond)
	require.Equal(t, "snapshot", event, "first coalesced event")

	// And there must NOT be a second one immediately — the burst
	// folded into a single fan-out.
	event2, _ := readNextSSEEvent(rdr, 200*time.Millisecond)
	require.Empty(t, event2,
		"a burst within the quiet window must produce exactly one SSE event (saw a second: %q)",
		event2)
}
