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

// TestDashboardProcessEditorNewDefaultChrome proves the actual new-template
// action opens the same start-only draft in both dashboard skins. It also
// exercises the first connected-node mutation so the smaller initial graph
// cannot silently regress connector, history, selection, or focus behavior.
func TestDashboardProcessEditorNewDefaultChrome(t *testing.T) {
	if os.Getenv("TCLAUDE_DASHSNAP") == "" {
		t.Skip("browser smoke — set TCLAUDE_DASHSNAP=1 (needs local Chrome)")
	}

	f := newFlow(t)
	seedDashSnapFixture(t, f)
	srv := httptest.NewServer(agentd.BuildDashboardHandlerForTest())
	defer srv.Close()

	states := []dashsnap.State{newProcessDefaultState(false), newProcessDefaultState(true)}
	outDir := filepath.Join(dashSnapOutRoot(t), "process-editor-new-default-"+time.Now().Format("20060102-150405.000"))
	shots, err := dashsnap.Capture(dashsnap.Config{
		BaseURL: srv.URL,
		OutDir:  outDir,
		Width:   1680,
		Height:  1050,
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
		t.Fatalf("new process-template default browser smoke failed:\n%s\ncontact sheet: %s",
			strings.Join(failed, "\n"), filepath.Join(outDir, "index.html"))
	}
	t.Logf("new process-template default browser smoke: %s", filepath.Join(outDir, "index.html"))
}

func newProcessDefaultState(wizard bool) dashsnap.State {
	skin := "default"
	if wizard {
		skin = "wizard"
	}
	return dashsnap.State{
		Key:     "process-editor-new-default-" + skin,
		Title:   "New process template — " + skin,
		Caption: "TCL-572 real Chrome: the production new-template action starts with one Start and no prepopulated End, reports the incomplete route honestly, and keeps connected insertion atomic and focusable.",
		Wizard:  wizard,
		JS: `return (async function(){
  var nav=document.querySelector('nav [data-tab="processes"]');
  if(!nav||nav.offsetParent===null) throw new Error('Processes nav is not visible');
  nav.click();
  var deadline=Date.now()+5000;
  while(!document.querySelector('#process-template-new')&&Date.now()<deadline){await new Promise(function(resolve){setTimeout(resolve,40);});}
  var create=document.querySelector('#process-template-new');
  if(!create) throw new Error('new-template action did not render');
  create.click();
  while(!document.querySelector('#process-editor-canvas .process-graph-svg')&&Date.now()<deadline){await new Promise(function(resolve){setTimeout(resolve,40);});}
  var mount=document.querySelector('#process-editor-canvas'),ed=mount&&mount.__processEditor;
  if(!ed) throw new Error('new-template editor did not mount');
  var paint=function(){return new Promise(function(resolve){requestAnimationFrame(function(){requestAnimationFrame(resolve);});});};
  await paint();
  var ids=Object.keys(ed.model.template.nodes),domIDs=Array.from(mount.querySelectorAll('.process-node')).map(function(node){return node.dataset.nodeId;});
  if(JSON.stringify(ids)!==JSON.stringify(['start'])||JSON.stringify(domIDs)!==JSON.stringify(['start'])) throw new Error('blank graph is not exactly one Start: model='+JSON.stringify(ids)+' dom='+JSON.stringify(domIDs));
  if(ed.model.edges.length!==1||ed.model.edges[0].from!==''||ed.model.edges[0].outcome!=='start'||ed.model.edges[0].to!=='start') throw new Error('blank graph lost its sole start pseudo-edge');
  if(Object.keys(ed.model.layout.nodes).length!==1||ed.model.layout.nodes.start.x!==120||ed.model.layout.nodes.start.y!==90) throw new Error('blank Start pin changed');
  if(ed.selection!==null||ed.model.dirty||ed.model.canUndo||ed.model.canRedo||ed.model.sourceHash!=='') throw new Error('blank selection/history/sourceHash contract changed');
  if(mount.querySelector('[data-node-id="end"],[data-node-type="end"],[data-node-type="task"]')) throw new Error('blank editor prepopulated End or task');
  var startOut=mount.querySelector('.process-node-ports[data-node-id="start"] .process-port-out');
  var endCard=Array.from(mount.querySelectorAll('.process-palette-card')).find(function(card){return JSON.parse(card.dataset.paletteItem).type==='end';});
  if(!startOut||startOut.getAttribute('role')!=='button'||!endCard||endCard.querySelector('.process-palette-insert').disabled) throw new Error('ordinary connect/add affordances are unavailable');
  ed.validateNow();
  while((!ed.validation.mapped||!ed.validation.mapped.entries.some(function(entry){return entry.code==='missing_next'&&entry.targetId==='start';}))&&Date.now()<deadline){await new Promise(function(resolve){setTimeout(resolve,25);});}
  await paint();
  if(!ed.validation.mapped||!ed.validation.mapped.entries.some(function(entry){return entry.code==='missing_next'&&entry.targetId==='start';})) throw new Error('start-only validation did not report missing_next');
  var issues=mount.querySelector('.process-issues-summary'),overlay=mount.querySelector('.process-node[data-node-id="start"] .process-overlay-anchor');
  if(!issues||issues.closest('.process-issues-panel').hidden||!overlay) throw new Error('start-only validation is not visible in the editor');
  var created=ed.addConnectedNodeType('end',{nodeId:'start',port:'out'},{x:120,y:320});
  await paint();await Promise.resolve();
  if(created!=='end'||Object.keys(ed.model.template.nodes).length!==2||ed.model.node('end').type!=='end') throw new Error('first connected successor was not added');
  var edge=ed.model.findEdge('start','pass');
  if(!edge||edge.to!=='end'||ed.model.undoStack.length!==1||!ed.model.canUndo) throw new Error('connected insertion was not one history step');
  if(!ed.selection||ed.selection.type!=='node'||ed.selection.id!=='end') throw new Error('connected successor was not selected');
  var focused=document.activeElement&&document.activeElement.closest&&document.activeElement.closest('[data-node-id]');
  if(!focused||focused.dataset.nodeId!=='end') throw new Error('connected successor did not receive graph focus');
  ed.applyHistory('undo');await paint();
  if(Object.keys(ed.model.template.nodes).length!==1||ed.model.node('end')||ed.model.edges.length!==1||ed.model.canUndo||!ed.model.canRedo||ed.selection!==null) throw new Error('one undo did not restore the start-only draft with one redo step');

  // Reopen through the real action so the captured visual state is a pristine
  // blank draft, not the post-acceptance undo state exercised above.
  document.querySelector('[data-process-close-view]').click();
  while((!document.querySelector('#process-template-new')||document.querySelector('#process-editor-canvas'))&&Date.now()<deadline){await new Promise(function(resolve){setTimeout(resolve,25);});}
  document.querySelector('#process-template-new').click();
  while((!document.querySelector('#process-editor-canvas')||document.querySelector('#process-editor-canvas').__processEditor===ed)&&Date.now()<deadline){await new Promise(function(resolve){setTimeout(resolve,25);});}
  mount=document.querySelector('#process-editor-canvas');ed=mount&&mount.__processEditor;
  if(!ed||Object.keys(ed.model.template.nodes).length!==1||ed.model.node('end')||ed.model.canUndo||ed.model.canRedo||ed.selection!==null) throw new Error('reopened blank draft did not reset history and selection');
  ed.validateNow();
  while((!ed.validation.mapped||!ed.validation.mapped.entries.some(function(entry){return entry.code==='missing_next'&&entry.targetId==='start';}))&&Date.now()<deadline){await new Promise(function(resolve){setTimeout(resolve,25);});}
  await paint();
  if(!mount.querySelector('.process-node[data-node-id="start"] .process-overlay-anchor')||mount.querySelector('[data-node-id="end"]')) throw new Error('reopened visual state is not the honest start-only draft');
})();`,
		SettleMS: 250,
	}
}
