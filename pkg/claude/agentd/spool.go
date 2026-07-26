package agentd

import (
	"bytes"
	"context"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The experimental file-spool transport (TCLAUDE_EXPERIMENTAL_FILE_TRANSPORT):
// the daemon-side consumer that turns per-agent request envelope files into
// ordinary requests against the same /v1 mux the Unix socket serves, and
// publishes response envelopes back. See pkg/claude/common/agentipc spool.go
// for the on-disk protocol and the identity model.
//
// Restart tolerance is the point: requests are plain files, so anything an
// agent wrote while the daemon was down is picked up by the startup scan,
// and the SQLite-backed bindings and idempotency layer carry everything
// else across restarts.

// spoolPeerPID is the synthetic PID stamped on spool-authenticated peers.
// Kernel PIDs are positive; a negative marker keeps classify()'s "PID 0 =
// unidentified" fail-closed branch intact while making spool callers
// recognisable in logs. Identity here comes from the directory binding,
// not from a process credential.
const spoolPeerPID = -1

// spoolStaleAfter bounds how long consumed-but-orphaned artifacts (claim
// files from a crash mid-request, response envelopes an agent never
// collected) survive before the sweeper removes them. Well above every
// client timeout, so a slow-but-alive exchange is never swept.
const spoolStaleAfter = 15 * time.Minute

// spoolProcessingSlots caps concurrently served spool requests, mirroring
// the natural bound the HTTP server gets from its connection handling.
const spoolProcessingSlots = 8

type spoolConsumer struct {
	handler http.Handler
	root    string
	rescan  time.Duration

	mu       sync.Mutex
	bindings map[string]db.SpoolBinding // by spool id
	byReqDir map[string]db.SpoolBinding // by watched req/ dir

	sem     chan struct{}
	watcher *fsnotify.Watcher // nil when fsnotify is unavailable; rescan covers
	wg      sync.WaitGroup
}

// startSpoolConsumer begins serving spool requests against handler (the
// bare /v1 mux — identity is stamped here per binding, not by the socket's
// withIdentity middleware). Returns a stop function. root is created if
// missing; rescan is the fallback poll interval that also discovers new
// bindings, with fsnotify providing low latency when available.
func startSpoolConsumer(handler http.Handler, root string, rescan time.Duration) (stop func()) {
	c := &spoolConsumer{
		handler:  handler,
		root:     root,
		rescan:   rescan,
		bindings: map[string]db.SpoolBinding{},
		byReqDir: map[string]db.SpoolBinding{},
		sem:      make(chan struct{}, spoolProcessingSlots),
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		slog.Warn("spool: cannot create spool root; file transport disabled", "root", root, "error", err)
		return func() {}
	}
	if w, err := fsnotify.NewWatcher(); err == nil {
		c.watcher = w
		if err := w.Add(root); err != nil {
			slog.Debug("spool: watch root failed; relying on rescan", "root", root, "error", err)
		}
	} else {
		slog.Debug("spool: fsnotify unavailable; relying on rescan", "error", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.wg.Add(1)
	go c.run(ctx)
	return func() {
		cancel()
		if c.watcher != nil {
			_ = c.watcher.Close()
		}
		c.wg.Wait()
	}
}

func (c *spoolConsumer) run(ctx context.Context) {
	defer c.wg.Done()
	// Startup pass first: requests written while the daemon was down are
	// served before we start waiting on events.
	c.refreshBindings()
	c.scanAll()
	ticker := time.NewTicker(c.rescan)
	defer ticker.Stop()
	var events chan fsnotify.Event
	var errs chan error
	if c.watcher != nil {
		events = c.watcher.Events
		errs = c.watcher.Errors
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.refreshBindings()
			c.scanAll()
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			c.handleEvent(ctx, ev)
		case _, ok := <-errs:
			if !ok {
				errs = nil
			}
		}
	}
}

// handleEvent reacts to one fsnotify event: a new directory under the
// spool root means a fresh binding may exist; a new envelope in a watched
// req/ dir is served immediately.
func (c *spoolConsumer) handleEvent(ctx context.Context, ev fsnotify.Event) {
	if !ev.Op.Has(fsnotify.Create) && !ev.Op.Has(fsnotify.Rename) {
		return
	}
	dir := filepath.Dir(ev.Name)
	if filepath.Clean(dir) == filepath.Clean(c.root) {
		c.refreshBindings()
		c.scanAll()
		return
	}
	c.mu.Lock()
	b, ok := c.byReqDir[filepath.Clean(dir)]
	c.mu.Unlock()
	if !ok || !agentipc.SpoolEnvelopeFile(filepath.Base(ev.Name)) {
		return
	}
	c.claimAndServe(ctx, b, ev.Name)
}

// refreshBindings reloads the active binding set from SQLite and (un)wires
// fsnotify watches to match. SQLite is the authority: a binding created by
// a spawn in another process appears here on the next refresh even if the
// root watch missed it.
func (c *spoolConsumer) refreshBindings() {
	list, err := db.ListActiveSpoolBindings()
	if err != nil {
		slog.Warn("spool: list bindings failed", "error", err)
		return
	}
	fresh := make(map[string]db.SpoolBinding, len(list))
	freshDirs := make(map[string]db.SpoolBinding, len(list))
	for _, b := range list {
		fresh[b.SpoolID] = b
		freshDirs[filepath.Clean(agentipc.SpoolReqDir(b.Dir))] = b
	}
	c.mu.Lock()
	prevDirs := c.byReqDir
	c.bindings = fresh
	c.byReqDir = freshDirs
	c.mu.Unlock()
	if c.watcher == nil {
		return
	}
	for dir := range freshDirs {
		if _, had := prevDirs[dir]; !had {
			if err := c.watcher.Add(dir); err != nil {
				slog.Debug("spool: watch req dir failed; rescan will cover it", "dir", dir, "error", err)
			}
		}
	}
	for dir := range prevDirs {
		if _, still := freshDirs[dir]; !still {
			_ = c.watcher.Remove(dir)
		}
	}
}

// scanAll serves every pending envelope across all bound req/ dirs and
// sweeps stale artifacts. This is the fsnotify-free correctness path; the
// watcher only improves latency.
func (c *spoolConsumer) scanAll() {
	c.mu.Lock()
	bindings := make([]db.SpoolBinding, 0, len(c.bindings))
	for _, b := range c.bindings {
		bindings = append(bindings, b)
	}
	c.mu.Unlock()
	for _, b := range bindings {
		reqDir := agentipc.SpoolReqDir(b.Dir)
		entries, err := os.ReadDir(reqDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			full := filepath.Join(reqDir, name)
			if agentipc.SpoolEnvelopeFile(name) {
				c.claimAndServe(context.Background(), b, full)
			} else {
				sweepStale(full)
			}
		}
		sweepDirStale(agentipc.SpoolRespDir(b.Dir))
	}
}

// claimAndServe atomically claims one request envelope (rename to a dotted
// claim file scanners ignore) and serves it on a bounded worker. The
// rename is the mutual exclusion between the fsnotify path, the rescan
// path, and any concurrent duplicate event: exactly one claimer wins.
func (c *spoolConsumer) claimAndServe(ctx context.Context, b db.SpoolBinding, reqPath string) {
	claimPath := filepath.Join(filepath.Dir(reqPath), "."+filepath.Base(reqPath)+".work")
	if err := os.Rename(reqPath, claimPath); err != nil {
		return // already claimed, withdrawn by the client, or gone
	}
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		// Consumer shutting down: leave the claim file; the post-restart
		// sweeper removes it and the client's retry re-submits.
		return
	}
	c.wg.Go(func() {
		defer func() { <-c.sem }()
		c.serveClaimed(b, claimPath)
	})
}

func (c *spoolConsumer) serveClaimed(b db.SpoolBinding, claimPath string) {
	defer func() { _ = os.Remove(claimPath) }()
	base := filepath.Base(claimPath)
	id := strings.TrimSuffix(strings.TrimPrefix(base, "."), ".json.work")
	respPath := agentipc.SpoolEnvelopePath(agentipc.SpoolRespDir(b.Dir), id)
	data, err := os.ReadFile(claimPath)
	if err != nil {
		slog.Warn("spool: read claimed request failed", "path", claimPath, "error", err)
		return
	}
	status, header, body := c.dispatch(b, data)
	respData, err := agentipc.EncodeSpoolResponse(agentipc.SpoolResponse{
		Status: status, Header: header, Body: body,
	})
	if err != nil {
		slog.Warn("spool: encode response failed", "conv", b.ConvID, "error", err)
		return
	}
	if err := agentipc.WriteSpoolFile(respPath, respData); err != nil {
		slog.Warn("spool: publish response failed", "path", respPath, "error", err)
	}
}

// dispatch decodes one request envelope and runs it through the /v1 mux
// with the binding's identity stamped. Malformed envelopes get a 400
// response rather than silence, so a broken client fails fast instead of
// timing out.
func (c *spoolConsumer) dispatch(b db.SpoolBinding, data []byte) (int, http.Header, []byte) {
	env, err := agentipc.DecodeSpoolRequest(data)
	if err != nil || env.Method == "" || !strings.HasPrefix(env.RequestURI, "/") {
		return http.StatusBadRequest, http.Header{"Content-Type": []string{"application/json"}},
			[]byte(`{"error":"malformed spool request envelope"}`)
	}
	req, err := http.NewRequest(env.Method, "http://_"+env.RequestURI, bytes.NewReader(env.Body))
	if err != nil {
		return http.StatusBadRequest, http.Header{"Content-Type": []string{"application/json"}},
			[]byte(`{"error":"invalid spool request"}`)
	}
	maps.Copy(req.Header, env.Header)
	// Possession of the bound directory is the caller identity; stamp the
	// peer the way withIdentity would for a socket caller that resolved to
	// this conv. The spool path never verifies an operator token — it is
	// an agent-class transport only.
	p := &peer{PID: spoolPeerPID, ConvID: b.ConvID, HasClaudeAncestor: true}
	req = req.WithContext(context.WithValue(req.Context(), peerKey{}, p))
	maybeFlushUndelivered(b.ConvID)
	enrollCallerOnce(b.ConvID)
	rec := &spoolResponseWriter{header: http.Header{}}
	c.handler.ServeHTTP(rec, req)
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	return rec.status, rec.header, rec.buf.Bytes()
}

// spoolResponseWriter captures a handler's response for envelope
// serialization. Deliberately minimal: the /v1 mux is plain
// request/response JSON with no streaming or hijacking.
type spoolResponseWriter struct {
	header http.Header
	status int
	buf    bytes.Buffer
}

func (w *spoolResponseWriter) Header() http.Header { return w.header }

func (w *spoolResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *spoolResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.buf.Write(p)
}

// sweepStale removes one non-envelope artifact (claim file, torn tmp file)
// once it is old enough to be certainly orphaned.
func sweepStale(path string) {
	info, err := os.Stat(path)
	if err != nil || time.Since(info.ModTime()) < spoolStaleAfter {
		return
	}
	_ = os.Remove(path)
}

// sweepDirStale removes uncollected response envelopes (their agent died
// or gave up) after the stale window.
func sweepDirStale(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		sweepStale(filepath.Join(dir, e.Name()))
	}
}
