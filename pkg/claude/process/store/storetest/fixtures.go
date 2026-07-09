package storetest

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

type CrashWindow string

const (
	CrashAfterLogBeforeManifest   CrashWindow = "after_log_before_manifest"
	CrashAfterManifestBeforeState CrashWindow = "after_manifest_before_state"
	CrashTornFinalLogLine         CrashWindow = "torn_final_log_line"
)

type CrashFixture struct {
	Root  string
	RunID string
	Store *store.FS
}

func BuildCrashFixture(t testing.TB, window CrashWindow) CrashFixture {
	t.Helper()
	root := t.TempDir()
	fs, err := store.NewFS(root)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := Template()
	templateRecord, err := fs.PutTemplate(t.Context(), tmpl)
	if err != nil {
		t.Fatal(err)
	}
	runID := "run_fixture"
	st := state.New(runID, templateRecord.Ref, templateRecord.Ref, []state.NodeInit{{ID: "implement", Type: model.NodeTypeTask}})
	if _, err := fs.CreateRun(t.Context(), store.RunRecord{ID: runID, TemplateRef: templateRecord.Ref}, st); err != nil {
		t.Fatal(err)
	}

	entry := LogEntry(runID, "implement", 1)
	switch window {
	case CrashAfterLogBeforeManifest:
		appendNodeLog(t, root, runID, entry)
	case CrashAfterManifestBeforeState:
		appendNodeLog(t, root, runID, entry)
		manifestEntry, err := evidence.ManifestEntryForLog(entry, "")
		if err != nil {
			t.Fatal(err)
		}
		appendManifest(t, root, runID, manifestEntry)
	case CrashTornFinalLogLine:
		path := filepath.Join(root, "runs", runID, "nodes", "implement", "log.jsonl")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"schemaVersion":1,"seq":1`), 0o644); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown crash window %q", window)
	}
	return CrashFixture{Root: root, RunID: runID, Store: fs}
}

func Template() *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion,
		Kind:       model.Kind,
		ID:         "demo",
		Start:      "implement",
		Nodes: map[string]model.Node{
			"implement": {Type: model.NodeTypeTask, Next: model.Next{"done": "end"}},
			"end":       {Type: model.NodeTypeEnd},
		},
	}
}

func LogEntry(runID, nodeID string, seq int64) evidence.LogEntry {
	at := time.Date(2026, 7, 9, 16, 30, 15, 123456789, time.FixedZone("TST", 90*60))
	return evidence.LogEntry{
		SchemaVersion: evidence.LogEntrySchemaVersion,
		Seq:           seq,
		At:            at,
		Scope:         evidence.Scope{Kind: evidence.ScopeNode, ID: nodeID},
		Kind:          evidence.EntryKindAttempt,
		Event: &state.Event{
			Type:      state.EventNodeAttemptStarted,
			Seq:       seq,
			At:        at,
			RunID:     runID,
			NodeID:    nodeID,
			Actor:     state.ActorRef("agent:agt_fixture"),
			CommandID: "cmd_fixture",
			EvidenceRef: "artifact:sha256:" +
				"0000000000000000000000000000000000000000000000000000000000000000",
		},
		EvidenceRef: "artifact:sha256:" +
			"0000000000000000000000000000000000000000000000000000000000000000",
	}
}

func AdminLogEntry(runID, nodeID string, seq int64) evidence.LogEntry {
	at := time.Date(2026, 7, 9, 16, 30, 15, 123456789, time.FixedZone("TST", 90*60))
	return evidence.LogEntry{
		SchemaVersion: evidence.LogEntrySchemaVersion,
		Seq:           seq,
		At:            at,
		Scope:         evidence.Scope{Kind: evidence.ScopeNode, ID: nodeID},
		Kind:          evidence.EntryKindAdmin,
		Event: &state.Event{
			Type:   state.EventAdminRepairRecorded,
			Seq:    seq,
			At:     at,
			RunID:  runID,
			Actor:  state.ActorRef("agent:agt_fixture"),
			Reason: "concurrent append probe",
		},
	}
}

func appendNodeLog(t testing.TB, root, runID string, entry evidence.LogEntry) {
	t.Helper()
	path := filepath.Join(root, "runs", runID, "nodes", entry.Scope.ID, "log.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := evidence.AppendLogEntry(f, entry); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func appendManifest(t testing.TB, root, runID string, entry evidence.ManifestEntry) {
	t.Helper()
	path := filepath.Join(root, "runs", runID, "manifest.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := evidence.AppendManifestEntry(f, entry); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
