package session

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

const exitLaunchBarrierPolls = 3000 // 30s at 10ms: bounded parent-failure fallback

const (
	exitLaunchBarrierWindow  = 30 * time.Second
	exitLaunchStaleAfter     = 2 * exitLaunchBarrierWindow
	exitLaunchArtifactPrefix = "barrier-"
)

type exitLaunchGuard struct {
	sessionID       string
	tmuxSession     string
	generation      string
	token           string
	tokenHash       string
	barrierPath     string
	paneID          string
	callbackEnabled bool
	enabled         bool
	released        bool
}

func newExitLaunchGuard(sessionID, tmuxSession string) (*exitLaunchGuard, error) {
	if !validExitLaunchIdentifier(sessionID, 128) || !validExitLaunchIdentifier(tmuxSession, 64) {
		return nil, fmt.Errorf("invalid exit-launch identity")
	}
	dataDir := strings.TrimSpace(config.DataDir())
	if dataDir == "" {
		return nil, fmt.Errorf("resolve private exit-launch directory")
	}
	dir := filepath.Join(dataDir, "exit-launch")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create exit-launch directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("protect exit-launch directory: %w", err)
	}
	cleanupStaleExitLaunchArtifacts(dir, time.Now())
	f, err := os.CreateTemp(dir, exitLaunchArtifactPrefix)
	if err != nil {
		return nil, fmt.Errorf("create exit-launch barrier: %w", err)
	}
	path := f.Name()
	cleanup := func() { _ = os.Remove(path) }
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		cleanup()
		return nil, err
	}
	if _, err := f.WriteString("pending"); err != nil {
		_ = f.Close()
		cleanup()
		return nil, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return nil, err
	}
	generation, err := randomExitHex(16)
	if err != nil {
		cleanup()
		return nil, err
	}
	token, err := randomExitHex(32)
	if err != nil {
		cleanup()
		return nil, err
	}
	hash := sha256.Sum256([]byte(token))
	return &exitLaunchGuard{
		sessionID: sessionID, tmuxSession: tmuxSession,
		generation: generation, token: token, tokenHash: hex.EncodeToString(hash[:]),
		barrierPath: path, enabled: true,
	}, nil
}

func disabledExitLaunchGuard(sessionID, tmuxSession string) *exitLaunchGuard {
	return &exitLaunchGuard{sessionID: sessionID, tmuxSession: tmuxSession, released: true}
}

func randomExitHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// wrap keeps the managed program behind a private bounded gate. The pane shell
// may perform existing launch proof checks outside this wrapper, but it cannot
// start the harness/shell until the parent has installed the pane-local hook,
// persisted the exact launch binding, and released this file.
func (g *exitLaunchGuard) wrap(cmd string) string {
	if g == nil || !g.enabled {
		return cmd
	}
	path := clcommon.ShellQuoteArg(g.barrierPath)
	return "tclaude_exit_gate=" + path + "; tclaude_exit_i=0; " +
		"while [ \"$(cat \"$tclaude_exit_gate\" 2>/dev/null)\" = pending ] && " +
		"[ \"$tclaude_exit_i\" -lt " + strconv.Itoa(exitLaunchBarrierPolls) + " ]; do " +
		"tclaude_exit_i=$((tclaude_exit_i + 1)); sleep 0.01; done; " +
		"tclaude_exit_gate_state=$(cat \"$tclaude_exit_gate\" 2>/dev/null); " +
		"rm -f -- \"$tclaude_exit_gate\"; " +
		"if [ \"$tclaude_exit_gate_state\" != go ]; then " +
		"printf '%s\\n' 'tclaude: managed pane exit-audit launch gate timed out or was cancelled' >&2; exit 125; fi; " + cmd
}

// armPaneHook installs only a pane-local hook. Failure is a supported
// degradation (older tmux): the launch proceeds and the daemon reaper records
// an explicit disappeared/unknown observation instead.
func (g *exitLaunchGuard) armPaneHook() {
	if g == nil || !g.enabled {
		return
	}
	paneID, err := livePaneIdentity(g.tmuxSession)
	if err != nil {
		slog.Warn("tmux exit audit unavailable; pane identity probe failed",
			"session_id", g.sessionID, "tmux_session", g.tmuxSession, "error", err)
		return
	}
	g.paneID = paneID
	hook, err := g.hookCommand()
	if err != nil {
		slog.Warn("tmux exit audit unavailable; callback command unresolved",
			"session_id", g.sessionID, "tmux_session", g.tmuxSession, "error", err)
		return
	}
	target := clcommon.ExactTarget(g.tmuxSession) + ":0.0"
	if err := clcommon.TmuxCommand("set-hook", "-p", "-t", target, "pane-exited", hook).Run(); err != nil {
		slog.Warn("tmux exit audit unavailable; pane-local hook unsupported",
			"session_id", g.sessionID, "tmux_session", g.tmuxSession, "pane_id", paneID, "error", err)
		return
	}
	g.callbackEnabled = true
	g.token = ""
}

func (g *exitLaunchGuard) hookCommand() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	args := []string{
		clcommon.ShellQuoteArg(exe), "session", "exit-callback",
		"--session-id", clcommon.ShellQuoteArg(g.sessionID),
		"--tmux-session", clcommon.ShellQuoteArg(g.tmuxSession),
		"--pane-id", clcommon.ShellQuoteArg("#{pane_id}"),
		"--generation", clcommon.ShellQuoteArg(g.generation),
		"--token", clcommon.ShellQuoteArg(g.token),
		"--exit-code", clcommon.ShellQuoteArg("#{pane_dead_status}"),
		"--signal", clcommon.ShellQuoteArg("#{pane_dead_signal}"),
	}
	// The hook value is a tmux command, not a shell command. run-shell receives
	// one quoted command string; tmux expands only its own bounded formats.
	return "run-shell " + clcommon.ShellQuoteArg(strings.Join(args, " ")), nil
}

func (g *exitLaunchGuard) bindAndRelease() error {
	if g == nil || !g.enabled {
		return nil
	}
	var bindErr error
	if g.callbackEnabled {
		bindErr = db.SetSessionExitLaunchBinding(g.sessionID, g.generation, g.tokenHash, g.paneID)
	} else {
		bindErr = db.SetSessionExitLaunchGeneration(g.sessionID, g.generation)
	}
	g.token = ""
	if bindErr != nil {
		// Audit persistence must never prevent the established launch. Remove any
		// partial authority, release the private gate, and let the reaper remain
		// the behavior-preserving fallback.
		_ = db.ClearSessionExitLaunchBinding(g.sessionID)
		g.callbackEnabled = false
		slog.Warn("exit audit: launch binding unavailable; continuing without callback",
			"session_id", g.sessionID, "tmux_session", g.tmuxSession, "error", bindErr)
	}
	if err := os.WriteFile(g.barrierPath, []byte("go"), 0o600); err != nil {
		return fmt.Errorf("release exit-launch barrier: %w", err)
	}
	g.released = true
	time.AfterFunc(exitLaunchBarrierWindow+time.Second, func() {
		if err := os.Remove(g.barrierPath); err == nil {
			slog.Warn("exit audit: removed unconsumed launch barrier after bounded window",
				"session_id", g.sessionID, "tmux_session", g.tmuxSession)
		}
	})
	return nil
}

func (g *exitLaunchGuard) abort() {
	if g == nil || !g.enabled || g.released {
		return
	}
	_ = os.WriteFile(g.barrierPath, []byte("abort"), 0o600)
	_ = os.Remove(g.barrierPath)
	g.token = ""
	g.released = true
}

func cleanupStaleExitLaunchArtifacts(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), exitLaunchArtifactPrefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < exitLaunchStaleAfter {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if err := os.Remove(path); err == nil {
			slog.Warn("exit audit: removed stale launch barrier", "artifact", entry.Name())
		}
	}
}

func livePaneIdentity(tmuxSession string) (string, error) {
	out, err := clcommon.TmuxCommand("display-message", "-p", "-t",
		clcommon.ExactTarget(tmuxSession)+":0.0", "#{pane_id}").Output()
	if err != nil {
		return "", err
	}
	paneID := strings.TrimSpace(string(out))
	if !validCallbackPaneID(paneID) {
		return "", fmt.Errorf("invalid pane id")
	}
	return paneID, nil
}

type exitCallbackParams struct {
	SessionID   string
	TmuxSession string
	PaneID      string
	Generation  string
	Token       string
	ExitCode    string
	Signal      string
}

func exitCallbackCmd() *cobra.Command {
	var p exitCallbackParams
	cmd := &cobra.Command{
		Use:    "exit-callback",
		Short:  "Record a managed tmux pane exit (internal)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runExitCallback(p)
		},
	}
	cmd.Flags().StringVar(&p.SessionID, "session-id", "", "internal session id")
	cmd.Flags().StringVar(&p.TmuxSession, "tmux-session", "", "internal tmux session")
	cmd.Flags().StringVar(&p.PaneID, "pane-id", "", "internal pane id")
	cmd.Flags().StringVar(&p.Generation, "generation", "", "internal launch generation")
	cmd.Flags().StringVar(&p.Token, "token", "", "internal callback token")
	cmd.Flags().StringVar(&p.ExitCode, "exit-code", "", "internal exit code")
	cmd.Flags().StringVar(&p.Signal, "signal", "", "internal signal")
	return cmd
}

func runExitCallback(p exitCallbackParams) error {
	if !validCallbackPaneID(p.PaneID) || !validExitLaunchIdentifier(p.SessionID, 128) ||
		!validExitLaunchIdentifier(p.TmuxSession, 64) ||
		!validCallbackHex(p.Generation, 32) || !validCallbackHex(p.Token, 64) {
		return fmt.Errorf("%w: malformed callback", db.ErrExitCallbackRejected)
	}
	reported, err := inspectDeadTmuxPane(p.PaneID)
	if err != nil {
		return fmt.Errorf("%w: verify dead pane: %v", db.ErrExitCallbackRejected, err)
	}
	if reported.TmuxSession != p.TmuxSession || reported.PaneID != p.PaneID ||
		reported.ExitCode != p.ExitCode || !strings.EqualFold(reported.Signal, p.Signal) {
		return fmt.Errorf("%w: tmux evidence mismatch", db.ErrExitCallbackRejected)
	}
	var code *int
	cause := db.AgentExitCauseUnknown
	if p.Signal != "" {
		cause = db.AgentExitCauseSignal
	} else if p.ExitCode != "" {
		n, err := strconv.Atoi(p.ExitCode)
		if err != nil || n < 0 || n > 255 {
			return fmt.Errorf("%w: invalid exit code", db.ErrExitCallbackRejected)
		}
		code = &n
		cause = db.AgentExitCauseNormal
	}
	hash := sha256.Sum256([]byte(p.Token))
	_, err = db.RecordAuthenticatedAgentExitObservation(db.AgentExitObservation{
		SessionID: p.SessionID, TmuxSession: p.TmuxSession, PaneID: p.PaneID,
		Observer: db.AgentExitObserverTmux, CauseKind: cause,
		ExitCode: code, Signal: strings.ToUpper(p.Signal), ObservedState: StatusExited,
	}, db.ExitCallbackAuth{
		Generation: p.Generation, TokenHash: hex.EncodeToString(hash[:]), PaneID: p.PaneID,
	})
	if errors.Is(err, db.ErrExitCallbackRejected) {
		return err
	}
	return err
}

type deadTmuxPane struct {
	TmuxSession string
	PaneID      string
	ExitCode    string
	Signal      string
}

func inspectDeadTmuxPane(paneID string) (deadTmuxPane, error) {
	if !validCallbackPaneID(paneID) {
		return deadTmuxPane{}, fmt.Errorf("invalid pane id")
	}
	const format = "#{session_name}|#{pane_id}|#{pane_dead}|#{pane_dead_status}|#{pane_dead_signal}"
	out, err := clcommon.TmuxCommand("display-message", "-p", "-t", paneID, format).Output()
	if err != nil {
		return deadTmuxPane{}, err
	}
	parts := strings.Split(strings.TrimSpace(string(out)), "|")
	if len(parts) != 5 || parts[2] != "1" || parts[1] != paneID {
		return deadTmuxPane{}, fmt.Errorf("pane is not the exact dead pane")
	}
	if parts[3] != "" && parts[4] != "" {
		return deadTmuxPane{}, fmt.Errorf("tmux reported both status and signal")
	}
	if parts[3] != "" {
		n, err := strconv.Atoi(parts[3])
		if err != nil || n < 0 || n > 255 {
			return deadTmuxPane{}, fmt.Errorf("invalid tmux exit status")
		}
	}
	if parts[4] != "" {
		if len(parts[4]) > 16 {
			return deadTmuxPane{}, fmt.Errorf("invalid tmux signal")
		}
		for _, r := range parts[4] {
			if (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') &&
				(r < '0' || r > '9') && r != '_' {
				return deadTmuxPane{}, fmt.Errorf("invalid tmux signal")
			}
		}
	}
	return deadTmuxPane{
		TmuxSession: parts[0], PaneID: parts[1], ExitCode: parts[3], Signal: strings.ToUpper(parts[4]),
	}, nil
}

func validCallbackPaneID(v string) bool {
	if len(v) < 2 || len(v) > 12 || v[0] != '%' {
		return false
	}
	for _, r := range v[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func validCallbackHex(v string, n int) bool {
	if len(v) != n {
		return false
	}
	for _, r := range v {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func validExitLaunchIdentifier(v string, max int) bool {
	if v == "" || len(v) > max {
		return false
	}
	for _, r := range v {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || strings.ContainsRune("_-.", r) {
			continue
		}
		return false
	}
	return true
}
