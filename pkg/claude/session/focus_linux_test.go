//go:build linux

package session

import (
	"errors"
	"os/exec"
	"reflect"
	"testing"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
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
	// Recompute the tclaude-path prefix the same way production does
	// (os.Executable() under the hood), so the assertion holds on any
	// machine without hardcoding /usr/bin/tclaude. The substring
	// version of this test let a hypothetical wrong-order rewrite —
	// e.g. "exec session attach 'id' tclaude" — pass silently; the
	// exact-string compare pins the contract.
	prefix := clcommon.DetectAbsoluteCmd("session", "attach")

	cases := []struct{ id, want string }{
		// UUID — no quoting drama, but pins the full shape.
		{
			"4d01388a-bc9d-4617-8170-166a4a503994",
			"exec " + prefix + " '4d01388a-bc9d-4617-8170-166a4a503994'",
		},
		// Short ID — yamzz's friend's log used short IDs.
		{"4d01388a", "exec " + prefix + " '4d01388a'"},
		// Human-set label with spaces — agent titles can be free-form.
		{"my agent", "exec " + prefix + " 'my agent'"},
		// Embedded single quote — POSIX `'\''` escape.
		{"o'reilly", `exec ` + prefix + ` 'o'\''reilly'`},
		// Shell metachars stay literal inside the single-quoted body
		// — the belt-and-braces protection sandbox-relevant inputs.
		{"$(rm -rf /)", "exec " + prefix + " '$(rm -rf /)'"},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			if got := buildLinuxAttachCmd(c.id); got != c.want {
				t.Errorf("buildLinuxAttachCmd(%q) =\n  got:  %s\n  want: %s", c.id, got, c.want)
			}
		})
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

// TestTryFocusAttachedSessionWithID_Native pins the orchestration
// around focusLinuxTmuxSession: which return state triggers the
// open-fresh-terminal fallback, and which doesn't. The previous
// version of this code (PR #201) spawned on ALL non-focused cases —
// including the "client attached but activate failed" case, which
// gave the human two attached clients to one tmux session. The
// 3-state refactor restricts the spawn to focusLinuxNoClients, the
// only case where opening a window is genuinely the right answer
// (matching macOS focus_darwin.go's `tty == ""` gate).
//
// Uses isWSLFn to force the native-Linux branch even when the test
// host is WSL2 — the human develops on WSL2, so a skip-on-WSL test
// would leave the regression guard inactive in the env where it
// would land first.
func TestTryFocusAttachedSessionWithID_Native(t *testing.T) {
	cases := []struct {
		name      string
		result    focusLinuxResult
		sessionID string
		wantSpawn bool
		wantArg   string // expected sessionID arg to openLinuxAttachTerminal when wantSpawn
	}{
		{
			name:      "focused -> no spawn (window was raised)",
			result:    focusLinuxFocused,
			sessionID: "4d01388a",
			wantSpawn: false,
		},
		{
			name:      "noClients -> spawn (the WSL-parity fallback)",
			result:    focusLinuxNoClients,
			sessionID: "4d01388a",
			wantSpawn: true,
			wantArg:   "4d01388a",
		},
		{
			name:      "tryFailed -> NO spawn (would duplicate attached clients)",
			result:    focusLinuxTryFailed,
			sessionID: "4d01388a",
			wantSpawn: false,
		},
		{
			name:      "noClients but empty sessionID -> no spawn (can't build attach cmd)",
			result:    focusLinuxNoClients,
			sessionID: "",
			wantSpawn: false,
		},
		{
			name:      "unknown (zero-value) -> NO spawn (caught by default arm)",
			result:    focusLinuxUnknown,
			sessionID: "4d01388a",
			wantSpawn: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Force the native-Linux branch even when the test host
			// IS WSL2 — yamzz's dev loop relies on this.
			prevWSL := isWSLFn
			isWSLFn = func() bool { return false }
			t.Cleanup(func() { isWSLFn = prevWSL })

			// Pin the default (open-on-focus) behavior; raise-only is
			// covered by TestTryFocusAttachedSessionWithID_RaiseOnly.
			prevRaise := focusRaiseOnlyFn
			focusRaiseOnlyFn = func() bool { return false }
			t.Cleanup(func() { focusRaiseOnlyFn = prevRaise })

			// Swap the dispatch seam to return the chosen result.
			prevFn := focusLinuxTmuxSessionFn
			focusLinuxTmuxSessionFn = func(string) focusLinuxResult { return tc.result }
			t.Cleanup(func() { focusLinuxTmuxSessionFn = prevFn })

			// Swap the terminal-launch seam to a recorder.
			var spawnArgs []string
			prevOpen := linuxOpenTerminal
			linuxOpenTerminal = func(cmd string) error {
				spawnArgs = append(spawnArgs, cmd)
				return nil
			}
			t.Cleanup(func() { linuxOpenTerminal = prevOpen })

			TryFocusAttachedSessionWithID("tmux-label", tc.sessionID)

			if tc.wantSpawn {
				if len(spawnArgs) != 1 {
					t.Fatalf("expected exactly 1 spawn, got %d (%v)", len(spawnArgs), spawnArgs)
				}
				wantCmd := buildLinuxAttachCmd(tc.wantArg)
				if spawnArgs[0] != wantCmd {
					t.Fatalf("spawn cmd =\n  got:  %s\n  want: %s", spawnArgs[0], wantCmd)
				}
			} else {
				if len(spawnArgs) != 0 {
					t.Fatalf("expected NO spawn, got %d (%v) — this is the regression PR #201's "+
						"unconditional fallback caused: spawning a second client when one was "+
						"already attached but activate failed", len(spawnArgs), spawnArgs)
				}
			}
		})
	}
}

// TestTryFocusAttachedSessionWithID_RaiseOnly pins the focus.raise_only
// opt-in: when set, the no-client case must NOT open a fresh terminal —
// focus raises an existing window or no-ops, and opening a console is the
// explicit "open window" action's job. This is the default-off behavior
// Yorz needs under a permissive compositor where the open-on-focus
// fallback popped a konsole on every "show" that hit a detached agent.
func TestTryFocusAttachedSessionWithID_RaiseOnly(t *testing.T) {
	// Force the native-Linux branch even on a WSL2 dev host.
	prevWSL := isWSLFn
	isWSLFn = func() bool { return false }
	t.Cleanup(func() { isWSLFn = prevWSL })

	// raise_only ON.
	prevRaise := focusRaiseOnlyFn
	focusRaiseOnlyFn = func() bool { return true }
	t.Cleanup(func() { focusRaiseOnlyFn = prevRaise })

	// No clients attached — the case that would spawn under the default.
	prevFn := focusLinuxTmuxSessionFn
	focusLinuxTmuxSessionFn = func(string) focusLinuxResult { return focusLinuxNoClients }
	t.Cleanup(func() { focusLinuxTmuxSessionFn = prevFn })

	var spawnArgs []string
	prevOpen := linuxOpenTerminal
	linuxOpenTerminal = func(cmd string) error {
		spawnArgs = append(spawnArgs, cmd)
		return nil
	}
	t.Cleanup(func() { linuxOpenTerminal = prevOpen })

	TryFocusAttachedSessionWithID("tmux-label", "4d01388a")

	if len(spawnArgs) != 0 {
		t.Fatalf("raise_only set: expected NO terminal spawn on no-client, got %d (%v)",
			len(spawnArgs), spawnArgs)
	}
}
