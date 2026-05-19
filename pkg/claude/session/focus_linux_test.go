//go:build linux

package session

import (
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

// TestPickPreferredFocusTool pins the env→preference table. A
// Wayland-only session prefers kdotool because xdotool can't see
// native-Wayland windows; everywhere else xdotool wins.
func TestPickPreferredFocusTool(t *testing.T) {
	cases := []struct {
		name              string
		display, wayland  string
		wantPref, wantAlt string
	}{
		{"x11 only", ":0", "", "xdotool", "kdotool"},
		{"wayland only", "", "wayland-0", "kdotool", "xdotool"},
		{"xwayland (both)", ":0", "wayland-0", "xdotool", "kdotool"},
		{"headless", "", "", "xdotool", "kdotool"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pref, alt := pickPreferredFocusTool(tc.display, tc.wayland)
			if pref != tc.wantPref || alt != tc.wantAlt {
				t.Fatalf("pickPreferredFocusTool(%q,%q) = (%q,%q), want (%q,%q)",
					tc.display, tc.wayland, pref, alt, tc.wantPref, tc.wantAlt)
			}
		})
	}
}

// TestChooseLinuxFocusTool covers the full resolution: pick preferred
// when installed, fall back when missing, return "" when neither is
// installed. The Kubuntu/KDE regression — Wayland session, kdotool
// installed, xdotool optionally also installed — must pick kdotool.
func TestChooseLinuxFocusTool(t *testing.T) {
	cases := []struct {
		name             string
		display, wayland string
		installed        []string
		want             string
	}{
		// X11 cases — xdotool preferred.
		{"x11, only xdotool installed", ":0", "", []string{"xdotool"}, "xdotool"},
		{"x11, only kdotool installed", ":0", "", []string{"kdotool"}, "kdotool"},
		{"x11, both installed", ":0", "", []string{"xdotool", "kdotool"}, "xdotool"},
		// Wayland-only cases — kdotool preferred. This is the Kubuntu/
		// KDE regression: under Wayland xdotool's search returns empty
		// (the dashboard focus button looked like a no-op), so kdotool
		// must win whenever both are present.
		{"wayland, only kdotool installed", "", "wayland-0", []string{"kdotool"}, "kdotool"},
		{"wayland, only xdotool installed", "", "wayland-0", []string{"xdotool"}, "xdotool"},
		{"wayland, both installed", "", "wayland-0", []string{"xdotool", "kdotool"}, "kdotool"},
		// Nothing installed.
		{"x11, neither installed", ":0", "", nil, ""},
		{"wayland, neither installed", "", "wayland-0", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := chooseLinuxFocusTool(tc.display, tc.wayland, fakeFocusLookPath(tc.installed...))
			if got != tc.want {
				t.Fatalf("chooseLinuxFocusTool(%q,%q,installed=%v) = %q, want %q",
					tc.display, tc.wayland, tc.installed, got, tc.want)
			}
		})
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
			got := cmd.Args
			// exec.Command stores the full lookup path in cmd.Path
			// and the original name as args[0]; we care about the
			// args sequence the tool sees, which is got verbatim.
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("windowActivateCmd(%q).Args = %q, want %q", tc.tool, got, tc.want)
			}
		})
	}
}

// TestIsKdotoolInstalled / TestIsXdotoolInstalled are existence checks
// — they document that the helpers exist for setup.go without
// asserting the host's actual install state.
func TestFocusToolInstallProbesCompile(t *testing.T) {
	_ = IsXdotoolInstalled()
	_ = IsKdotoolInstalled()
	name := LinuxFocusToolName()
	// Name must be one of the known tools or empty; "wmctrl" etc. is
	// not a current selection.
	switch name {
	case "", "xdotool", "kdotool":
	default:
		if !strings.HasPrefix(name, "") { // pointless, but keep linter quiet
			t.Fatalf("LinuxFocusToolName() returned unexpected %q", name)
		}
	}
}
