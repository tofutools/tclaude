//go:build linux

package terminal

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// resolveTerminalLauncher detects the terminal to use once (see
// terminal.go's Resolve). Under WSL it locates Windows Terminal; on
// native Linux it walks candidates looking for an installed emulator.
// The display check is deliberately left to launch time — it is a
// cheap env read, not a lookup, and the display can come up after the
// daemon starts.
func resolveTerminalLauncher(candidates []string) (string, launcher, error) {
	if isWSL() {
		wt := findWindowsTerminal()
		id := "cmd.exe"
		if wt != "" {
			id = "windows-terminal"
		}
		return id, func(command string) error {
			argv := wslCmdArgv(command, wt)
			return startDetached(exec.Command(argv[0], argv[1:]...))
		}, nil
	}

	id, binPath, argvFn, err := resolveLinuxTerminal(candidates, exec.LookPath)
	if err != nil {
		return "", nil, err
	}
	return id, func(command string) error {
		if derr := haveDisplay(); derr != nil {
			return derr
		}
		return startDetached(exec.Command(binPath, argvFn(command)...))
	}, nil
}

// isWSL detects if we're running in Windows Subsystem for Linux.
func isWSL() bool {
	data, err := os.ReadFile("/proc/version")
	if err != nil {
		return false
	}
	lower := strings.ToLower(string(data))
	return strings.Contains(lower, "microsoft") || strings.Contains(lower, "wsl")
}

// wslCmdArgv builds the argv that launches a terminal under WSL. When
// Windows Terminal is available it gets a new tab; otherwise cmd.exe
// opens a fresh window. Either way the WSL-side command is passed to
// `wsl -e sh -c <command>` as a single argv element, so the shell
// quoting baked into command (from openShellCmd / openAttachCmd)
// survives without a second round of word-splitting. Pure — split out
// from openWSL so the argv can be unit-tested.
func wslCmdArgv(command, wtPath string) []string {
	if wtPath != "" {
		// wt.exe new-tab wsl -e sh -c "<command>"
		return []string{wtPath, "new-tab", "wsl", "-e", "sh", "-c", command}
	}
	// Fallback: cmd.exe opens a window that then runs wsl.
	return []string{"cmd.exe", "/c", "start", "wsl", "-e", "sh", "-c", command}
}

// findWindowsTerminal looks for Windows Terminal executable.
func findWindowsTerminal() string {
	// Try PATH first
	if path, err := exec.LookPath("wt.exe"); err == nil {
		return path
	}

	// Common paths for Windows Terminal
	user := os.Getenv("USER")
	if user == "" {
		user = os.Getenv("LOGNAME")
	}

	basePaths := []string{
		filepath.Join("/mnt/c/Users", user, "AppData/Local/Microsoft/WindowsApps/wt.exe"),
	}

	for _, p := range basePaths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}

	// Try glob for WindowsApps
	pattern := filepath.Join("/mnt/c/Users", user, "AppData/Local/Microsoft/WindowsApps/wt.exe")
	if matches, _ := filepath.Glob(pattern); len(matches) > 0 {
		return matches[0]
	}

	return ""
}

// lookPathFunc is the exec.LookPath seam. Production passes
// exec.LookPath; tests pass a fake that "installs" a chosen subset of
// terminals so buildLinuxTerminalArgv can be exercised without the
// machine's real PATH.
type lookPathFunc func(string) (string, error)

// linuxTerminals maps a canonical terminal ID (terminal.go) to the argv
// that follows the terminal binary. The binary name is the ID itself.
//
// Every entry hands the command to `sh -c` as three *separate* argv
// elements — "sh", "-c", command — never as one re-quoted string. This
// matters: command already carries shell quoting from openShellCmd /
// openAttachCmd (paths and labels run through shellSingleQuote), so a
// second word-splitting pass would mangle it. The historical
// xfce4-terminal entry did exactly that — `-e "sh -c '"+command+"'"` —
// and broke on every command, because command always contains single
// quotes. terminal_linux_test.go's containsExact check guards against
// that regression returning.
//
// The flags differ because the CLIs differ:
//   - `--`  gnome-terminal: everything after is the program + its args.
//   - `-e`  xterm / konsole / alacritty / ghostty / x-terminal-emulator:
//     the xterm convention — program + args follow as separate words.
//     Must be the last option, so nothing trails "sh -c <command>".
//   - `-x`  xfce4-terminal: like `-e` but consumes the *remaining* args
//     as the program + args. xfce4-terminal's own `-e` wants a single
//     string and is the trap described above; `-x` avoids it.
//   - kitty / foot take the program + args directly, no flag. No `--`
//     prefix: the program here is "sh" (no leading dash), so option
//     parsing stops cleanly at it, and not every kitty/foot version
//     accepts a bare `--`.
//   - wezterm runs through its `start` subcommand, `--` ending start's
//     own options.
var linuxTerminals = map[string]func(command string) []string{
	IDGhostty:       func(c string) []string { return []string{"-e", "sh", "-c", c} },
	IDKitty:         func(c string) []string { return []string{"sh", "-c", c} },
	IDWezterm:       func(c string) []string { return []string{"start", "--", "sh", "-c", c} },
	IDAlacritty:     func(c string) []string { return []string{"-e", "sh", "-c", c} },
	IDFoot:          func(c string) []string { return []string{"sh", "-c", c} },
	IDKonsole:       func(c string) []string { return []string{"-e", "sh", "-c", c} },
	IDGnomeTerminal: func(c string) []string { return []string{"--", "sh", "-c", c} },
	IDXfce4Terminal: func(c string) []string { return []string{"-x", "sh", "-c", c} },
	IDXTermEmulator: func(c string) []string { return []string{"-e", "sh", "-c", c} },
	IDXterm:         func(c string) []string { return []string{"-e", "sh", "-c", c} },
}

// resolveLinuxTerminal walks candidates (the priority-ordered IDs from
// orderedCandidates) and returns the first one that is both launchable
// on Linux and installed (per lookPath): its ID, its absolute binary
// path, and the argv-builder for it. IDs with no Linux launcher — e.g.
// iterm2, terminal-app — are skipped. Returns an error naming the
// launchable candidates it tried when none is installed. Pure apart
// from lookPath, so it carries the unit-test coverage for terminal
// selection.
func resolveLinuxTerminal(candidates []string, lookPath lookPathFunc) (id, binPath string, argvFn func(string) []string, err error) {
	var tried []string
	for _, cand := range candidates {
		fn, ok := linuxTerminals[cand]
		if !ok {
			continue // not launchable on Linux (e.g. iterm2, terminal-app)
		}
		tried = append(tried, cand)
		if path, lerr := lookPath(cand); lerr == nil {
			return cand, path, fn, nil
		}
	}
	return "", "", nil, fmt.Errorf("no terminal emulator found (tried: %s)", strings.Join(tried, ", "))
}

// haveDisplay reports whether a graphical display is reachable. The
// terminal inherits agentd's environment, so when agentd was started
// outside a desktop session — a bare systemd service, an SSH shell —
// neither DISPLAY (X11) nor WAYLAND_DISPLAY is set and the terminal
// would fail to map a window. exec.Command(...).Start() still succeeds
// in that case (the fork/exec works; the child dies later connecting to
// the display), so without this check the failure is silent. Turning it
// into an up-front error gives the /api/term caller a 500 with a real
// reason and the auto-focus caller a meaningful log line.
func haveDisplay() error {
	if os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "" {
		return nil
	}
	return fmt.Errorf("no graphical display: neither DISPLAY nor WAYLAND_DISPLAY is set " +
		"(agentd may have been started outside a desktop session)")
}
