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

// TestDashboardProcessEditorScrollbarsChrome is TCL-571's bounded visual and
// computed-style proof. It forces overflow in the real editor shell, palette,
// and validation list in both skins. A production process-viewer table class
// is rendered alongside them as the negative control: its native scrollbar
// must not acquire the editor tokens or geometry.
func TestDashboardProcessEditorScrollbarsChrome(t *testing.T) {
	if os.Getenv("TCLAUDE_DASHSNAP") == "" {
		t.Skip("browser smoke — set TCLAUDE_DASHSNAP=1 (needs local Chrome)")
	}

	f := newFlow(t)
	seedDashSnapFixture(t, f)
	srv := httptest.NewServer(agentd.BuildDashboardHandlerForTest())
	defer srv.Close()

	states := []dashsnap.State{
		processEditorScrollbarState(false),
		processEditorScrollbarState(true),
	}
	outDir := filepath.Join(dashSnapOutRoot(t), "process-editor-scrollbars-"+time.Now().Format("20060102-150405.000"))
	shots, err := dashsnap.Capture(dashsnap.Config{
		BaseURL:        srv.URL,
		OutDir:         outDir,
		ShowScrollbars: true,
		Width:          1440,
		Height:         650,
		States:         states,
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
		t.Fatalf("process editor scrollbar smoke failed:\n%s\ncontact sheet: %s",
			strings.Join(failed, "\n"), filepath.Join(outDir, "index.html"))
	}
	t.Logf("process editor scrollbar smoke: %s", filepath.Join(outDir, "index.html"))
}

func processEditorScrollbarState(wizard bool) dashsnap.State {
	skin := "default"
	if wizard {
		skin = "wizard"
	}
	expected := map[bool]struct {
		track, thumb, hover, active, corner, firefox string
	}{
		false: {
			track: "#0d1117", thumb: "#6e7681", hover: "#8b949e",
			active: "#b1bac4", corner: "#161b22",
			firefox: "rgb(110, 118, 129) rgb(13, 17, 23)",
		},
		true: {
			track: "#181226", thumb: "#755da0", hover: "#957ac0",
			active: "#d4af37", corner: "#211832",
			firefox: "rgb(117, 93, 160) rgb(24, 18, 38)",
		},
	}[wizard]
	extra := fmt.Sprintf(`
  var view=document.querySelector('#process-editor-view.process-scroll-surface');
  var palette=document.querySelector('.process-editor-palette.process-scroll-surface');
  var issues=document.querySelector('.process-issues-list.process-scroll-surface');
  var panel=document.querySelector('.process-issues-panel');
  if(!view||!palette||!issues||!panel) throw new Error('editor scrollbar fixture is incomplete');

  // Force visible overflow without enlarging the production fixture itself.
  var cards=Array.from(palette.querySelectorAll('.process-palette-card'));
  if(!cards.length) throw new Error('editor palette has no cards to overflow');
  for(var i=0;i<18;i++){
    var clone=cards[i%%cards.length].cloneNode(true);
    clone.removeAttribute('draggable');
    clone.querySelector('.process-palette-card-label').textContent='Overflow node '+(i+1);
    palette.appendChild(clone);
  }
  panel.hidden=false; panel.open=true; issues.replaceChildren();
  for(var j=0;j<18;j++){
    var li=document.createElement('li');
    li.innerHTML='<button type="button" class="process-issue process-issue-warning"><span class="process-issue-glyph">!</span><span class="process-issue-target">node-'+j+'</span><span class="process-issue-message">Intentional overflow diagnostic '+(j+1)+'</span></button>';
    li.firstElementChild.style.minWidth='620px';
    issues.appendChild(li);
  }

  // A visible real production-class negative control. It deliberately lives
  // outside the editor and must keep its native scrollbar contract.
  var unrelated=document.createElement('div');
  unrelated.className='process-viewer-table-wrap';
  unrelated.setAttribute('data-dashsnap-unrelated-scroll','');
  unrelated.style.cssText='position:fixed;z-index:95;right:18px;bottom:38px;width:245px;height:150px;padding:8px;background:#161b22;border:1px solid #30363d;border-radius:7px;color:#c9d1d9;font:11px/1.5 ui-monospace,monospace;';
  unrelated.innerHTML='<strong>Viewer table — native bar</strong><div style="height:620px;padding-top:6px">Unrelated dashboard overflow<br>'+Array.from({length:30},function(_,n){return 'viewer row '+(n+1);}).join('<br>')+'</div>';
  document.body.appendChild(unrelated);

  await editorPaint(); await editorPaint();
  var expected={track:%q,thumb:%q,hover:%q,active:%q,corner:%q};
  var expectedFirefox=%q;
  var token=function(element,name){return getComputedStyle(element).getPropertyValue(name).trim();};
  [view,palette,issues].forEach(function(element){
    Object.entries(expected).forEach(function(entry){
      var got=token(element,'--process-scrollbar-'+(entry[0]==='hover'?'thumb-hover':entry[0]==='active'?'thumb-active':entry[0]));
      if(got!==entry[1]) throw new Error(element.className+' '+entry[0]+' token is '+got+', want '+entry[1]);
    });
    if(CSS.supports('scrollbar-color: red blue')){
      var color=getComputedStyle(element).scrollbarColor;
      if(color!==expectedFirefox) throw new Error(element.className+' Firefox colors are '+color+', want '+expectedFirefox);
    }
    var webkit=getComputedStyle(element,'::-webkit-scrollbar');
    if(webkit.width&&webkit.width!=='10px') throw new Error(element.className+' WebKit scrollbar width is '+webkit.width);
  });
  [view,palette,issues,unrelated].forEach(function(element){
    if(element.scrollHeight<=element.clientHeight+1) throw new Error(element.className+' did not overflow: '+element.scrollHeight+' <= '+element.clientHeight);
    element.scrollTop=Math.min(54,element.scrollHeight-element.clientHeight);
    if(element.scrollTop<=0) throw new Error(element.className+' cannot scroll by pointer/keyboard geometry');
  });
  if(issues.scrollWidth<=issues.clientWidth+1) throw new Error('diagnostic list did not expose the styled scrollbar corner');
  issues.scrollLeft=54;
  if(issues.scrollLeft<=0) throw new Error('diagnostic list cannot scroll horizontally');
  if(token(unrelated,'--process-scrollbar-thumb')!=='') throw new Error('editor thumb token leaked into process viewer table');
  if(CSS.supports('scrollbar-color: red blue')&&getComputedStyle(unrelated).scrollbarColor!=='auto'){
    throw new Error('unrelated viewer table scrollbar was restyled: '+getComputedStyle(unrelated).scrollbarColor);
  }
  unrelated.scrollTop=0;
`, expected.track, expected.thumb, expected.hover, expected.active, expected.corner, expected.firefox)
	return dashsnap.State{
		Key:      "process-editor-scrollbars-" + skin,
		Title:    "Process editor scrollbars — " + skin,
		Caption:  "TCL-571: intentional shell, palette and diagnostic overflow uses scoped default/wizard track, thumb and corner tokens; the viewer-table negative control retains its native scrollbar.",
		Wizard:   wizard,
		JS:       processEditorStateJS(extra),
		SettleMS: 300,
	}
}
