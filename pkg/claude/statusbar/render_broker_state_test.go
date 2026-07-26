package statusbar

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
)

// brokeredHostState composes the change gate, the read TTL and the
// fail-soft branch, and it is where the properties that keep a
// several-times-a-second render off the socket actually live. Testing the
// pieces in isolation left those properties asserted only by comments —
// in particular "the digest is deliberately NOT advanced here", which is
// what stops a failure from becoming permanent data loss.
//
// These stand a real daemon up on a unix socket at the path the client
// resolves, so the whole round trip runs: routing, digest, cache, HTTP,
// response handling.

type fakeDaemon struct {
	t        *testing.T
	requests []BrokeredRenderRequest
	respond  func(BrokeredRenderRequest) (int, BrokeredRenderResponse)
}

// serveFakeDaemon listens on the agentd socket path the client will dial,
// so postRenderToDaemon is exercised rather than stubbed.
func serveFakeDaemon(t *testing.T, d *fakeDaemon) {
	t.Helper()
	// A unix socket path is capped at ~108 bytes, well under what a Go
	// test's temp dir can reach, so use the supported explicit override
	// with a short base rather than the derived ~/.tclaude/api path.
	sockDir, err := os.MkdirTemp("/tmp", "tclsb")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "a.sock")
	t.Setenv(agentipc.SocketEnv, sock)

	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req BrokeredRenderRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		d.requests = append(d.requests, req)
		code, resp := d.respond(req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		require.NoError(t, json.NewEncoder(w).Encode(resp))
	}))
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
}

func okDaemon(BrokeredRenderRequest) (int, BrokeredRenderResponse) {
	return http.StatusOK, BrokeredRenderResponse{Owned: true, Applied: true, PinnedWindow: 450000}
}

func sampleRender() renderRequest {
	return renderRequest{
		EnvSessionID: "spwn-state",
		RenderConvID: "conv-state",
		Payload:      []byte(`{"session_id":"conv-state","model":{"display_name":"Opus 5"}}`),
	}
}

// The whole point of the change gate: an idle agent whose payload has not
// moved costs no traffic at all. Without this a wrapped agent would put a
// socket round trip on a display refresh, several times a second, forever.
func TestBrokeredHostState_UnchangedRenderSendsNothing(t *testing.T) {
	withTempCacheDir(t)
	d := &fakeDaemon{t: t, respond: okDaemon}
	serveFakeDaemon(t, d)

	req := sampleRender()
	facts := brokeredHostState(req)
	require.Len(t, d.requests, 1, "the first render has nothing cached, so it goes out")
	assert.True(t, d.requests[0].ApplyWrites)
	assert.EqualValues(t, 450000, facts.PinnedWindow)

	again := brokeredHostState(req)
	assert.Len(t, d.requests, 1,
		"an identical render must record nothing and read from the cache")
	assert.EqualValues(t, 450000, again.PinnedWindow,
		"the cached reads still have to reach the bar")
}

// A changed payload goes out IMMEDIATELY, with no interval in front of
// it. The pre-compact guard judges from the context snapshot this write
// carries, so a rate discipline here would hand it stale evidence.
func TestBrokeredHostState_ChangedRenderGoesOutImmediately(t *testing.T) {
	withTempCacheDir(t)
	d := &fakeDaemon{t: t, respond: okDaemon}
	serveFakeDaemon(t, d)

	req := sampleRender()
	brokeredHostState(req)
	require.Len(t, d.requests, 1)

	// The tokens ticked: a different payload, same instant.
	changed := req
	changed.Payload = []byte(`{"session_id":"conv-state","model":{"display_name":"Opus 5"},"context_window":{"used_percentage":41}}`)
	brokeredHostState(changed)

	require.Len(t, d.requests, 2, "a changed payload must not wait for any interval")
	assert.True(t, d.requests[1].ApplyWrites)
}

// The daemon can answer 200 and still have failed to write — a busy
// SQLite file, say. The change gate removes the direct path's automatic
// retry, so a failure the daemon merely logged would cost the snapshot
// until the agent's next token tick. Applied is what restores the retry.
func TestBrokeredHostState_UnappliedWritesAreRetried(t *testing.T) {
	withTempCacheDir(t)
	failing := true
	d := &fakeDaemon{t: t, respond: func(BrokeredRenderRequest) (int, BrokeredRenderResponse) {
		if failing {
			return http.StatusOK, BrokeredRenderResponse{Owned: true, Applied: false}
		}
		return okDaemon(BrokeredRenderRequest{})
	}}
	serveFakeDaemon(t, d)

	req := sampleRender()
	brokeredHostState(req)
	require.Len(t, d.requests, 1)

	failing = false
	brokeredHostState(req)
	require.Len(t, d.requests, 2,
		"a render the daemon could not record must be re-sent, not assumed landed")
	assert.True(t, d.requests[1].ApplyWrites, "and re-sent as a WRITE, not a reads-only refresh")

	brokeredHostState(req)
	assert.Len(t, d.requests, 2,
		"once it lands, the gate closes again")
}

// A transport failure must not advance the digest either — but it also
// must not turn into a round trip per render. An agent the daemon can
// never place would otherwise open a socket, be refused, and log, several
// times a second for the rest of its life.
func TestBrokeredHostState_RefusalBacksOffButStillRetriesTheWrite(t *testing.T) {
	withTempCacheDir(t)
	d := &fakeDaemon{t: t, respond: func(BrokeredRenderRequest) (int, BrokeredRenderResponse) {
		return http.StatusForbidden, BrokeredRenderResponse{}
	}}
	serveFakeDaemon(t, d)

	req := sampleRender()
	for range 5 {
		facts := brokeredHostState(req)
		assert.Empty(t, facts.Owned, "a refused render must claim no write authority")
	}
	assert.Len(t, d.requests, 1,
		"a refusal must back off to the read TTL, not cost a round trip per render")

	// Past the backoff the write is retried — the digest was never
	// advanced, so the obligation outlived the suppression.
	c := loadRenderCache(req.EnvSessionID)
	require.NotNil(t, c)
	c.ReadsAt = time.Now().Add(-2 * renderReadTTL)
	c.FailedAt = time.Now().Add(-2 * renderRetryBackoff)
	saveRenderCache(req.EnvSessionID, *c)

	brokeredHostState(req)
	require.Len(t, d.requests, 2)
	assert.True(t, d.requests[1].ApplyWrites,
		"the retry must still carry the writes; a refusal is not an acknowledgement")
}

// An unreachable daemon must never take the pane down with it, and must
// leave the write owed rather than believed-landed.
func TestBrokeredHostState_UnreachableDaemonIsSoftAndOwesTheWrite(t *testing.T) {
	withTempCacheDir(t)
	t.Setenv(agentipc.SocketEnv, "/tmp/tclaude-no-such-daemon.sock")

	req := sampleRender()
	facts := brokeredHostState(req)
	assert.Empty(t, facts.Owned)
	assert.Zero(t, facts.PinnedWindow, "no facts, but also no panic and no error")

	c := loadRenderCache(req.EnvSessionID)
	require.NotNil(t, c, "the attempt is recorded so the retry backs off")
	assert.Empty(t, c.Digest,
		"nothing was acknowledged, so nothing may be treated as recorded")
}

// Cosmetic reads coast on the TTL, but the write gate is independent of
// it: a render that changed nothing still refreshes its reads once the
// TTL lapses, and it does so WITHOUT re-sending writes.
func TestBrokeredHostState_StaleReadsRefreshWithoutResendingWrites(t *testing.T) {
	withTempCacheDir(t)
	d := &fakeDaemon{t: t, respond: okDaemon}
	serveFakeDaemon(t, d)

	req := sampleRender()
	brokeredHostState(req)
	require.Len(t, d.requests, 1)

	c := loadRenderCache(req.EnvSessionID)
	require.NotNil(t, c)
	c.ReadsAt = time.Now().Add(-2 * renderReadTTL)
	saveRenderCache(req.EnvSessionID, *c)

	brokeredHostState(req)
	require.Len(t, d.requests, 2, "expired reads must be refreshed")
	assert.False(t, d.requests[1].ApplyWrites,
		"but an unchanged payload must not re-record what is already there")
}

// The usage fallback is asked for only when the payload carried no
// buckets of its own, and a cached answer that never contained usage must
// not be mistaken for one that did.
func TestBrokeredHostState_AsksForUsageOnlyWhenTheRenderNeedsIt(t *testing.T) {
	withTempCacheDir(t)
	d := &fakeDaemon{t: t, respond: okDaemon}
	serveFakeDaemon(t, d)

	req := sampleRender()
	req.WantUsage = true
	brokeredHostState(req)
	require.Len(t, d.requests, 1)
	assert.True(t, d.requests[0].WantUsage)

	// The daemon answered without usage. An unchanged render must ask
	// again rather than coast on an answer that never carried it.
	brokeredHostState(req)
	assert.Len(t, d.requests, 2,
		"a cached response with no usage cannot serve a render that needs usage")
}
