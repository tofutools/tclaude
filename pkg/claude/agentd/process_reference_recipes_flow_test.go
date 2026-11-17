package agentd_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/executor"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// tk_c324d5a4: the operator's two reference implementation processes, as
// the inline editor builds them (jstest/process-reference-recipes.mjs), run
// through the daemon along their main paths. Agents report and the operator
// decides exactly as they would; a script says what each one answers. The
// CI step is a command, stubbed to pass or fail; the settings gates are
// rules, so the run's params and CI's result alone route them.

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

// startReferenceRun starts a run of a reference process whose CI step
// (gh pr checks, a command) comes out as ci says, authorized as the
// operator does when starting it.
func startReferenceRun(t *testing.T, f *testharness.Flow, root, runID, key string, params map[string]string, ci engine.ProgramOutcome) {
	t.Helper()
	tmpl, err := agentd.ReferenceRecipeTemplateForTest(key)
	require.NoError(t, err)
	putProcessRuntimeTemplate(t, root, tmpl)
	t.Cleanup(agentd.SetProcessProgramPerformForTest(func(_ context.Context, dispatch *executor.Dispatch) (executor.Result, error) {
		command := dispatch.Command()
		require.Equal(t, "ci", command.NodeID, "the only command is CI")
		exit := 0
		if ci == engine.ProgramFailed {
			exit = 1
		}
		return processObservation(command, ci, exit), nil
	}))
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": runID, "templateId": tmpl.ID, "group": "crew", "params": params, "authorizeProgramProfiles": []string{""},
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

// ruleVerdicts are the outcomes the run's rules chose, in order, read from
// every page of its events (a reference run is longer than one page).
func ruleVerdicts(t *testing.T, f *testharness.Flow, runID string) []string {
	t.Helper()
	var out []string
	after := int64(0)
	for range 100 {
		rec := processRuntimeRequest(t, f, http.MethodGet, fmt.Sprintf("/v1/process/runs/%s/events?after=%d", runID, after), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var page processRuntimeEventPage
		testharness.DecodeJSON(t, rec, &page)
		if len(page.Events) == 0 {
			return out
		}
		for _, event := range page.Events {
			after = event.Sequence
			if event.Kind != "decision_recorded" || event.Actor != "rule" {
				continue
			}
			var decided ruleEvent
			require.NoError(t, json.Unmarshal(event.Payload, &decided))
			out = append(out, decided.Verdict)
		}
	}
	t.Fatal("too many event pages")
	return nil
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
// reviews, and the lead sends it round once more before moving on. CI runs;
// the Merge rule merges only with auto-merge on and CI passed, else the PR
// is left for the operator.
func TestReferenceSimple_IterateOnceThenShip(t *testing.T) {
	for _, tc := range []struct {
		autoMerge bool
		ci        engine.ProgramOutcome
		merged    bool
	}{
		{true, engine.ProgramSucceeded, true},
		{false, engine.ProgramSucceeded, false},
		{true, engine.ProgramFailed, false},
	} {
		t.Run(fmt.Sprintf("auto_merge=%v,ci=%s", tc.autoMerge, tc.ci), func(t *testing.T) {
			f, root := processRuntimeFlow(t)
			crew := haveReferenceCrew(t, f)
			const runID = "run_reference_simple"
			startReferenceRun(t, f, root, runID, "simple", map[string]string{"idea": "dark mode", "auto_merge": fmt.Sprint(tc.autoMerge)}, tc.ci)
			visits, status := drive(t, f, crew, runID, script{
				"idea-decided":       {"not-yet", "decided"},
				"iterate-or-move-on": {"iterate", "move-on"},
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
			for _, node := range []string{"discuss-the-idea", "provision-an-implementor", "iterate-or-move-on", "ready-the-pr"} {
				assert.Equal(t, crew.lead, visitsTo(visits, node)[0].agent, "%s is the lead's", node)
			}
			assert.Empty(t, visitsTo(visits, "merge"), "nobody is asked to merge: a rule decides")
			want := "leave"
			if tc.merged {
				want = "merge"
			}
			assert.Equal(t, []string{want}, ruleVerdicts(t, f, runID))
			assert.Equal(t, tc.merged, len(visitsTo(visits, "merge-the-pr")) == 1, "merged only with auto-merge on and CI passed")
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
	startReferenceRun(t, f, root, runID, "advanced", map[string]string{"idea": "dark mode", "allow_amend": "false", "auto_merge": "true"}, engine.ProgramSucceeded)
	visits, status := drive(t, f, crew, runID, script{
		"idea-decided":          {"decided"},
		"fix-minors-or-proceed": {"proceed"},
		"meets-the-criteria":    {"meets"},
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
	assert.Equal(t, []string{"merge"}, ruleVerdicts(t, f, runID))
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
	startReferenceRun(t, f, root, runID, "advanced", map[string]string{"idea": "dark mode", "allow_amend": "false", "auto_merge": "false"}, engine.ProgramSucceeded)
	visits, status := drive(t, f, crew, runID, script{
		"idea-decided":          {"decided"},
		"review.vote.2":         {"changes", "approved"},
		"fix-minors-or-proceed": {"fix-minors"},
		"meets-the-criteria":    {"meets"},
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
// the rule lets the lead judge the gap, the lead amends them and the PR goes
// ready; not allowed, the rule sends the work straight back to the
// implementor, and the next round meets them.
func TestReferenceAdvanced_MissedCriteriaAmendedOnlyWhenAllowed(t *testing.T) {
	for _, allow := range []bool{true, false} {
		t.Run(fmt.Sprintf("allow_amend=%v", allow), func(t *testing.T) {
			f, root := processRuntimeFlow(t)
			crew := haveReferenceCrew(t, f)
			const runID = "run_reference_criteria"
			startReferenceRun(t, f, root, runID, "advanced", map[string]string{"idea": "dark mode", "allow_amend": fmt.Sprint(allow), "auto_merge": "false"}, engine.ProgramSucceeded)
			visits, status := drive(t, f, crew, runID, script{
				"idea-decided":          {"decided"},
				"fix-minors-or-proceed": {"proceed"},
				"meets-the-criteria":    {"misses", "meets"},
				"amend-the-criteria":    {"amend"},
			})
			require.Equal(t, engine.RunCompleted, status, "%v", nodesOf(visits))
			gate := visitsTo(visits, "amend-the-criteria")
			if allow {
				assert.Equal(t, []string{"allowed", "leave"}, ruleVerdicts(t, f, runID))
				require.Len(t, gate, 1, "allowed: the lead judges the gap")
				assert.Equal(t, crew.lead, gate[0].agent)
				assert.Len(t, visitsTo(visits, "amend-criteria-and-design"), 1)
				assert.Len(t, visitsTo(visits, "implement"), 1)
				assert.Len(t, visitsTo(visits, "meets-the-criteria"), 1, "amended: straight on to ready")
			} else {
				assert.Equal(t, []string{"rework", "leave"}, ruleVerdicts(t, f, runID))
				assert.Empty(t, gate, "not allowed: the lead is not even asked")
				assert.Empty(t, visitsTo(visits, "amend-criteria-and-design"))
				assert.Len(t, visitsTo(visits, "implement"), 2, "back to the implementor")
				assert.Len(t, visitsTo(visits, "review.vote.1"), 2, "and through the reviewers again")
			}
			assert.Len(t, visitsTo(visits, "ready-the-pr"), 1)
		})
	}
}

// Scenario (example 1, reviewer tracks): four reviewers review alongside
// each other. The security reviewer asks for changes: only its track goes
// round, its fix done by the implementor, and the same reviewer looks
// again; the other three approve once. The lead goes on only once all four
// approved, and with auto-merge on and CI passed the rule merges.
func TestReferenceAdvancedTracks_OneReviewerLoopsAlone(t *testing.T) {
	f, root := processRuntimeFlow(t)
	crew := haveReferenceCrew(t, f)
	const runID = "run_reference_tracks"
	startReferenceRun(t, f, root, runID, "advanced-tracks", map[string]string{"idea": "dark mode", "allow_amend": "false", "auto_merge": "true"}, engine.ProgramSucceeded)
	visits, status := drive(t, f, crew, runID, script{
		"idea-decided":          {"decided"},
		"cold-review":           {"approved"},
		"security-review":       {"changes", "approved"},
		"product-review":        {"approved"},
		"engineering-review":    {"approved"},
		"fix-minors-or-proceed": {"proceed"},
		"meets-the-criteria":    {"meets"},
	})
	require.Equal(t, engine.RunCompleted, status, "%v", nodesOf(visits))
	require.Len(t, visitsTo(visits, "implement"), 1, "the work is not redone: the fix is the track's own")
	security := visitsTo(visits, "security-review")
	require.Len(t, security, 2, "the security track goes round once more")
	assert.Equal(t, security[0].agent, security[1].agent, "the same reviewer looks again")
	for _, review := range []string{"cold-review", "product-review", "engineering-review"} {
		assert.Len(t, visitsTo(visits, review), 1, "%s approves once, unaffected", review)
	}
	reviewers := map[string]bool{}
	for _, review := range []string{"cold-review", "security-review", "product-review", "engineering-review"} {
		for _, v := range visitsTo(visits, review) {
			assert.NotEqual(t, crew.implementor, v.agent, "%s: a reviewer did not implement it", review)
			reviewers[v.agent] = true
		}
	}
	assert.Len(t, reviewers, 4, "four different reviewers")
	fixes := 0
	for _, v := range visits {
		if strings.HasPrefix(v.node, "fix") && v.node != "fix-minors-or-proceed" {
			fixes++
			assert.Equal(t, crew.implementor, v.agent, "%s is the implementor's", v.node)
		}
	}
	assert.Equal(t, 1, fixes, "one fix, on the security track")
	minors := slices.Index(nodesOf(visits), "fix-minors-or-proceed")
	require.GreaterOrEqual(t, minors, 0)
	for _, v := range visits[minors:] {
		assert.NotContains(t, v.node, "review", "the lead goes on only after every review")
	}
	assert.Equal(t, []string{"merge"}, ruleVerdicts(t, f, runID))
	assert.Len(t, visitsTo(visits, "merge-the-pr"), 1)
}
