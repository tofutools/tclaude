package workgraph

import (
	"reflect"
	"sort"
	"testing"
)

// build reconstructs a template from a mermaid chart + optional node defs via
// the same RebuildFromSnapshot path agentd uses, so these tests exercise the
// real reconstruction, not a hand-built struct.
func build(t *testing.T, mermaid string, nodes map[string]*Node) *Template {
	t.Helper()
	tmpl, err := RebuildFromSnapshot(mermaid, nodes)
	if err != nil {
		t.Fatalf("RebuildFromSnapshot: %v", err)
	}
	return tmpl
}

func sortedCopy(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

func assertSet(t *testing.T, label string, got, want []string) {
	t.Helper()
	g, w := sortedCopy(got), sortedCopy(want)
	if len(g) == 0 {
		g = []string{}
	}
	if len(w) == 0 {
		w = []string{}
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("%s: got %v, want %v", label, got, want)
	}
}

func TestAdvance_LinearPass(t *testing.T) {
	tmpl := build(t, "flowchart TD\n a --> b\n b --> c\n", nil)
	res := Advance(tmpl, "a", OutcomePass, map[string]NodeRunState{
		"b": NodePending, "c": NodePending,
	})
	assertSet(t, "ready", res.Ready, []string{"b"})
	assertSet(t, "skipped", res.Skipped, nil)
}

func TestAdvance_EnumBranchSkipsSibling(t *testing.T) {
	mmd := "flowchart TD\n" +
		" start{Pick} -->|left| a\n" +
		" start -->|right| b\n" +
		" a --> j\n b --> j\n j --> done\n"
	tmpl := build(t, mmd, nil)

	all := func() map[string]NodeRunState {
		return map[string]NodeRunState{
			"a": NodePending, "b": NodePending, "j": NodePending, "done": NodePending,
		}
	}

	left := Advance(tmpl, "start", "left", all())
	assertSet(t, "left ready", left.Ready, []string{"a"})
	// b is unreachable once we go left; j is still fed by a, done still by j.
	assertSet(t, "left skipped", left.Skipped, []string{"b"})

	right := Advance(tmpl, "start", "right", all())
	assertSet(t, "right ready", right.Ready, []string{"b"})
	assertSet(t, "right skipped", right.Skipped, []string{"a"})
}

func TestAdvance_JoinAllWaitsThenFires(t *testing.T) {
	// Diamond join: start branches, the abandoned arm is already skipped, and
	// settling the live arm fires the JoinAll node since its only other
	// predecessor is settled.
	mmd := "flowchart TD\n" +
		" start{Pick} -->|left| a\n start -->|right| b\n" +
		" a --> j\n b --> j\n j --> done\n"
	tmpl := build(t, mmd, nil)

	res := Advance(tmpl, "a", OutcomePass, map[string]NodeRunState{
		"start": NodeSettled,
		"a":     NodeSettled, // just settled
		"b":     NodeSettled, // skipped earlier
		"j":     NodePending,
		"done":  NodePending,
	})
	assertSet(t, "ready", res.Ready, []string{"j"})
	assertSet(t, "skipped", res.Skipped, nil)
}

func TestAdvance_JoinAllParallelHoldsUntilBothArrive(t *testing.T) {
	// True parallel fan-out: fork activates both arms; the join must not fire
	// until BOTH have settled.
	mmd := "flowchart TD\n" +
		" fork --> a\n fork --> b\n a --> j\n b --> j\n j --> done\n"
	tmpl := build(t, mmd, nil)

	// fork settles → both arms ready, nothing skipped.
	openRes := Advance(tmpl, "fork", OutcomePass, map[string]NodeRunState{
		"a": NodePending, "b": NodePending, "j": NodePending, "done": NodePending,
	})
	assertSet(t, "fork ready", openRes.Ready, []string{"a", "b"})
	assertSet(t, "fork skipped", openRes.Skipped, nil)

	// a settles while b is still live → join NOT satisfied yet.
	aRes := Advance(tmpl, "a", OutcomePass, map[string]NodeRunState{
		"fork": NodeSettled, "a": NodeSettled, "b": NodeLive,
		"j": NodePending, "done": NodePending,
	})
	assertSet(t, "a ready (join not ready)", aRes.Ready, nil)
	assertSet(t, "a skipped", aRes.Skipped, nil)

	// b settles too → join fires.
	bRes := Advance(tmpl, "b", OutcomePass, map[string]NodeRunState{
		"fork": NodeSettled, "a": NodeSettled, "b": NodeSettled,
		"j": NodePending, "done": NodePending,
	})
	assertSet(t, "b ready (join ready)", bRes.Ready, []string{"j"})
}

func TestAdvance_JoinAnyFiresOnFirstArrival(t *testing.T) {
	mmd := "flowchart TD\n" +
		" fork --> a\n fork --> b\n a --> j\n b --> j\n j --> done\n"
	tmpl := build(t, mmd, map[string]*Node{
		"j": {Join: JoinAny},
	})
	res := Advance(tmpl, "a", OutcomePass, map[string]NodeRunState{
		"fork": NodeSettled, "a": NodeSettled, "b": NodeLive,
		"j": NodePending, "done": NodePending,
	})
	assertSet(t, "join-any ready on first arrival", res.Ready, []string{"j"})
}

func TestAdvance_LoopBackNotSkipped(t *testing.T) {
	// test -->|fail| implement is a loop-back. Taking the pass path must NOT
	// skip implement (it already ran / is settled), nor anything still live.
	mmd := "flowchart TD\n" +
		" implement --> test\n" +
		" test -->|pass| review\n test -->|fail| implement\n" +
		" review --> done\n"
	tmpl := build(t, mmd, map[string]*Node{
		"test": {OnFail: OnFailContinue},
	})
	res := Advance(tmpl, "test", OutcomePass, map[string]NodeRunState{
		"implement": NodeSettled, // already ran
		"test":      NodeSettled,
		"review":    NodePending,
		"done":      NodePending,
	})
	assertSet(t, "ready", res.Ready, []string{"review"})
	assertSet(t, "skipped (no loop-back skip)", res.Skipped, nil)
}

func TestAdvance_FailFollowsFailEdge(t *testing.T) {
	mmd := "flowchart TD\n" +
		" build --> test\n" +
		" build -->|fail| cleanup\n" +
		" test --> done\n cleanup --> done\n"
	tmpl := build(t, mmd, map[string]*Node{
		"build": {OnFail: OnFailContinue},
	})
	res := Advance(tmpl, "build", OutcomeFail, map[string]NodeRunState{
		"test": NodePending, "cleanup": NodePending, "done": NodePending,
	})
	assertSet(t, "fail ready", res.Ready, []string{"cleanup"})
	// test was the success path; taking fail orphans it.
	assertSet(t, "fail skipped", res.Skipped, []string{"test"})
}

func TestAdvance_TransitiveSkip(t *testing.T) {
	// Abandoning a branch skips its whole exclusive sub-tree, not just the head.
	mmd := "flowchart TD\n" +
		" start{Pick} -->|x| a\n start -->|y| b\n" +
		" b --> b2\n b2 --> b3\n" +
		" a --> done\n b3 --> done\n"
	tmpl := build(t, mmd, nil)
	res := Advance(tmpl, "start", "x", map[string]NodeRunState{
		"a": NodePending, "b": NodePending, "b2": NodePending,
		"b3": NodePending, "done": NodePending,
	})
	assertSet(t, "ready", res.Ready, []string{"a"})
	assertSet(t, "skipped sub-tree", res.Skipped, []string{"b", "b2", "b3"})
}

// Regression: a join node with loop-back predecessors must NOT wait for those
// not-yet-run predecessors. The example template's `implement` is fed by
// plan-->, test-->|fail|, review-->|changes|; when plan settles, implement must
// ready immediately rather than deadlocking on test/review (which run AFTER it).
func TestAdvance_LoopBackPredecessorDoesNotDeadlockJoin(t *testing.T) {
	mmd := "flowchart TD\n" +
		" plan --> implement\n" +
		" implement --> test\n" +
		" test -->|pass| review\n test -->|fail| implement\n" +
		" review -->|approved| deploy\n review -->|changes| implement\n" +
		" deploy --> done\n"
	tmpl := build(t, mmd, map[string]*Node{
		"test":   {OnFail: OnFailContinue, Verify: Verify{Kind: VerifyEnum, Values: []string{"pass"}}},
		"review": {Verify: Verify{Kind: VerifyEnum, Values: []string{"approved", "changes"}}},
	})
	// plan settles first; everything else pending. implement has 3 incoming
	// edges (plan + two loop-backs) but must still fire on plan alone.
	res := Advance(tmpl, "plan", OutcomePass, map[string]NodeRunState{
		"implement": NodePending, "test": NodePending, "review": NodePending,
		"deploy": NodePending, "done": NodePending,
	})
	assertSet(t, "implement readies on plan despite loop-back preds", res.Ready, []string{"implement"})
	assertSet(t, "nothing skipped (all still reachable via the loop)", res.Skipped, nil)
}

// Regression: a branch edge straight into a JoinAll node. Taking the live arm
// must ready the join (the other arm is dead and will be skipped), NOT skip the
// join + everything downstream and falsely complete. This is the cold-review
// "direct-branch-into-JoinAll" case.
func TestAdvance_DirectBranchIntoJoinReadiesNotSkips(t *testing.T) {
	mmd := "flowchart TD\n" +
		" s{Pick} -->|a| j\n" +
		" s -->|b| q\n q --> j\n" +
		" j --> done\n"
	tmpl := build(t, mmd, nil)
	// s settles on outcome a: edge s-->j is taken. The other pred of j is q,
	// which sits on the not-taken b-branch and will be skipped — so j must
	// ready now, not stall.
	res := Advance(tmpl, "s", "a", map[string]NodeRunState{
		"j": NodePending, "q": NodePending, "done": NodePending,
	})
	assertSet(t, "join readies on the taken arm", res.Ready, []string{"j"})
	assertSet(t, "the not-taken arm q is skipped", res.Skipped, []string{"q"})
}

func TestAdvance_NilTemplate(t *testing.T) {
	res := Advance(nil, "x", OutcomePass, nil)
	assertSet(t, "ready", res.Ready, nil)
	assertSet(t, "skipped", res.Skipped, nil)
}

func TestAllowedOutcomes(t *testing.T) {
	tmpl := build(t, "flowchart TD\n review{R} --> done\n plain --> done\n", map[string]*Node{
		"review": {Verify: Verify{Kind: VerifyEnum, Values: []string{"approved", "changes"}}},
		"plain":  {Executor: Executor{Kind: ExecHuman}},
	})
	assertSet(t, "enum outcomes", tmpl.AllowedOutcomes("review"),
		[]string{"approved", "changes", OutcomeFail})
	assertSet(t, "plain outcomes", tmpl.AllowedOutcomes("plain"),
		[]string{OutcomeFail, OutcomePass})
	// Unknown node → safe default.
	assertSet(t, "unknown outcomes", tmpl.AllowedOutcomes("nope"),
		[]string{OutcomeFail, OutcomePass})
}

func TestFailHalts(t *testing.T) {
	mmd := "flowchart TD\n" +
		" stop --> done\n" +
		" cont --> done\n cont -->|fail| recover\n" +
		" contNoEdge --> done\n" +
		" recover --> done\n"
	tmpl := build(t, mmd, map[string]*Node{
		"cont":       {OnFail: OnFailContinue},
		"contNoEdge": {OnFail: OnFailContinue}, // continue but no |fail| edge
	})
	if !tmpl.FailHalts("stop") {
		t.Error("stop: default on_fail must halt")
	}
	if tmpl.FailHalts("cont") {
		t.Error("cont: on_fail continue + |fail| edge must not halt")
	}
	if !tmpl.FailHalts("contNoEdge") {
		t.Error("contNoEdge: continue without a |fail| edge must halt (nowhere to go)")
	}
	if !tmpl.FailHalts("missing") {
		t.Error("missing node: must halt")
	}
}

func TestRebuildFromSnapshot_RecomputesEntry(t *testing.T) {
	tmpl, err := RebuildFromSnapshot("flowchart TD\n a --> b\n b --> c\n", nil)
	if err != nil {
		t.Fatalf("RebuildFromSnapshot: %v", err)
	}
	assertSet(t, "entry", tmpl.Entry, []string{"a"})
	if len(tmpl.Edges) != 2 {
		t.Errorf("edges: got %d, want 2", len(tmpl.Edges))
	}
	if _, ok := tmpl.MermaidNodes["c"]; !ok {
		t.Error("chart node c missing after rebuild")
	}
}

// --- JOH-39: loop re-entry, loop-body, max-visits ---

// A taken back-edge into a settled ancestor is reported as Reentry, not Ready.
func TestAdvance_BackEdgeReportsReentry(t *testing.T) {
	// implement --> test; test -->|fail| implement (loop); test -->|pass| done
	mmd := "flowchart TD\n implement --> test\n test -->|fail| implement\n test -->|pass| done\n"
	tmpl := build(t, mmd, map[string]*Node{
		"implement": {OnFail: OnFailContinue},
		"test":      {OnFail: OnFailContinue},
	})
	// implement already ran (done), test just settled fail.
	res := Advance(tmpl, "test", OutcomeFail, map[string]NodeRunState{
		"implement": NodeSettled, "test": NodeSettled, "done": NodePending,
	})
	assertSet(t, "ready", res.Ready, nil)
	assertSet(t, "reentry", res.Reentry, []string{"implement"})
	// done is on the not-taken (pass) branch this iteration → skipped this settle,
	// but it's pending+reachable-from-nothing-live here; assert it's not readied.
	assertSet(t, "no spurious ready", res.Ready, nil)
}

// test -->|pass| done takes the forward edge; no reentry.
func TestAdvance_PassBreaksLoop(t *testing.T) {
	mmd := "flowchart TD\n implement --> test\n test -->|fail| implement\n test -->|pass| done\n"
	tmpl := build(t, mmd, map[string]*Node{"test": {OnFail: OnFailContinue}})
	res := Advance(tmpl, "test", OutcomePass, map[string]NodeRunState{
		"implement": NodeSettled, "test": NodeSettled, "done": NodePending,
	})
	assertSet(t, "ready", res.Ready, []string{"done"})
	assertSet(t, "reentry", res.Reentry, nil)
}

// A taken edge into an already-settled FORWARD node (late JoinAny predecessor)
// is NOT a loop-back: it must not be reported as Reentry.
func TestAdvance_LateForwardEdgeIsNotReentry(t *testing.T) {
	// a --> j, b --> j (j is JoinAny). j already fired off a; now b settles.
	mmd := "flowchart TD\n a --> j\n b --> j\n j --> done\n"
	tmpl := build(t, mmd, map[string]*Node{"j": {Join: JoinAny}})
	res := Advance(tmpl, "b", OutcomePass, map[string]NodeRunState{
		"a": NodeSettled, "j": NodeSettled, "done": NodeLive,
	})
	assertSet(t, "reentry", res.Reentry, nil) // j is downstream of b, not an ancestor
	assertSet(t, "ready", res.Ready, nil)
}

func TestLoopBody_BetweenTargetAndBackEdgeSource(t *testing.T) {
	// implement --> review --> test; test -->|fail| implement; test -->|pass| done
	mmd := "flowchart TD\n implement --> review\n review --> test\n" +
		" test -->|fail| implement\n test -->|pass| done\n"
	tmpl := build(t, mmd, map[string]*Node{"test": {OnFail: OnFailContinue}})
	body := tmpl.LoopBody("implement", "test")
	got := []string{}
	for id := range body {
		got = append(got, id)
	}
	assertSet(t, "loop body", got, []string{"implement", "review", "test"})
	if body["done"] {
		t.Errorf("done (the break branch) must not be in the loop body")
	}
}

func TestEffectiveMaxVisits(t *testing.T) {
	cases := []struct {
		max       int
		def       int
		wantCap   int
		wantUnbnd bool
	}{
		{0, 20, 20, false}, // 0 → default cap
		{-1, 20, 0, true},  // -1 → unbounded
		{5, 20, 5, false},  // explicit
		{1, 20, 1, false},  // explicit minimal
	}
	for _, c := range cases {
		cap, unb := EffectiveMaxVisits(&Node{MaxVisits: c.max}, c.def)
		if cap != c.wantCap || unb != c.wantUnbnd {
			t.Errorf("EffectiveMaxVisits(max=%d, def=%d) = (%d,%v), want (%d,%v)",
				c.max, c.def, cap, unb, c.wantCap, c.wantUnbnd)
		}
	}
	if cap, unb := EffectiveMaxVisits(nil, 7); cap != 7 || unb {
		t.Errorf("nil node: got (%d,%v), want (7,false)", cap, unb)
	}
}
