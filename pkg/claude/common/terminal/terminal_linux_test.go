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
// resolveLinuxTerminal will pick.
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

// TestResolveLinuxTerminal_PerTerminal pins the exact argv produced for
// every supported terminal. The command deliberately carries the
// embedded single quotes that openShellCmd bakes in — the regression
// that broke the old string-wrapping xfce4-terminal entry.
func TestResolveLinuxTerminal_PerTerminal(t *testing.T) {
	const command = `cd '/home/me/my repo' && exec sh -c 'exec "${SHELL:-bash}"'`

	cases := []struct {
		terminal string
		wantArgv []string
	}{
		{IDXTermEmulator, []string{"/usr/bin/x-terminal-emulator", "-e", "sh", "-c", command}},
		{IDGnomeTerminal, []string{"/usr/bin/gnome-terminal", "--", "sh", "-c", command}},
		{IDKonsole, []string{"/usr/bin/konsole", "-e", "sh", "-c", command}},
		{IDXfce4Terminal, []string{"/usr/bin/xfce4-terminal", "-x", "sh", "-c", command}},
		{IDAlacritty, []string{"/usr/bin/alacritty", "-e", "sh", "-c", command}},
		{IDKitty, []string{"/usr/bin/kitty", "sh", "-c", command}},
		{IDFoot, []string{"/usr/bin/foot", "sh", "-c", command}},
		{IDGhostty, []string{"/usr/bin/ghostty", "-e", "sh", "-c", command}},
		{IDWezterm, []string{"/usr/bin/wezterm", "start", "--", "sh", "-c", command}},
		{IDXterm, []string{"/usr/bin/xterm", "-e", "sh", "-c", command}},
	}

	for _, tc := range cases {
		t.Run(tc.terminal, func(t *testing.T) {
			id, binPath, argvFn, err := resolveLinuxTerminal(terminalPriority, fakeLookPath(tc.terminal))
			if err != nil {
				t.Fatalf("resolveLinuxTerminal: unexpected error: %v", err)
			}
			if id != tc.terminal {
				t.Fatalf("id = %q, want %q", id, tc.terminal)
			}
			argv := append([]string{binPath}, argvFn(command)...)
			if !reflect.DeepEqual(argv, tc.wantArgv) {
				t.Fatalf("argv = %q, want %q", argv, tc.wantArgv)
			}
			// The core invariant: the command must reach the argv as
			// one verbatim element, never re-quoted into a larger
			// string. The old xfce4 entry produced "sh -c '<command>'"
			// — a single element that is NOT equal to command — so
			// this check fails the moment that bug returns.
			if !slices.Contains(argv, command) {
				t.Fatalf("argv %q does not carry the command as a single verbatim element", argv)
			}
		})
	}
}

// TestResolveLinuxTerminal_Preference checks that the first launchable
// candidate in the given order wins when several are installed — the
// mechanism behind --terminal / config / auto-detect priority.
func TestResolveLinuxTerminal_Preference(t *testing.T) {
	// xterm is last in terminalPriority, gnome-terminal earlier — gnome wins.
	id, _, _, err := resolveLinuxTerminal(terminalPriority, fakeLookPath(IDXterm, IDGnomeTerminal))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != IDGnomeTerminal {
		t.Fatalf("id = %q, want gnome-terminal (earlier in priority order)", id)
	}

	// A candidate slice with kitty spliced to the front (what
	// orderedCandidates does for --terminal=kitty) picks kitty even
	// though gnome-terminal is also installed.
	kittyFirst := append([]string{IDKitty}, terminalPriority...)
	id, _, _, err = resolveLinuxTerminal(kittyFirst, fakeLookPath(IDGnomeTerminal, IDKitty))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != IDKitty {
		t.Fatalf("id = %q, want kitty (preferred, spliced to front)", id)
	}
}

// TestResolveLinuxTerminal_SkipsNonLinux confirms IDs with no Linux
// launcher (iterm2, terminal-app) are skipped rather than matched.
func TestResolveLinuxTerminal_SkipsNonLinux(t *testing.T) {
	// Only iterm2 "installed" — it has no Linux launcher, so resolution
	// must fail rather than pick it.
	if _, _, _, err := resolveLinuxTerminal(terminalPriority, fakeLookPath(IDITerm2)); err == nil {
		t.Fatal("expected an error: iterm2 has no Linux launcher and must not be selected")
	}
}

// TestResolveLinuxTerminal_NoneInstalled checks the error path names
// the terminals it looked for, so the failure is actionable.
func TestResolveLinuxTerminal_NoneInstalled(t *testing.T) {
	_, _, _, err := resolveLinuxTerminal(terminalPriority, fakeLookPath())
	if err == nil {
		t.Fatal("expected an error when no terminal is installed")
	}
	for _, name := range []string{IDGnomeTerminal, IDKonsole, IDXterm} {
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
	const command = `cd '/home/me/repo' && exec sh -c 'exec "${SHELL:-bash}"'`

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
		if !slices.Contains(argv, command) {
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
