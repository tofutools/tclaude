package agentd_test

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/agentd/dashsnap"
)

// TestDashboardProcessEditorLayoutChrome is the real-layout acceptance check
// for TCL-420. The ordinary asset test guards the CSS contract in CI; this
// optional smoke asks Chrome to compute the short/tall geometry in both skins.
func TestDashboardProcessEditorLayoutChrome(t *testing.T) {
	if os.Getenv("TCLAUDE_DASHSNAP") == "" {
		t.Skip("browser smoke — set TCLAUDE_DASHSNAP=1 (needs local Chrome)")
	}

	f := newFlow(t)
	seedDashSnapFixture(t, f)
	srv := httptest.NewServer(agentd.BuildDashboardHandlerForTest())
	defer srv.Close()

	viewports := []struct {
		name   string
		height int
		short  bool
	}{
		{name: "short", height: 560, short: true},
		{name: "tall", height: 1050},
	}
	for _, viewport := range viewports {
		t.Run(viewport.name, func(t *testing.T) {
			states := []dashsnap.State{
				processEditorLayoutState(viewport.name, false, viewport.short),
				processEditorLayoutState(viewport.name, true, viewport.short),
			}
			outDir := filepath.Join(dashSnapOutRoot(t), "process-editor-layout-"+viewport.name+"-"+time.Now().Format("20060102-150405.000"))
			shots, err := dashsnap.Capture(dashsnap.Config{
				BaseURL: srv.URL,
				OutDir:  outDir,
				Width:   1680,
				Height:  viewport.height,
				States:  states,
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
				t.Fatalf("process editor %s viewport smoke failed:\n%s\ncontact sheet: %s",
					viewport.name, strings.Join(failed, "\n"), filepath.Join(outDir, "index.html"))
			}
			t.Logf("process editor %s viewport smoke: %s", viewport.name, filepath.Join(outDir, "index.html"))
		})
	}
}

func processEditorLayoutState(viewport string, wizard, short bool) dashsnap.State {
	skin := "default"
	if wizard {
		skin = "wizard"
	}
	extra := `
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  var doc = document.documentElement;
  var main = document.querySelector('main');
  var subnav = document.querySelector('.process-subnav');
  var view = document.querySelector('#process-editor-view');
  var close = view.querySelector('[data-process-close-view]');
  var mount = document.querySelector('#process-editor-canvas');
  var editor = mount.querySelector('.process-editor');
  var header = editor.querySelector('.process-editor-header');
  var graph = editor.querySelector('.process-graph');
  var inspector = editor.querySelector('.process-editor-inspector');
  var footer = document.querySelector('footer');
  var boxes = [main, subnav, view, close, mount, editor, header, graph, inspector, footer];
  if (boxes.some(function(el){ return !el; })) throw new Error('editor layout fixture is incomplete');
  if (getComputedStyle(document.body).display !== 'flex') throw new Error('Processes editor did not activate the flex app shell');
  if (doc.scrollHeight > window.innerHeight + 1 || document.body.scrollHeight > window.innerHeight + 1) {
    throw new Error('Processes editor scrolls the outer document: doc=' + doc.scrollHeight + ', body=' + document.body.scrollHeight + ', viewport=' + window.innerHeight);
  }
  var footerTop = footer.getBoundingClientRect().top;
  var mainBottom = main.getBoundingClientRect().bottom;
  var viewBox = view.getBoundingClientRect();
  if (mainBottom > footerTop + 1 || viewBox.bottom > footerTop + 1) throw new Error('editor workspace extends behind the fixed footer');
  if (subnav.getBoundingClientRect().bottom > close.getBoundingClientRect().top + 1) throw new Error('Processes subnav overlaps the editor toolbar');
  if (close.getBoundingClientRect().bottom > mount.getBoundingClientRect().top + 1) throw new Error('editor toolbar overlaps the canvas mount');
  if (graph.clientWidth < 400 || graph.clientHeight < 318) throw new Error('graph lost its minimum usable canvas: ' + graph.clientWidth + 'x' + graph.clientHeight);
  if (editor.clientHeight !== mount.clientHeight) throw new Error('editor does not fill its canvas mount');
`
	if short {
		extra += `
  if (view.scrollHeight <= view.clientHeight + 1) throw new Error('short viewport did not move editor overflow into its internal scroll region');
  var oldTop = view.scrollTop;
  view.scrollTop = view.scrollHeight;
  await new Promise(function(resolve){ requestAnimationFrame(function(){ requestAnimationFrame(resolve); }); });
  viewBox = view.getBoundingClientRect();
  if (inspector.getBoundingClientRect().bottom > viewBox.bottom + 1 || inspector.getBoundingClientRect().bottom > footerTop + 1) {
    throw new Error('short viewport cannot scroll the inspector clear of the footer');
  }
  view.scrollTop = oldTop;
`
	} else {
		extra += `
  if (view.scrollHeight > view.clientHeight + 1) throw new Error('tall viewport has an unnecessary editor scrollbar');
  if (graph.clientHeight < 600) throw new Error('tall viewport did not donate remaining workspace to the graph: ' + graph.clientHeight);
  if (Math.abs(mount.getBoundingClientRect().bottom - viewBox.bottom) > 2) throw new Error('editor mount does not fill the tall canvas row');
  if (inspector.getBoundingClientRect().bottom > footerTop + 1) throw new Error('tall editor inspector extends behind the footer');
`
	}
	return dashsnap.State{
		Key:      fmt.Sprintf("process-editor-layout-%s-%s", viewport, skin),
		Title:    fmt.Sprintf("Process editor layout — %s %s", viewport, skin),
		Caption:  "TCL-420 computed-layout smoke: the editor consumes the real tab workspace, keeps document scrolling bounded, and preserves usable graph/inspector geometry.",
		Wizard:   wizard,
		JS:       processEditorStateJS(extra),
		SettleMS: 300,
	}
}
