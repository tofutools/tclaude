package agentd_test

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/agentd/dashsnap"
)

// TestDashboardProcessEditorClipboardChrome is the focused trusted-input proof
// for TCL-564. The ordinary Node suite covers the full fail-closed matrix; this
// optional smoke proves real Chrome emits copy/paste from Ctrl-C/Ctrl-V and the
// browser clipboard carries the versioned payload in both dashboard skins.
func TestDashboardProcessEditorClipboardChrome(t *testing.T) {
	if os.Getenv("TCLAUDE_DASHSNAP") == "" {
		t.Skip("browser smoke — set TCLAUDE_DASHSNAP=1 (needs local Chrome)")
	}

	f := newFlow(t)
	seedDashSnapFixture(t, f)
	srv := httptest.NewServer(agentd.BuildDashboardHandlerForTest())
	defer srv.Close()

	var base dashsnap.State
	for _, state := range baseStates() {
		if state.Key == "process-editor-browser-copy-paste" {
			base = state
			break
		}
	}
	if base.Key == "" {
		t.Fatal("clipboard browser state missing from the dashboard smoke matrix")
	}
	regular := base
	regular.Key = "default-" + base.Key
	wizard := base
	wizard.Key = "wizard-" + base.Key
	wizard.Wizard = true

	outDir := filepath.Join(dashSnapOutRoot(t), "process-editor-clipboard-"+time.Now().Format("20060102-150405.000"))
	shots, err := dashsnap.Capture(dashsnap.Config{
		BaseURL: srv.URL,
		OutDir:  outDir,
		Width:   1680,
		Height:  1050,
		States:  []dashsnap.State{regular, wizard},
	})
	if err != nil {
		t.Fatalf("dashsnap.Capture: %v", err)
	}
	var failed []string
	for _, shot := range shots {
		if shot.Err != "" {
			failed = append(failed, shot.State.Key+": "+shot.Err)
		}
	}
	if len(failed) != 0 {
		t.Fatalf("process editor clipboard browser smoke failed:\n%s\ncontact sheet: %s",
			strings.Join(failed, "\n"), filepath.Join(outDir, "index.html"))
	}
	t.Logf("process editor clipboard browser smoke: %s", filepath.Join(outDir, "index.html"))
}
