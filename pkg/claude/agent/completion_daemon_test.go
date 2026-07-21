package agent

import (
	"net"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc/agentipctest"
)

// serveCompletionSocket stands up a real Unix-socket HTTP server at a
// temp path and points the client's socket lookup at it, so the
// completion fallback exercises its actual transport rather than a stub.
func serveCompletionSocket(t *testing.T, h http.Handler) {
	t.Helper()
	// A Unix socket path is length-limited (~104 bytes) and t.TempDir()
	// embeds the test name; ShortSocketDir finds a base that fits.
	sock := filepath.Join(agentipctest.ShortSocketDir(t), "s.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err, "listen on %s", sock)

	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	t.Setenv(agentipc.SocketEnv, sock)
}

// The conv-selector completion fallback (TCL-611) asks the daemon for
// peers when the local index is unreadable. It must filter by the typed
// prefix, suppress duplicate conv prefixes, and render titles as
// completion descriptions.
func TestConvSelectorsFromDaemon_FiltersAndDedupes(t *testing.T) {
	serveCompletionSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/peers", r.URL.Path, "fallback reads the peers endpoint")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"conv_id":"aaaa1111-2222-3333-4444-555566667777","title":"sandbox-lead"},
			{"conv_id":"aaaa1111-9999-8888-7777-666655554444","title":"same-prefix-twin"},
			{"conv_id":"bbbb2222-3333-4444-5555-666677778888","title":"other-agent"},
			{"conv_id":"short","title":"too-short-to-complete"},
			{"conv_id":"cccc3333-4444-5555-6666-777788889999","title":""}
		]`))
	}))

	t.Run("prefix filters the candidate set", func(t *testing.T) {
		out := convSelectorsFromDaemon("aaaa")
		require.Len(t, out, 1, "one entry survives the prefix filter and the dedupe: %v", out)
		assert.Equal(t, "aaaa1111\tsandbox-lead", out[0],
			"completion is <prefix>\\t<title>, first match winning the shared prefix")
	})

	t.Run("empty prefix offers every distinct conv prefix", func(t *testing.T) {
		out := convSelectorsFromDaemon("")
		assert.ElementsMatch(t,
			[]string{"aaaa1111\tsandbox-lead", "bbbb2222\tother-agent", "cccc3333"},
			out,
			"duplicate prefixes collapse, a short conv-id is skipped, and an untitled conv completes bare")
	})

	t.Run("a non-matching prefix completes nothing", func(t *testing.T) {
		assert.Empty(t, convSelectorsFromDaemon("zzzz"), "no candidate starts with zzzz")
	})
}

// Completion runs on every <tab>: a daemon that is down, erroring, or
// serving garbage must complete nothing, never surface an error.
func TestConvSelectorsFromDaemon_FailsSilently(t *testing.T) {
	t.Run("no daemon listening", func(t *testing.T) {
		t.Setenv(agentipc.SocketEnv, filepath.Join(agentipctest.ShortSocketDir(t), "absent.sock"))
		assert.Empty(t, convSelectorsFromDaemon(""), "an unreachable daemon completes nothing")
	})

	t.Run("daemon refuses the request", func(t *testing.T) {
		serveCompletionSocket(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"code":"auth","error":"unidentified caller"}`, http.StatusForbidden)
		}))
		assert.Empty(t, convSelectorsFromDaemon(""), "a refused read completes nothing")
	})

	t.Run("daemon returns undecodable body", func(t *testing.T) {
		serveCompletionSocket(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"not":"an array"}`))
		}))
		assert.Empty(t, convSelectorsFromDaemon(""), "a malformed body completes nothing")
	})
}
