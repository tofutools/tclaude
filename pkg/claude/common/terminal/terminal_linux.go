//go:build linux

package terminal

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// OpenWithCommand opens a new terminal window running the given command.
func OpenWithCommand(command string) error {
	if isWSL() {
		return openWSL(command)
	}
	return openLinuxNative(command)
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

// openWSL opens a terminal in WSL environment via Windows Terminal or cmd.exe.
func openWSL(command string) error {
	argv := wslCmdArgv(command, findWindowsTerminal())
	return startDetached(exec.Command(argv[0], argv[1:]...))
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

// linuxTerminal is one terminal-emulator candidate: the binary name to
// look for, and how to turn a shell command into the argv that follows
// that binary.
type linuxTerminal struct {
	name string
	argv func(command string) []string
}

// linuxTerminals lists the terminal emulators openLinuxNative tries, in
// order of preference. x-terminal-emulator goes first so a Debian-style
// system honours the user's chosen default terminal.
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
//   - `-e`  xterm / konsole / alacritty / x-terminal-emulator: the
//     xterm convention — program + args follow as separate words. Must
//     be the last option, so nothing trails "sh -c <command>".
//   - `-x`  xfce4-terminal: like `-e` but consumes the *remaining* args
//     as the program + args. xfce4-terminal's own `-e` wants a single
//     string and is the trap described above; `-x` avoids it.
//   - kitty / foot take the program + args directly, no flag. No `--`
//     prefix: the program here is "sh" (no leading dash), so option
//     parsing stops cleanly at it, and not every kitty/foot version
//     accepts a bare `--`.
//   - wezterm runs through its `start` subcommand, `--` ending start's
//     own options.
var linuxTerminals = []linuxTerminal{
	{"x-terminal-emulator", func(c string) []string { return []string{"-e", "sh", "-c", c} }},
	{"gnome-terminal", func(c string) []string { return []string{"--", "sh", "-c", c} }},
	{"konsole", func(c string) []string { return []string{"-e", "sh", "-c", c} }},
	{"xfce4-terminal", func(c string) []string { return []string{"-x", "sh", "-c", c} }},
	{"alacritty", func(c string) []string { return []string{"-e", "sh", "-c", c} }},
	{"kitty", func(c string) []string { return []string{"sh", "-c", c} }},
	{"foot", func(c string) []string { return []string{"sh", "-c", c} }},
	{"wezterm", func(c string) []string { return []string{"start", "--", "sh", "-c", c} }},
	{"xterm", func(c string) []string { return []string{"-e", "sh", "-c", c} }},
}

// buildLinuxTerminalArgv finds the first installed terminal emulator
// (per lookPath) and returns the full exec argv — [absolutePath,
// args...] — for running command in it. Returns an error naming every
// terminal it tried when none is installed. Pure apart from lookPath,
// so it carries the unit-test coverage for terminal selection and argv
// construction.
func buildLinuxTerminalArgv(command string, lookPath lookPathFunc) ([]string, error) {
	for _, t := range linuxTerminals {
		if path, err := lookPath(t.name); err == nil {
			return append([]string{path}, t.argv(command)...), nil
		}
	}
	tried := make([]string, len(linuxTerminals))
	for i, t := range linuxTerminals {
		tried[i] = t.name
	}
	return nil, fmt.Errorf("no terminal emulator found (tried: %s)", strings.Join(tried, ", "))
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

// openLinuxNative opens a terminal on native (non-WSL) Linux.
func openLinuxNative(command string) error {
	if err := haveDisplay(); err != nil {
		return err
	}
	argv, err := buildLinuxTerminalArgv(command, exec.LookPath)
	if err != nil {
		return err
	}
	return startDetached(exec.Command(argv[0], argv[1:]...))
}

// startDetached starts cmd and reaps it in the background.
//
// Two problems it solves, both rooted in agentd being a long-lived
// daemon:
//
//   - Zombies. A child agentd never wait()s for stays a zombie in the
//     process table for agentd's entire lifetime. The OS only reparents
//     (and so reaps) the children of a process that has *exited* —
//     agentd hasn't. The terminal window outlives this call, so we
//     can't block; instead a goroutine wait()s and reaps it whenever
//     the window is finally closed.
//   - Signal bleed. Without a new session the terminal shares agentd's
//     process group; a Ctrl-C in whatever shell launched agentd would
//     also kill the user's freshly-opened agent windows. Setsid puts
//     the terminal in its own session, so it survives both that and an
//     agentd restart.
func startDetached(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	debug := os.Getenv("TCLAUDE_DEBUG") != ""
	go func() {
		err := cmd.Wait()
		if debug && err != nil {
			fmt.Printf("[debug] terminal: %q exited: %v\n", cmd.Path, err)
		}
	}()
	return nil
}
