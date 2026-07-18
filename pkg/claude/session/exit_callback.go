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
	"sync/atomic"
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
	paneExitGenerationOption = "@tclaude_exit_generation"
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

func newExitLaunchGuard(sessionID, tmuxSession, generation string) (*exitLaunchGuard, error) {
	if !validExitLaunchIdentifier(sessionID, 128) || !validExitLaunchIdentifier(tmuxSession, 64) {
		return nil, fmt.Errorf("invalid exit-launch identity")
	}
	if !validCallbackHex(generation, 32) {
		return nil, fmt.Errorf("invalid exit-launch generation")
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

func disabledExitLaunchGuard(sessionID, tmuxSession, generation string) *exitLaunchGuard {
	return &exitLaunchGuard{
		sessionID: sessionID, tmuxSession: tmuxSession, generation: generation,
		released: true,
	}
}

var exitGenerationFallbackCounter atomic.Uint64
var exitRandomRead = rand.Read

// newExitLaunchGeneration creates non-secret launch identity independently of
// the private barrier/token setup. crypto/rand is preferred; the hash fallback
// remains fresh enough to invalidate predecessor authority even on RNG failure.
func newExitLaunchGeneration(sessionID, tmuxSession string) string {
	if generation, err := randomExitHex(16); err == nil {
		return generation
	}
	seed := fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%d", sessionID, tmuxSession,
		time.Now().UnixNano(), os.Getpid(), exitGenerationFallbackCounter.Add(1))
	hash := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(hash[:16])
}

func randomExitHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := exitRandomRead(b); err != nil {
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
	if !nativePaneDiedHookAvailable() {
		slog.Warn("tmux exit audit unavailable; native pane-died hook unsupported",
			"session_id", g.sessionID, "tmux_session", g.tmuxSession, "pane_id", paneID)
		return
	}
	hook, err := g.hookCommand()
	if err != nil {
		slog.Warn("tmux exit audit unavailable; callback command unresolved",
			"session_id", g.sessionID, "tmux_session", g.tmuxSession, "error", err)
		return
	}
	target := clcommon.ExactTarget(g.tmuxSession) + ":0.0"
	if err := clcommon.TmuxCommand("set-option", "-p", "-t", target, "remain-on-exit", "on").Run(); err != nil {
		slog.Warn("tmux exit audit unavailable; remain-on-exit unsupported",
			"session_id", g.sessionID, "tmux_session", g.tmuxSession, "pane_id", paneID, "error", err)
		return
	}
	if err := clcommon.TmuxCommand("set-option", "-p", "-t", target,
		paneExitGenerationOption, g.generation).Run(); err != nil {
		_ = clcommon.TmuxCommand("set-option", "-p", "-t", target, "remain-on-exit", "off").Run()
		slog.Warn("tmux exit audit unavailable; pane launch identity unsupported",
			"session_id", g.sessionID, "tmux_session", g.tmuxSession, "pane_id", paneID, "error", err)
		return
	}
	if err := clcommon.TmuxCommand("set-hook", "-p", "-t", target, "pane-died", hook).Run(); err != nil {
		_ = clcommon.TmuxCommand("set-option", "-p", "-u", "-t", target, paneExitGenerationOption).Run()
		_ = clcommon.TmuxCommand("set-option", "-p", "-t", target, "remain-on-exit", "off").Run()
		slog.Warn("tmux exit audit unavailable; pane-local hook unsupported",
			"session_id", g.sessionID, "tmux_session", g.tmuxSession, "pane_id", paneID, "error", err)
		return
	}
	g.callbackEnabled = true
	g.token = ""
}

func nativePaneDiedHookAvailable() bool {
	out, err := clcommon.TmuxCommand("show-hooks", "-g", "pane-died").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == "pane-died" {
			return true
		}
	}
	return false
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
	callback := "run-shell " + clcommon.ShellQuoteArg(strings.Join(args, " "))
	tmuxPrefix := "tmux -L " + clcommon.ShellQuoteArg(clcommon.TmuxSocketName)
	paneTarget := clcommon.ShellQuoteArg(g.paneID)
	sessionTarget := clcommon.ShellQuoteArg(clcommon.ExactTarget(g.tmuxSession))
	expected := clcommon.ShellQuoteArg(g.tmuxSession + "|" + g.paneID + "|1|" + g.generation)
	probe := tmuxPrefix + " display-message -p -t " + paneTarget +
		" '##{session_name}|##{pane_id}|##{pane_dead}|##{" + paneExitGenerationOption + "}' 2>/dev/null"
	watchdogShell := "sleep 30; tclaude_exit_expected=" + expected + "; tclaude_exit_i=0; " +
		"while [ \"$tclaude_exit_i\" -lt 3 ]; do " +
		"tclaude_exit_current=$(" + probe + ") || exit 0; " +
		"[ \"$tclaude_exit_current\" = \"$tclaude_exit_expected\" ] || exit 0; " +
		tmuxPrefix + " kill-pane -t " + paneTarget + " && exit 0; " +
		"tclaude_exit_i=$((tclaude_exit_i + 1)); done; " +
		"tclaude_exit_current=$(" + probe + ") || exit 0; " +
		"if [ \"$tclaude_exit_current\" = \"$tclaude_exit_expected\" ]; then " +
		tmuxPrefix + " kill-session -t " + sessionTarget +
		" || printf '%s\\n' 'tclaude: retained dead pane cleanup failed after bounded retries' >&2; fi"
	watchdog := "run-shell -b " + clcommon.ShellQuoteArg(watchdogShell)
	// The hook value is a tmux command list. The bounded watchdog is armed
	// first, then the authenticated callback records before removing the pane.
	return watchdog + "; " + callback, nil
}

func (g *exitLaunchGuard) bind() {
	if g == nil || !g.enabled {
		return
	}
	var bindErr error
	if g.callbackEnabled {
		bindErr = db.SetSessionExitLaunchBinding(g.sessionID, g.generation, g.tokenHash, g.paneID)
	}
	g.token = ""
	if bindErr != nil {
		// Audit persistence must never prevent the established launch. Remove any
		// partial authority, release the private gate, and let the reaper remain
		// the behavior-preserving fallback.
		_ = db.ClearSessionExitLaunchBinding(g.sessionID, g.generation)
		g.disarmPaneHook()
		slog.Warn("exit audit: launch binding unavailable; continuing without callback",
			"session_id", g.sessionID, "tmux_session", g.tmuxSession, "error", bindErr)
	}
}

func (g *exitLaunchGuard) release() error {
	if g == nil || !g.enabled {
		return nil
	}
	releaseStateDurable := true
	if err := db.MarkSessionExitLaunchReleased(g.sessionID, g.generation); err != nil {
		releaseStateDurable = false
		if fallbackErr := db.MarkSessionExitLaunchUngated(g.sessionID, g.generation); fallbackErr != nil {
			slog.Warn("exit audit: launch release state unavailable; continuing launch",
				"session_id", g.sessionID, "tmux_session", g.tmuxSession, "error", err)
		}
	}
	// Make the durable phase visible before the pane can pass the file gate.
	// Otherwise a very short-lived runtime could callback while the row still
	// says pending and be misclassified as a pre-harness launch failure.
	if err := os.WriteFile(g.barrierPath, []byte("go"), 0o600); err != nil {
		if releaseStateDurable {
			if stateErr := db.MarkSessionExitLaunchPending(g.sessionID, g.generation); stateErr != nil {
				slog.Warn("exit audit: could not restore pre-harness state after gate release failure",
					"session_id", g.sessionID, "tmux_session", g.tmuxSession, "error", stateErr)
			}
		}
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
	g.disarmPaneHook()
	g.token = ""
	g.released = true
}

func (g *exitLaunchGuard) disarmPaneHook() {
	if g == nil || !g.callbackEnabled || g.tmuxSession == "" {
		return
	}
	target := clcommon.ExactTarget(g.tmuxSession) + ":0.0"
	_ = clcommon.TmuxCommand("set-hook", "-p", "-u", "-t", target, "pane-died").Run()
	_ = clcommon.TmuxCommand("set-option", "-p", "-u", "-t", target, paneExitGenerationOption).Run()
	_ = clcommon.TmuxCommand("set-option", "-p", "-t", target, "remain-on-exit", "off").Run()
	g.callbackEnabled = false
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
		reported.Generation != p.Generation ||
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
	if err != nil {
		slog.Warn("exit audit: callback could not record managed pane exit; retained evidence left for bounded recovery",
			"session_id", p.SessionID, "tmux_session", p.TmuxSession,
			"pane_id", p.PaneID, "error", err)
		return fmt.Errorf("record managed pane exit: %w", err)
	}
	cleanupEvidence := PaneExitEvidence{
		TmuxSession: reported.TmuxSession, PaneID: reported.PaneID,
		Generation: p.Generation, ExitCode: code, Signal: strings.ToUpper(reported.Signal),
	}
	if err := CleanupDeadTmuxPane(cleanupEvidence); err != nil {
		slog.Warn("exit audit: callback recorded but dead pane cleanup failed",
			"session_id", p.SessionID, "tmux_session", p.TmuxSession,
			"pane_id", p.PaneID, "error", err)
		return fmt.Errorf("clean recorded dead pane: %w", err)
	}
	return nil
}

type deadTmuxPane struct {
	TmuxSession string
	PaneID      string
	ExitCode    string
	Signal      string
	Generation  string
}

// PaneExitEvidence is tmux's direct evidence for the retained pane bootstrap.
// ExitCode and Signal are mutually exclusive; neither is inferred from a child
// process or shell convention.
type PaneExitEvidence struct {
	TmuxSession string
	PaneID      string
	Generation  string
	ExitCode    *int
	Signal      string
}

func InspectDeadTmuxSessionPane(tmuxSession string) (PaneExitEvidence, error) {
	if !validExitLaunchIdentifier(tmuxSession, 64) {
		return PaneExitEvidence{}, fmt.Errorf("invalid tmux session")
	}
	const format = "#{session_name}|#{pane_id}|#{pane_dead}|#{pane_dead_status}|#{pane_dead_signal}|#{@tclaude_exit_generation}"
	out, err := clcommon.TmuxCommand("display-message", "-p", "-t",
		clcommon.ExactTarget(tmuxSession)+":0.0", format).Output()
	if err != nil {
		return PaneExitEvidence{}, err
	}
	dead, err := parseDeadTmuxPane(strings.TrimSpace(string(out)), "")
	if err != nil {
		return PaneExitEvidence{}, err
	}
	if dead.TmuxSession != tmuxSession {
		return PaneExitEvidence{}, fmt.Errorf("tmux session evidence mismatch")
	}
	var code *int
	if dead.ExitCode != "" {
		n, _ := strconv.Atoi(dead.ExitCode)
		code = &n
	}
	return PaneExitEvidence{
		TmuxSession: dead.TmuxSession, PaneID: dead.PaneID,
		Generation: dead.Generation, ExitCode: code, Signal: dead.Signal,
	}, nil
}

// CleanupDeadTmuxPane removes a retained corpse without ever killing a pane
// that has since been respawned. Repeated kill-pane failures fall back to the
// exact managed session only while the same pane still reports dead.
func CleanupDeadTmuxPane(evidence PaneExitEvidence) error {
	if !validExitLaunchIdentifier(evidence.TmuxSession, 64) || !validCallbackPaneID(evidence.PaneID) {
		return fmt.Errorf("invalid dead pane cleanup target")
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		current, err := InspectDeadTmuxSessionPane(evidence.TmuxSession)
		if err != nil {
			if clcommon.TmuxCommand("has-session", "-t", clcommon.ExactTarget(evidence.TmuxSession)).Run() != nil {
				return nil
			}
			return fmt.Errorf("refuse cleanup without current dead-pane proof: %w", err)
		}
		if !samePaneExitEvidence(current, evidence) {
			return fmt.Errorf("refuse cleanup after pane exit identity changed")
		}
		lastErr = clcommon.TmuxCommand("kill-pane", "-t", clcommon.ExactTarget(evidence.PaneID)).Run()
		if lastErr == nil {
			return nil
		}
	}
	current, err := InspectDeadTmuxSessionPane(evidence.TmuxSession)
	if err != nil || !samePaneExitEvidence(current, evidence) {
		return fmt.Errorf("dead pane cleanup failed safely: %w", lastErr)
	}
	if err := clcommon.TmuxCommand("kill-session", "-t", clcommon.ExactTarget(evidence.TmuxSession)).Run(); err != nil {
		if clcommon.TmuxCommand("has-session", "-t", clcommon.ExactTarget(evidence.TmuxSession)).Run() != nil {
			return nil
		}
		return fmt.Errorf("dead pane cleanup fallback failed: %w", err)
	}
	return nil
}

func samePaneExitEvidence(current, expected PaneExitEvidence) bool {
	if current.TmuxSession != expected.TmuxSession || current.PaneID != expected.PaneID ||
		current.Signal != expected.Signal || !intEqual(current.ExitCode, expected.ExitCode) {
		return false
	}
	return expected.Generation == "" || current.Generation == expected.Generation
}

func intEqual(a, b *int) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func inspectDeadTmuxPane(paneID string) (deadTmuxPane, error) {
	if !validCallbackPaneID(paneID) {
		return deadTmuxPane{}, fmt.Errorf("invalid pane id")
	}
	const format = "#{session_name}|#{pane_id}|#{pane_dead}|#{pane_dead_status}|#{pane_dead_signal}|#{@tclaude_exit_generation}"
	out, err := clcommon.TmuxCommand("display-message", "-p", "-t", paneID, format).Output()
	if err != nil {
		return deadTmuxPane{}, err
	}
	return parseDeadTmuxPane(strings.TrimSpace(string(out)), paneID)
}

func parseDeadTmuxPane(output, expectedPaneID string) (deadTmuxPane, error) {
	parts := strings.Split(output, "|")
	if len(parts) != 6 || parts[2] != "1" || !validCallbackPaneID(parts[1]) ||
		(expectedPaneID != "" && parts[1] != expectedPaneID) {
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
	if parts[5] != "" && !validCallbackHex(parts[5], 32) {
		return deadTmuxPane{}, fmt.Errorf("invalid tmux launch generation")
	}
	return deadTmuxPane{
		TmuxSession: parts[0], PaneID: parts[1], ExitCode: parts[3], Signal: strings.ToUpper(parts[4]),
		Generation: parts[5],
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
