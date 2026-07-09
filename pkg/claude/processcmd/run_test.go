package processcmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

func TestApplyParamDefaultsRejectsDuplicatesAndStoresDefaults(t *testing.T) {
	if _, err := parseParams([]string{"ticket=A", "ticket=B"}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate param error, got %v", err)
	}

	required := true
	params, err := applyParamDefaults(&model.Template{
		Params: map[string]model.Param{
			"ticket": {Type: "string", Required: &required, Default: "TCL-271"},
			"tries":  {Type: "number", Default: 2},
		},
	}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if params["ticket"] != "TCL-271" || params["tries"] != "2" {
		t.Fatalf("params = %#v", params)
	}
}

func TestRunRejectsUnsafeRunID(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	var out bytes.Buffer

	err := runRun(cmd, &runParams{
		Template:  writeManualFlowTemplate(t),
		StoreRoot: filepath.Join(t.TempDir(), "store"),
		RunID:     "bad\nid",
		Param:     []string{"ticket=TCL-271"},
	}, &out)
	if err == nil || !strings.Contains(err.Error(), "run id must match") {
		t.Fatalf("expected unsafe run id error, got %v", err)
	}
}

func TestRunStoresDefaultParams(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	root := filepath.Join(t.TempDir(), "store")
	templatePath := writeTemplate(t, `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: defaults-demo
params:
  ticket:
    type: string
    required: true
    default: TCL-271
start: implement
nodes:
  implement:
    type: task
    performer:
      kind: human
      ask: Implement
    next:
      pass: end
  end:
    type: end
`)
	var out bytes.Buffer
	if err := runRun(cmd, &runParams{Template: templatePath, StoreRoot: root, RunID: "defaults_demo"}, &out); err != nil {
		t.Fatal(err)
	}
	fs, err := store.NewFS(root)
	if err != nil {
		t.Fatal(err)
	}
	run, err := fs.GetRun(t.Context(), "defaults_demo")
	if err != nil {
		t.Fatal(err)
	}
	if run.Params["ticket"] != "TCL-271" {
		t.Fatalf("run params = %#v", run.Params)
	}
}

func TestRunAllowProgramsPersistsOptInAndAdminAudit(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	root := filepath.Join(t.TempDir(), "store")
	templatePath := writeTemplate(t, `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: program-demo
start: check
nodes:
  check:
    type: task
    performer:
      kind: program
      run: /bin/true
    next:
      pass: end
  end:
    type: end
`)
	var out bytes.Buffer
	if err := runRun(cmd, &runParams{
		Template:      templatePath,
		StoreRoot:     root,
		RunID:         "program_demo",
		AllowPrograms: true,
	}, &out); err != nil {
		t.Fatal(err)
	}
	fs, err := store.NewFS(root)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := fs.LoadRun(t.Context(), "program_demo")
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Run.AllowPrograms {
		t.Fatal("run did not persist allowPrograms")
	}
	if len(snapshot.State.AdminRecords) != 1 || snapshot.State.AdminRecords[0].Type != state.EventAdminProgramsAllowed || !strings.Contains(snapshot.State.AdminRecords[0].Reason, "--allow-programs") {
		t.Fatalf("admin records = %#v", snapshot.State.AdminRecords)
	}
	runLog, err := fs.ReadRunLog(t.Context(), "program_demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(runLog) != 1 || runLog[0].Event == nil || runLog[0].Event.Type != state.EventAdminProgramsAllowed {
		t.Fatalf("run log = %#v", runLog)
	}
}

func writeTemplate(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "template.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
