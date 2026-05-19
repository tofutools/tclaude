//go:build linux

package session

import (
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// fakeFocusLookPath returns a lookPath stub that "installs" only the
// named binaries (each resolving to /usr/bin/<name>), so the
// preferred/fallback resolution can be exercised without the host's
// real PATH.
func fakeFocusLookPath(installed ...string) func(string) (string, error) {
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

// TestIsKDESession pins the KDE-detection table. KDE_SESSION_VERSION
// non-empty is the strongest signal (KDE Plasma's startup sets it).
// XDG_CURRENT_DESKTOP is a colon-separated list per spec, and the
// match is case-insensitive — "KDE", "kde", and "KDE:plasma" all qualify.
func TestIsKDESession(t *testing.T) {
	cases := []struct {
		name          string
		desktop, sver string
		want          bool
	}{
		{"kde session version 6", "", "6", true},
		{"kde session version anything", "", "5", true},
		{"xdg KDE only", "KDE", "", true},
		{"xdg kde lower", "kde", "", true},
		{"xdg KDE:plasma", "KDE:plasma", "", true},
		{"xdg plasma:KDE trailing", "plasma:KDE", "", true},
		{"xdg gnome", "GNOME", "", false},
		{"xdg sway", "sway", "", false},
		{"xdg ubuntu:gnome", "ubuntu:GNOME", "", false},
		{"empty everything", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isKDESession(tc.desktop, tc.sver); got != tc.want {
				t.Fatalf("isKDESession(%q,%q) = %v, want %v", tc.desktop, tc.sver, got, tc.want)
			}
		})
	}
}

// TestPickPreferredFocusTool pins the env→preference table. The key
// invariant: kdotool is preferred ONLY on KDE-Wayland — preferring it
// on non-KDE Wayland (GNOME, Sway, Hyprland) would just hit kdotool's
// upstream "Unsupported KDE version" bail and force the xdotool
// fallback. Everywhere else xdotool wins.
func TestPickPreferredFocusTool(t *testing.T) {
	cases := []struct {
		name              string
		display, wayland  string
		desktop, sver     string
		wantPref, wantAlt string
	}{
		// X11 sessions — xdotool wins regardless of desktop env.
		{"x11 only, no desktop env", ":0", "", "", "", "xdotool", "kdotool"},
		{"x11 only, KDE", ":0", "", "KDE", "6", "xdotool", "kdotool"},
		{"x11 only, GNOME", ":0", "", "GNOME", "", "xdotool", "kdotool"},

		// XWayland (DISPLAY + WAYLAND_DISPLAY both set).
		{"xwayland KDE", ":0", "wayland-0", "KDE", "6", "kdotool", "xdotool"},
		{"xwayland GNOME", ":0", "wayland-0", "GNOME", "", "xdotool", "kdotool"},

		// Pure Wayland (WAYLAND_DISPLAY set, DISPLAY unset).
		{"wayland KDE Plasma", "", "wayland-0", "KDE", "6", "kdotool", "xdotool"},
		{"wayland GNOME", "", "wayland-0", "GNOME", "", "xdotool", "kdotool"},
		{"wayland Sway", "", "wayland-0", "sway", "", "xdotool", "kdotool"},
		{"wayland Hyprland", "", "wayland-0", "Hyprland", "", "xdotool", "kdotool"},
		// SSH into a Wayland host: no DESKTOP env propagated. Without
		// the KDE signal we cannot prefer kdotool — fall through to
		// xdotool (which won't see Wayland windows, but won't error
		// out either).
		{"wayland, no desktop env", "", "wayland-0", "", "", "xdotool", "kdotool"},

		// Headless.
		{"headless", "", "", "", "", "xdotool", "kdotool"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pref, alt := pickPreferredFocusTool(tc.display, tc.wayland, tc.desktop, tc.sver)
			if pref != tc.wantPref || alt != tc.wantAlt {
				t.Fatalf("pickPreferredFocusTool(%q,%q,%q,%q) = (%q,%q), want (%q,%q)",
					tc.display, tc.wayland, tc.desktop, tc.sver,
					pref, alt, tc.wantPref, tc.wantAlt)
			}
		})
	}
}

// TestChooseLinuxFocusTool covers the full resolution: pick preferred
// when installed, fall back when missing, return "" when neither is
// installed. The Kubuntu/KDE regression — Wayland session, KDE
// detected, kdotool installed — must pick kdotool.
func TestChooseLinuxFocusTool(t *testing.T) {
	cases := []struct {
		name             string
		display, wayland string
		desktop, sver    string
		installed        []string
		want             string
	}{
		// X11 cases — xdotool preferred.
		{"x11, only xdotool", ":0", "", "", "", []string{"xdotool"}, "xdotool"},
		{"x11, only kdotool", ":0", "", "", "", []string{"kdotool"}, "kdotool"},
		{"x11, both installed", ":0", "", "", "", []string{"xdotool", "kdotool"}, "xdotool"},

		// KDE Wayland — kdotool preferred (the Kubuntu/KDE regression).
		{"kde-wayland, only kdotool", "", "wayland-0", "KDE", "6", []string{"kdotool"}, "kdotool"},
		{"kde-wayland, only xdotool", "", "wayland-0", "KDE", "6", []string{"xdotool"}, "xdotool"},
		{"kde-wayland, both installed", "", "wayland-0", "KDE", "6", []string{"xdotool", "kdotool"}, "kdotool"},

		// Non-KDE Wayland — xdotool preferred even when kdotool is
		// installed, because kdotool refuses to run on non-KDE.
		{"gnome-wayland, both installed", "", "wayland-0", "GNOME", "", []string{"xdotool", "kdotool"}, "xdotool"},
		{"sway-wayland, both installed", "", "wayland-0", "sway", "", []string{"xdotool", "kdotool"}, "xdotool"},
		{"hyprland-wayland, both installed", "", "wayland-0", "Hyprland", "", []string{"xdotool", "kdotool"}, "xdotool"},
		// Non-KDE Wayland, only kdotool installed — we still pick it
		// as the fallback (it's all we have). The empty-result path
		// is below.
		{"gnome-wayland, only kdotool", "", "wayland-0", "GNOME", "", []string{"kdotool"}, "kdotool"},

		// Nothing installed.
		{"x11, neither installed", ":0", "", "", "", nil, ""},
		{"kde-wayland, neither installed", "", "wayland-0", "KDE", "6", nil, ""},
		{"gnome-wayland, neither installed", "", "wayland-0", "GNOME", "", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := chooseLinuxFocusTool(tc.display, tc.wayland, tc.desktop, tc.sver,
				fakeFocusLookPath(tc.installed...))
			if got != tc.want {
				t.Fatalf("chooseLinuxFocusTool(%q,%q,%q,%q,installed=%v) = %q, want %q",
					tc.display, tc.wayland, tc.desktop, tc.sver, tc.installed, got, tc.want)
			}
		})
	}
}

// TestResolveLinuxFocusToolPublic exercises the public resolver via
// the t.Setenv + focusLookPath swap path that agentd actually walks —
// confirming the four env reads + the lookPath seam end-to-end, not
// just the inner chooseLinuxFocusTool helper. The previous
// implementation cached the answer in a sync.Once: this test would
// have been impossible to write against that cache without an
// invalidation hook, and the cache itself was the reason agentd's
// "tool" choice could lock in stale if the daemon started before the
// graphical session.
func TestResolveLinuxFocusToolPublic(t *testing.T) {
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")
	t.Setenv("XDG_CURRENT_DESKTOP", "KDE")
	t.Setenv("KDE_SESSION_VERSION", "6")

	prev := focusLookPath
	focusLookPath = fakeFocusLookPath("xdotool", "kdotool")
	t.Cleanup(func() { focusLookPath = prev })

	if got := resolveLinuxFocusTool(); got != "kdotool" {
		t.Fatalf("resolveLinuxFocusTool() on KDE Wayland with both installed = %q, want kdotool", got)
	}
	if got := LinuxFocusToolName(); got != "kdotool" {
		t.Fatalf("LinuxFocusToolName() on KDE Wayland with both installed = %q, want kdotool", got)
	}

	// Re-resolving picks up a changed install state — the cache that
	// used to live here would have stuck on the first answer.
	focusLookPath = fakeFocusLookPath("xdotool")
	if got := resolveLinuxFocusTool(); got != "xdotool" {
		t.Fatalf("resolveLinuxFocusTool() after kdotool removed = %q, want xdotool (fallback)", got)
	}
}

// TestWindowActivateCmd pins the per-tool argv shape. xdotool gets
// --sync (it waits for the X round-trip); kdotool does NOT, because
// its KWin DBus call is already synchronous and kdotool rejects --sync
// as an unknown option.
func TestWindowActivateCmd(t *testing.T) {
	cases := []struct {
		tool string
		want []string
	}{
		{"xdotool", []string{"xdotool", "windowactivate", "--sync", "0x123"}},
		{"kdotool", []string{"kdotool", "windowactivate", "0x123"}},
		// Anything other than xdotool falls through to the kdotool
		// shape (no --sync) — defensive: an unrecognised tool that
		// happens to be xdotool-compatible-minus-sync still works.
		{"some-future-tool", []string{"some-future-tool", "windowactivate", "0x123"}},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			cmd := windowActivateCmd(tc.tool, "0x123")
			if !reflect.DeepEqual(cmd.Args, tc.want) {
				t.Fatalf("windowActivateCmd(%q).Args = %q, want %q", tc.tool, cmd.Args, tc.want)
			}
		})
	}
}

// TestFocusToolInstallProbesCompile is an existence check: the helpers
// IsXdotoolInstalled / IsKdotoolInstalled / LinuxFocusToolName must
// stay exported (setup.go consumes them) even when no test asserts the
// host's actual install state.
func TestFocusToolInstallProbesCompile(t *testing.T) {
	_ = IsXdotoolInstalled()
	_ = IsKdotoolInstalled()
	name := LinuxFocusToolName()
	switch name {
	case "", "xdotool", "kdotool":
	default:
		t.Fatalf("LinuxFocusToolName() returned unexpected %q", name)
	}
}

// TestLinuxShellSingleQuote pins the quoting helper. UUID session IDs
// don't trigger the escape path on their own, but agent LABELS can,
// and the focus button accepts both — defensive testing.
func TestLinuxShellSingleQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"abc-123", `'abc-123'`},
		{"4d01388a-bc9d-4617-8170-166a4a503994", `'4d01388a-bc9d-4617-8170-166a4a503994'`},
		{"label with spaces", `'label with spaces'`},
		// Single-quoted body terminates, '\'' inserts a literal quote,
		// new single-quoted body opens — the POSIX-portable trick.
		{"o'reilly", `'o'\''reilly'`},
		{"$(rm -rf /)", `'$(rm -rf /)'`}, // shell metachars stay literal
		{"", `''`},
	}
	for _, c := range cases {
		if got := linuxShellSingleQuote(c.in); got != c.want {
			t.Errorf("linuxShellSingleQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestBuildLinuxAttachCmd pins the new-window payload shape: absolute
// tclaude path, `session attach`, single-quoted label, exec-prefix so
// the wrapping sh terminates by exec-replacement (the same trick
// agentd's openAttachCmd uses, and the same trick openShellCmd's
// trailing `exec sh -c` uses for interactive shells). The exec
// prefix is what lets a later "hide" detach close the tab cleanly
// without leaving an orphaned shell prompt.
func TestBuildLinuxAttachCmd(t *testing.T) {
	cmd := buildLinuxAttachCmd("4d01388a")
	if !strings.HasPrefix(cmd, "exec ") {
		t.Errorf("buildLinuxAttachCmd should start with 'exec ' so a later detach closes the tab; got: %s", cmd)
	}
	if !strings.Contains(cmd, "session attach") {
		t.Errorf("buildLinuxAttachCmd should invoke `tclaude session attach`; got: %s", cmd)
	}
	if !strings.Contains(cmd, "'4d01388a'") {
		t.Errorf("buildLinuxAttachCmd should pass the label as a single-quoted shell word; got: %s", cmd)
	}
	// And the quoting must survive a label with embedded quotes —
	// the agent title field is human-set, so this is realistic.
	cmd = buildLinuxAttachCmd("o'reilly")
	if !strings.Contains(cmd, `'o'\''reilly'`) {
		t.Errorf("buildLinuxAttachCmd should escape embedded single quotes; got: %s", cmd)
	}
}

// TestOpenLinuxAttachTerminal_EmptyID confirms the empty-sessionID
// guard — without it, the terminal would launch a bare `tclaude
// session attach` (no label), which dumps the usage page.
func TestOpenLinuxAttachTerminal_EmptyID(t *testing.T) {
	var called bool
	prev := linuxOpenTerminal
	linuxOpenTerminal = func(string) error { called = true; return nil }
	t.Cleanup(func() { linuxOpenTerminal = prev })

	openLinuxAttachTerminal("")
	if called {
		t.Fatal("openLinuxAttachTerminal must not invoke the terminal when sessionID is empty")
	}
}

// TestOpenLinuxAttachTerminal_HappyPath confirms a real sessionID
// reaches the terminal seam with the cmd buildLinuxAttachCmd builds.
// The fallback gap this fills (no window opens on focus when no
// client is attached) was the Kubuntu/KDE regression's second half —
// kdotool alone wasn't going to fix it.
func TestOpenLinuxAttachTerminal_HappyPath(t *testing.T) {
	var got string
	prev := linuxOpenTerminal
	linuxOpenTerminal = func(c string) error { got = c; return nil }
	t.Cleanup(func() { linuxOpenTerminal = prev })

	openLinuxAttachTerminal("4d01388a")

	want := buildLinuxAttachCmd("4d01388a")
	if got != want {
		t.Fatalf("linuxOpenTerminal saw %q, want %q", got, want)
	}
}

// TestOpenLinuxAttachTerminal_OpenError verifies the best-effort
// contract: a terminal-launch failure must NOT panic and must NOT
// propagate to the caller — same contract as the rest of the focus
// path. The dashboard reports "focused" regardless because the
// underlying TryFocusAttachedSession returns nothing.
func TestOpenLinuxAttachTerminal_OpenError(t *testing.T) {
	prev := linuxOpenTerminal
	linuxOpenTerminal = func(string) error { return errors.New("no terminal found") }
	t.Cleanup(func() { linuxOpenTerminal = prev })

	// Must not panic. Returns nothing — best-effort.
	openLinuxAttachTerminal("4d01388a")
}
