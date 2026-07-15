package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// When an agent's terminal window is hidden/detached from OUTSIDE the
// multiplexer — the per-agent eye button, the command palette's per-agent hide,
// or a bulk unfocus — the agent's live-session tmux client is detached
// server-side, which would leave any open multiplexer pane for that agent
// showing a stale "disconnected". These structural guards pin the wiring that
// closes the corresponding pane instead. There is no JS test runner, so they
// assert against the embedded module source.

func readDashboardJS(t *testing.T, name string) string {
	t.Helper()
	data, err := fs.ReadFile(dashboardAssetsFS, "js/"+name)
	if err != nil {
		t.Fatalf("read js/%s: %v", name, err)
	}
	return string(data)
}

// TestTerminalsCore_CloseForHideSkipsDetach pins that the close-on-external-hide
// path closes the pane WITHOUT re-running /api/hide: the detach already happened
// server-side, so re-hiding would be redundant (and, for a pane that's somehow
// still live, wrong). The mechanism is closePane's skipDetach opt.
func TestTerminalsCore_CloseForHideSkipsDetach(t *testing.T) {
	src := readDashboardJS(t, "terminal-shell-actions.js")
	if !strings.Contains(src, "function closeForHide(") {
		t.Error("terminal shell actions must expose closeForHide() to close panes on an external hide")
	}
	if !strings.Contains(src, "closePane(pane.key, { skipDetach: true })") {
		t.Error("terminal shell closeForHide must close with { skipDetach: true } — the detach " +
			"already ran server-side, so it must NOT re-POST /api/hide")
	}
}

// TestTerminalsTab_ExportsHideClosers pins the two entry points the hide callers
// use, and that the bulk twin only closes agents whose outcome is 'detached'
// (never focus / no_window / failed).
func TestTerminalsTab_ExportsHideClosers(t *testing.T) {
	src := readDashboardJS(t, "terminals-tab.js")
	for _, needle := range []string{
		"export function closeTerminalsForConvs(",
		"export function closeTerminalsForWindowOp(",
		"o.outcome === 'detached'",
	} {
		if !strings.Contains(src, needle) {
			t.Errorf("terminals-tab.js missing %q — hide→close-pane wiring broken", needle)
		}
	}
}

// TestHidePathsClosePanes pins that every "hide from outside the multiplexer"
// path notifies the Terminals tab to close the matching pane: the per-agent eye
// button (row-actions.js), the palette per-agent hide + bulk unfocus
// (palette.js), and the window-picker modal's unfocus (refresh.js).
func TestHidePathsClosePanes(t *testing.T) {
	cases := []struct {
		file, needle, why string
	}{
		{"row-actions.js", "closeTerminalsForConvs([agent])",
			"the eye-button hide case must close the agent's multiplexer pane"},
		{"palette.js", "closeTerminalsForConvs([conv])",
			"the palette per-agent hide must close that agent's multiplexer pane"},
		{"palette.js", "closeTerminalsForWindowOp(out.agents)",
			"the palette bulk unfocus must close the detached agents' multiplexer panes"},
		{"refresh.js", "closeTerminalsForWindowOp(out.agents)",
			"the window-picker modal's unfocus must close the detached agents' multiplexer panes"},
	}
	for _, c := range cases {
		src := readDashboardJS(t, c.file)
		if !strings.Contains(src, c.needle) {
			t.Errorf("%s missing %q — %s", c.file, c.needle, c.why)
		}
	}
}
