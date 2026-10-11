package agentd

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

type fakePeerReads struct {
	mu    sync.Mutex
	reqs  []fedPeerViewRequest
	reply func(n int, req fedPeerViewRequest) (fedPeerViewReply, error)
	clock time.Time
}

func (f *fakePeerReads) cache() *peerReadCache {
	return &peerReadCache{
		entries: map[peerReadKey]*peerReadEntry{},
		now:     func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.clock },
		exchange: func(_ context.Context, _ *fedRuntime, _ *db.FederationPeer, req fedPeerViewRequest) (fedPeerViewReply, error) {
			f.mu.Lock()
			f.reqs = append(f.reqs, req)
			n := len(f.reqs)
			f.mu.Unlock()
			return f.reply(n, req)
		},
	}
}

func (f *fakePeerReads) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.reqs) }
func (f *fakePeerReads) at(i int) fedPeerViewRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs[i]
}
func (f *fakePeerReads) advance(d time.Duration) {
	f.mu.Lock()
	f.clock = f.clock.Add(d)
	f.mu.Unlock()
}

func testRuntime(t *testing.T) *fedRuntime {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &fedRuntime{ctx: ctx, cancel: cancel}
}

func snapshotReply(body string) fedPeerViewReply {
	return fedPeerViewReply{Status: 200, Header: http.Header{"Etag": {`"v1"`}, "Content-Type": {"application/json"}}, Body: []byte(body)}
}

func TestPeerReadsServeEveryReaderFromOneConditionalRead(t *testing.T) {
	f := &fakePeerReads{clock: time.Unix(1000, 0)}
	f.reply = func(n int, req fedPeerViewRequest) (fedPeerViewReply, error) {
		if req.Header.Get("If-None-Match") == `"v1"` {
			return fedPeerViewReply{Status: http.StatusNotModified, Header: http.Header{"Etag": {`"v1"`}}}, nil
		}
		return snapshotReply(`{"groups":[]}`), nil
	}
	c := f.cache()
	rt := testRuntime(t)
	peer := &db.FederationPeer{InstanceID: "inst_b"}
	req := fedPeerViewRequest{Method: "GET", URI: "/api/snapshot", Header: http.Header{}}
	const every = 5 * time.Second

	first, err := c.read(context.Background(), rt, peer, req, every)
	require.NoError(t, err)
	require.Equal(t, `{"groups":[]}`, string(first.Body))
	f.advance(2 * time.Second)
	second, err := c.read(context.Background(), rt, peer, req, every)
	require.NoError(t, err)
	require.Equal(t, 1, f.count(), "a second dashboard within the interval reads the cache")
	require.Equal(t, "2", second.Header.Get("Age"))

	browser := req
	browser.Header = http.Header{"If-None-Match": {`"v1"`}}
	notModified, err := c.read(context.Background(), rt, peer, browser, every)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotModified, notModified.Status, "a browser holding the version gets a bodyless 304 here")
	require.Empty(t, notModified.Body)

	f.advance(5 * time.Second)
	again, err := c.read(context.Background(), rt, peer, req, every)
	require.NoError(t, err)
	require.Equal(t, 2, f.count(), "an expired copy is read again")
	require.Equal(t, `"v1"`, f.at(1).Header.Get("If-None-Match"), "conditionally")
	require.Equal(t, `{"groups":[]}`, string(again.Body), "a 304 keeps the body")
}

func TestPeerReadsServeFailuresAndExpireOnFederationChanges(t *testing.T) {
	f := &fakePeerReads{clock: time.Unix(1000, 0)}
	offline := errors.New("peer is offline")
	f.reply = func(n int, _ fedPeerViewRequest) (fedPeerViewReply, error) {
		if n == 2 {
			return fedPeerViewReply{}, offline
		}
		return snapshotReply(`{}`), nil
	}
	c := f.cache()
	rt := testRuntime(t)
	peer := &db.FederationPeer{InstanceID: "inst_b"}
	req := fedPeerViewRequest{Method: "GET", URI: "/api/snapshot", Header: http.Header{}}

	_, err := c.read(context.Background(), rt, peer, req, 5*time.Second)
	require.NoError(t, err)
	c.invalidate("inst_other")
	_, err = c.read(context.Background(), rt, peer, req, 5*time.Second)
	require.NoError(t, err)
	require.Equal(t, 1, f.count(), "another peer's change leaves this copy")

	c.invalidate("inst_b")
	_, err = c.read(context.Background(), rt, peer, req, 5*time.Second)
	require.ErrorIs(t, err, offline, "a failure is served as it came, so the dashboard marks the node")
	require.Equal(t, 2, f.count())
	_, err = c.read(context.Background(), rt, peer, req, 5*time.Second)
	require.ErrorIs(t, err, offline, "until the next read")

	c.invalidate("")
	_, err = c.read(context.Background(), rt, peer, req, 5*time.Second)
	require.NoError(t, err, "a local federation change expires everything")
	require.Equal(t, 3, f.count())
	require.Empty(t, f.at(2).Header.Get("If-None-Match"), "after a failure the read is unconditional")

	other := testRuntime(t)
	_, err = c.read(context.Background(), other, peer, req, 5*time.Second)
	require.NoError(t, err)
	require.Equal(t, 4, f.count(), "a replaced runtime starts afresh")
}

func TestPeerReadsRefreshWhileReadAndStopWhenIdle(t *testing.T) {
	f := &fakePeerReads{clock: time.Unix(1000, 0)}
	f.reply = func(int, fedPeerViewRequest) (fedPeerViewReply, error) { return snapshotReply(`{}`), nil }
	c := f.cache()
	rt := testRuntime(t)
	peer := &db.FederationPeer{InstanceID: "inst_b"}
	req := fedPeerViewRequest{Method: "GET", URI: "/api/snapshot", Header: http.Header{}}

	_, err := c.read(context.Background(), rt, peer, req, 20*time.Millisecond)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return f.count() >= 3 }, 2*time.Second, 5*time.Millisecond, "the refresher keeps it warm")

	f.advance(time.Minute)
	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.entries) == 0
	}, 2*time.Second, 5*time.Millisecond, "nobody reading: the refresher stops and drops it")
	settled := f.count()
	time.Sleep(60 * time.Millisecond)
	require.Equal(t, settled, f.count())

	_, err = c.read(context.Background(), rt, peer, req, 20*time.Millisecond)
	require.NoError(t, err)
	rt.cancel()
	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.entries) == 0
	}, 2*time.Second, 5*time.Millisecond, "federation stopping stops the refresher")
}

func TestPeerReadPollIntervalOnlyForDeclaredPolls(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet, "/api/peer/inst_b/snapshot", nil)
	r.SetPathValue("tail", "snapshot")
	require.Zero(t, peerReadPollInterval(r, nil), "undeclared reads are proxied as before")
	r.Header.Set(peerReadPollHeader, "5000")
	require.Equal(t, 5*time.Second, peerReadPollInterval(r, nil))
	r.Header.Set(peerReadPollHeader, "10")
	require.Equal(t, peerReadMinEvery, peerReadPollInterval(r, nil))
	r.SetPathValue("tail", "mailbox")
	require.Zero(t, peerReadPollInterval(r, nil), "only allowlisted reads")
	post, _ := http.NewRequest(http.MethodPost, "/api/peer/inst_b/snapshot", nil)
	post.SetPathValue("tail", "snapshot")
	post.Header.Set(peerReadPollHeader, "5000")
	require.Zero(t, peerReadPollInterval(post, nil))
}

func TestPeerSnapshotETagIgnoresItsTimestamp(t *testing.T) {
	respond := func(body, inm string) *peerViewResponse {
		out := &peerViewResponse{header: make(http.Header)}
		out.WriteHeader(200)
		_, _ = out.Write([]byte(body))
		r, _ := http.NewRequest(http.MethodGet, "/api/snapshot", nil)
		if inm != "" {
			r.Header.Set("If-None-Match", inm)
		}
		peerSnapshotETag(out, r)
		return out
	}
	a := respond(`{"agents":[],"generated_at":"2026-10-11T10:00:00.1Z","groups":[]}`, "")
	b := respond(`{"agents":[],"generated_at":"2026-10-11T10:00:05.2Z","groups":[]}`, "")
	require.Equal(t, a.Header().Get("ETag"), b.Header().Get("ETag"), "a rebuild of the same projection keeps its tag")
	require.NotEqual(t, a.Header().Get("ETag"), respond(`{"agents":[{}],"generated_at":"x","groups":[]}`, "").Header().Get("ETag"))
	same := respond(`{"agents":[],"generated_at":"2026-10-11T10:00:09Z","groups":[]}`, a.Header().Get("ETag"))
	require.Equal(t, http.StatusNotModified, same.statusCode())
	require.Zero(t, same.body.Len())
}

func TestPeerReadsInvalidationDuringAReadIsNotLost(t *testing.T) {
	f := &fakePeerReads{clock: time.Unix(1000, 0)}
	release := make(chan struct{})
	var once sync.Once
	var c *peerReadCache
	f.reply = func(n int, _ fedPeerViewRequest) (fedPeerViewReply, error) {
		if n == 2 {
			once.Do(func() { c.invalidate("inst_b") })
			<-release
		}
		return snapshotReply(`{}`), nil
	}
	c = f.cache()
	rt := testRuntime(t)
	peer := &db.FederationPeer{InstanceID: "inst_b"}
	req := fedPeerViewRequest{Method: "GET", URI: "/api/snapshot", Header: http.Header{}}
	_, err := c.read(context.Background(), rt, peer, req, 5*time.Second)
	require.NoError(t, err)

	c.invalidate("inst_b")
	close(release)
	reply, err := c.read(context.Background(), rt, peer, req, 5*time.Second)
	require.NoError(t, err)
	require.Equal(t, 3, f.count(), "a read invalidated in flight is read once more")
	require.Equal(t, "0", reply.Header.Get("Age"))
}
