package engine

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

func TestInstantiateAppliesDefaultsRequiresParamsAndPinsExactRef(t *testing.T) {
	fs, err := store.NewFS(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	required := true
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "shared-create", Start: "done",
		Params: map[string]model.Param{
			"ticket": {Type: "string", Required: &required},
			"tries":  {Type: "number", Default: 2},
		},
		Nodes: map[string]model.Node{"done": {Type: model.NodeTypeEnd}},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Instantiate(t.Context(), fs, InstantiateRequest{TemplateRef: record.Ref}); !IsInstantiateInputError(err) || !strings.Contains(err.Error(), "missing required") {
		t.Fatalf("missing required param error = %v", err)
	}
	run, err := Instantiate(t.Context(), fs, InstantiateRequest{
		TemplateRef: record.Ref, Params: map[string]string{"ticket": "TCL-300"},
		Now: time.Date(2026, 7, 14, 16, 30, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.ID != "shared-create-20260714-163000" || run.TemplateRef != record.Ref {
		t.Fatalf("run = %#v", run)
	}
	if run.Params["ticket"] != "TCL-300" || run.Params["tries"] != "2" {
		t.Fatalf("params = %#v", run.Params)
	}
	if run.Template == nil || run.Template.ID != tmpl.ID {
		t.Fatalf("pinned template = %#v", run.Template)
	}
}

func TestInstantiateRejectsUnknownParamAndUnsafeRunID(t *testing.T) {
	fs, err := store.NewFS(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "input-check", Start: "done",
		Params: map[string]model.Param{"known": {Type: "string"}},
		Nodes:  map[string]model.Node{"done": {Type: model.NodeTypeEnd}},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []InstantiateRequest{
		{TemplateRef: record.Ref, Params: map[string]string{"unknown": "x"}},
		{TemplateRef: record.Ref, RunID: "bad\nid"},
	} {
		if _, err := Instantiate(t.Context(), fs, request); !IsInstantiateInputError(err) {
			t.Fatalf("request %#v error = %v", request, err)
		}
	}
}

func TestInstantiateRejectsInvalidStoredTemplateWithoutCreatingRun(t *testing.T) {
	fs, err := store.NewFS(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "invalid-stored", Start: "missing",
		Nodes: map[string]model.Node{"done": {Type: model.NodeTypeEnd}},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Instantiate(t.Context(), fs, InstantiateRequest{TemplateRef: record.Ref, RunID: "must-not-exist"}); !IsInstantiateInputError(err) || !strings.Contains(err.Error(), "validation errors") {
		t.Fatalf("invalid template error = %v", err)
	}
	if _, err := fs.GetRun(t.Context(), "must-not-exist"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRun error = %v, want ErrNotFound", err)
	}
}

func TestInstantiateGeneratedIDsRetrySameTimestampCollisions(t *testing.T) {
	fs, err := store.NewFS(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "same-second", Start: "done",
		Nodes: map[string]model.Node{"done": {Type: model.NodeTypeEnd}},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 14, 17, 30, 0, 0, time.UTC)
	first, err := Instantiate(t.Context(), fs, InstantiateRequest{TemplateRef: record.Ref, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Instantiate(t.Context(), fs, InstantiateRequest{TemplateRef: record.Ref, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != "same-second-20260714-173000" || second.ID != "same-second-20260714-173000-2" {
		t.Fatalf("generated ids = %q, %q", first.ID, second.ID)
	}
}

func TestInstantiateReplayExistingRequiresIdenticalResolvedInputs(t *testing.T) {
	fs, err := store.NewFS(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "retry-safe", Start: "done",
		Params: map[string]model.Param{
			"issue": {Type: "string"},
			"tries": {Type: "number", Default: 2},
		},
		Nodes: map[string]model.Node{"done": {Type: model.NodeTypeEnd}},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	if err != nil {
		t.Fatal(err)
	}
	request := InstantiateRequest{
		TemplateRef: record.Ref, RunID: "retry-safe-attempt", Params: map[string]string{"issue": "TCL-300"},
		ReplayExisting: true,
	}
	first, err := Instantiate(t.Context(), fs, request)
	if err != nil {
		t.Fatal(err)
	}
	replay := request
	replay.Params = map[string]string{"issue": "TCL-300", "tries": "2"}
	second, err := Instantiate(t.Context(), fs, replay)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || !first.CreatedAt.Equal(second.CreatedAt) {
		t.Fatalf("replay created a different run: first=%#v second=%#v", first, second)
	}

	conflict := request
	conflict.Params = map[string]string{"issue": "TCL-301"}
	if _, err := Instantiate(t.Context(), fs, conflict); !errors.Is(err, store.ErrRunExists) {
		t.Fatalf("different-payload replay error = %v, want ErrRunExists", err)
	}
	withoutReplay := request
	withoutReplay.ReplayExisting = false
	if _, err := Instantiate(t.Context(), fs, withoutReplay); !errors.Is(err, store.ErrRunExists) {
		t.Fatalf("ordinary explicit-id duplicate error = %v, want ErrRunExists", err)
	}
	runs, err := fs.ListRuns(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Params["issue"] != "TCL-300" {
		t.Fatalf("durable runs after retries = %#v", runs)
	}
}
