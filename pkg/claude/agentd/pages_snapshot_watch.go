package agentd

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// `tclaude agent page snapshot --watch` (tk_83873896): rather than tclaude
// guessing when a page is ready, the agent watches it settle and decides.
// The daemon opens the page once and streams one line per tick: the time,
// requests in flight, DOM mutations, hashes of the visible text, layout and
// pixels, the title, and what changed. A tick may carry a frame. The watch
// ends with a final capture of the page as it is then: at --for, or when the
// agent asks (`page snapshot --session ID`, or a line on the watch's stdin).
// The capture is of the same open page the agent saw go still, never a
// fresh load.

const (
	pageWatchDefaultEvery = 500 * time.Millisecond
	pageWatchMinEvery     = 100 * time.Millisecond
	pageWatchMaxEvery     = 5 * time.Second
	pageWatchDefaultFor   = 10 * time.Second
	pageWatchMaxFor       = 60 * time.Second // the hard cap: a page that never settles cannot hold a browser longer
	pageWatchMaxFrames    = 120
	pageWatchMinScale     = 0.1
)

type pageWatchRequest struct {
	pageSnapshotRequest
	EveryMS int64 `json:"every_ms"`
	ForMS   int64 `json:"for_ms"`
	// FrameEvery sends a frame with every Nth tick (0: none).
	FrameEvery int     `json:"frame_every"`
	FrameScale float64 `json:"frame_scale"` // 0 < scale <= 1 (0: 1)
}

// pageWatchEvent is one line of a watch's stream.
type pageWatchEvent struct {
	// Type is start, tick, final (the watch ran its time and captured),
	// end (the agent's capture request took the final capture) or error.
	Type      string `json:"type"`
	Session   string `json:"session,omitempty"`
	N         int    `json:"n,omitempty"`
	ElapsedMS int64  `json:"elapsed_ms"`

	// start
	EveryMS  int64 `json:"every_ms,omitempty"`
	ForMS    int64 `json:"for_ms,omitempty"`
	Width    int   `json:"width,omitempty"`
	Height   int   `json:"height,omitempty"`
	MarksSeq int64 `json:"marks_seq,omitempty"`
	Patches  int   `json:"patches,omitempty"`

	// tick
	Loaded     bool     `json:"loaded,omitempty"`
	ReadyState string   `json:"ready_state,omitempty"`
	Path       string   `json:"path,omitempty"`
	Title      string   `json:"title,omitempty"`
	InFlight   int      `json:"in_flight"`
	LongLived  int      `json:"long_lived,omitempty"`
	Requests   int      `json:"requests"`
	Mutations  int      `json:"mutations"`
	TextHash   string   `json:"text_hash,omitempty"`
	TextLen    int      `json:"text_len"`
	LayoutHash string   `json:"layout_hash,omitempty"`
	PixelHash  string   `json:"pixel_hash,omitempty"`
	Changed    []string `json:"changed"`
	StillMS    int64    `json:"still_ms"`
	Frame      string   `json:"frame,omitempty"` // a PNG, base64
	Note       string   `json:"note,omitempty"`

	// final
	PNG string `json:"png,omitempty"`

	// end, error
	Reason string `json:"reason,omitempty"`
	Code   string `json:"code,omitempty"`
	Error  string `json:"error,omitempty"`
}

// pageWatch is one running watch, reachable by its session id from the
// agent that started it.
type pageWatch struct {
	id     string
	caller string
	pageID string
	resp   pageSnapshotResponse // what a capture answers with, but the image
	reqs   chan pageCaptureReq
	done   chan struct{}
}

type pageCaptureReq struct {
	keep  bool // a frame, and the watch goes on; else the final capture, which ends it
	reply chan pageCaptureReply
}

type pageCaptureReply struct {
	png     []byte
	elapsed time.Duration
	still   time.Duration
	err     error
}

var pageWatches = struct {
	mu sync.Mutex
	by map[string]*pageWatch
}{by: map[string]*pageWatch{}}

func newPageWatchID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "w" + hex.EncodeToString(b[:])
}

func clampDuration(ms int64, def, lo, hi time.Duration) time.Duration {
	if ms <= 0 {
		return def
	}
	return min(max(time.Duration(ms)*time.Millisecond, lo), hi)
}

// POST /v1/pages/{name}/snapshot/watch — {seq?, no_marks?, every_ms?,
// for_ms?, frame_every?, frame_scale?}: an NDJSON stream of pageWatchEvent.
func handlePageSnapshotWatch(w http.ResponseWriter, r *http.Request) {
	var req pageWatchRequest
	t, ok := pageSnapshotPrepare(w, r, &req)
	if !ok {
		return
	}
	if req.FrameEvery < 0 || req.FrameScale < 0 || req.FrameScale > 1 || (req.FrameScale > 0 && req.FrameScale < pageWatchMinScale) {
		writeError(w, http.StatusBadRequest, "invalid_arg", fmt.Sprintf("frame_every is 0 or more, and frame_scale is from %.1f to 1", pageWatchMinScale))
		return
	}
	every := clampDuration(req.EveryMS, pageWatchDefaultEvery, pageWatchMinEvery, pageWatchMaxEvery)
	watchFor := clampDuration(req.ForMS, pageWatchDefaultFor, time.Second, pageWatchMaxFor)
	scale := req.FrameScale
	if scale == 0 {
		scale = 1
	}
	release, ok := pageSnapshotAcquire(w, t.owner)
	if !ok {
		return
	}
	defer release()

	watch := &pageWatch{id: newPageWatchID(), caller: t.caller, pageID: t.job.pageID, resp: t.resp, reqs: make(chan pageCaptureReq), done: make(chan struct{})}
	pageWatches.mu.Lock()
	pageWatches.by[watch.id] = watch
	pageWatches.mu.Unlock()
	defer func() {
		pageWatches.mu.Lock()
		delete(pageWatches.by, watch.id)
		pageWatches.mu.Unlock()
		close(watch.done)
	}()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	flusher, _ := w.(http.Flusher)
	start := time.Now()
	emit := func(ev pageWatchEvent) bool {
		if ev.Type != "start" {
			ev.ElapsedMS = time.Since(start).Milliseconds()
		}
		if enc.Encode(ev) != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	fail := func(err error) {
		code := "snapshot_failed"
		switch {
		case errors.Is(err, errNoChrome):
			code = "no_chrome"
		case errors.Is(err, errPageLeftOrigin):
			code = "left_origin"
		}
		emit(pageWatchEvent{Type: "error", Code: code, Error: err.Error()})
	}
	if !emit(pageWatchEvent{Type: "start", Session: watch.id, EveryMS: every.Milliseconds(), ForMS: watchFor.Milliseconds(),
		Width: t.job.Width, Height: t.job.Height, Path: t.resp.Path, MarksSeq: t.resp.MarksSeq, Patches: t.resp.Patches}) {
		return
	}

	// The watch, then at most a load's worth of time for the final capture.
	ctx, cancel := context.WithTimeout(r.Context(), watchFor+pageSnapshotTimeout)
	defer cancel()
	b, err := pageSnapshotOpen(ctx, t.job)
	if err != nil {
		fail(err)
		return
	}
	defer b.Close()

	var track pageChangeTracker
	frames := 0
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for n := 1; ; n++ {
		at := time.Since(start)
		if at >= watchFor {
			png, err := b.Final(ctx)
			if err != nil {
				fail(err)
				return
			}
			emit(pageWatchEvent{Type: "final", Reason: "for", StillMS: track.still(at).Milliseconds(), PNG: base64.StdEncoding.EncodeToString(png)})
			return
		}
		p, err := b.Probe(ctx, true)
		if err != nil {
			fail(err)
			return
		}
		at = time.Since(start)
		ev := pageWatchEvent{Type: "tick", N: n, Loaded: p.Loaded, ReadyState: p.ReadyState, Path: p.Path, Title: p.Title,
			InFlight: p.InFlight, LongLived: p.LongLived, Requests: p.Requests, Mutations: p.Mutations,
			TextHash: p.TextHash, TextLen: p.TextLen, LayoutHash: p.Layout, PixelHash: p.Pixels, Note: p.Note}
		ev.Changed = track.observe(p, at)
		if ev.Changed == nil {
			ev.Changed = []string{}
		}
		ev.StillMS = track.still(at).Milliseconds()
		if req.FrameEvery > 0 && (n-1)%req.FrameEvery == 0 && p.Note == "" {
			if frames < pageWatchMaxFrames {
				if png, err := b.Frame(ctx, scale); err == nil {
					ev.Frame = base64.StdEncoding.EncodeToString(png)
					frames++
				} else if errors.Is(err, errPageLeftOrigin) {
					fail(err)
					return
				} else {
					ev.Note = "no frame: " + err.Error()
				}
			} else if frames == pageWatchMaxFrames {
				ev.Note = fmt.Sprintf("no more frames: a watch sends at most %d", pageWatchMaxFrames)
				frames++
			}
		}
		if !emit(ev) {
			return
		}
		// Between ticks the agent may ask for a capture.
		for next := false; !next; {
			select {
			case <-ticker.C:
				next = true
			case <-ctx.Done():
				fail(ctx.Err())
				return
			case c := <-watch.reqs:
				at := time.Since(start)
				reply := pageCaptureReply{elapsed: at, still: track.still(at)}
				if c.keep {
					reply.png, reply.err = b.Frame(ctx, 1)
					c.reply <- reply
					if errors.Is(reply.err, errPageLeftOrigin) {
						fail(reply.err)
						return
					}
					continue
				}
				reply.png, reply.err = b.Final(ctx)
				c.reply <- reply
				if reply.err != nil {
					fail(reply.err)
					return
				}
				emit(pageWatchEvent{Type: "end", Reason: "captured", StillMS: reply.still.Milliseconds()})
				return
			}
		}
	}
}

type pageCaptureRequest struct {
	Keep bool `json:"keep"`
}

// POST /v1/pages/{name}/snapshot/watch/{session}/capture — {keep?}: a PNG of
// the watched page as it is now. Without keep it is the final capture (with
// the patches and marks) and ends the watch; with keep it is a frame and the
// watch goes on.
func handlePageSnapshotCapture(w http.ResponseWriter, r *http.Request) {
	p, _, caller, ok := pageForAs(w, r, PermGroupsPagesRead)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, pageSnapshotMaxBody)
	var req pageCaptureRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid request: "+err.Error())
		return
	}
	pageWatches.mu.Lock()
	watch := pageWatches.by[r.PathValue("session")]
	pageWatches.mu.Unlock()
	livePages.mu.Lock()
	pageID := p.id
	livePages.mu.Unlock()
	if watch == nil || watch.caller != caller || watch.pageID != pageID {
		writeError(w, http.StatusNotFound, "not_found", "no watch of this page with that session is running (it may have ended: a watch ends at --for, or at its first capture)")
		return
	}
	c := pageCaptureReq{keep: req.Keep, reply: make(chan pageCaptureReply, 1)}
	select {
	case watch.reqs <- c:
	case <-watch.done:
		writeError(w, http.StatusNotFound, "not_found", "that watch has ended")
		return
	case <-r.Context().Done():
		return
	}
	var reply pageCaptureReply
	select {
	case reply = <-c.reply:
	case <-watch.done:
		// It may have answered as it ended.
		select {
		case reply = <-c.reply:
		default:
			writeError(w, http.StatusNotFound, "not_found", "the watch ended before it captured")
			return
		}
	case <-r.Context().Done():
		return
	}
	if reply.err != nil {
		writePageSnapshotError(w, reply.err)
		return
	}
	still := reply.still.Milliseconds()
	resp := watch.resp
	resp.PNG = base64.StdEncoding.EncodeToString(reply.png)
	resp.WatchMS, resp.StillMS = reply.elapsed.Milliseconds(), &still
	resp.Captured = "final"
	if req.Keep {
		// A frame is the page as it is: no patches or marks drawn.
		resp.Captured, resp.MarksSeq, resp.Patches = "frame", 0, 0
	}
	writeJSON(w, http.StatusOK, resp)
}
