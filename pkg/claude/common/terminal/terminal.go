// Package terminal provides cross-platform terminal launching
// capabilities — opening a new terminal window that runs a given
// command, used by agentd's spawn auto-focus and shell-attach features.
//
// OpenWithCommand is implemented per-platform in terminal_*.go. This
// file holds the platform-agnostic parts: the catalogue of terminal
// emulators tclaude knows how to launch, their auto-detect priority,
// and the human's explicit preference.
//
// Terminal selection resolves in three tiers (highest first):
//
//  1. an explicit --terminal flag on `tclaude agentd serve`
//  2. the `terminal` field in ~/.tclaude/config.json
//  3. terminalPriority — auto-detect, trying the candidates in order
//
// Tiers 1 and 2 funnel through SetPreferred, which simply splices the
// chosen terminal to the front of the tier-3 order. So an unavailable
// preference degrades gracefully to auto-detect rather than failing.
package terminal

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
)

// Canonical terminal IDs. These are the values the --terminal flag and
// the config `terminal` field accept, and the keys of each platform's
// launcher registry. Kept lowercase and hyphenated so they double as
// human-typeable names.
const (
	IDGhostty       = "ghostty"
	IDKitty         = "kitty"
	IDWezterm       = "wezterm"
	IDAlacritty     = "alacritty"
	IDFoot          = "foot"
	IDITerm2        = "iterm2"
	IDKonsole       = "konsole"
	IDGnomeTerminal = "gnome-terminal"
	IDXfce4Terminal = "xfce4-terminal"
	IDXTermEmulator = "x-terminal-emulator"
	IDXterm         = "xterm"
	IDTerminalApp   = "terminal-app"
)

// terminalPriority is the auto-detect order, most-preferred first,
// shared by every platform. The guiding principle (and the behaviour
// the user asked for): a terminal someone went out of their way to
// install — Ghostty, kitty, WezTerm, Alacritty — is almost certainly
// their daily driver, so it outranks a desktop-environment default
// (Konsole, GNOME Terminal) which in turn outranks the generic
// last-resort fallbacks (x-terminal-emulator, xterm, Terminal.app).
//
// The list is cross-platform: each platform's registry only has
// launchers for the subset that runs there, so the IDs it can't launch
// are simply skipped. iTerm2 sits below the GPU terminals on purpose —
// "has kitty and iTerm2 → wants kitty".
var terminalPriority = []string{
	IDGhostty,
	IDKitty,
	IDWezterm,
	IDAlacritty,
	IDFoot,
	IDITerm2,
	IDKonsole,
	IDGnomeTerminal,
	IDXfce4Terminal,
	IDXTermEmulator,
	IDXterm,
	IDTerminalApp,
}

// terminalAliases maps friendly spellings a human might type for
// --terminal / the config field onto canonical IDs.
var terminalAliases = map[string]string{
	"iterm":        IDITerm2,
	"iterm2":       IDITerm2,
	"terminal":     IDTerminalApp,
	"terminal.app": IDTerminalApp,
	"terminalapp":  IDTerminalApp,
	"apple":        IDTerminalApp,
	"gnome":        IDGnomeTerminal,
	"xfce":         IDXfce4Terminal,
	"xfce4":        IDXfce4Terminal,
	"x-terminal":   IDXTermEmulator,
}

// CanonicalTerminalID normalises a user-supplied terminal name to a
// canonical ID, accepting both the IDs themselves and the friendly
// aliases. Returns "" when the name is unrecognised, so callers can
// warn instead of silently mis-selecting.
func CanonicalTerminalID(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return ""
	}
	if id, ok := terminalAliases[n]; ok {
		return id
	}
	if slices.Contains(terminalPriority, n) {
		return n
	}
	return ""
}

// KnownTerminalIDs returns every canonical terminal ID, in priority
// order — for --help text and "unknown terminal" diagnostics.
func KnownTerminalIDs() []string {
	return slices.Clone(terminalPriority)
}

// preferred is the human's terminal choice once SetPreferred records
// it. Process-wide and set once at startup (claude.go's PersistentPreRun
// for the config tier, agentd serve for the flag tier), so it is read
// without locking by OpenWithCommand.
var preferred string

// SetPreferred records the human's terminal preference. The name is
// canonicalised; an unrecognised name clears the preference, so
// selection falls back to plain auto-detect. Call once at process
// startup, before any OpenWithCommand.
func SetPreferred(name string) {
	preferred = CanonicalTerminalID(name)
}

// orderedCandidates returns the terminal IDs to try, in order: the
// preferred terminal (when one is set) first, then terminalPriority
// with the preferred ID de-duplicated out.
func orderedCandidates() []string {
	if preferred == "" {
		return slices.Clone(terminalPriority)
	}
	out := make([]string, 0, len(terminalPriority))
	out = append(out, preferred)
	for _, id := range terminalPriority {
		if id != preferred {
			out = append(out, id)
		}
	}
	return out
}

// launcher opens a new terminal window running command. It is the
// product of resolution: the terminal has already been picked and its
// binary located, so a launcher only builds the per-call argv and
// spawns it — no lookups.
type launcher func(command string) error

// resolveTerminalLauncher detects which terminal to use, walking
// candidates (the priority-ordered IDs from orderedCandidates), and
// returns a launcher bound to it plus a short ID for logging.
// Implemented per-platform in terminal_{linux,darwin,windows}.go. This
// is where the expensive work lives — exec.LookPath, .app bundle stats,
// osascript probes — so it runs exactly once (see Resolve).

// Resolution cache. Terminal detection is done once and the resulting
// launcher reused for every OpenWithCommand, so spawning an agent in a
// terminal never re-runs LookPath / bundle stats / osascript. Guarded
// by resolveMu; resolved flips true after the first resolution.
var (
	resolveMu      sync.Mutex
	resolved       bool
	resolvedID     string
	cachedLauncher launcher
	cachedErr      error
)

// Resolve performs terminal detection once and caches the result.
// agentd calls it at startup — after SetPreferred — so detection cost
// is paid then, not on the first (or every) agent spawn, and a
// detection failure surfaces in the startup log. Safe to call
// repeatedly and concurrently; only the first call detects. Returns
// the detection error, if any.
func Resolve() error {
	resolveMu.Lock()
	defer resolveMu.Unlock()
	if !resolved {
		resolvedID, cachedLauncher, cachedErr = resolveTerminalLauncher(orderedCandidates())
		resolved = true
	}
	return cachedErr
}

// ResolvedTerminal returns the ID of the terminal Resolve picked, or ""
// if resolution has not run or found nothing. For startup logging.
func ResolvedTerminal() string {
	resolveMu.Lock()
	defer resolveMu.Unlock()
	return resolvedID
}

// ErrRefusedInTest is the error OpenWithCommand returns when called
// from a test binary. Exposed so tests can pin the guard.
var ErrRefusedInTest = errors.New(
	"refusing to open a terminal window from a test binary; swap the caller's test seam instead")

// OpenWithCommand opens a new terminal window running command, using
// the terminal chosen by Resolve. If the process never called Resolve
// (e.g. the `tclaude` CLI rather than the daemon), the first call
// resolves lazily.
//
// Test binaries are refused unconditionally: tests exercise
// terminal-opening code paths through seams (agentd's openTerminal,
// session's linuxOpenTerminal), so a call that reaches this far under
// `go test` is a missing swap — and on a developer machine with a
// display it would pop a real terminal window onto the desktop
// (TCL-584). Refusing returns the same degraded "could not open"
// error path a headless host already exercises.
func OpenWithCommand(command string) error {
	if testing.Testing() {
		return ErrRefusedInTest
	}
	if err := Resolve(); err != nil {
		return err
	}
	return cachedLauncher(command)
}
