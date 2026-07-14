package processcmd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

func TestParseParamsRejectsDuplicates(t *testing.T) {
	if _, err := parseParams([]string{"ticket=A", "ticket=B"}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate param error, got %v", err)
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

func TestRunInvalidParamsDoNotPublishLocalTemplate(t *testing.T) {
	templatePath := writeTemplate(t, `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: atomic-local
params:
  ticket:
    type: string
    required: true
start: end
nodes:
  end:
    type: end
`)
	for name, params := range map[string][]string{
		"missing required": nil,
		"unknown":          {"ticket=TCL-300", "secret=must-not-publish"},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := &cobra.Command{}
			cmd.SetContext(t.Context())
			root := filepath.Join(t.TempDir(), "store")
			err := runRun(cmd, &runParams{Template: templatePath, StoreRoot: root, RunID: "atomic-run", Param: params}, &bytes.Buffer{})
			if err == nil {
				t.Fatal("runRun succeeded, want invalid params error")
			}
			fs, openErr := store.NewFS(root)
			if openErr != nil {
				t.Fatal(openErr)
			}
			records, listErr := fs.ListTemplates(t.Context())
			if listErr != nil {
				t.Fatal(listErr)
			}
			if len(records) != 0 {
				t.Fatalf("failed run published template versions: %#v", records)
			}
			if _, headErr := fs.GetTemplateHead(t.Context(), "atomic-local"); !errors.Is(headErr, store.ErrNotFound) {
				t.Fatalf("GetTemplateHead error = %v, want ErrNotFound", headErr)
			}
		})
	}
}

func TestRunFromOlderTemplateFileDoesNotMoveEditorHead(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	root := filepath.Join(t.TempDir(), "store")
	v1Path := writeTemplate(t, `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: stable-head
description: first
start: end
nodes:
  end:
    type: end
`)
	v1Source, err := os.ReadFile(v1Path)
	if err != nil {
		t.Fatal(err)
	}
	v2Source := strings.Replace(string(v1Source), "description: first", "description: second", 1)
	v2, err := model.Parse([]byte(v2Source))
	if err != nil {
		t.Fatal(err)
	}
	v2Path := writeTemplate(t, v2Source)
	var out bytes.Buffer
	if err := runRun(cmd, &runParams{Template: v2Path, StoreRoot: root, RunID: "new_file_run"}, &out); err != nil {
		t.Fatal(err)
	}
	fs, err := store.NewFS(root)
	if err != nil {
		t.Fatal(err)
	}
	v2Record, err := fs.GetTemplateHead(t.Context(), v2.Template.ID)
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runRun(cmd, &runParams{Template: v1Path, StoreRoot: root, RunID: "old_file_run"}, &out); err != nil {
		t.Fatal(err)
	}
	head, err := fs.GetTemplateHead(t.Context(), "stable-head")
	if err != nil {
		t.Fatal(err)
	}
	if head.Ref != v2Record.Ref {
		t.Fatalf("editor head moved while running old file: got %s, want %s", head.Ref, v2Record.Ref)
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
