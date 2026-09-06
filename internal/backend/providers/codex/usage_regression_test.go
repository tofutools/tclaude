package codex

import (
	"context"
	"encoding/json"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUsageRegressionStableSourceKey(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "sessions")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout.jsonl")
	raw := []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"native-session\"}}\n{\"timestamp\":\"2026-09-07T01:00:00Z\",\"type\":\"event_msg\",\"payload\":{\"type\":\"token_count\",\"info\":{\"total_token_usage\":{\"input_tokens\":100}}}}\n")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	reader := usageReader{provider: &Provider{nativeHome: home}}
	req := ports.UsageCollectionRequest{Native: model.NativeConversationEvidence{Reference: "native-session"}, Execution: model.Execution{UpdatedAt: time.Now().UTC()}}
	a, err := reader.Collect(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	token := sourceToken{StateRoot: home, SessionID: "native-session", Transcript: path}
	tokenRaw, _ := json.Marshal(token)
	req.History = &ports.HistorySourceSelection{SourceToken: string(tokenRaw)}
	b, err := reader.Collect(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if a.SourceKey != b.SourceKey {
		t.Fatalf("same rollout/revision uses different keys: %s -> %s (revision equal=%v)", a.SourceKey, b.SourceKey, a.SourceRevision == b.SourceRevision)
	}
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "usage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var first model.UsageObservation
	for i, collected := range []ports.CollectedUsage{a, b} {
		id := model.UsageObservationID("first")
		if i == 1 {
			id = "second"
		}
		record, repeated, err := store.RecordUsage(t.Context(), app.UsageWrite{SourceKey: collected.SourceKey, Cumulative: true, Observation: model.UsageObservation{ID: id, Attribution: model.UsageAttribution{ConversationID: "conversation", Precision: model.UsageAttributionConversation}, Harness: Name, Source: collected.Source, SourceRevision: collected.SourceRevision, ObservedAt: collected.ObservedAt, CollectedAt: time.Now().UTC(), Counters: collected.Counters, Coverage: collected.Coverage}})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = record
		} else if !repeated || record.ID != first.ID {
			t.Fatalf("catalog availability duplicated actual reader observation: %+v repeated=%v", record, repeated)
		}
	}

}
