//go:build linux

package terminal

import (
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// fakeLookPath returns a lookPathFunc that "installs" only the named
// binaries, each resolving to /usr/bin/<name>. Anything else is
// reported missing — letting a test pin exactly which terminal
// buildLinuxTerminalArgv will pick.
func fakeLookPath(installed ...string) lookPathFunc {
	set := make(map[string]bool, len(installed))
	for _, n := range installed {
		set[n] = true
	}
	return func(name string) (string, error) {
		if set[name] {
			return "/usr/bin/" + name, nil
		}
		return "", exec.ErrNotFound
	}
}

// containsExact reports whether argv has want as one whole element.
func containsExact(argv []string, want string) bool {
	return slices.Contains(argv, want)
}

// TestBuildLinuxTerminalArgv_PerTerminal pins the exact argv produced
// for every supported terminal. The command deliberately carries the
// embedded single quotes that openShellCmd bakes in — the regression
// that broke the old string-wrapping xfce4-terminal entry.
func TestBuildLinuxTerminalArgv_PerTerminal(t *testing.T) {
	const command = `cd '/home/me/my repo' && exec "${SHELL:-bash}"`

	cases := []struct {
		terminal string
		want     []string
	}{
		{"x-terminal-emulator", []string{"/usr/bin/x-terminal-emulator", "-e", "sh", "-c", command}},
		{"gnome-terminal", []string{"/usr/bin/gnome-terminal", "--", "sh", "-c", command}},
		{"konsole", []string{"/usr/bin/konsole", "-e", "sh", "-c", command}},
		{"xfce4-terminal", []string{"/usr/bin/xfce4-terminal", "-x", "sh", "-c", command}},
		{"alacritty", []string{"/usr/bin/alacritty", "-e", "sh", "-c", command}},
		{"kitty", []string{"/usr/bin/kitty", "sh", "-c", command}},
		{"foot", []string{"/usr/bin/foot", "sh", "-c", command}},
		{"wezterm", []string{"/usr/bin/wezterm", "start", "--", "sh", "-c", command}},
		{"xterm", []string{"/usr/bin/xterm", "-e", "sh", "-c", command}},
	}

	for _, tc := range cases {
		t.Run(tc.terminal, func(t *testing.T) {
			argv, err := buildLinuxTerminalArgv(command, fakeLookPath(tc.terminal))
			if err != nil {
				t.Fatalf("buildLinuxTerminalArgv: unexpected error: %v", err)
			}
			if !reflect.DeepEqual(argv, tc.want) {
				t.Fatalf("argv = %q, want %q", argv, tc.want)
			}
			// The core invariant: the command must reach the argv as
			// one verbatim element, never re-quoted into a larger
			// string. The old xfce4 entry produced "sh -c '<command>'"
			// — a single element that is NOT equal to command — so
			// this check fails the moment that bug returns.
			if !containsExact(argv, command) {
				t.Fatalf("argv %q does not carry the command as a single verbatim element", argv)
			}
		})
	}
}

// TestBuildLinuxTerminalArgv_Preference checks that the first terminal
// in linuxTerminals order wins when several are installed.
func TestBuildLinuxTerminalArgv_Preference(t *testing.T) {
	// xterm is last in the list, gnome-terminal earlier — gnome wins.
	argv, err := buildLinuxTerminalArgv("echo hi", fakeLookPath("xterm", "gnome-terminal"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if argv[0] != "/usr/bin/gnome-terminal" {
		t.Fatalf("argv[0] = %q, want gnome-terminal (earlier in preference order)", argv[0])
	}

	// Everything installed → x-terminal-emulator, the first entry.
	argv, err = buildLinuxTerminalArgv("echo hi", fakeLookPath(
		"x-terminal-emulator", "gnome-terminal", "konsole", "xfce4-terminal",
		"alacritty", "kitty", "foot", "wezterm", "xterm"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if argv[0] != "/usr/bin/x-terminal-emulator" {
		t.Fatalf("argv[0] = %q, want x-terminal-emulator (first in preference order)", argv[0])
	}
}

// TestBuildLinuxTerminalArgv_NoneInstalled checks the error path names
// the terminals it looked for, so the failure is actionable.
func TestBuildLinuxTerminalArgv_NoneInstalled(t *testing.T) {
	_, err := buildLinuxTerminalArgv("echo hi", fakeLookPath())
	if err == nil {
		t.Fatal("expected an error when no terminal is installed")
	}
	for _, name := range []string{"gnome-terminal", "konsole", "xterm"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q should name the %q candidate it tried", err, name)
		}
	}
}

// TestHaveDisplay covers the up-front display check that turns a
// silent headless failure into an actionable error.
func TestHaveDisplay(t *testing.T) {
	cases := []struct {
		name, display, wayland string
		wantErr                bool
	}{
		{"x11", ":0", "", false},
		{"wayland", "", "wayland-0", false},
		{"both", ":0", "wayland-0", false},
		{"headless", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DISPLAY", tc.display)
			t.Setenv("WAYLAND_DISPLAY", tc.wayland)
			err := haveDisplay()
			if tc.wantErr != (err != nil) {
				t.Fatalf("haveDisplay() err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// TestWSLCmdArgv pins the WSL launch argv for both the Windows Terminal
// path and the cmd.exe fallback, and confirms the command rides through
// as one verbatim element.
func TestWSLCmdArgv(t *testing.T) {
	const command = `cd '/home/me/repo' && exec "${SHELL:-bash}"`

	withWT := wslCmdArgv(command, "/mnt/c/wt.exe")
	wantWT := []string{"/mnt/c/wt.exe", "new-tab", "wsl", "-e", "sh", "-c", command}
	if !reflect.DeepEqual(withWT, wantWT) {
		t.Fatalf("wslCmdArgv(wt) = %q, want %q", withWT, wantWT)
	}

	fallback := wslCmdArgv(command, "")
	wantFallback := []string{"cmd.exe", "/c", "start", "wsl", "-e", "sh", "-c", command}
	if !reflect.DeepEqual(fallback, wantFallback) {
		t.Fatalf("wslCmdArgv(fallback) = %q, want %q", fallback, wantFallback)
	}

	for _, argv := range [][]string{withWT, fallback} {
		if !containsExact(argv, command) {
			t.Fatalf("argv %q does not carry the command as a single verbatim element", argv)
		}
	}
}

// TestStartDetached exercises the real Start + background-reap path
// against trivial binaries — covering the one production line that
// can't be a pure unit test.
func TestStartDetached(t *testing.T) {
	// `true` exists on every Linux box and exits 0 immediately; the
	// background goroutine should reap it without anything leaking.
	if err := startDetached(exec.Command("true")); err != nil {
		t.Fatalf("startDetached(true): unexpected error: %v", err)
	}

	// A binary that does not exist must surface as a Start error.
	if err := startDetached(exec.Command("/nonexistent/tclaude-no-such-binary")); err == nil {
		t.Fatal("startDetached: expected an error for a missing binary")
	}
}
