package agentd

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// codexApprovalMonitorDebounce lets Codex's atomic config edit settle before
// the daemon validates and promotes an appended app-tool approval.
var codexApprovalMonitorDebounce = 100 * time.Millisecond

type codexApprovalMonitor struct {
	watcher *fsnotify.Watcher
	dir     string
	stop    <-chan struct{}
	done    chan struct{}
}

func startCodexApprovalMonitor(stop <-chan struct{}) *codexApprovalMonitor {
	dir, err := harness.CodexConfigDir()
	if err != nil {
		slog.Warn("codex-approval-monitor: cannot resolve Codex config directory", "error", err)
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("codex-approval-monitor: cannot create Codex config directory", "path", dir, "error", err)
		return nil
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		slog.Warn("codex-approval-monitor: failed to create watcher", "error", err)
		return nil
	}
	if err := w.Add(dir); err != nil {
		_ = w.Close()
		slog.Warn("codex-approval-monitor: failed to watch Codex config directory", "path", dir, "error", err)
		return nil
	}
	m := &codexApprovalMonitor{watcher: w, dir: dir, stop: stop, done: make(chan struct{})}
	go m.loop()
	slog.Info("codex-approval-monitor: watching managed launch profiles", "root", dir)
	return m
}

func (m *codexApprovalMonitor) wait() {
	if m != nil {
		<-m.done
	}
}

type codexApprovalFire struct {
	path string
	id   uint64
}

func (m *codexApprovalMonitor) loop() {
	defer close(m.done)
	defer func() { _ = m.watcher.Close() }()

	// A startup scan recovers approvals written while agentd was down, as long
	// as the owning Codex pane is still alive and has not removed its profile.
	if entries, err := os.ReadDir(m.dir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				m.reconcile(filepath.Join(m.dir, entry.Name()))
			}
		}
	}

	fireCh := make(chan codexApprovalFire, 32)
	timers := make(map[string]*time.Timer)
	ids := make(map[string]uint64)
	var nextID uint64
	defer func() {
		for _, timer := range timers {
			timer.Stop()
		}
	}()

	for {
		select {
		case <-m.stop:
			return
		case fire := <-fireCh:
			if ids[fire.path] != fire.id {
				continue
			}
			delete(ids, fire.path)
			delete(timers, fire.path)
			m.reconcile(fire.path)
		case event, ok := <-m.watcher.Events:
			if !ok {
				return
			}
			if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename) == 0 ||
				!harness.IsCodexAgentLaunchProfilePath(event.Name) {
				continue
			}
			if timer := timers[event.Name]; timer != nil {
				timer.Stop()
			}
			nextID++
			id := nextID
			ids[event.Name] = id
			path := event.Name
			timers[path] = time.AfterFunc(codexApprovalMonitorDebounce, func() {
				select {
				case fireCh <- codexApprovalFire{path: path, id: id}:
				case <-m.stop:
				}
			})
		case err, ok := <-m.watcher.Errors:
			if !ok {
				return
			}
			slog.Warn("codex-approval-monitor: watcher error", "error", err)
		}
	}
}

func (m *codexApprovalMonitor) reconcile(path string) {
	if !harness.IsCodexAgentLaunchProfilePath(path) {
		return
	}
	report, err := harness.PromoteCodexLaunchProfileApprovals(path)
	if err != nil {
		if os.IsNotExist(err) || strings.Contains(err.Error(), "no baseline seal") {
			slog.Debug("codex-approval-monitor: profile not eligible for promotion", "path", path, "error", err)
			return
		}
		slog.Warn("codex-approval-monitor: refused profile approval promotion", "path", path, "error", err)
		return
	}
	if len(report.Conflicts) > 0 {
		slog.Warn("codex-approval-monitor: kept existing global approval decisions",
			"path", path, "conflicts", report.Conflicts)
	}
	if report.Added > 0 {
		slog.Info("codex-approval-monitor: persisted app-tool Always allow choice",
			"path", path, "added", report.Added, "already_present", report.Existing)
	}
}
