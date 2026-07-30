package agentd_test

import (
	"errors"
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

// TestDashSnapSandboxPreviewOverflow is TCL-862's real-layout
// acceptance check: an expanded effective-policy-preview bucket full of long
// rule rows must wrap inside the bucket instead of widening the
// sandbox-profile editor. It measures the editor's own boxes (overlay, card,
// preview section, every expanded bucket) rather than the page, because the
// dashboard shell is deliberately horizontally scrollable at narrow widths
// (JOH-313: header/nav widen to the content width and the tab strip never
// shrinks), so a page-level delta says nothing about this modal.
//
// 720px is where the defect was reported and 1280px is the roomy layout; the
// two together bound the responsive range this modal is designed for. Below
// ~552px the card's own min-width (520px) exceeds a flex-centred overlay and
// it is clipped regardless of the preview's contents — a separate, known floor
// this test deliberately does not sample. Both a Claude target and an OpenCode
// one are captured: the two targets bucket the same rules differently and the
// OpenCode run adds the long launch-blocked prose under a bucket, which is the
// widest content the preview can hold.
//
// The name shares TestDashSnap's prefix so the documented `-run TestDashSnap`
// smoke commands pick it up, and it honours TCLAUDE_DASHSNAP_FILTER/SHARD the
// same way so the canonical four-shard loop splits it like any other matrix.
func TestDashSnapSandboxPreviewOverflow(t *testing.T) {
	if os.Getenv("TCLAUDE_DASHSNAP") == "" {
		t.Skip("browser smoke — set TCLAUDE_DASHSNAP=1 (needs local Chrome)")
	}

	f := newFlow(t)
	seedDashSnapFixture(t, f)
	srv := httptest.NewServer(agentd.BuildDashboardHandlerForTest())
	defer srv.Close()

	var states []dashsnap.State
	for _, mode := range []string{"claude", "opencode"} {
		for _, width := range []int{1280, 720} {
			for _, wizard := range []bool{false, true} {
				skin := "regular"
				if wizard {
					skin = "wizard"
				}
				states = append(states, dashsnap.State{
					Key:     fmt.Sprintf("sandbox-preview-overflow-%s-%s-%d", mode, skin, width),
					Title:   fmt.Sprintf("Sandbox preview buckets — %s, %s %dpx", mode, skin, width),
					Caption: "Expanded effective-policy-preview buckets holding 11 long network rows plus the control-socket row keep the profile editor free of horizontal overflow and clipped labels.",
					Wizard:  wizard,
					Width:   width,
					Height:  1200,
					JS:      sandboxPreviewOverflowJS(mode),
					// The preview re-renders on each target-selector change;
					// give the last prediction room to paint before measuring.
					SettleMS: 700,
				})
			}
		}
	}

	if filter := os.Getenv("TCLAUDE_DASHSNAP_FILTER"); filter != "" {
		var filtered []dashsnap.State
		for _, state := range states {
			if strings.Contains(state.Key, filter) {
				filtered = append(filtered, state)
			}
		}
		if len(filtered) == 0 {
			t.Skipf("TCLAUDE_DASHSNAP_FILTER %q matches no preview-overflow state", filter)
		}
		states = filtered
	}
	matrixSize := len(states)
	shard, err := dashsnap.ParseShard(os.Getenv("TCLAUDE_DASHSNAP_SHARD"))
	if err != nil {
		t.Fatalf("TCLAUDE_DASHSNAP_SHARD: %v", err)
	}
	states = shard.Pick(states)
	if len(states) == 0 {
		t.Skipf("TCLAUDE_DASHSNAP_SHARD %d/%d selects no states (%d after filtering) — all covered by lower shards",
			shard.Index, shard.Total, matrixSize)
	}

	outDir := filepath.Join(dashSnapOutRoot(t),
		"sandbox-preview-overflow-"+time.Now().Format("20060102-150405.000")+shard.Suffix())
	shots, err := dashsnap.Capture(dashsnap.Config{
		BaseURL: srv.URL,
		OutDir:  outDir,
		Width:   1280,
		Height:  1200,
		States:  states,
	})
	// An environment without a usable Chrome must never read as a dashboard
	// regression — the same contract every other smoke here follows.
	if errors.Is(err, dashsnap.ErrBrowserUnavailable) {
		t.Skipf("environment: %v", err)
	}
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
		t.Fatalf("sandbox preview overflow smoke failed:\n%s\ncontact sheet: %s",
			strings.Join(failed, "\n"), filepath.Join(outDir, "index.html"))
	}
	t.Logf("sandbox preview overflow smoke: %s", filepath.Join(outDir, "index.html"))
}

// sandboxPreviewOverflowJS opens the real profile editor on TCL-859's evidence
// seed (11 network rules plus the control-socket row), selects a Linux
// tclaude-layer target, expands every populated preview bucket, and hard-fails
// on horizontal overflow or clipped labels inside the editor.
func sandboxPreviewOverflowJS(mode string) string {
	harnessName := "claude"
	targetText := "Claude on Linux · tclaude sandbox"
	if mode == "opencode" {
		harnessName = "opencode"
		targetText = "OpenCode on Linux · tclaude sandbox"
	}
	return fmt.Sprintf(`return (async function(){
  var module = await import('/static/js/sandbox-profiles.js');
  var allow = [
    {domain:'api.anthropic.com',include_subdomains:true,ports:[443]},
    {domain:'statsig.anthropic.com',include_subdomains:true,ports:[443]},
    {domain:'objects.githubusercontent.com',include_subdomains:true,ports:[443]},
    {domain:'registry.npmjs.org',include_subdomains:true,ports:[443]},
    {domain:'proxy.golang.org',include_subdomains:true,ports:[443]},
    {domain:'sum.golang.org',include_subdomains:true,ports:[443]},
    {domain:'raw.githubusercontent.com',include_subdomains:true,ports:[443]},
    {host:'github.com',ports:[443]},
    {host:'api.github.com',ports:[443]},
    {cidr:'192.0.2.0/24',ports:[443,8443]},
    {loopback:true,ports:[11434]}
  ];
  module.openSandboxProfileEditor({
    name:'dashsnap-preview-overflow-%s',filesystem:[],environment:[],includes:[],agent_directories:[],
    network:{baseline:'deny',packs:[],deny_packs:[],allow:allow,deny:[]},
    unix_sockets:{mode:'closed'}
  });
  var deadline=Date.now()+6000;
  while(!document.querySelector('#sandbox-profile-editor-evaluate-harness')&&Date.now()<deadline){
    await new Promise(function(resolve){setTimeout(resolve,40);});
  }
  var modal=document.querySelector('#sandbox-profile-editor-modal');
  if(!modal) throw new Error('preview-overflow-%s: sandbox editor did not open');
  function choose(select,value){
    select.value=value;
    select.dispatchEvent(new Event('change',{bubbles:true}));
  }
  choose(document.querySelector('#sandbox-profile-editor-evaluate-harness'),%q);
  deadline=Date.now()+3000;
  while(![...document.querySelector('#sandbox-profile-editor-evaluate-implementation').options]
    .some(function(option){return option.value==='tclaude-layer';})&&Date.now()<deadline){
    await new Promise(function(resolve){setTimeout(resolve,30);});
  }
  choose(document.querySelector('#sandbox-profile-editor-evaluate-implementation'),'tclaude-layer');
  choose(document.querySelector('#sandbox-profile-editor-evaluate-platform'),'linux');
  deadline=Date.now()+8000;
  while((!document.querySelector('.sbx-policy-target')||
    !document.querySelector('.sbx-policy-target').textContent.includes(%q))&&Date.now()<deadline){
    await new Promise(function(resolve){setTimeout(resolve,50);});
  }
  var target=document.querySelector('.sbx-policy-target');
  if(!target||!target.textContent.includes(%q)){
    throw new Error('preview-overflow-%s: selected target prediction did not settle');
  }
  var section=document.querySelector('#sandbox-profile-editor-effective-policy-section');
  section.open=true;
  var buckets=[...document.querySelectorAll('.sbx-rule-bucket')];
  if(!buckets.length) throw new Error('preview-overflow-%s: no rule buckets rendered');
  buckets.forEach(function(bucket){bucket.open=true;});
  var populated=buckets.filter(function(bucket){
    return bucket.querySelectorAll('.sbx-rule-row').length>0;
  });
  var networkRows=[...document.querySelectorAll('.sbx-rule-row')]
    .filter(function(row){return row.textContent.includes('network:');});
  if(networkRows.length!==11){
    throw new Error('preview-overflow-%s: expected 11 network rule rows, saw '+networkRows.length);
  }
  var socketRows=[...document.querySelectorAll('.sbx-rule-row')]
    .filter(function(row){return row.textContent.includes('Unix socket');});
  if(!socketRows.length){
    throw new Error('preview-overflow-%s: control-socket rule row missing from the preview');
  }
  await new Promise(function(resolve){requestAnimationFrame(function(){requestAnimationFrame(resolve);});});
  // The dashboard shell itself is horizontally scrollable by design at narrow
  // widths, so measure the editor's own boxes only.
  var probes=[
    ['modal-overlay',modal],
    ['modal-card',modal.querySelector('.cron-create-modal')],
    ['effective-policy',section]
  ];
  populated.forEach(function(bucket,index){probes.push(['bucket-'+index,bucket]);});
  var overflow=[];
  probes.forEach(function(probe){
    var node=probe[1];
    if(!node) return;
    var delta=node.scrollWidth-node.clientWidth;
    if(delta>1) overflow.push(probe[0]+' +'+delta+'px');
  });
  var clippedLabels=[...document.querySelectorAll(
    '.sbx-rule-row > span:first-child, .sbx-rule-bucket > summary, .sbx-rule-reason')]
    .filter(function(node){return node.scrollWidth-node.clientWidth>1;})
    .map(function(node){return (node.textContent||'').slice(0,60);});
  if(overflow.length||clippedLabels.length){
    // Name the widest offending boxes so a failure points at the element that
    // refuses to wrap instead of only reporting a container delta.
    var limit=modal.querySelector('.cron-create-modal').getBoundingClientRect().right;
    var offenders=[...modal.querySelectorAll('*')]
      .map(function(node){
        var rect=node.getBoundingClientRect();
        return {node:node,over:Math.round(rect.right-limit)};
      })
      .filter(function(entry){return entry.over>1;})
      .sort(function(a,b){return b.over-a.over;})
      .slice(0,6)
      .map(function(entry){
        return entry.node.tagName.toLowerCase()+'.'+(entry.node.className||'')
          +' over='+entry.over+'px';
      });
    throw new Error('preview-overflow-%s: overflow '+JSON.stringify(overflow)
      +' clipped '+JSON.stringify(clippedLabels)+' offenders '+JSON.stringify(offenders));
  }
  section.scrollIntoView({block:'center'});
  await new Promise(function(resolve){setTimeout(resolve,120);});
})();`,
		mode, mode, harnessName, targetText, targetText,
		mode, mode, mode, mode, mode)
}
