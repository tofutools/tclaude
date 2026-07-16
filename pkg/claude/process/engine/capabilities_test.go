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

func capabilityParallelTemplate(join model.JoinPolicy) *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "parallel-capability", Start: "fork",
		Nodes: map[string]model.Node{
			"fork":  {Type: model.NodeTypeParallel, Next: model.Next{"left": "merge", "right": "merge"}},
			"merge": {Type: model.NodeTypeEnd, Join: join},
		},
	}
}
