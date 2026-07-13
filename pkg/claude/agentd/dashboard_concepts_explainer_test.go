package agentd

import "testing"

// TestDashboardHTML_ConceptsExplainer pins the JOH-352 concepts explainer in the
// embedded dashboard source. The template editor gains one collapsible panel
// that tells the work-pattern / process / rhythms story TOGETHER — the point of
// the ticket is the interplay, not three separate blurbs. The text has to land
// in BOTH vocab modes with the SAME facts, and those facts must stay honest to
// the semantics (pattern = one-shot with re-brief re-delivering the CURRENT
// pattern; process = advisory, blocks nothing; rhythms = a deploy-time cron
// SNAPSHOT that editing the template later does not retune). It is all
// client-side HTML/CSS, so — like the other dashboard render guards — this
// string-searches the embedded source rather than running the JS.
func TestDashboardHTML_ConceptsExplainer(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !dashboardSourceContains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// The panel exists as a collapsed <details> above the pattern/process/rhythm
	// sections, with both body variants present in the DOM (CSS picks one).
	must(`id="template-editor-concepts"`, "the concepts <details> panel exists")
	must("tpl-concepts-plain", "the plain-mode body variant is present")
	must("tpl-concepts-wizard", "the wizard-mode body variant is present")

	// (a) Plain-mode summary + the together framing.
	must("ⓘ How deploying works — pattern, process & rhythms", "plain summary names all three concepts")
	must("Three things shape a deployed force and they work <b>together</b>", "plain body leads with the together story")

	// (b) Plain-mode honesty checklist — the three lifecycle claims that must
	// stay TRUE to the code.
	must("<b>Re-brief</b> re-delivers the template's <i>current</i> pattern", "work pattern = one-shot, re-brief re-delivers the CURRENT pattern")
	must("nothing is blocked, no permissions change, nothing auto-advances", "process is advisory — advancing blocks nothing")
	must("does <b>not</b> retune a force already in the field", "rhythms are a deploy-time snapshot, not retuned by later edits")

	// (c) Wizard-mode summary + the SAME facts in flavored voice.
	must("🔮 How a summoning works — rite, quest & drumbeats", "wizard summary names all three concepts")
	must("Rite of command — the opening whispers.", "wizard names the work pattern as the rite")
	must("Quest plan — the chapters.", "wizard names the process as the quest plan")
	must("Drumbeats — the pulse.", "wizard names the rhythms as the drumbeats")
	must("will <b>not</b> retune a party already afield", "wizard keeps the deploy-time-snapshot fact")

	// (d) The lifecycle table (both modes) carries the enforced?/repeats? columns.
	must("no — re-brief re-sends on demand", "plain table: work pattern does not repeat, re-brief re-sends")
	must("no — advisory", "plain table: process is not enforced")
	must("no — a re-brief re-speaks it", "wizard table mirror of the re-brief-on-demand row")

	// (e) The plain/wizard body swap is CSS-driven (theme toggle re-flavours the
	// panel with no re-render) and the panel carries the editor's wizard skin (a
	// white-panel regression can't be caught by strings, but the block's ABSENCE
	// can).
	must("body.wizard .tpl-concepts-plain { display: none; }", "wizard hides the plain body")
	must("body.wizard .tpl-concepts-wizard { display: block; }", "wizard shows the wizard body")
	must("body.wizard #template-editor-modal .tpl-concepts {", "the concepts panel has a wizard skin block")
}
