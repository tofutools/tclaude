package engine

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

func TestInstantiationCapabilityMatrix(t *testing.T) {
	legacy := capabilityLegacyTemplate()
	localAny := capabilityLocalMergeTemplate(model.JoinAny)
	parallel := capabilityParallelTemplate(model.JoinAll)
	any := capabilityParallelTemplate(model.JoinAny)
	foundation := FoundationEngineCapabilities()
	parallelAll := testEngineCapabilities(CapabilityFoundationV1, CapabilityParallelAllV1)
	parallelAny := testEngineCapabilities(CapabilityFoundationV1, CapabilityParallelAllV1, CapabilityParallelAnyV1)

	for _, tc := range []struct {
		name string
		tmpl *model.Template
		caps EngineCapabilities
		want string
	}{
		{name: "foundation accepts legacy", tmpl: legacy, caps: foundation},
		{name: "foundation accepts local any without parallel", tmpl: localAny, caps: foundation},
		{name: "foundation rejects parallel", tmpl: parallel, caps: foundation, want: string(CapabilityParallelAllV1)},
		{name: "all accepts parallel all", tmpl: parallel, caps: parallelAll},
		{name: "all rejects any", tmpl: any, caps: parallelAll, want: string(CapabilityParallelAnyV1)},
		{name: "any accepts any", tmpl: any, caps: parallelAny},
		{name: "all without foundation is incoherent", tmpl: legacy, caps: testEngineCapabilities(CapabilityParallelAllV1), want: "incoherent"},
		{name: "any without all is incoherent", tmpl: legacy, caps: testEngineCapabilities(CapabilityFoundationV1, CapabilityParallelAnyV1), want: "incoherent"},
		{name: "any alone is incoherent", tmpl: legacy, caps: testEngineCapabilities(CapabilityParallelAnyV1), want: "incoherent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateInstantiation(tc.tmpl, InstantiateRequest{RunID: "run", EngineCapabilities: tc.caps})
			if tc.want == "" && err != nil {
				t.Fatal(err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestFoundationEngineInstantiatesPromotedLegacyAnyWithoutParallel(t *testing.T) {
	parsed, err := model.ParseAuthoring([]byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: legacy-local-any
start: choose
nodes:
  choose:
    type: decision
    performer: {kind: human, ask: choose}
    next: {left: merge, right: merge}
  merge:
    type: end
    metadata: {join: any}
`))
	if err != nil || parsed.Diagnostics.HasErrors() {
		t.Fatalf("parse authoring = %v, diagnostics=%#v", err, parsed.Diagnostics.Errors())
	}
	if parsed.Template.Nodes["merge"].Join != model.JoinAny {
		t.Fatalf("legacy join was not promoted: %#v", parsed.Template.Nodes["merge"])
	}
	fs, err := store.NewFS(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	record, err := fs.PutTemplate(t.Context(), parsed.Template)
	if err != nil {
		t.Fatal(err)
	}
	run, err := Instantiate(t.Context(), fs, InstantiateRequest{
		TemplateRef: record.Ref, RunID: "legacy-local-any-run", EngineCapabilities: FoundationEngineCapabilities(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.ID != "legacy-local-any-run" {
		t.Fatalf("run = %#v", run)
	}
}

func TestFoundationEngineCannotCreateParallelRun(t *testing.T) {
	fs, err := store.NewFS(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	record, err := fs.PutTemplate(t.Context(), capabilityParallelTemplate(model.JoinAll))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Instantiate(t.Context(), fs, InstantiateRequest{
		TemplateRef: record.Ref, RunID: "must-not-exist", EngineCapabilities: FoundationEngineCapabilities(),
	})
	if !IsInstantiateInputError(err) || !strings.Contains(err.Error(), string(CapabilityParallelAllV1)) {
		t.Fatalf("instantiate error = %v", err)
	}
	runs, listErr := fs.ListRuns(t.Context())
	if listErr != nil || len(runs) != 0 {
		t.Fatalf("parallel run was created: %#v, %v", runs, listErr)
	}
	production := FoundationEngineCapabilities()
	if production.Supports(CapabilityParallelAllV1) || production.Supports(CapabilityParallelAnyV1) {
		t.Fatalf("authoring slice exposed parallel execution: %#v", production)
	}
	released := ProductionEngineCapabilities()
	if !released.Supports(CapabilityFoundationV1) || !released.Supports(CapabilityParallelAllV1) || released.Supports(CapabilityParallelAnyV1) {
		t.Fatalf("production capability gate is not indivisible foundation+all: %#v", released)
	}
	if err := ValidateInstantiation(capabilityParallelTemplate(model.JoinAll), InstantiateRequest{RunID: "released", EngineCapabilities: released}); err != nil {
		t.Fatalf("production all capability rejected exact all template: %v", err)
	}
	if err := ValidateInstantiation(capabilityParallelTemplate(model.JoinAny), InstantiateRequest{RunID: "released-any", EngineCapabilities: released}); err == nil || !strings.Contains(err.Error(), string(CapabilityParallelAnyV1)) {
		t.Fatalf("production all capability admitted any: %v", err)
	}
}

func testEngineCapabilities(values ...ExecutionCapability) EngineCapabilities {
	supported := make(map[ExecutionCapability]struct{}, len(values))
	for _, value := range values {
		supported[value] = struct{}{}
	}
	return EngineCapabilities{supported: supported}
}

func capabilityLegacyTemplate() *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "legacy-capability", Start: "done",
		Nodes: map[string]model.Node{"done": {Type: model.NodeTypeEnd}},
	}
}

func capabilityLocalMergeTemplate(join model.JoinPolicy) *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "local-merge-capability", Start: "choose",
		Nodes: map[string]model.Node{
			"choose": {
				Type: model.NodeTypeDecision, Performer: &model.Performer{Kind: model.PerformerHuman, Ask: "choose"},
				Next: model.Next{"left": "merge", "right": "merge"},
			},
			"merge": {Type: model.NodeTypeEnd, Join: join},
		},
	}
}

func capabilityParallelTemplate(join model.JoinPolicy) *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "parallel-capability", Start: "fork",
		Nodes: map[string]model.Node{
			"fork":  {Type: model.NodeTypeParallel, Next: model.Next{"left": "merge", "right": "merge"}},
			"merge": {Type: model.NodeTypeEnd, Join: join},
		},
	}
}
