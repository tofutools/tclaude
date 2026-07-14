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
