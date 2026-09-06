package sqlite

import (
	"context"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"path/filepath"
	"testing"
	"time"
)

func TestUsageRegressionHistoricalConversationUsage(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	at := time.Now().UTC()
	_, err = s.db.Exec(`INSERT INTO conversations(id,revision,created_at,updated_at) VALUES('conversation_usage',1,?,?)`, nanos(at), nanos(at))
	if err != nil {
		t.Fatal(err)
	}
	w := usageWrite("historical_usage", "rev", at, 100)
	w.Observation.Historical = true
	w.Observation.Provenance = "offline-snapshot"
	if _, _, err = s.ImportHistoricalUsage(ctx, app.HistoricalUsageWrite(w)); err != nil {
		t.Fatal(err)
	}
	result, err := s.QueryUsage(ctx, app.UsageFilter{Target: app.UsageTarget{ConversationID: "conversation_usage"}}, model.AuthorityRequest{Principal: model.OperatorPrincipal(), Action: model.ActionReadUsage}, at)
	if err != nil {
		t.Fatalf("imported usage for real conversation cannot be read without execution: %v", err)
	}
	if len(result.Observations) != 1 {
		t.Fatalf("got %d observations", len(result.Observations))
	}
}
