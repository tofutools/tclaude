package engine

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// compoundStageTemplate is the canonical compound fixture: start -> build ->
// end, where build declares a program-backed plan stage, one check step, and a
// review stage. mutate customizes the compound node before the template is
// assembled, which is how the eligibility table reaches one stage slot at a
// time.
func compoundStageTemplate(mutate func(node *model.Node)) *model.Template {
	build := compoundTask("end")
	if mutate != nil {
		mutate(&build)
	}
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "compound", Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "build"}},
			"build": build,
			"end":   {Type: model.NodeTypeEnd},
		},
	}
}

func compoundTask(next string) model.Node {
	return model.Node{
		Type:      model.NodeTypeTask,
		Performer: &model.Performer{Kind: model.PerformerProgram, Profile: "worker", Run: "do"},
		Plan: &model.Step{ID: "plan",
			Performer: model.Performer{Kind: model.PerformerProgram, Profile: "planner", Run: "plan"}},
		Checks: []model.Step{{ID: "unit",
			Performer: model.Performer{Kind: model.PerformerProgram, Profile: "worker", Run: "unit"}}},
		Review: &model.Step{ID: "review",
			Performer: model.Performer{Kind: model.PerformerProgram, Profile: "reviewer", Run: "review"}},
		Next: model.Next{model.DefaultOutcome: next},
	}
}

// compoundStageIDs is the exact expansion the fixture must produce, in order.
var compoundStageIDs = []string{"build.plan", "build.do", "build.test.unit", "build.review", "build.done"}

func TestPrepareExpandsCompoundIntoExactOrderedStagesWithBoundStagePrograms(t *testing.T) {
	definition := mustPrepare(t, compoundStageTemplate(nil), nil)

	want := []string{"start", "build", "build.plan", "build.do", "build.test.unit", "build.review", "build.done", "end"}
	if got := preparedNodeIDs(definition); !slices.Equal(got, want) {
		t.Fatalf("prepared order\n got: %v\nwant: %v", got, want)
	}

	parent := definition.nodes[definition.index["build"]]
	if parent.kind != definitionCompound {
		t.Fatalf("build kind = %d", parent.kind)
	}
	stageIDs := make([]string, 0, len(parent.children))
	for _, childIndex := range parent.children {
		stageIDs = append(stageIDs, definition.nodes[childIndex].id)
	}
	if !slices.Equal(stageIDs, compoundStageIDs) {
		t.Fatalf("derived stages\n got: %v\nwant: %v", stageIDs, compoundStageIDs)
	}

	// Every stage program is bound from its own performer, and the parent's do
	// performer belongs to the do stage rather than to the parent.
	for stageID, wantRun := range map[string]string{
		"build.plan": "plan", "build.do": "do", "build.test.unit": "unit", "build.review": "review",
	} {
		stage := definition.nodes[definition.index[stageID]]
		if stage.kind != definitionTask || stage.program.Run != wantRun {
			t.Fatalf("stage %q = kind %d, run %q", stageID, stage.kind, stage.program.Run)
		}
		if stage.maxAttempts != 1 || stage.retryAuthored {
			t.Fatalf("stage %q carries a retry budget: %#v", stageID, stage)
		}
	}
	if done := definition.nodes[definition.index["build.done"]]; done.kind != definitionStageDone {
		t.Fatalf("done stage kind = %d", done.kind)
	}
	if parent.program.Run != "" {
		t.Fatalf("compound parent bound a program of its own: %#v", parent.program)
	}

	// Stages carry no edges of any kind: the parent keeps exactly the authored
	// ones, and sequencing is the ordered child list.
	for _, childIndex := range parent.children {
		child := definition.nodes[childIndex]
		if len(child.outgoing) != 0 || len(child.incoming) != 0 {
			t.Fatalf("stage %q has synthetic edges: in %v out %v", child.id, child.incoming, child.outgoing)
		}
	}
	if len(parent.outgoing) != 1 || len(parent.incoming) != 1 {
		t.Fatalf("compound parent lost its authored edges: in %v out %v", parent.incoming, parent.outgoing)
	}
}

func TestCompoundStagesRunInOrderAndSettleTheParentEdgeExactlyOnce(t *testing.T) {
	tmpl := compoundStageTemplate(nil)
	definition := mustPrepare(t, tmpl, nil)
	checkpoint, err := Initialize("run-compound", definition)
	if err != nil {
		t.Fatal(err)
	}

	// Activating the compound is engine-owned: the parent runs, only its first
	// stage becomes ready, and its authored route stays unresolved.
	checkpoint, command := advanceAndPlan(t, checkpoint, definition)
	if checkpoint.Nodes["build"] != NodeRunning {
		t.Fatalf("compound parent = %q", checkpoint.Nodes["build"])
	}
	if command == nil || command.NodeID != "build.plan" {
		t.Fatalf("first command = %#v", command)
	}
	if checkpoint.Edges["build"][model.DefaultOutcome] != EdgeUnresolved {
		t.Fatalf("parent edge settled before its stages ran: %q", checkpoint.Edges["build"][model.DefaultOutcome])
	}
	for _, stageID := range compoundStageIDs[1:] {
		if checkpoint.Nodes[stageID] != NodePending {
			t.Fatalf("stage %q = %q before its turn", stageID, checkpoint.Nodes[stageID])
		}
	}

	checkpoint, dispatched := driveToTerminal(t, checkpoint, definition, command)
	if want := compoundStageIDs[:4]; !slices.Equal(dispatched, want) {
		t.Fatalf("dispatch order\n got: %v\nwant: %v", dispatched, want)
	}
	if checkpoint.Status != RunCompleted {
		t.Fatalf("run status = %q", checkpoint.Status)
	}
	for nodeID, status := range checkpoint.Nodes {
		if status != NodeDone {
			t.Fatalf("terminal node %q = %q", nodeID, status)
		}
	}
	if checkpoint.Edges["build"][model.DefaultOutcome] != EdgeArrived {
		t.Fatalf("parent edge = %q", checkpoint.Edges["build"][model.DefaultOutcome])
	}

	// The done stage is the one path that settles the parent, and it settles it
	// once: replaying it fails closed rather than arriving at "end" twice.
	if _, err := Apply(checkpoint, definition, advanced("build.done")); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("replayed done stage: %v", err)
	}
}

func TestCompoundEntryNodeActivatesItsFirstStage(t *testing.T) {
	tmpl := compoundStageTemplate(nil)
	tmpl.Start = "build"
	delete(tmpl.Nodes, "start")
	definition := mustPrepare(t, tmpl, nil)
	if got := preparedNodeIDs(definition)[0]; got != "build" {
		t.Fatalf("prepared entry = %q", got)
	}

	checkpoint, err := Initialize("run-entry", definition)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Nodes["build"] != NodeReady {
		t.Fatalf("entry compound = %q", checkpoint.Nodes["build"])
	}
	checkpoint, command := advanceAndPlan(t, checkpoint, definition)
	if command == nil || command.NodeID != "build.plan" {
		t.Fatalf("entry compound first command = %#v", command)
	}
	checkpoint, _ = driveToTerminal(t, checkpoint, definition, command)
	if checkpoint.Status != RunCompleted {
		t.Fatalf("run status = %q", checkpoint.Status)
	}
}

// TestCompoundSurvivesColdLoadAtEveryStageBoundary walks the run one stage at a
// time and, at every boundary, throws the whole in-memory state away: the
// checkpoint is encoded, the definition is re-prepared from the template alone,
// and the checkpoint is decoded back through the load boundary. Expansion is a
// pure projection, so nothing about it is persisted and nothing is reverified —
// the stages simply come back identical.
func TestCompoundSurvivesColdLoadAtEveryStageBoundary(t *testing.T) {
	tmpl := compoundStageTemplate(nil)
	definition := mustPrepare(t, tmpl, nil)
	checkpoint, err := Initialize("run-cold", definition)
	if err != nil {
		t.Fatal(err)
	}

	boundaries := 0
	for range 4 * len(definition.nodes) {
		checkpoint, definition = coldReload(t, tmpl, checkpoint)
		boundaries++
		next, command, _, err := AdvanceAndPlan(checkpoint, definition)
		if err != nil {
			t.Fatalf("advance and plan: %v", err)
		}
		checkpoint = next
		if command == nil {
			break
		}
		checkpoint, definition = coldReload(t, tmpl, checkpoint)
		boundaries++
		if checkpoint, err = Apply(checkpoint, definition, observed(command, ProgramSucceeded, 0)); err != nil {
			t.Fatalf("observe %q after cold load: %v", command.NodeID, err)
		}
	}
	if checkpoint.Status != RunCompleted {
		t.Fatalf("run status = %q after %d cold loads", checkpoint.Status, boundaries)
	}
	// Four work stages: a load before and after each dispatch, plus the final
	// pass that advances the done stage and the end node.
	if boundaries != 9 {
		t.Fatalf("cold loads = %d", boundaries)
	}
}

// TestCompoundPersistsNoExpansionState pins the durable encoding mid-run: the
// v3 checkpoint gains no expansion field, no stage cursor, and no synthetic
// child edges. Stages appear where ordinary nodes appear, because that is all
// they are.
func TestCompoundPersistsNoExpansionState(t *testing.T) {
	definition := mustPrepare(t, compoundStageTemplate(nil), nil)
	checkpoint, err := Initialize("run-shape", definition)
	if err != nil {
		t.Fatal(err)
	}
	// Park the run mid-expansion, with the do stage outstanding.
	checkpoint, plan := advanceAndPlan(t, checkpoint, definition)
	if checkpoint, err = Apply(checkpoint, definition, observed(plan, ProgramSucceeded, 0)); err != nil {
		t.Fatal(err)
	}
	checkpoint, do := advanceAndPlan(t, checkpoint, definition)
	if do == nil || do.NodeID != "build.do" {
		t.Fatalf("second command = %#v", do)
	}

	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"version":3,"runId":"run-shape","status":"running",` +
		`"nodes":{"build":"running","build.do":"running","build.done":"pending","build.plan":"done",` +
		`"build.review":"pending","build.test.unit":"pending","end":"pending","start":"done"},` +
		`"attempts":{"build.do":1,"build.plan":1},` +
		`"edges":{"build":{"next":"unresolved"},"start":{"next":"arrived"}},` +
		`"awaitingDecisions":null,` +
		`"commands":[{"id":"cmd_9_run-shape_8_build.do_1_program","kind":"program","nodeId":"build.do",` +
		`"attempt":1,"program":{"profile":"worker","run":"do"}}]}`
	if string(encoded) != want {
		t.Fatalf("mid-expansion checkpoint JSON\n got: %s\nwant: %s", encoded, want)
	}
}

// TestParallelCompoundParentsComposeThroughThePluralOutbox runs two compounds
// concurrently under one fork: both hold an outstanding stage command at the
// same time, and neither parent's stage sequence disturbs the other's.
func TestParallelCompoundParentsComposeThroughThePluralOutbox(t *testing.T) {
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "parallel-compound", Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "fork"}},
			"fork":  {Type: model.NodeTypeParallel, Next: model.Next{"left": "left", "right": "right"}},
			"left":  compoundTask("join"),
			"right": compoundTask("join"),
			"join":  joinAllTask("end", "true"),
			"end":   {Type: model.NodeTypeEnd},
		},
	}
	definition := mustPrepare(t, tmpl, nil)
	checkpoint, err := Initialize("run-parallel", definition)
	if err != nil {
		t.Fatal(err)
	}

	// Both compounds activate in one settlement pass, and each holds its own
	// stage command: a bounded driver refills by calling AdvanceAndPlan again.
	checkpoint, first := advanceAndPlan(t, checkpoint, definition)
	checkpoint, second := advanceAndPlan(t, checkpoint, definition)
	if first == nil || second == nil || first.NodeID != "left.plan" || second.NodeID != "right.plan" {
		t.Fatalf("parallel stage commands = %#v / %#v", first, second)
	}
	if len(checkpoint.Commands) != 2 {
		t.Fatalf("outbox = %#v", checkpoint.Commands)
	}
	if checkpoint.Nodes["left"] != NodeRunning || checkpoint.Nodes["right"] != NodeRunning {
		t.Fatalf("compound parents = %q / %q", checkpoint.Nodes["left"], checkpoint.Nodes["right"])
	}

	// Observing one branch's stage advances only that branch.
	if checkpoint, err = Apply(checkpoint, definition, observed(first, ProgramSucceeded, 0)); err != nil {
		t.Fatal(err)
	}
	if checkpoint.Nodes["left.do"] != NodeReady || checkpoint.Nodes["right.do"] != NodePending {
		t.Fatalf("sibling stage moved: left.do %q right.do %q",
			checkpoint.Nodes["left.do"], checkpoint.Nodes["right.do"])
	}
	if len(checkpoint.Commands) != 1 || checkpoint.Commands[0].NodeID != "right.plan" {
		t.Fatalf("sibling command was disturbed: %#v", checkpoint.Commands)
	}

	checkpoint, _ = driveToTerminal(t, checkpoint, definition, second)
	if checkpoint.Status != RunCompleted {
		t.Fatalf("run status = %q", checkpoint.Status)
	}
	if checkpoint.Edges["left"][model.DefaultOutcome] != EdgeArrived ||
		checkpoint.Edges["right"][model.DefaultOutcome] != EdgeArrived {
		t.Fatalf("compound edges = %q / %q",
			checkpoint.Edges["left"][model.DefaultOutcome], checkpoint.Edges["right"][model.DefaultOutcome])
	}
}

// TestExclusiveRouteSkipsCompoundAndEveryDerivedStage covers the closure
// direction: stages have no incoming edges, so nothing but the parent's own
// skip can ever close them.
func TestExclusiveRouteSkipsCompoundAndEveryDerivedStage(t *testing.T) {
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "compound-decision", Start: "start",
		Nodes: map[string]model.Node{
			"start":    {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "choose"}},
			"choose":   humanDecision("Build it?", model.Next{"build": "build", "skip": "shortcut"}),
			"build":    compoundTask("merge"),
			"shortcut": programTask("merge", "true"),
			"merge":    programTask("end", "true"),
			"end":      {Type: model.NodeTypeEnd},
		},
	}
	definition := mustPrepare(t, tmpl, nil)
	checkpoint, err := Initialize("run-skip", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, command := advanceAndPlan(t, checkpoint, definition)
	if command != nil {
		t.Fatalf("planned before the decision: %#v", command)
	}
	checkpoint, err = Apply(checkpoint, definition,
		Transition{Kind: TransitionDecisionRecorded, Decision: &DecisionRecord{NodeID: "choose", Verdict: "skip"}})
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Nodes["build"] != NodeSkipped {
		t.Fatalf("compound parent = %q", checkpoint.Nodes["build"])
	}
	for _, stageID := range compoundStageIDs {
		if checkpoint.Nodes[stageID] != NodeSkipped {
			t.Fatalf("derived stage %q = %q", stageID, checkpoint.Nodes[stageID])
		}
	}

	checkpoint, command = advanceAndPlan(t, checkpoint, definition)
	if command == nil || command.NodeID != "shortcut" {
		t.Fatalf("taken route command = %#v", command)
	}
	checkpoint, _ = driveToTerminal(t, checkpoint, definition, command)
	if checkpoint.Status != RunCompleted {
		t.Fatalf("run status = %q", checkpoint.Status)
	}
}

// TestFailedStageKeepsTheCompoundParentRunningWhileTheRunDrains pins the
// failure semantics: a stage is an ordinary fail-fast task, and the parent stays
// Running because only the done stage completes it. The sibling branch's program
// is still in flight, so the run drains before it finishes failed.
func TestFailedStageKeepsTheCompoundParentRunningWhileTheRunDrains(t *testing.T) {
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "failing-compound", Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "fork"}},
			"fork":  {Type: model.NodeTypeParallel, Next: model.Next{"left": "left", "right": "right"}},
			"left":  compoundTask("join"),
			"right": programTask("join", "true"),
			"join":  joinAllTask("end", "true"),
			"end":   {Type: model.NodeTypeEnd},
		},
	}
	definition := mustPrepare(t, tmpl, nil)
	checkpoint, err := Initialize("run-failing", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, stage := advanceAndPlan(t, checkpoint, definition)
	checkpoint, sibling := advanceAndPlan(t, checkpoint, definition)
	if stage == nil || stage.NodeID != "left.plan" || sibling == nil || sibling.NodeID != "right" {
		t.Fatalf("commands = %#v / %#v", stage, sibling)
	}

	checkpoint, err = Apply(checkpoint, definition, observed(stage, ProgramFailed, 1))
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Nodes["left.plan"] != NodeFailed {
		t.Fatalf("failed stage = %q", checkpoint.Nodes["left.plan"])
	}
	if checkpoint.Nodes["left"] != NodeRunning {
		t.Fatalf("compound parent = %q; only the done stage may complete it", checkpoint.Nodes["left"])
	}
	if checkpoint.Nodes["left.do"] != NodePending || checkpoint.Nodes["left.done"] != NodePending {
		t.Fatalf("later stages moved after a failure: %q / %q",
			checkpoint.Nodes["left.do"], checkpoint.Nodes["left.done"])
	}
	if checkpoint.Status != RunRunning || !Draining(checkpoint) {
		t.Fatalf("run status = %q, draining = %v", checkpoint.Status, Draining(checkpoint))
	}
	if _, _, _, err := AdvanceAndPlan(checkpoint, definition); err != nil {
		t.Fatal(err)
	}
	if len(checkpoint.Commands) != 1 || checkpoint.Commands[0].NodeID != "right" {
		t.Fatalf("draining outbox = %#v", checkpoint.Commands)
	}

	checkpoint, err = Apply(checkpoint, definition, observed(sibling, ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Status != RunFailed {
		t.Fatalf("drained run status = %q", checkpoint.Status)
	}
	if checkpoint.Nodes["left"] != NodeRunning {
		t.Fatalf("compound parent completed without its done stage: %q", checkpoint.Nodes["left"])
	}
	if checkpoint.Edges["left"][model.DefaultOutcome] != EdgeUnresolved {
		t.Fatalf("failed compound settled its authored route: %q", checkpoint.Edges["left"][model.DefaultOutcome])
	}
}

// TestCompoundRejectsDerivedNodeIDsAboveTheExecutableEnvelope covers the case
// authoring cannot see: the parent id and the step id are each individually
// fine, but the id their expansion derives is past the envelope a run's durable
// evidence can name. Such a run would be created and then wedge on its first
// transition, so it has to be refused before it exists.
func TestCompoundRejectsDerivedNodeIDsAboveTheExecutableEnvelope(t *testing.T) {
	// The two derived shapes, each measured against the exact suffix
	// model.ExpandNode appends, one byte either side of the boundary.
	//
	// A plan-only compound derives ".plan", ".do", and ".done", so ".plan" is the
	// longest suffix a parent id has to fit under. A checks-only compound with a
	// one-byte parent isolates the step id instead.
	longestPlanParent := strings.Repeat("a", model.MaxExecutableNodeIDBytes-len(".plan"))
	longestCheckStep := strings.Repeat("c", model.MaxExecutableNodeIDBytes-len("p.test."))

	planOnly := func(nodeID string) *model.Template {
		tmpl := compoundStageTemplate(func(node *model.Node) {
			node.Checks, node.Review = nil, nil
		})
		renameCompoundParent(tmpl, nodeID)
		return tmpl
	}
	checksOnly := func(stepID string) *model.Template {
		tmpl := compoundStageTemplate(func(node *model.Node) {
			node.Plan, node.Review = nil, nil
			node.Checks = []model.Step{{ID: stepID,
				Performer: model.Performer{Kind: model.PerformerProgram, Run: "unit"}}}
		})
		renameCompoundParent(tmpl, "p")
		return tmpl
	}

	tests := []struct {
		name     string
		tmpl     func() *model.Template
		wantPath string
	}{
		{
			name: "longest accepted parent.plan",
			tmpl: func() *model.Template { return planOnly(longestPlanParent) },
		},
		{
			name:     "parent.plan one byte too long",
			tmpl:     func() *model.Template { return planOnly(longestPlanParent + "a") },
			wantPath: "nodes." + longestPlanParent + "a.plan",
		},
		{
			name: "longest accepted parent.test.step",
			tmpl: func() *model.Template { return checksOnly(longestCheckStep) },
		},
		{
			name:     "parent.test.step one byte too long",
			tmpl:     func() *model.Template { return checksOnly(longestCheckStep + "c") },
			wantPath: "nodes.p.checks[0]",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tmpl := test.tmpl()
			assertAuthoringValid(t, tmpl)

			diagnostics := CheckEligibility(tmpl)
			if test.wantPath == "" {
				if len(diagnostics) != 0 {
					t.Fatalf("eligibility diagnostics = %#v", diagnostics)
				}
				// It must genuinely prepare, not merely pass the bound.
				if _, err := Prepare(tmpl, nil); err != nil {
					t.Fatalf("prepare: %v", err)
				}
				return
			}
			if !hasCode(diagnostics, "expanded_node_id_limit") {
				t.Fatalf("missing expanded_node_id_limit in %#v", diagnostics)
			}
			var found bool
			for _, diagnostic := range diagnostics {
				if diagnostic.Code == "expanded_node_id_limit" && diagnostic.Path == test.wantPath {
					found = true
				}
			}
			if !found {
				t.Fatalf("no expanded_node_id_limit at %q in %#v", test.wantPath, diagnostics)
			}
			// Ineligible means no run: preparation refuses rather than building a
			// definition whose transitions could never be recorded.
			if _, err := Prepare(tmpl, nil); !errors.Is(err, ErrTemplateIneligible) {
				t.Fatalf("prepare error = %v", err)
			}
		})
	}
}

// TestRejectsAuthoredNodeIDsAboveTheExecutableEnvelope is the same boundary for
// an ordinary authored node, with no compound involved. Authoring bounds the id
// charset and not its length, so a long id passes validation and then wedges the
// run it creates; the ceiling is a property of what a run can NAME, so it has to
// hold for every prepared node, not only derived ones.
func TestRejectsAuthoredNodeIDsAboveTheExecutableEnvelope(t *testing.T) {
	for _, test := range []struct {
		name     string
		size     int
		rejected bool
	}{
		{name: "at the ceiling", size: model.MaxExecutableNodeIDBytes},
		{name: "one byte over", size: model.MaxExecutableNodeIDBytes + 1, rejected: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			nodeID := strings.Repeat("t", test.size)
			tmpl := sequentialTemplate(nodeID)
			assertAuthoringValid(t, tmpl)

			diagnostics := CheckEligibility(tmpl)
			if !test.rejected {
				if len(diagnostics) != 0 {
					t.Fatalf("eligibility diagnostics = %#v", diagnostics)
				}
				if _, err := Prepare(tmpl, nil); err != nil {
					t.Fatalf("prepare: %v", err)
				}
				return
			}
			if !hasCode(diagnostics, "node_id_limit") {
				t.Fatalf("missing node_id_limit in %#v", diagnostics)
			}
			if _, err := Prepare(tmpl, nil); !errors.Is(err, ErrTemplateIneligible) {
				t.Fatalf("prepare error = %v", err)
			}
		})
	}
}

// TestExecutableNodeIDEnvelopeMatchesTheDurableEvidenceBound pins the two
// constants together. The eligibility bound is only meaningful because it is the
// SAME envelope the durable evidence row enforces; if persistence ever tightened
// its own limit independently, this check would start admitting runs that wedge.
func TestExecutableNodeIDEnvelopeMatchesTheDurableEvidenceBound(t *testing.T) {
	if model.MaxExecutableNodeIDBytes != db.MaxProcessRunNodeIDBytes {
		t.Fatalf("executable node id envelope %d disagrees with the durable evidence bound %d",
			model.MaxExecutableNodeIDBytes, db.MaxProcessRunNodeIDBytes)
	}
}

// renameCompoundParent re-keys the fixture's compound node and the start edge
// that reaches it.
func renameCompoundParent(tmpl *model.Template, nodeID string) {
	build := tmpl.Nodes["build"]
	delete(tmpl.Nodes, "build")
	tmpl.Nodes[nodeID] = build
	start := tmpl.Nodes["start"]
	start.Next = model.Next{model.DefaultOutcome: nodeID}
	tmpl.Nodes["start"] = start
}

func preparedNodeIDs(definition *Definition) []string {
	ids := make([]string, 0, len(definition.nodes))
	for _, node := range definition.nodes {
		ids = append(ids, node.id)
	}
	return ids
}

// driveToTerminal succeeds the given outstanding command and then plans and
// succeeds every command the run offers, returning the node ids it dispatched
// starting from that first one.
func driveToTerminal(t *testing.T, checkpoint Checkpoint, definition *Definition, command *Command) (Checkpoint, []string) {
	t.Helper()
	var dispatched []string
	for range 4 * len(definition.nodes) {
		if command == nil {
			return checkpoint, dispatched
		}
		dispatched = append(dispatched, command.NodeID)
		next, err := Apply(checkpoint, definition, observed(command, ProgramSucceeded, 0))
		if err != nil {
			t.Fatalf("observe %q: %v", command.NodeID, err)
		}
		checkpoint, command = advanceAndPlan(t, next, definition)
	}
	t.Fatalf("run did not settle: %#v", checkpoint)
	return checkpoint, dispatched
}

// coldReload throws every in-memory derivation away: the checkpoint goes through
// its durable encoding and the definition is re-prepared from the template
// alone, exactly as a daemon restart would rebuild them.
func coldReload(t *testing.T, tmpl *model.Template, checkpoint Checkpoint) (Checkpoint, *Definition) {
	t.Helper()
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	definition := mustPrepare(t, tmpl, nil)
	decoded, err := DecodeCheckpoint(encoded, definition)
	if err != nil {
		t.Fatalf("cold load: %v", err)
	}
	return decoded, definition
}
