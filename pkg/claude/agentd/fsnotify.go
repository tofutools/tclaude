package agentd

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
)

// convMonitorDebounce is how long a .jsonl must be quiet after a Write
// before the monitor re-indexes it. An active conversation appends
// turns in a burst while Claude is responding; debouncing collapses a
// burst into one ScanAndUpsertFile rather than reparsing per turn.
//
// 500ms is well under the 5s dashboard poll, so a title / first-prompt
// change is visible on the very next render — a brand-new conversation
// barely shows "(untitled)" — while still coalescing the rapid writes
// of a single response (CC streams turns at intervals well under half
// a second). The indexed fields (customTitle, summary, first prompt,
// file mtime) are otherwise near-static. Create / Remove / Rename
// events are NOT debounced; they re-index immediately.
//
// A package var, not a const, so flow tests can shrink it to keep the
// suite fast.
var convMonitorDebounce = 500 * time.Millisecond

// convMonitor is the daemon's live conv_index maintainer: ONE
// fsnotify.Watcher over ~/.claude/projects/ that calls
// convops.ScanAndUpsertFile whenever a conversation .jsonl changes, so
// the SQLite conv_index cache stays continuously fresh and readers (the
// dashboard) can trust cached rows instead of re-stat+reparsing the
// .jsonl on every access.
//
// fsnotify is NOT recursive on any platform — a watch on a directory
// reports events only for its direct children, never for files inside
// un-added subdirectories. So "one monitor" means one Watcher object
// (one goroutine, one event channel) that Add()s the projects root —
// to catch new project dirs — plus every existing project subdir — to
// catch .jsonl writes within. A typical machine has a few dozen project
// dirs; a few dozen watches is trivial.
type convMonitor struct {
	watcher     *fsnotify.Watcher
	projectsDir string
	stop        <-chan struct{}
	done        chan struct{}
}

// startConvMonitor resolves the projects root, creates the watcher,
// Add()s the root + every project subdir, and launches the event-loop
// goroutine. The Add() calls run synchronously here, so once this
// returns the watches are already in place — a caller that writes a
// .jsonl afterwards is guaranteed an event for it.
//
// Returns nil — after logging a warning — when the projects dir can't
// be resolved or the watcher can't be created (e.g. a host with no
// inotify). The daemon must still run; a nil monitor just means
// conv_index is not self-maintaining.
//
// The event loop stops when `stop` is closed; wait() blocks until the
// goroutine has fully exited.
func startConvMonitor(stop <-chan struct{}) *convMonitor {
	projectsDir := convops.ClaudeProjectsDir()
	if projectsDir == "" {
		slog.Warn("conv-monitor: cannot resolve ~/.claude/projects; conv_index will not auto-refresh")
		return nil
	}
	// Ensure the root exists before we watch it — on a machine where
	// Claude Code has never run the dir may be absent, and fsnotify
	// can't Add() a missing path. Best-effort; a failure just means the
	// Add() below logs a warning.
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		slog.Debug("conv-monitor: could not create projects root", "path", projectsDir, "error", err)
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		slog.Warn("conv-monitor: failed to create fsnotify watcher; conv_index will not auto-refresh", "error", err)
		return nil
	}

	m := &convMonitor{
		watcher:     w,
		projectsDir: projectsDir,
		stop:        stop,
		done:        make(chan struct{}),
	}

	// Watch the projects root so new project subdirs are detected as
	// they appear.
	if err := w.Add(projectsDir); err != nil {
		slog.Warn("conv-monitor: failed to watch projects root", "path", projectsDir, "error", err)
	}
	// Watch every existing project subdir — fsnotify is not recursive,
	// so the root watch alone would never see a .jsonl write.
	if entries, err := os.ReadDir(projectsDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				if err := w.Add(filepath.Join(projectsDir, e.Name())); err != nil {
					slog.Debug("conv-monitor: failed to watch project subdir", "name", e.Name(), "error", err)
				}
			}
		}
	}

	go m.loop()
	slog.Info("conv-monitor: watching for .jsonl changes", "root", projectsDir)
	return m
}

// wait blocks until the monitor's event-loop goroutine has fully
// exited. Nil-safe so callers needn't branch on a failed start. The
// daemon shares cronStop and is fire-and-forget like its sibling
// background goroutines; wait() exists so flow tests can stop the
// monitor synchronously before tearing down the test HOME / DB.
func (m *convMonitor) wait() {
	if m == nil {
		return
	}
	<-m.done
}

// fireEvent is what a settled debounce timer enqueues onto the loop:
// the path to re-index plus the timer that fired. Carrying the timer
// lets the loop tell whether a later Write has since installed a fresh
// timer for the same path (which it must not drop from the map).
type fireEvent struct {
	path  string
	timer *time.Timer
}

// loop is the single event-loop goroutine. Every conv_index write
// happens here, on this one goroutine: debounce timers only enqueue a
// fireEvent, they never touch the DB themselves. So once loop returns,
// no ScanAndUpsertFile call is left in flight — which is what lets a
// flow test stop the monitor and tear down its HOME without a race.
func (m *convMonitor) loop() {
	defer close(m.done)

	timers := make(map[string]*time.Timer)
	defer func() {
		for _, t := range timers {
			t.Stop()
		}
		_ = m.watcher.Close()
	}()

	// Debounce timers send the settled path here; the loop drains it
	// and does the (single-threaded) re-index. Buffered so a timer
	// firing during a slow re-index doesn't block its goroutine.
	fireCh := make(chan fireEvent, 64)
	// known tracks .jsonl paths we've already seen, so a Create event
	// for an already-tracked file (some platforms re-emit Create on
	// writes) is debounced as a Write rather than re-indexed eagerly.
	known := make(map[string]bool)

	// Startup scan: fsnotify only delivers events for *future* changes,
	// so re-index every .jsonl that already exists, once, here. This
	// covers a conv_index cache that is empty (fresh machine) or stale
	// (changes that landed while the daemon was down) — without it an
	// unchanged conversation would never be (re)indexed. Runs on the
	// loop goroutine before the select, so it stays single-threaded
	// with event handling; fsnotify events arriving meanwhile queue in
	// the watcher and are processed once the scan returns (idempotent,
	// so a file covered by both the scan and a queued event is fine).
	m.startupScan(known)

	for {
		select {
		case <-m.stop:
			return
		case fe := <-fireCh:
			// Forget the timer only if it is still the current one for
			// this path: a Write that arrived after fe.timer fired may
			// have installed a fresh timer that must stay tracked (so a
			// later Write resets it, and shutdown still Stop()s it).
			if timers[fe.path] == fe.timer {
				delete(timers, fe.path)
			}
			m.reindex(fe.path)
		case event, ok := <-m.watcher.Events:
			if !ok {
				return
			}
			m.handleEvent(event, timers, fireCh, known)
		case err, ok := <-m.watcher.Errors:
			if !ok {
				return
			}
			// A dropped event just means a slightly stale row until the
			// next change for that conv — keep listening. Log it,
			// though: fsnotify errors are rare (typically event-queue
			// overflow) and a burst of them is the breadcrumb that
			// explains a conv_index that has gone stale.
			slog.Warn("conv-monitor: fsnotify watcher error", "error", err)
		}
	}
}

// handleEvent routes one raw fsnotify event. A new project dir gets
// Add()'d (fsnotify is not recursive); a .jsonl create / remove /
// rename re-indexes immediately; a .jsonl write is debounced per-file.
func (m *convMonitor) handleEvent(event fsnotify.Event, timers map[string]*time.Timer, fireCh chan fireEvent, known map[string]bool) {
	path := event.Name

	// A new directory directly under the projects root is a new project
	// — start watching it, then reindexDir it to catch up on any .jsonl
	// already present. Add() before the scan, so the scan is the
	// catch-up for files that landed before the watch registered. This
	// narrows — does not fully close — the dir-appears-then-file-written
	// race: a file created after os.ReadDir returns but before the
	// kernel-side watch is live still gets no event. The window is
	// sub-millisecond in practice and the next write to that file
	// re-indexes it anyway.
	if event.Op&fsnotify.Create != 0 && filepath.Dir(path) == m.projectsDir {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			if err := m.watcher.Add(path); err != nil {
				slog.Debug("conv-monitor: failed to watch new project subdir", "path", path, "error", err)
			}
			m.reindexDir(path, known)
			return
		}
	}

	if !strings.HasSuffix(path, ".jsonl") {
		return
	}
	// Depth gate: the file must sit exactly at
	// projects/<projectdir>/<conv>.jsonl. This rejects the contents of
	// the CC-internal <convID>/ and memory/ subdirs. (Flat sub-agent
	// <convID>.jsonl files in the project dir still pass — only the
	// nested directories are skipped.)
	if filepath.Dir(filepath.Dir(path)) != m.projectsDir {
		return
	}

	// Remove / Rename: act now. ScanAndUpsertFile is self-cleaning — an
	// os.Stat miss makes it delete the conv_index row.
	if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
		if t, ok := timers[path]; ok {
			t.Stop()
			delete(timers, path)
		}
		delete(known, path)
		m.reindex(path)
		return
	}

	// First sighting of a file (Create): index it immediately so a
	// brand-new conversation shows up without waiting out the debounce.
	if event.Op&fsnotify.Create != 0 && !known[path] {
		known[path] = true
		if t, ok := timers[path]; ok {
			t.Stop()
			delete(timers, path)
		}
		m.reindex(path)
		return
	}

	// Write to a file we've already seen — debounce: reset the per-file
	// timer so the re-index fires only after the writes go quiet. The
	// quiet window also means the .jsonl is read well after Claude Code
	// finished appending, so a torn last line is unlikely; the parser
	// (convops.parseJSONLSession) skips any unparseable line regardless.
	// Filter to Write/Create — bare Chmod (touch / permission change) on
	// a known file is not a content change and should not reindex.
	if event.Op&(fsnotify.Write|fsnotify.Create) == 0 {
		return
	}
	known[path] = true
	if t, ok := timers[path]; ok {
		t.Stop()
	}
	var t *time.Timer
	t = time.AfterFunc(convMonitorDebounce, func() {
		select {
		case fireCh <- fireEvent{path: path, timer: t}:
		case <-m.stop:
		}
	})
	timers[path] = t
}

// startupScan re-indexes every existing .jsonl under the projects root
// once, at monitor start — see the call site in loop for why. Honours
// m.stop so a daemon shutting down mid-scan does not drag boot out.
func (m *convMonitor) startupScan(known map[string]bool) {
	entries, err := os.ReadDir(m.projectsDir)
	if err != nil {
		return
	}
	indexed := 0
	for _, e := range entries {
		select {
		case <-m.stop:
			return
		default:
		}
		if e.IsDir() {
			indexed += m.reindexDir(filepath.Join(m.projectsDir, e.Name()), known)
		}
	}
	slog.Info("conv-monitor: startup scan complete", "indexed", indexed)
}

// reindexDir re-indexes every .jsonl directly inside dir, marking each
// path known (so a later platform-re-emitted Create is debounced as a
// Write rather than re-indexed eagerly). Used by the startup scan and
// when a new project dir is discovered — both cover files that existed
// before a watch on their dir was live. Goes through reindexIfStale so
// a daemon restart doesn't re-parse every .jsonl that hasn't changed
// since the last index pass. Returns the file count.
func (m *convMonitor) reindexDir(dir string, known map[string]bool) int {
	files, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, f := range files {
		if !f.IsDir() && strings.HasSuffix(f.Name(), ".jsonl") {
			path := filepath.Join(dir, f.Name())
			known[path] = true
			m.reindexIfStale(path)
			n++
		}
	}
	return n
}

// reindex refreshes one conversation's conv_index row from its .jsonl.
// convops.ScanAndUpsertFile is idempotent and self-cleaning — it
// upserts the row on a change and deletes it when the file is gone — so
// the same call covers writes, creates, and removes.
//
// This is the single seam where a "conv changed" event would be
// published for the future SSE / dashboard-push PR (see
// docs/plans .../dashboard-realtime-push.md). PR 1 has exactly one
// effect — the conv_index refresh — and deliberately builds no
// broadcaster / fan-out around it.
func (m *convMonitor) reindex(path string) {
	convops.ScanAndUpsertFile(path)
}

// reindexIfStale re-parses path only when the on-disk file is newer
// than (or has a different size from) the cached conv_index row, or
// when no cached row exists at all. Mirrors RefreshConvIndexEntry's
// guard, keyed by path so it suits the startup scan that walks the
// filesystem first.
//
// Why the guard matters: the startup scan covers every .jsonl under
// ~/.claude/projects/. On a long-lived install with hundreds of convs
// that mostly never change after a session ends, an unconditional
// reparse on every daemon boot would be hundreds of file reads + parses
// for no new data. With the guard, only convs that actually changed
// while the daemon was down (or were never indexed) hit the parser.
//
// Live fsnotify events stay on the unconditional `reindex` path —
// they've already proved a change occurred, so the guard would just be
// a redundant stat.
func (m *convMonitor) reindexIfStale(path string) {
	convID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if len(convID) != 36 {
		return
	}
	// RefreshConvIndexEntry returns the row when the cache is fresh OR
	// after reparsing if it was stale; nil only when no cached row
	// existed (or it was just dropped because the file disappeared).
	// In that nil case we still want to try a direct scan in case the
	// file is on disk but unindexed (fresh install).
	if convops.RefreshConvIndexEntry(convID) == nil {
		convops.ScanAndUpsertFile(path)
	}
}
