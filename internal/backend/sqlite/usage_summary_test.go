package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestUsageSummaryRefusesOversizedReadInsteadOfTruncatingTotals(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "backend.db"))
	require.NoError(t, err)
	defer store.Close()
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	_, err = store.db.ExecContext(ctx, `WITH RECURSIVE sample(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM sample WHERE n<?)
 INSERT INTO usage_observations(id,agent_id,conversation_id,execution_id,attribution_precision,harness,source_key,source,source_revision,observed_at,collected_at,counters_json,counter_coverage,cost_coverage,cumulative)
 SELECT 'usage-'||n,CASE WHEN n=1 THEN 'chosen' ELSE '' END,'conversation','','conversation','fixture','source-'||n,'fixture','1',?,?,'[{"Unit":"input_tokens","Value":1}]','complete','unsupported',0 FROM sample`, maxSummarySamples+1, nanos(at), nanos(at))
	require.NoError(t, err)
	filter := app.UsageSummaryFilter{After: at, Before: at.Add(time.Hour)}
	rows, err := store.UsageSummarySamples(ctx, model.OperatorPrincipal(), filter)
	require.ErrorIs(t, err, app.ErrInvalid)
	require.Nil(t, rows)
	filter.AgentID = "chosen"
	rows, err = store.UsageSummarySamples(ctx, model.OperatorPrincipal(), filter)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	_, err = store.UsageSummarySamples(ctx, model.AgentPrincipal("chosen"), filter)
	require.ErrorIs(t, err, app.ErrUnauthorized)
}
