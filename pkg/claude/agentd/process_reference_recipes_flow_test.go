package agentd_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// tk_c324d5a4: the operator's two reference implementation processes, as
// the inline editor builds them (jstest/process-reference-recipes.mjs), run
// through the daemon along their main paths. Agents report and the operator
// decides exactly as they would; a script says what each one answers.
//
// TODO(tk_c324d5a4): "Amend the criteria" and "Merge" are lead decisions
// that quote params.allow_amend and params.auto_merge, so here the lead is
// told the setting and the script answers as a lead obeying it would. Once
// the engine has rule decisions, those nodes decide themselves from the
// params: drop their script entries and assert the setting alone routes.

// referenceCrew is the group a reference run works in: a lead, an
// implementor, and four reviewers free to take any review.
type referenceCrew struct {
	convOf      map[string]string
	lead        string
	implementor string
}

func haveReferenceCrew(t *testing.T, f *testharness.Flow) referenceCrew {
	t.Helper()
	f.HaveGroup("crew")
	crew := referenceCrew{convOf: map[string]string{}}
	add := func(conv, name, role, status string) string {
		id := havePerformerAgent(t, f, "crew", conv, name, role, status)
		crew.convOf[id] = conv
		return id
	}
	// The lead is busy, so "anyone" reviews go to the free reviewers first.
	crew.lead = add("rlea-1111-2222-3333-4444", "lena", "lead", "working")
	crew.implementor = add("rimp-1111-2222-3333-4444", "ivan", "implementor", "idle")
	for i, name := range []string{"rosa", "sami", "tove", "ulla"} {
		add(fmt.Sprintf("rrv%d-1111-2222-3333-4444", i), name, "reviewer", "idle")
	}
	return crew
}

func startReferenceRun(t *testing.T, f *testharness.Flow, root, runID, key string, params map[string]string) {
	t.Helper()
	tmpl, err := agentd.ReferenceRecipeTemplateForTest(key)
	require.NoError(t, err)
	putProcessRuntimeTemplate(t, root, tmpl)
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": runID, "templateId": tmpl.ID, "group": "crew", "params": params, "authorizeProgramProfiles": []string{},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	agentd.WaitForProcessDispatchForTest()
}

// script is what each node answers, visit by visit: a verdict for a
// decision (an agent's or the operator's), "" for a step that is just done.
// A gate's votes are keyed by vote ("review.vote.2"); a vote with no entry
// approves. Once a node's list runs out it keeps its last answer.
type script map[string][]string

// visit is one answer the run took: which node, from whom, which verdict.
type visit struct {
	node, agent, verdict string
}

// drive answers the run until it ends, and returns what it was asked, in
// order.
func drive(t *testing.T, f *testharness.Flow, crew referenceCrew, runID string, answers script) ([]visit, engine.RunStatus) {
	t.Helper()
	seen := map[string]int{}
	answer := func(node string) string {
		list := answers[node]
		if len(list) == 0 {
			if strings.Contains(node, ".vote.") {
				return "approved"
			}
			return ""
		}
		n := seen[node]
		seen[node]++
		if n >= len(list) {
			return list[len(list)-1]
		}
		return list[n]
	}
	var visits []visit
	for range 200 {
		view := showPerformerRun(t, f, runID)
		if view.Status != engine.RunRunning {
			return visits, view.Status
		}
		acted := false
		for _, command := range view.Commands {
			if command.Performer == nil || command.Performer.AgentID == "" {
				continue
			}
			conv := crew.convOf[command.Performer.AgentID]
			require.NotEmpty(t, conv, "%s went to an agent outside the crew", command.NodeID)
			verdict := answer(command.NodeID)
			body := map[string]any{"runId": runID, "nodeId": command.NodeID, "attempt": command.Attempt,
				"outcome": "succeeded", "note": fmt.Sprintf("%s by %s: %s", command.NodeID, command.Performer.Name, verdict)}
			if verdict != "" {
				body["verdict"] = verdict
			}
			rec := reportAs(t, f, conv, body)
			require.Equal(t, http.StatusOK, rec.Code, "%s: %s", command.NodeID, rec.Body.String())
			visits = append(visits, visit{command.NodeID, command.Performer.AgentID, verdict})
			acted = true
		}
		for _, decision := range showLoopRun(t, f, runID).AwaitingDecisions {
			verdict := answer(decision.NodeID)
			require.NotEmpty(t, verdict, "the operator is asked %s, and the script has no answer", decision.NodeID)
			require.Equal(t, http.StatusAccepted, decideLoop(t, f, runID, map[string]any{"nodeId": decision.NodeID, "verdict": verdict}))
			visits = append(visits, visit{decision.NodeID, "operator", verdict})
			acted = true
		}
		require.True(t, acted, "the run is stuck: %+v", view)
	}
	t.Fatal("the run did not end")
	return nil, ""
}

func nodesOf(visits []visit) []string {
	out := make([]string, 0, len(visits))
	for _, v := range visits {
		out = append(out, v.node)
	}
	return out
}

func visitsTo(visits []visit, node string) []visit {
	var out []visit
	for _, v := range visits {
		if v.node == node {
			out = append(out, v)
		}
	}
	return out
}

// briefAbout is the last process message conv got that mentions text.
func briefAbout(t *testing.T, conv, text string) string {
	t.Helper()
	found := ""
	for _, message := range processInbox(t, conv) {
		if strings.Contains(message.Body, text) {
			found = message.Body
		}
	}
	require.NotEmpty(t, found, "no process message mentions %q", text)
	return found
}

// Scenario (example 2): the operator decides the idea on the second pass;
// the implementor builds, a cold reviewer who is not the implementor
// reviews, and the lead sends it round once more before moving on. Auto-merge
// on: the lead merges. Auto-merge off: the PR is left for the operator.
func TestReferenceSimple_IterateOnceThenShip(t *testing.T) {
	for _, autoMerge := range []bool{true, false} {
		t.Run(fmt.Sprintf("auto_merge=%v", autoMerge), func(t *testing.T) {
			f, root := processRuntimeFlow(t)
			crew := haveReferenceCrew(t, f)
			const runID = "run_reference_simple"
			startReferenceRun(t, f, root, runID, "simple", map[string]string{"idea": "dark mode", "auto_merge": fmt.Sprint(autoMerge)})
			merge := "leave"
			if autoMerge {
				merge = "merge"
			}
			visits, status := drive(t, f, crew, runID, script{
				"idea-decided":       {"not-yet", "decided"},
				"iterate-or-move-on": {"iterate", "move-on"},
				"merge":              {merge},
			})
			require.Equal(t, engine.RunCompleted, status, "%v", nodesOf(visits))
			assert.Len(t, visitsTo(visits, "discuss-the-idea"), 2, "not yet: back to discussing")
			builds := visitsTo(visits, "build-the-feature")
			require.Len(t, builds, 2, "iterate: back to the implementor")
			for _, build := range builds {
				assert.Equal(t, crew.implementor, build.agent)
			}
			for _, review := range visitsTo(visits, "cold-review") {
				assert.NotEqual(t, crew.implementor, review.agent, "the cold reviewer did not build it")
			}
			for _, node := range []string{"discuss-the-idea", "provision-an-implementor", "iterate-or-move-on", "ready-the-pr", "merge"} {
				assert.Equal(t, crew.lead, visitsTo(visits, node)[0].agent, "%s is the lead's", node)
			}
			// The lead is told the setting it decides by.
			assert.Contains(t, briefAbout(t, crew.convOf[crew.lead], "Auto-merge is"), fmt.Sprintf("Auto-merge is %v.", autoMerge))
			assert.Equal(t, autoMerge, len(visitsTo(visits, "merge-the-pr")) == 1, "merged only when auto-merge is on")
		})
	}
}

// Scenario (example 1), all approve: the four reviewers, none of them the
// implementor and no two the same, approve together; the lead proceeds
// without fixing minors, the work meets the criteria, and it merges.
func TestReferenceAdvanced_AllApproveAndShip(t *testing.T) {
	f, root := processRuntimeFlow(t)
	crew := haveReferenceCrew(t, f)
	const runID = "run_reference_all_approve"
	startReferenceRun(t, f, root, runID, "advanced", map[string]string{"idea": "dark mode", "allow_amend": "false", "auto_merge": "true"})
	visits, status := drive(t, f, crew, runID, script{
		"idea-decided":          {"decided"},
		"fix-minors-or-proceed": {"proceed"},
		"meets-the-criteria":    {"meets"},
		"merge":                 {"merge"},
	})
	require.Equal(t, engine.RunCompleted, status, "%v", nodesOf(visits))
	reviewers := map[string]bool{}
	for k := 1; k <= 4; k++ {
		votes := visitsTo(visits, fmt.Sprintf("review.vote.%d", k))
		require.Len(t, votes, 1, "vote %d", k)
		assert.NotEqual(t, crew.implementor, votes[0].agent, "a reviewer did not implement it")
		reviewers[votes[0].agent] = true
	}
	assert.Len(t, reviewers, 4, "four different reviewers")
	assert.Equal(t, []string{
		"discuss-the-idea", "idea-decided", "define-acceptance-criteria", "provision-an-implementor", "implement",
	}, nodesOf(visits)[:5])
	assert.Empty(t, visitsTo(visits, "fix-the-minors"))
	assert.Empty(t, visitsTo(visits, "amend-the-criteria"))
	assert.Len(t, visitsTo(visits, "merge-the-pr"), 1)
}

// Scenario (example 1): the security reviewer asks for changes. The work goes
// back to the implementor with what every reviewer said, not just the
// dissent; next round all approve, the lead has the minors fixed (by the
// implementor), the work meets the criteria, and with auto-merge off the PR
// is left for the operator.
func TestReferenceAdvanced_OneReviewerAsksChangesThenMinorsAreFixed(t *testing.T) {
	f, root := processRuntimeFlow(t)
	crew := haveReferenceCrew(t, f)
	const runID = "run_reference_changes"
	startReferenceRun(t, f, root, runID, "advanced", map[string]string{"idea": "dark mode", "allow_amend": "false", "auto_merge": "false"})
	visits, status := drive(t, f, crew, runID, script{
		"idea-decided":          {"decided"},
		"review.vote.2":         {"changes", "approved"},
		"fix-minors-or-proceed": {"fix-minors"},
		"meets-the-criteria":    {"meets"},
		"merge":                 {"leave"},
	})
	require.Equal(t, engine.RunCompleted, status, "%v", nodesOf(visits))
	implements := visitsTo(visits, "implement")
	require.Len(t, implements, 2, "changes: back to the implementor")
	assert.Equal(t, implements[0].agent, implements[1].agent)
	// The second round's brief carries every reviewer's reasons.
	var round2 string
	for _, message := range processInbox(t, crew.convOf[crew.implementor]) {
		if strings.Contains(message.Body, "review.vote.2") {
			round2 = message.Body
			break
		}
	}
	require.NotEmpty(t, round2, "the implementor is told why it came back")
	for k := 1; k <= 4; k++ {
		assert.Contains(t, round2, fmt.Sprintf("review.vote.%d by", k), "vote %d's note travels with the round", k)
	}
	fixes := visitsTo(visits, "fix-the-minors")
	require.Len(t, fixes, 1)
	assert.Equal(t, implements[0].agent, fixes[0].agent, "the minors go to the same implementor")
	assert.Empty(t, visitsTo(visits, "merge-the-pr"), "auto-merge off: left for the operator")
}

// Scenario (example 1): the work misses the criteria. With amending allowed
// the lead amends them and the PR goes ready; with it not allowed the lead
// sends the work back to the implementor, and the next round meets them.
func TestReferenceAdvanced_MissedCriteriaAmendedOnlyWhenAllowed(t *testing.T) {
	for _, allow := range []bool{true, false} {
		t.Run(fmt.Sprintf("allow_amend=%v", allow), func(t *testing.T) {
			f, root := processRuntimeFlow(t)
			crew := haveReferenceCrew(t, f)
			const runID = "run_reference_criteria"
			startReferenceRun(t, f, root, runID, "advanced", map[string]string{"idea": "dark mode", "allow_amend": fmt.Sprint(allow), "auto_merge": "false"})
			amend := "rework"
			if allow {
				amend = "amend"
			}
			visits, status := drive(t, f, crew, runID, script{
				"idea-decided":          {"decided"},
				"fix-minors-or-proceed": {"proceed"},
				"meets-the-criteria":    {"misses", "meets"},
				"amend-the-criteria":    {amend},
				"merge":                 {"leave"},
			})
			require.Equal(t, engine.RunCompleted, status, "%v", nodesOf(visits))
			gate := visitsTo(visits, "amend-the-criteria")
			require.Len(t, gate, 1)
			assert.Equal(t, crew.lead, gate[0].agent)
			assert.Contains(t, briefAbout(t, crew.convOf[crew.lead], "The operator allowed amending"), fmt.Sprintf("The operator allowed amending: %v.", allow), "the lead is told the setting")
			if allow {
				assert.Len(t, visitsTo(visits, "amend-criteria-and-design"), 1)
				assert.Len(t, visitsTo(visits, "implement"), 1)
				assert.Len(t, visitsTo(visits, "meets-the-criteria"), 1, "amended: straight on to ready")
			} else {
				assert.Empty(t, visitsTo(visits, "amend-criteria-and-design"))
				assert.Len(t, visitsTo(visits, "implement"), 2, "not allowed: back to the implementor")
				assert.Len(t, visitsTo(visits, "review.vote.1"), 2, "and through the reviewers again")
			}
			assert.Len(t, visitsTo(visits, "ready-the-pr"), 1)
		})
	}
}
