package agentd

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// peerReads coalesces the dashboard's peer polls (tcl-qamqg7). Fused Groups
// and the Map poll every ticked peer from every open dashboard; each poll used
// to be a full peer-view exchange through the hub (a sealed open and answer,
// then a relayed stream), and a peer snapshot is tens of KB. Here a poll reads
// through: polls that arrive within peerReadFresh of the last read share it
// (concurrent ones wait on the one in flight), and every read asks the peer
// conditionally, so an unchanged peer answers 304 without a body. There is no
// background refresher: nothing runs while nobody polls, and what a dashboard
// shows is at most one of its own intervals old, as without the cache.
//
// Only reads a dashboard poll opts into are coalesced: it declares its
// interval in peerReadPollHeader. Failures are served as they came, so the
// dashboard's offline and stale handling is unchanged. Federation changes
// expire entries so the next read asks again: this node's catalog broadcasts
// (trust, grants, group shares) expire all, a peer's inbound catalog expires
// that peer's. The proxy still rechecks trust on each request, so nothing
// outlives a revocation, and a replaced runtime starts its entries afresh.

const peerReadPollHeader = "X-Tclaude-Poll-Ms"

const (
	peerReadMinEvery = 2 * time.Second
	peerReadMaxEvery = time.Minute
	// peerReadFresh is how long a read is shared with later polls.
	peerReadFresh = time.Second
	// peerReadIdle drops an entry (and its ETag) nobody has polled for a while.
	peerReadIdle = 2 * time.Minute
)

// peerReadTails are the peer GETs a poll may share: idempotent reads whose
// reply depends only on the peer's authority over this node.
var peerReadTails = map[string]bool{"snapshot": true, "node-summary": true}

// peerReadPollInterval is the declared poll interval of a shareable read, or
// zero when the request is not one.
func peerReadPollInterval(r *http.Request, body []byte) time.Duration {
	if r.Method != http.MethodGet || len(body) != 0 || !peerReadTails[r.PathValue("tail")] {
		return 0
	}
	ms, err := strconv.Atoi(r.Header.Get(peerReadPollHeader))
	if err != nil || ms <= 0 {
		return 0
	}
	return min(max(time.Duration(ms)*time.Millisecond, peerReadMinEvery), peerReadMaxEvery)
}

type peerReadKey struct{ peer, uri string }

type peerReadEntry struct {
	rt       *fedRuntime
	peer     *db.FederationPeer
	uri      string
	reply    fedPeerViewReply // last reply; its ETag makes the next read conditional
	err      error            // last failure, served until a read succeeds
	at       time.Time        // when the outcome was last confirmed
	gen      uint64           // bumped by invalidate: a read begun earlier lands expired
	lastRead time.Time
	inflight chan struct{} // closed when the current read completes
}

type peerReadCache struct {
	mu       sync.Mutex
	entries  map[peerReadKey]*peerReadEntry
	now      func() time.Time
	exchange func(context.Context, *fedRuntime, *db.FederationPeer, fedPeerViewRequest) (fedPeerViewReply, error)
}

var peerReads = &peerReadCache{entries: map[peerReadKey]*peerReadEntry{}, now: time.Now, exchange: exchangePeerView}

// invalidate expires every entry (peer == "") or one peer's: the next read
// asks the peer again, still conditionally, so an unchanged peer costs a 304.
func (c *peerReadCache) invalidate(peer string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, e := range c.entries {
		if peer == "" || key.peer == peer {
			e.at = time.Time{}
			e.gen++
		}
	}
}

// read serves req, reading the peer first unless a read completed within
// peerReadFresh.
func (c *peerReadCache) read(ctx context.Context, rt *fedRuntime, peer *db.FederationPeer, req fedPeerViewRequest) (fedPeerViewReply, error) {
	key := peerReadKey{peer.InstanceID, req.URI}
	c.mu.Lock()
	now := c.now()
	c.sweep(now)
	e := c.entries[key]
	if e == nil || e.rt != rt {
		e = &peerReadEntry{rt: rt, uri: req.URI}
		c.entries[key] = e
	}
	e.peer, e.lastRead = peer, now
	// Twice at most: a read invalidated while in flight is read once more.
	for try := 0; try < 2 && (e.at.IsZero() || now.Sub(e.at) > peerReadFresh); try++ {
		if e.inflight == nil {
			c.startRead(e)
		}
		wait := e.inflight
		c.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return fedPeerViewReply{}, ctx.Err()
		}
		c.mu.Lock()
		now = c.now()
	}
	reply, err, at := e.reply, e.err, e.at
	c.mu.Unlock()
	if err != nil {
		return fedPeerViewReply{}, err
	}
	reply.Header = reply.Header.Clone()
	if reply.Header == nil {
		reply.Header = http.Header{}
	}
	// A browser holding the current version gets a bodyless 304 from here.
	if tag := reply.Header.Get("ETag"); tag != "" && reply.Status == http.StatusOK && etagMatches(req.Header.Get("If-None-Match"), tag) {
		reply = fedPeerViewReply{Status: http.StatusNotModified, Header: http.Header{"Etag": {tag}}}
	}
	age := time.Duration(0)
	if !at.IsZero() {
		age = c.now().Sub(at)
	}
	reply.Header.Set("Age", strconv.Itoa(int(age/time.Second)))
	return reply, nil
}

// sweep drops entries nobody has polled for peerReadIdle, and entries of a
// runtime that has stopped. Called with c.mu held.
func (c *peerReadCache) sweep(now time.Time) {
	for key, e := range c.entries {
		if e.inflight == nil && (now.Sub(e.lastRead) > peerReadIdle || e.rt.ctx.Err() != nil) {
			delete(c.entries, key)
		}
	}
}

// startRead reads e's peer; callers wait on e.inflight. The read is not tied
// to any one request, so a dashboard that goes away does not fail the others
// waiting on it; it ends with the federation runtime. Called with c.mu held.
func (c *peerReadCache) startRead(e *peerReadEntry) {
	done := make(chan struct{})
	e.inflight = done
	req := fedPeerViewRequest{Method: http.MethodGet, URI: e.uri, Header: http.Header{}}
	if tag := e.reply.Header.Get("ETag"); tag != "" && e.err == nil {
		req.Header.Set("If-None-Match", tag)
	}
	rt, peer, gen := e.rt, e.peer, e.gen
	go func() {
		reply, err := c.exchange(rt.ctx, rt, peer, req)
		c.mu.Lock()
		defer c.mu.Unlock()
		switch {
		case err != nil:
			e.err = err
		case reply.Status == http.StatusNotModified && e.reply.Status == http.StatusOK:
			e.err = nil
		default:
			e.reply, e.err = reply, nil
		}
		// An invalidation during the read leaves its result expired, so the
		// next read asks again rather than serving it as fresh.
		if e.gen == gen {
			e.at = c.now()
		}
		e.inflight = nil
		close(done)
	}()
}

// etagMatches reports whether an If-None-Match header names tag (weak
// validators compare by their opaque tag, as for the node summary).
func etagMatches(header, tag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(candidate), "W/"))
		if candidate == tag || candidate == "*" {
			return true
		}
	}
	return false
}
