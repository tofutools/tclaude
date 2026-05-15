//go:build darwin

package terminal

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// darwinTerminal is one macOS terminal-emulator candidate.
type darwinTerminal struct {
	// detect reports whether the terminal is installed. For
	// CLI-launched terminals it also returns the absolute binary path;
	// the AppleScript-driven iTerm2 / Terminal.app return "".
	detect func() (binPath string, installed bool)
	// open launches the terminal running command. binPath is whatever
	// detect returned (ignored by the AppleScript terminals).
	open func(binPath, command string) error
}

// darwinTerminals maps canonical IDs (terminal.go) to their macOS
// launcher.
//
// iTerm2 and Terminal.app are driven through AppleScript: osascript
// types the command into a fresh default-profile window — the user's
// login shell — which sidesteps the launchd-PATH trap (homebrew
// binaries like tmux are unreachable from a daemon's minimal PATH).
//
// Ghostty / kitty / WezTerm / Alacritty have no comparable scripting
// surface, so they are launched via their CLI binary. To sidestep the
// very same trap the command is wrapped in a login shell — see
// loginShellArgv.
var darwinTerminals = map[string]darwinTerminal{
	IDGhostty:     {detect: bundleDetector("Ghostty", "ghostty"), open: cliOpener(ghosttyArgv)},
	IDKitty:       {detect: bundleDetector("kitty", "kitty"), open: cliOpener(kittyArgv)},
	IDWezterm:     {detect: bundleDetector("WezTerm", "wezterm"), open: cliOpener(weztermArgv)},
	IDAlacritty:   {detect: bundleDetector("Alacritty", "alacritty"), open: cliOpener(alacrittyArgv)},
	IDITerm2:      {detect: func() (string, bool) { return "", isAppInstalled("iTerm") }, open: openITerm2},
	IDTerminalApp: {detect: func() (string, bool) { return "", true }, open: openTerminalApp},
}

// resolveTerminalLauncher detects the macOS terminal to use once (see
// terminal.go's Resolve), walking candidates. Terminal.app always
// detects as present — it ships with every macOS — so resolution
// always succeeds.
func resolveTerminalLauncher(candidates []string) (string, launcher, error) {
	for _, id := range candidates {
		t, ok := darwinTerminals[id]
		if !ok {
			continue // not launchable on macOS (e.g. konsole, foot)
		}
		binPath, installed := t.detect()
		if !installed {
			continue
		}
		open, bp := t.open, binPath
		return id, func(command string) error { return open(bp, command) }, nil
	}
	return "", nil, fmt.Errorf("no terminal emulator found")
}

// bundleDetector returns a detect func for a CLI-launched terminal.
func bundleDetector(appName, binName string) func() (string, bool) {
	return func() (string, bool) {
		if p := findBundleBinary(appName, binName); p != "" {
			return p, true
		}
		return "", false
	}
}

// findBundleBinary returns the absolute path of a terminal's executable
// — PATH first (a user who symlinked the CLI), then the standard
// /Applications and ~/Applications .app bundle locations — or "" if it
// is not installed.
func findBundleBinary(appName, binName string) string {
	if p, err := exec.LookPath(binName); err == nil {
		return p
	}
	paths := []string{
		filepath.Join("/Applications", appName+".app", "Contents", "MacOS", binName),
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, "Applications", appName+".app", "Contents", "MacOS", binName))
	}
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

// cliOpener turns a pure argv builder into an open func that spawns the
// terminal detached — background-reaped, in its own session (see
// startDetached).
func cliOpener(argvFn func(binPath, command string) []string) func(string, string) error {
	return func(binPath, command string) error {
		argv := argvFn(binPath, command)
		return startDetached(exec.Command(argv[0], argv[1:]...))
	}
}

// loginShellArgv wraps command so it runs inside the user's login
// shell; the terminal launches this argv as its program.
//
// A macOS terminal launched straight from a daemon inherits launchd's
// minimal PATH — homebrew binaries (tmux, and on Apple Silicon
// everything under /opt/homebrew/bin) are missing. Routing through
// `$SHELL -l` sources the user's login profile (~/.zprofile etc., where
// `brew shellenv` lives), restoring the full PATH. /bin/sh is only a
// trampoline: it always exists and expands $SHELL from the inherited
// environment before exec-ing the real login shell.
func loginShellArgv(command string) []string {
	inner := `exec "${SHELL:-/bin/zsh}" -l -c ` + singleQuote(command)
	return []string{"/bin/sh", "-c", inner}
}

// singleQuote wraps s as a single POSIX-shell word.
func singleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ghosttyArgv / kittyArgv / weztermArgv / alacrittyArgv build the exec
// argv for each CLI terminal. The login-shell wrap is passed as
// separate argv elements — never re-quoted into a string a second
// word-splitting pass could mangle.
func ghosttyArgv(binPath, command string) []string {
	return append([]string{binPath, "-e"}, loginShellArgv(command)...)
}

func kittyArgv(binPath, command string) []string {
	return append([]string{binPath}, loginShellArgv(command)...)
}

func weztermArgv(binPath, command string) []string {
	return append([]string{binPath, "start", "--"}, loginShellArgv(command)...)
}

func alacrittyArgv(binPath, command string) []string {
	return append([]string{binPath, "-e"}, loginShellArgv(command)...)
}

// openITerm2 / openTerminalApp drive the two AppleScript terminals.
func openITerm2(_, command string) error {
	return runOsascript(iTermScript(command), "iTerm2", os.Getenv("TCLAUDE_DEBUG") != "")
}

func openTerminalApp(_, command string) error {
	return runOsascript(terminalAppScript(command), "Terminal.app", os.Getenv("TCLAUDE_DEBUG") != "")
}

// iTermScript builds the AppleScript that opens a new iTerm2 window with
// the default profile and types command into it. The default profile
// starts the user's login shell — full PATH/env — and `write text` then
// runs the command inside it. This deliberately avoids
// `create window with default profile command "..."`, which falls into
// the launchd-PATH trap where bare execs can't find homebrew binaries
// like tmux.
func iTermScript(command string) string {
	return `tell application "iTerm2"
	activate
	set newWindow to (create window with default profile)
	tell current session of newWindow
		write text "` + escapeAppleScript(command) + `"
	end tell
end tell`
}

// terminalAppScript builds the AppleScript that opens a new Terminal.app
// window running command via `do script` (keystroke-fed into a fresh shell).
func terminalAppScript(command string) string {
	return `tell application "Terminal"
	activate
	do script "` + escapeAppleScript(command) + `"
end tell`
}

// runOsascript executes an AppleScript via osascript, logging the script
// and its output when TCLAUDE_DEBUG is set.
func runOsascript(script, label string, debug bool) error {
	if debug {
		fmt.Printf("[debug] terminal: %s AppleScript: %s\n", label, strings.TrimSpace(script))
	}
	out, err := exec.Command("osascript", "-e", script).CombinedOutput()
	if debug {
		fmt.Printf("[debug] terminal: %s output=%q err=%v\n", label, strings.TrimSpace(string(out)), err)
	}
	return err
}

// isAppInstalled reports whether the named application bundle can be
// located on disk. `osascript -e 'id of application "Name"'` succeeds
// iff the app exists.
func isAppInstalled(appName string) bool {
	return exec.Command("osascript", "-e",
		`id of application "`+escapeAppleScript(appName)+`"`).Run() == nil
}

// escapeAppleScript escapes a string for safe interpolation inside an
// AppleScript double-quoted literal.
func escapeAppleScript(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	return s
}
