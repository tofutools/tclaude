package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

const summaryUsageColumns = `id,agent_id,conversation_id,execution_id,attribution_precision,harness,source,source_revision,observed_at,collected_at,counters_json,cost_amount,cost_currency,cost_kind,counter_coverage,cost_coverage,coverage_reason,historical,provenance,cumulative`
const maxSummarySamples = 50000

type sourceUsageScanner struct {
	scanner
	source *string
}

func (s sourceUsageScanner) Scan(dest ...any) error {
	return s.scanner.Scan(append([]any{s.source}, dest...)...)
}

func (s *Store) UsageSummarySamples(ctx context.Context, principal model.Principal, f app.UsageSummaryFilter) ([]app.UsageSummarySample, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if principal.Kind != model.PrincipalOperator {
		return nil, app.ErrUnauthorized
	}
	filters := []string{"observed_at>=?", "observed_at<?"}
	args := []any{nanos(f.After), nanos(f.Before)}
	for _, part := range []struct{ column, value string }{{"harness", f.Harness}, {"agent_id", string(f.AgentID)}, {"conversation_id", string(f.ConversationID)}} {
		if part.value != "" {
			filters = append(filters, part.column+"=?")
			args = append(args, part.value)
		}
	}
	// Include one prior cumulative sample per selected source. It establishes a
	// baseline without exposing private source identities through the public API.
	query := `WITH selected AS (SELECT * FROM usage_observations WHERE ` + strings.Join(filters, " AND ") + `), baseline AS (
 SELECT u.* FROM usage_observations u WHERE u.cumulative=1 AND u.observed_at<?
 AND EXISTS(SELECT 1 FROM selected v WHERE v.source_key=u.source_key AND v.cumulative=1)
 AND NOT EXISTS(SELECT 1 FROM usage_observations newer WHERE newer.source_key=u.source_key AND newer.observed_at<? AND (newer.observed_at>u.observed_at OR (newer.observed_at=u.observed_at AND newer.collected_at>u.collected_at) OR (newer.observed_at=u.observed_at AND newer.collected_at=u.collected_at AND newer.id>u.id)))
 ) SELECT source_key,` + summaryUsageColumns + ` FROM (SELECT * FROM selected UNION ALL SELECT * FROM baseline) ORDER BY source_key,observed_at,collected_at,id LIMIT ?`
	args = append(args, nanos(f.After), nanos(f.After), maxSummarySamples+1)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	samples := make([]app.UsageSummarySample, 0)
	for rows.Next() {
		var sample app.UsageSummarySample
		sample.Observation, sample.Cumulative, err = scanUsage(sourceUsageScanner{rows, &sample.SourceKey})
		if err != nil {
			return nil, err
		}
		samples = append(samples, sample)
		if len(samples) > maxSummarySamples {
			return nil, fmt.Errorf("%w: summary exceeds 50000 source samples; narrow the observed range or target", app.ErrInvalid)
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return samples, tx.Commit()
}
