package agentd

import (
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// codexApprovalMonitorDebounce lets Codex's atomic config edit settle before
// the daemon parses and promotes durable user choices from the launch profile.
var codexApprovalMonitorDebounce = 100 * time.Millisecond

var codexApprovalCleanupTailRe = regexp.MustCompile(`^; (?:}; )?exit \$tclaude_(?:launch|resume)_status$`)

var codexApprovalPaneStartCommands = func() ([]byte, error) {
	return clcommon.TmuxCommand("list-panes", "-a", "-F", "#{pane_dead}\t#{pane_start_command}").Output()
}

type codexApprovalMonitor struct {
	watcher   *fsnotify.Watcher
	dir       string
	stop      <-chan struct{}
	processed chan string
	done      chan struct{}
}

func startCodexApprovalMonitor(stop <-chan struct{}) *codexApprovalMonitor {
	return startCodexApprovalMonitorWithProcessing(stop, nil)
}

func startCodexApprovalMonitorWithProcessing(stop <-chan struct{}, processed chan string) *codexApprovalMonitor {
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
	m := &codexApprovalMonitor{
		watcher:   w,
		dir:       dir,
		stop:      stop,
		processed: processed,
		done:      make(chan struct{}),
	}
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

	m.reconcileLiveProfilesAtStartup()

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
			if m.processed != nil {
				select {
				case m.processed <- fire.path:
				default:
				}
			}
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

// reconcileLiveProfilesAtStartup recovers durable choices written while agentd
// was down, but only for managed profiles named by currently live tmux panes.
// Crash leftovers and retired/stopped sessions remain untouched for the normal
// age-bounded stale-profile sweep.
func (m *codexApprovalMonitor) reconcileLiveProfilesAtStartup() {
	paneStarts, err := liveCodexApprovalPaneStarts()
	if err != nil {
		slog.Warn("codex-approval-monitor: cannot inspect live startup profiles", "error", err)
		return
	}
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		slog.Warn("codex-approval-monitor: cannot scan startup profiles", "path", m.dir, "error", err)
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(m.dir, entry.Name())
		if harness.IsCodexAgentLaunchProfilePath(path) && codexApprovalProfileOwnedByLivePane(path, paneStarts) {
			m.reconcile(path)
		}
	}
}

func liveCodexApprovalPaneStarts() ([]string, error) {
	out, err := codexApprovalPaneStartCommands()
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return nil, nil // no tmux server or no live panes
		}
		return nil, err
	}
	return codexApprovalLivePaneStarts(string(out)), nil
}

func codexApprovalLivePaneStarts(data string) []string {
	commands := make([]string, 0)
	for line := range strings.SplitSeq(data, "\n") {
		dead, command, ok := strings.Cut(line, "\t")
		if !ok || dead != "0" || command == "" {
			continue
		}
		commands = append(commands, command)
	}
	return commands
}

func codexApprovalProfileOwnedByLivePane(path string, paneStarts []string) bool {
	profile := strings.TrimSuffix(filepath.Base(path), ".config.toml")
	profileArg := " -p " + profile
	cleanup := "rm -f -- " + clcommon.ShellQuoteArg(path)
	for _, rendered := range paneStarts {
		// Script-launch shape (current): `sh <launch-script> <profile-path>`.
		// The profile path rides the pane argv as an inert marker precisely so
		// this recovery can match it (session.CodexProfileMarkerArgs).
		if codexApprovalScriptPaneOwnsProfile(rendered, path) {
			return true
		}
		// Inline `sh -c "…"` shape: panes launched by a pre-script tclaude
		// that are still alive across this daemon restart.
		command, ok := codexApprovalRawShellCommand(rendered)
		if !ok {
			continue
		}
		cleanupAt := strings.LastIndex(command, cleanup)
		if cleanupAt < 0 || !codexApprovalHasProfileArg(command[:cleanupAt], profileArg) ||
			cleanupAt != strings.LastIndex(command, "rm -f -- ") {
			continue
		}
		tail := strings.TrimSpace(command[cleanupAt+len(cleanup):])
		if codexApprovalCleanupTailRe.MatchString(tail) {
			return true
		}
	}
	return false
}

// codexApprovalScriptPaneOwnsProfile matches the script-launch pane shape —
// `sh <launch-script> <profile-path>` — against one rendered
// #{pane_start_command}. The argv is daemon-built and carries no prompt or
// other user text (the whole bootstrap lives in the script file), so an
// exact decoded-word match is sound where the inline shape needed the
// codex-segment/cleanup-tail heuristics. Word 1 must look like a tclaude
// launch script so arbitrary `sh <something> <path>` panes cannot claim a
// profile.
func codexApprovalScriptPaneOwnsProfile(rendered, profilePath string) bool {
	words, ok := codexApprovalRenderedWords(rendered)
	if !ok || len(words) < 3 || words[0] != "sh" || !codexApprovalLaunchScriptWord(words[1]) {
		return false
	}
	for _, word := range words[2:] {
		if word == profilePath {
			return true
		}
	}
	return false
}

func codexApprovalLaunchScriptWord(word string) bool {
	return filepath.Base(filepath.Dir(word)) == "launch-scripts" &&
		strings.HasPrefix(filepath.Base(word), "launch-")
}

// codexApprovalRenderedWords splits a tmux #{pane_start_command} into decoded
// argv words, reversing tmux's per-word args_escape rendering: words are
// whitespace-separated at top level; a double-quoted word drops the quotes
// and unescapes tmux's dquote set (backslash before \ " $ or backtick); a
// single-quoted word drops the quotes verbatim; a backslash outside quotes
// escapes the next byte. Unterminated quoting fails the whole parse.
func codexApprovalRenderedWords(rendered string) ([]string, bool) {
	var words []string
	var cur strings.Builder
	inWord := false
	flush := func() {
		if inWord {
			words = append(words, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	for i := 0; i < len(rendered); i++ {
		switch c := rendered[i]; c {
		case ' ', '\t':
			flush()
		case '"':
			inWord = true
			i++
			for ; i < len(rendered) && rendered[i] != '"'; i++ {
				if rendered[i] == '\\' && i+1 < len(rendered) && strings.ContainsRune(`\"$`+"`", rune(rendered[i+1])) {
					i++
				}
				cur.WriteByte(rendered[i])
			}
			if i >= len(rendered) {
				return nil, false
			}
		case '\'':
			inWord = true
			i++
			for ; i < len(rendered) && rendered[i] != '\''; i++ {
				cur.WriteByte(rendered[i])
			}
			if i >= len(rendered) {
				return nil, false
			}
		case '\\':
			inWord = true
			if i+1 < len(rendered) {
				i++
				cur.WriteByte(rendered[i])
			}
		default:
			inWord = true
			cur.WriteByte(c)
		}
	}
	flush()
	return words, true
}

func codexApprovalHasProfileArg(command, profileArg string) bool {
	unquoted := shellUnquotedMask(command)
	for segment := range strings.SplitSeq(unquoted, ";") {
		segment = strings.TrimSpace(segment)
		if strings.HasPrefix(segment, "codex ") && strings.Contains(segment, profileArg) {
			return true
		}
	}
	return false
}

// shellUnquotedMask preserves unquoted shell syntax byte-for-byte and blanks
// quoted/escaped content. It is intentionally a small recognizer, not a shell
// evaluator: startup recovery only needs to distinguish the generated `codex
// ... -p ...` command segment from prompt text that happens to discuss one.
func shellUnquotedMask(command string) string {
	masked := []byte(command)
	var quote byte
	for i := 0; i < len(masked); i++ {
		char := masked[i]
		if quote != 0 {
			masked[i] = ' '
			if quote == '"' && char == '\\' && i+1 < len(masked) {
				i++
				masked[i] = ' '
				continue
			}
			if char == quote {
				quote = 0
			}
			continue
		}
		switch char {
		case '\'', '"':
			quote = char
			masked[i] = ' '
		case '\\':
			masked[i] = ' '
			if i+1 < len(masked) {
				i++
				masked[i] = ' '
			}
		}
	}
	return string(masked)
}

// codexApprovalRawShellCommand reverses tmux's pane_start_command rendering
// for the `sh -c <command>` shape tclaude launches. tmux wraps the command in
// double quotes and backslash-escapes characters special inside those quotes;
// decoding them lets exact cleanup paths compare correctly even when
// CODEX_HOME contains spaces, dollars, quotes, backslashes, or backticks.
func codexApprovalRawShellCommand(rendered string) (string, bool) {
	const prefix = `sh -c "`
	if !strings.HasPrefix(rendered, prefix) || !strings.HasSuffix(rendered, `"`) {
		return "", false
	}
	quoted := rendered[len(prefix) : len(rendered)-1]
	var raw strings.Builder
	raw.Grow(len(quoted))
	for i := 0; i < len(quoted); i++ {
		if quoted[i] == '\\' && i+1 < len(quoted) && strings.ContainsRune(`\"$`+"`", rune(quoted[i+1])) {
			i++
		}
		raw.WriteByte(quoted[i])
	}
	return raw.String(), true
}

func (m *codexApprovalMonitor) reconcile(path string) {
	if !harness.IsCodexAgentLaunchProfilePath(path) {
		return
	}
	report, err := harness.PromoteCodexLaunchProfileChanges(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Debug("codex-approval-monitor: profile not eligible for promotion", "path", path, "error", err)
			return
		}
		slog.Warn("codex-approval-monitor: refused launch-profile promotion", "path", path, "error", err)
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
	if len(report.NoticeConflicts) > 0 {
		slog.Warn("codex-approval-monitor: kept existing global notice preferences",
			"path", path, "conflicts", report.NoticeConflicts)
	}
	if report.NoticesAdded > 0 {
		slog.Info("codex-approval-monitor: persisted Codex notice dismissal",
			"path", path, "added", report.NoticesAdded, "already_present", report.NoticesExisting)
	}
}
