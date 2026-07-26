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
	"github.com/tofutools/tclaude/pkg/claude/common/config"
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

// spoolStaleAfter bounds both directions of staleness. Requests older than
// this are refused (swept, never served): a SIGKILL'd client cannot
// withdraw its envelope, and executing a mutation days later under the
// agent's identity has no socket-path analog — a socket request dies with
// its connection. In the other direction it bounds how long orphaned
// artifacts (claim files from a crash mid-request, response envelopes an
// agent never collected) survive before the sweeper removes them. Well
// above every client timeout, so a slow-but-alive exchange is never
// affected.
const spoolStaleAfter = 15 * time.Minute

// spoolProcessingSlots caps concurrently EXECUTING spool handlers. Note
// this is a global cap the socket path does not have (net/http runs one
// goroutine per connection): handlers that legitimately block in-request —
// the human-approval popup can hold a request for minutes — each occupy a
// slot for their whole wait, so the cap is sized generously. Workers
// waiting for a slot are parked goroutines holding no claim, so a stalled
// slot never wedges the consumer loop and clients can still withdraw
// unclaimed requests.
const spoolProcessingSlots = 32

// spoolServingMarker is a heartbeat file the consumer maintains at the
// spool root (touched every rescan tick). Clients use its freshness as a
// fast liveness probe (see the agent package's spoolConsumerLikelyUp): a
// provisioned agent whose daemon runs WITHOUT the feature flag (or died)
// would otherwise discover it only by waiting out a full request timeout
// per call.
const spoolServingMarker = ".serving"

// spoolTransportEnabled resolves the experimental flag from either switch:
// the TCLAUDE_EXPERIMENTAL_FILE_TRANSPORT environment variable or the
// features.file_spool_transport config field (the dashboard Config tab's
// Experimental section edits the latter). Config is re-read on every call,
// so the supervisor below observes toggles live.
func spoolTransportEnabled() bool {
	if agentipc.FileTransportEnabled() {
		return true
	}
	cfg, err := config.Load()
	return err == nil && cfg.FileSpoolTransportEnabled()
}

// spoolConfigPollInterval is how often the supervisor re-resolves the flag.
// Config loads are one small-file read; the same cadence class as the
// per-request config reads the process-routes gate already does.
const spoolConfigPollInterval = 5 * time.Second

// spoolShouldServe decides whether the consumer must run, and why. The
// flag alone does NOT decide: agents provisioned while the flag was on may
// still be running after the flag goes off (a dashboard toggle, or a
// daemon restarted without the env var after an upgrade), and a spool
// agent inside a socketless sandbox has no other channel. So the flag
// gates PROVISIONING (see session.ApplyAgentSpoolEnv), while serving
// continues for existing bindings until they retire — drain mode. Once the
// last binding is revoked and swept, the consumer stops on the next poll.
func spoolShouldServe() (serve bool, draining bool) {
	if spoolTransportEnabled() {
		return true, false
	}
	bindings, err := db.ListActiveSpoolBindings()
	if err != nil {
		slog.Warn("spool: list bindings for drain check failed", "error", err)
		return false, false
	}
	return len(bindings) > 0, len(bindings) > 0
}

// startSpoolSupervisor keeps a spool consumer running exactly while it is
// needed — flag on, or existing spool agents to drain — re-checking every
// poll interval so a dashboard Config-tab toggle takes effect without a
// daemon restart. Returns a stop function for daemon shutdown.
func startSpoolSupervisor(handler http.Handler, poll, rescan time.Duration) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() {
		var stopConsumer func()
		wasDraining := false
		apply := func() {
			serve, draining := spoolShouldServe()
			switch {
			case serve && stopConsumer == nil:
				if draining {
					slog.Info("file-spool transport draining: flag off, serving existing spool agents until they retire", "root", agentipc.SpoolRoot())
				} else {
					slog.Info("experimental file-spool transport enabled", "root", agentipc.SpoolRoot())
				}
				stopConsumer = startSpoolConsumer(handler, agentipc.SpoolRoot(), rescan)
			case !serve && stopConsumer != nil:
				if wasDraining {
					slog.Info("file-spool transport drained: last spool agent retired; consumer stopped")
				} else {
					slog.Info("experimental file-spool transport disabled")
				}
				stopConsumer()
				stopConsumer = nil
			}
			wasDraining = draining
		}
		apply()
		ticker := time.NewTicker(poll)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				if stopConsumer != nil {
					stopConsumer()
				}
				return
			case <-ticker.C:
				apply()
			}
		}
	})
	return func() {
		cancel()
		wg.Wait()
	}
}

type spoolConsumer struct {
	handler http.Handler
	root    string
	rescan  time.Duration

	mu       sync.Mutex
	bindings map[string]db.SpoolBinding // by spool id
	byReqDir map[string]db.SpoolBinding // by watched req/ dir
	inflight map[string]struct{}        // req paths with a worker spawned

	sem     chan struct{}
	watcher *fsnotify.Watcher // nil when fsnotify is unavailable; rescan covers
	wg      sync.WaitGroup
}

// startSpoolConsumer begins serving spool requests against handler (the
// bare /v1 mux — identity is stamped here per directory binding, not by the
// socket's withIdentity middleware). Returns a stop function. root is
// created if missing; rescan is the fallback poll interval that also
// discovers new bindings, with fsnotify providing low latency when
// available.
func startSpoolConsumer(handler http.Handler, root string, rescan time.Duration) (stop func()) {
	c := &spoolConsumer{
		handler:  handler,
		root:     root,
		rescan:   rescan,
		bindings: map[string]db.SpoolBinding{},
		byReqDir: map[string]db.SpoolBinding{},
		inflight: map[string]struct{}{},
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
		// In-flight handlers observe the cancelled ctx through their request
		// context (a blocked approval wait unwinds), so this returns promptly.
		c.wg.Wait()
		_ = os.Remove(filepath.Join(c.root, spoolServingMarker))
	}
}

func (c *spoolConsumer) run(ctx context.Context) {
	defer c.wg.Done()
	// Startup pass first: requests written while the daemon was down are
	// served before we start waiting on events.
	c.touchMarker()
	c.refreshBindings()
	c.scanAll(ctx)
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
			c.touchMarker()
			c.refreshBindings()
			c.scanAll(ctx)
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

// touchMarker maintains the .serving heartbeat clients use as a fast
// liveness probe.
func (c *spoolConsumer) touchMarker() {
	path := filepath.Join(c.root, spoolServingMarker)
	now := time.Now()
	if err := os.Chtimes(path, now, now); err == nil {
		return
	}
	if err := os.WriteFile(path, []byte("agentd file-spool consumer heartbeat\n"), 0o600); err != nil {
		slog.Debug("spool: cannot write serving marker", "path", path, "error", err)
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
		c.scanAll(ctx)
		return
	}
	c.mu.Lock()
	b, ok := c.byReqDir[filepath.Clean(dir)]
	c.mu.Unlock()
	if !ok || !agentipc.SpoolEnvelopeFile(filepath.Base(ev.Name)) {
		return
	}
	c.maybeServe(ctx, b, ev.Name)
}

// refreshBindings reloads the active binding set from SQLite, (un)wires
// fsnotify watches to match, and sweeps revoked bindings' directories.
// SQLite is the authority: a binding created by a spawn in another process
// appears here on the next refresh even if the root watch missed it.
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
	if c.watcher != nil {
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
	c.sweepRevoked()
}

// sweepRevoked destroys the on-disk capability of every revoked binding
// (retire revokes; see retireAgentConv) and then drops the row. Running it
// on every refresh makes revocation converge even if the revoker crashed
// right after the UPDATE.
func (c *spoolConsumer) sweepRevoked() {
	revoked, err := db.ListRevokedSpoolBindings()
	if err != nil {
		slog.Warn("spool: list revoked bindings failed", "error", err)
		return
	}
	for _, b := range revoked {
		// Only remove directories that live under this consumer's root — a
		// corrupted row must not turn into an arbitrary RemoveAll.
		if filepath.Dir(filepath.Clean(b.Dir)) != filepath.Clean(c.root) {
			slog.Warn("spool: revoked binding dir outside spool root; row dropped, dir untouched",
				"spool_id", b.SpoolID, "dir", b.Dir)
		} else if err := os.RemoveAll(b.Dir); err != nil {
			slog.Warn("spool: remove revoked spool dir failed", "dir", b.Dir, "error", err)
			continue // keep the row so the next refresh retries
		}
		if err := db.DeleteSpoolBinding(b.SpoolID); err != nil {
			slog.Warn("spool: delete revoked binding row failed", "spool_id", b.SpoolID, "error", err)
		}
	}
}

// scanAll serves every pending envelope across all bound req/ dirs and
// sweeps stale artifacts. This is the fsnotify-free correctness path; the
// watcher only improves latency.
func (c *spoolConsumer) scanAll(ctx context.Context) {
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
				c.maybeServe(ctx, b, full)
			} else {
				sweepStale(full)
			}
		}
		sweepDirStale(agentipc.SpoolRespDir(b.Dir))
	}
}

// maybeServe spawns a worker for one request envelope, unless one is
// already in flight for it. The worker acquires an execution slot BEFORE
// claiming, so a saturated consumer leaves envelopes unclaimed on disk —
// where their clients can still withdraw them on timeout — instead of
// claiming work it cannot start. The claim rename remains the mutual
// exclusion: however many paths noticed the file, exactly one claimer
// wins. The inflight set only bounds goroutine buildup across rescans.
func (c *spoolConsumer) maybeServe(ctx context.Context, b db.SpoolBinding, reqPath string) {
	// Refuse stale requests outright rather than executing them arbitrarily
	// late under the agent's identity (see spoolStaleAfter).
	if info, err := os.Stat(reqPath); err != nil {
		return
	} else if time.Since(info.ModTime()) > spoolStaleAfter {
		slog.Warn("spool: refusing stale request envelope", "path", reqPath, "age", time.Since(info.ModTime()).Round(time.Second))
		_ = os.Remove(reqPath)
		return
	}
	c.mu.Lock()
	if _, busy := c.inflight[reqPath]; busy {
		c.mu.Unlock()
		return
	}
	c.inflight[reqPath] = struct{}{}
	c.mu.Unlock()
	c.wg.Go(func() {
		defer func() {
			c.mu.Lock()
			delete(c.inflight, reqPath)
			c.mu.Unlock()
		}()
		select {
		case c.sem <- struct{}{}:
		case <-ctx.Done():
			return // never claimed; served after restart or withdrawn by its client
		}
		defer func() { <-c.sem }()
		claimPath := filepath.Join(filepath.Dir(reqPath), "."+filepath.Base(reqPath)+".work")
		if err := os.Rename(reqPath, claimPath); err != nil {
			return // already claimed, withdrawn by the client, or gone
		}
		// Stamp the claim time: the rename preserves the envelope's write
		// mtime, but the crash-orphan sweep window must start now.
		now := time.Now()
		_ = os.Chtimes(claimPath, now, now)
		c.serveClaimed(ctx, b, claimPath)
	})
}

func (c *spoolConsumer) serveClaimed(ctx context.Context, b db.SpoolBinding, claimPath string) {
	defer func() { _ = os.Remove(claimPath) }()
	base := filepath.Base(claimPath)
	id := strings.TrimSuffix(strings.TrimPrefix(base, "."), ".json.work")
	respPath := agentipc.SpoolEnvelopePath(agentipc.SpoolRespDir(b.Dir), id)
	data, err := os.ReadFile(claimPath)
	if err != nil {
		slog.Warn("spool: read claimed request failed", "path", claimPath, "error", err)
		return
	}
	status, header, body := c.dispatch(ctx, b, data)
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
func (c *spoolConsumer) dispatch(ctx context.Context, b db.SpoolBinding, data []byte) (status int, header http.Header, body []byte) {
	// The socket path gets per-request panic recovery from net/http's
	// connection goroutines; the spool path must provide its own, or one
	// bad handler takes down the daemon for everyone — and the client's
	// retry would crash-loop it through the startup scan.
	defer func() {
		if r := recover(); r != nil {
			slog.Error("spool: handler panic recovered", "conv", b.ConvID, "panic", r)
			status = http.StatusInternalServerError
			header = http.Header{"Content-Type": []string{"application/json"}}
			body = []byte(`{"error":"internal error"}`)
		}
	}()
	env, err := agentipc.DecodeSpoolRequest(data)
	if err != nil || env.Method == "" || !strings.HasPrefix(env.RequestURI, "/") {
		return http.StatusBadRequest, http.Header{"Content-Type": []string{"application/json"}},
			[]byte(`{"error":"malformed spool request envelope"}`)
	}
	// The consumer ctx is the request ctx: daemon shutdown unwinds even a
	// handler blocked in a long in-request wait (human-approval popup).
	req, err := http.NewRequestWithContext(ctx, env.Method, "http://_"+env.RequestURI, bytes.NewReader(env.Body))
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
