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

// peerReads keeps the dashboard's peer polls warm (tcl-qamqg7). Fused Groups
// and the Map poll every ticked peer from every open dashboard; each poll used
// to be a full peer-view exchange through the hub (a sealed open and answer,
// then a relayed stream), and a peer snapshot is tens of KB. Here one
// background refresher per peer and URL serves every dashboard and tab, and
// asks conditionally, so an unchanged peer answers 304 without a body.
//
// Only reads a dashboard poll opts into are cached: it declares its interval
// in peerReadPollHeader, which also sets how often the refresher reads. The
// refresher stops once nobody has read for a while, so nothing runs without an
// open dashboard, and it lives on the federation runtime, so nothing runs
// without federation. Failures are served as they came, so the dashboard's
// offline and stale handling is unchanged. Federation changes expire entries
// so the next read asks again: this node's catalog broadcasts (trust, grants,
// group shares) expire all, a peer's inbound catalog expires that peer's. The
// proxy still rechecks trust on each request, so nothing outlives a
// revocation, and a replaced runtime starts its entries afresh.

const peerReadPollHeader = "X-Tclaude-Poll-Ms"

const (
	peerReadMinEvery = 2 * time.Second
	peerReadMaxEvery = time.Minute
	peerReadIdleMin  = 30 * time.Second
)

// peerReadTails are the peer GETs a poll may cache: idempotent reads whose
// reply depends only on the peer's authority over this node.
var peerReadTails = map[string]bool{"snapshot": true, "node-summary": true}

// peerReadPollInterval is the declared poll interval of a cacheable read, or
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
	every    time.Duration    // shortest interval declared by a recent reader
	everyAt  time.Time        // last read declaring every
	lastRead time.Time
	running  bool          // a refresher owns this entry
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
		}
	}
}

func peerReadIdle(every time.Duration) time.Duration {
	return max(peerReadIdleMin, 3*every)
}

// read serves req from the cache, reading the peer first when the cached
// outcome is older than every (or absent).
func (c *peerReadCache) read(ctx context.Context, rt *fedRuntime, peer *db.FederationPeer, req fedPeerViewRequest, every time.Duration) (fedPeerViewReply, error) {
	key := peerReadKey{peer.InstanceID, req.URI}
	c.mu.Lock()
	e := c.entries[key]
	if e == nil || e.rt != rt {
		e = &peerReadEntry{rt: rt, uri: req.URI}
		c.entries[key] = e
	}
	now := c.now()
	e.peer, e.lastRead = peer, now
	// The shortest interval wins; a longer one takes over once the reader
	// that asked for the shorter has gone quiet.
	if e.every == 0 || every <= e.every || now.Sub(e.everyAt) > 2*e.every {
		e.every, e.everyAt = every, now
	}
	if e.at.IsZero() || now.Sub(e.at) > e.every+time.Second {
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
	}
	// The refresher ends with the runtime's context; it holds nothing a
	// runtime stop would have to wait for.
	if !e.running && c.entries[key] == e && rt.ctx.Err() == nil {
		e.running = true
		go c.refresh(key, e)
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
	reply.Header.Set("Age", strconv.Itoa(int(c.now().Sub(at)/time.Second)))
	return reply, nil
}

// startRead reads e's peer in the background; callers wait on e.inflight.
// The read is not tied to any one request: a dashboard that goes away does
// not fail the others waiting on it. Called with c.mu held.
func (c *peerReadCache) startRead(e *peerReadEntry) {
	done := make(chan struct{})
	e.inflight = done
	req := fedPeerViewRequest{Method: http.MethodGet, URI: e.uri, Header: http.Header{}}
	if tag := e.reply.Header.Get("ETag"); tag != "" && e.err == nil {
		req.Header.Set("If-None-Match", tag)
	}
	rt, peer := e.rt, e.peer
	go func() {
		reply, err := c.exchange(context.Background(), rt, peer, req)
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
		e.at = c.now()
		e.inflight = nil
		close(done)
	}()
}

// refresh keeps e warm while it is read, reading the peer every e.every.
func (c *peerReadCache) refresh(key peerReadKey, e *peerReadEntry) {
	for {
		c.mu.Lock()
		every := e.every
		c.mu.Unlock()
		timer := time.NewTimer(every)
		select {
		case <-e.rt.ctx.Done():
			timer.Stop()
			c.mu.Lock()
			e.running = false
			if c.entries[key] == e {
				delete(c.entries, key)
			}
			c.mu.Unlock()
			return
		case <-timer.C:
		}
		c.mu.Lock()
		if c.entries[key] != e || c.now().Sub(e.lastRead) > peerReadIdle(e.every) {
			e.running = false
			if c.entries[key] == e {
				delete(c.entries, key)
			}
			c.mu.Unlock()
			return
		}
		if e.inflight == nil {
			c.startRead(e)
		}
		c.mu.Unlock()
	}
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
