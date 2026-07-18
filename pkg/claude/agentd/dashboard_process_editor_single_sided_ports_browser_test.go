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

// TestDashboardProcessEditorSingleSidedPortsChrome uses trusted DevTools input
// for both pointer and keyboard connections, then captures the exact one-sided
// connector presentation in both dashboard skins.
func TestDashboardProcessEditorSingleSidedPortsChrome(t *testing.T) {
	if os.Getenv("TCLAUDE_DASHSNAP") == "" {
		t.Skip("browser smoke — set TCLAUDE_DASHSNAP=1 (needs local Chrome)")
	}

	f := newFlow(t)
	seedDashSnapFixture(t, f)
	srv := httptest.NewServer(agentd.BuildDashboardHandlerForTest())
	defer srv.Close()

	outDir := filepath.Join(dashSnapOutRoot(t), "process-editor-single-sided-ports-"+time.Now().Format("20060102-150405.000"))
	shots, err := dashsnap.Capture(dashsnap.Config{
		BaseURL: srv.URL,
		OutDir:  outDir,
		Width:   1680,
		Height:  1050,
		States: []dashsnap.State{
			processEditorSingleSidedPortsState(false),
			processEditorSingleSidedPortsState(true),
		},
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
		t.Fatalf("single-sided process connector browser smoke failed:\n%s\ncontact sheet: %s",
			strings.Join(failed, "\n"), filepath.Join(outDir, "index.html"))
	}
	t.Logf("single-sided process connector browser smoke: %s", filepath.Join(outDir, "index.html"))
}

func processEditorSingleSidedPortsState(wizard bool) dashsnap.State {
	skin := "default"
	if wizard {
		skin = "wizard"
	}
	startOut := `.process-port-layer [data-node-id="begin"] .process-port-out`
	endIn := `.process-port-layer [data-node-id="ship"] .process-port-in`
	workOut := `.process-port-layer [data-node-id="work"] .process-port-out`
	return dashsnap.State{
		Key:     "process-editor-single-sided-ports-" + skin,
		Title:   "Process editor single-sided Start/End — " + skin,
		Caption: "TCL-574 trusted Chrome: Start exposes only its outgoing connector, End only its incoming connector, ordinary nodes retain both, and real pointer/keyboard connection lifecycles preserve save, history, geometry, focus, and ARIA semantics in both skins.",
		Wizard:  wizard,
		JS: processEditorStateJS(`
  ed.model.addNode('task',{id:'work',name:'Work',x:220,y:280});
  ed.model.setEdgeTarget('begin','pass','work');
  ed.model.addEdge('work','pass','ship');
  ed.refresh({fit:true});
  await editorPaint();await editorPaint();
  var root=document.querySelector('.process-graph'),portLayer=root.querySelector('.process-port-layer');
  var ports=function(id){return Array.from(portLayer.querySelectorAll('[data-node-id="'+id+'"] .process-port'));};
  var identities=function(){return Array.from(portLayer.querySelectorAll('.process-port')).map(function(port){return port.closest('[data-node-id]').dataset.nodeId+':'+port.dataset.port;});};
  var expected=['begin:out','ship:in','work:in','work:out'];
  if(JSON.stringify(identities())!==JSON.stringify(expected)) throw new Error('connector presence/order mismatch: '+JSON.stringify(identities()));
  if(ports('begin').length!==1||ports('begin')[0].dataset.port!=='out'||ports('ship').length!==1||ports('ship')[0].dataset.port!=='in'||ports('work').length!==2) throw new Error('single-sided connector counts are wrong');
  if(portLayer.querySelector('[data-node-id="begin"] .process-port-in')||portLayer.querySelector('[data-node-id="ship"] .process-port-out')) throw new Error('impossible connector DOM still exists');
  Array.from(portLayer.querySelectorAll('.process-port')).forEach(function(port){
    if(port.getAttribute('role')!=='button'||port.getAttribute('tabindex')!=='0'||!port.getAttribute('aria-label')) throw new Error('live connector lost native focus/ARIA semantics');
  });
  if(!ports('begin')[0].getAttribute('aria-label').startsWith('Output port for begin')||!ports('ship')[0].getAttribute('aria-label').startsWith('Input port for ship')) throw new Error('single-sided accessible names are wrong');

  // The shared viewer graph intentionally omits editor availability metadata:
  // prove in a real browser that its existing two-port DOM remains unchanged.
  var graphModule=await import('/static/js/process-graph.js');
  var modelModule=await import('/static/js/process-edit-model.js');
  var makeProbeHost=function(){var host=document.createElement('div');host.style.cssText='position:absolute;left:-10000px;top:0;width:640px;height:480px';document.body.appendChild(host);return host;};
  var viewerHost=makeProbeHost(),viewerGraph=new graphModule.ProcessGraph(viewerHost,{nodes:[{id:'viewer-start',type:'start',label:'Start'},{id:'viewer-end',type:'end',label:'End'}],edges:[]},{fitOnRender:false});
  var viewerPorts=Array.from(viewerHost.querySelectorAll('.process-port')).map(function(port){return port.closest('[data-node-id]').dataset.nodeId+':'+port.dataset.port;});
  viewerPorts.sort();
  if(JSON.stringify(viewerPorts)!==JSON.stringify(['viewer-end:in','viewer-end:out','viewer-start:in','viewer-start:out'])) throw new Error('viewer default connector DOM changed: '+JSON.stringify(viewerPorts));
  viewerGraph.destroy();viewerHost.remove();

  // Loaded legacy ordinary edges across the now-unavailable sides are data,
  // not newly authored connections: render both, then delete/undo one and
  // require exact structural recovery. Template.Start remains in the payload.
  var legacyView=structuredClone(ed.model.saveBody());
  legacyView.edges.push({from:'work',outcome:'legacy-in',to:'begin'},{from:'ship',outcome:'legacy-out',to:'work'});
  var legacyModel=new modelModule.ProcessEditModel(legacyView),legacySave=JSON.stringify(legacyModel.saveBody());
  var legacyHost=makeProbeHost(),legacyGraph=new graphModule.ProcessGraph(legacyHost,legacyModel.graph(),{fitOnRender:false});
  var legacyEdgeCount=legacyModel.graph().edges.length,renderedLegacyEdges=Array.from(legacyHost.querySelectorAll('.process-edge'));
  if(renderedLegacyEdges.length!==legacyEdgeCount||!renderedLegacyEdges.some(function(edge){return edge.dataset.from==='work'&&edge.dataset.to==='begin';})||!renderedLegacyEdges.some(function(edge){return edge.dataset.from==='ship'&&edge.dataset.to==='work';})) throw new Error('loaded legacy illegal-side edges did not render');
  var legacyGeometry=JSON.stringify(legacyGraph.layout.edges.map(function(edge){return [edge.id,edge.path,edge.label.x,edge.label.y];}));
  legacyModel.deleteEdge('work','legacy-in');legacyModel.undo();
  if(JSON.stringify(legacyModel.saveBody())!==legacySave) throw new Error('legacy illegal-side edge delete/undo did not round-trip exactly');
  legacyGraph.setGraph(legacyModel.graph());
  if(JSON.stringify(legacyGraph.layout.edges.map(function(edge){return [edge.id,edge.path,edge.label.x,edge.label.y];}))!==legacyGeometry) throw new Error('legacy illegal-side routing changed after delete/undo');
  legacyGraph.destroy();legacyHost.remove();

  // Exercise controller commits against stale/non-rendered sides. Each must
  // fail closed without model, history, selection, or save mutation.
  var assertRejected=function(run,label){var before={save:JSON.stringify(ed.model.saveBody()),rev:ed.model.rev,undo:ed.model.undoStack.length,redo:ed.model.redoStack.length,selection:JSON.stringify(ed.selection)};run();var after={save:JSON.stringify(ed.model.saveBody()),rev:ed.model.rev,undo:ed.model.undoStack.length,redo:ed.model.redoStack.length,selection:JSON.stringify(ed.selection)};if(JSON.stringify(after)!==JSON.stringify(before)) throw new Error(label+' partially mutated editor state');};
  assertRejected(function(){ed.onPortDragStart({nodeId:'ship',port:'out'});ed.onPortDragEnd({nodeId:'ship',port:'out',point:{x:0,y:0},targetNodeId:'work',targetPort:'in',emptyCanvas:false});},'stale End/out source');
  ed.onPortDragStart({nodeId:'begin',port:'out'});
  ed.model.template.nodes.work.type='start';
  assertRejected(function(){ed.onPortDragEnd({nodeId:'begin',port:'out',point:{x:0,y:0},targetNodeId:'work',targetPort:'in',emptyCanvas:false});},'stale Start/in target');
  ed.model.template.nodes.work.type='task';ed.refresh();await editorPaint();

  var geometry=function(){var layout=ed.graph.layoutSnapshot();return layout.edges.map(function(edge){return [edge.id,edge.path,edge.label.x,edge.label.y];});};
  window.__singleSided={ed:ed,root:root,portLayer:portLayer,expected:expected,
    save:JSON.stringify(ed.model.saveBody()),geometry:JSON.stringify(geometry()),edges:ed.model.edges.length,
    identities:identities,geometryNow:geometry};
`),
		Actions: []dashsnap.BrowserAction{
			{Kind: "mouse-down-at", JS: `var r=document.querySelector('` + workOut + `').getBoundingClientRect();return {x:r.left+r.width/2,y:r.top+r.height/2};`},
			{Kind: "move-to-at", JS: `var r=document.querySelector('` + startOut + `').getBoundingClientRect();return {x:r.left+r.width/2,y:r.top+r.height/2};`, Steps: 8},
			{Kind: "mouse-up"},
			{Kind: "eval", JS: `var s=window.__singleSided;if(JSON.stringify(s.ed.model.saveBody())!==s.save||s.ed.model.edges.length!==s.edges||s.ed.graph.hasActiveInteraction()) throw new Error('trusted pointer authored an ordinary edge into Start');`},
			{Kind: "eval", JS: `document.querySelector('` + workOut + `').focus();`},
			{Kind: "key", Key: "enter"},
			{Kind: "eval", JS: `var s=window.__singleSided;if(!s.ed.graph.hasActiveInteraction()) throw new Error('illegal keyboard connection source did not activate');document.querySelector('` + startOut + `').focus();`},
			{Kind: "key", Key: "enter"},
			{Kind: "eval", JS: `var s=window.__singleSided;if(JSON.stringify(s.ed.model.saveBody())!==s.save||s.ed.model.edges.length!==s.edges) throw new Error('trusted keyboard authored an ordinary edge into Start');`},
			{Kind: "eval", JS: `document.querySelector('` + workOut + `').focus();`},
			{Kind: "key", Key: "enter"},
			{Kind: "eval", JS: `if(window.__singleSided.ed.graph.hasActiveInteraction()) throw new Error('illegal keyboard connection did not cancel');`},
			{Kind: "mouse-down-at", JS: `var r=document.querySelector('` + startOut + `').getBoundingClientRect();return {x:r.left+r.width/2,y:r.top+r.height/2};`},
			{Kind: "move-to-at", JS: `var r=document.querySelector('` + endIn + `').getBoundingClientRect();return {x:r.left+r.width/2,y:r.top+r.height/2};`, Steps: 8},
			{Kind: "mouse-up"},
			{Kind: "eval", JS: `var s=window.__singleSided;
  if(s.ed.model.edges.length!==s.edges+1||!s.ed.model.edges.some(function(edge){return edge.from==='begin'&&edge.to==='ship';})) throw new Error('trusted pointer connection did not commit');
  s.ed.closeInline(false);s.ed.applyHistory('undo');`},
			{Kind: "eval", JS: `document.querySelector('` + startOut + `').focus();`},
			{Kind: "key", Key: "enter"},
			{Kind: "eval", JS: `var s=window.__singleSided,source=document.querySelector('` + startOut + `');
  if(!s.ed.graph.hasActiveInteraction()||source.getAttribute('aria-pressed')!=='true'||!source.classList.contains('is-keyboard-source')) throw new Error('trusted keyboard source did not activate');
  document.querySelector('` + endIn + `').focus();`},
			{Kind: "key", Key: "enter"},
			{Kind: "eval", JS: `return new Promise(function(resolve,reject){requestAnimationFrame(function(){requestAnimationFrame(function(){try{
  var s=window.__singleSided;
  if(s.ed.model.edges.length!==s.edges+1||!s.ed.model.edges.some(function(edge){return edge.from==='begin'&&edge.to==='ship';})) throw new Error('trusted keyboard connection did not commit');
  s.ed.closeInline(false);s.ed.applyHistory('undo');
  requestAnimationFrame(function(){requestAnimationFrame(function(){try{
    if(JSON.stringify(s.ed.model.saveBody())!==s.save) throw new Error('pointer/keyboard undo did not restore save payload');
    if(JSON.stringify(s.geometryNow())!==s.geometry) throw new Error('legal edge endpoint/routing geometry changed');
    if(JSON.stringify(s.identities())!==JSON.stringify(s.expected)) throw new Error('connector focus order changed after interactions');
    if(s.ed.graph.hasActiveInteraction()) throw new Error('connection lifecycle remained active');
    s.ed.setSelection(null);s.ed.graph.fit();resolve();
  }catch(error){reject(error);}});});
  }catch(error){reject(error);}});});});`},
		},
		SettleMS: 300,
	}
}
