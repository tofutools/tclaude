package sqlite

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

const usageSchema = `
CREATE TABLE IF NOT EXISTS usage_observations (
  id TEXT PRIMARY KEY,
  agent_id TEXT NOT NULL DEFAULT '', conversation_id TEXT NOT NULL, execution_id TEXT NOT NULL,
	 attribution_precision TEXT NOT NULL,
  harness TEXT NOT NULL, source_key TEXT NOT NULL, source TEXT NOT NULL, source_revision TEXT NOT NULL,
  observed_at INTEGER NOT NULL, collected_at INTEGER NOT NULL, counters_json BLOB NOT NULL,
  cost_amount TEXT, cost_currency TEXT, cost_kind TEXT,
  counter_coverage TEXT NOT NULL, cost_coverage TEXT NOT NULL, coverage_reason TEXT NOT NULL DEFAULT '',
  cumulative INTEGER NOT NULL, historical INTEGER NOT NULL DEFAULT 0,
  UNIQUE(source_key, source_revision)
);
CREATE INDEX IF NOT EXISTS usage_by_execution ON usage_observations(execution_id,observed_at DESC,id);
CREATE INDEX IF NOT EXISTS usage_by_conversation ON usage_observations(conversation_id,observed_at DESC,id);
`

func (s *Store) ensureUsageSchema(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, usageSchema)
	return err
}

func (s *Store) ResolveUsageTarget(ctx context.Context, target app.UsageTarget, authority model.AuthorityRequest, at time.Time) (app.UsageTargetRecord, error) {
	if err := s.ensureUsageSchema(ctx); err != nil {
		return app.UsageTargetRecord{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.UsageTargetRecord{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var execution model.Execution
	if target.ExecutionID != "" {
		execution, err = scanExecution(tx.QueryRowContext(ctx, executionSelect+` WHERE id=?`, target.ExecutionID))
	} else {
		execution, err = scanExecution(tx.QueryRowContext(ctx, executionSelect+` WHERE conversation_id=? ORDER BY created_at DESC,id DESC LIMIT 1`, target.ConversationID))
	}
	if err != nil {
		return app.UsageTargetRecord{}, err
	}
	if target.ConversationID != "" && execution.ConversationID != target.ConversationID {
		return app.UsageTargetRecord{}, app.ErrNotFound
	}
	authority.Resource = model.ResourceSelector{Kind: model.ResourceExecution, ExecutionID: execution.ID}
	decision, err := authorizeTx(ctx, tx, authority, at)
	if err != nil || !decision.Allowed {
		if err == nil {
			err = app.ErrUnauthorized
		}
		return app.UsageTargetRecord{}, err
	}
	out := app.UsageTargetRecord{Execution: execution}
	var revision model.Revision
	err = tx.QueryRowContext(ctx, `SELECT revision FROM history_catalog WHERE conversation_id=?`, execution.ConversationID).Scan(&revision)
	if err == nil {
		resolved, resolveErr := resolveUsageHistory(ctx, tx, execution.ConversationID, revision)
		if resolveErr != nil {
			return out, resolveErr
		}
		out.History = &resolved
	} else if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	return out, tx.Commit()
}

func (s *Store) RecordUsage(ctx context.Context, write app.UsageWrite) (model.UsageObservation, bool, error) {
	if err := s.ensureUsageSchema(ctx); err != nil {
		return model.UsageObservation{}, false, err
	}
	counters, err := json.Marshal(write.Observation.Counters)
	if err != nil {
		return model.UsageObservation{}, false, err
	}
	var amount, currency, kind any
	if write.Observation.Cost != nil {
		amount, currency, kind = write.Observation.Cost.Amount, write.Observation.Cost.Currency, write.Observation.Cost.Kind
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO usage_observations(id,agent_id,conversation_id,execution_id,attribution_precision,harness,source_key,source,source_revision,observed_at,collected_at,counters_json,cost_amount,cost_currency,cost_kind,counter_coverage,cost_coverage,coverage_reason,cumulative,historical) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		write.Observation.ID, write.Observation.Attribution.AgentID, write.Observation.Attribution.ConversationID, write.Observation.Attribution.ExecutionID,
		write.Observation.Attribution.Precision, write.Observation.Harness, write.SourceKey, write.Observation.Source, write.Observation.SourceRevision, nanos(write.Observation.ObservedAt), nanos(write.Observation.CollectedAt), counters,
		amount, currency, kind, write.Observation.Coverage.Counters, write.Observation.Coverage.Cost, write.Observation.Coverage.Reason, write.Cumulative, write.Observation.Historical)
	if err == nil {
		if bumpErr := s.bump(ctx); bumpErr != nil {
			return model.UsageObservation{}, false, bumpErr
		}
		return write.Observation, false, nil
	}
	if !strings.Contains(strings.ToLower(err.Error()), "unique") {
		return model.UsageObservation{}, false, classify(err)
	}
	existing, cumulative, readErr := s.usageBySourceRevision(ctx, write.SourceKey, write.Observation.SourceRevision)
	if readErr != nil {
		return model.UsageObservation{}, false, readErr
	}
	want := write.Observation
	want.ID, want.CollectedAt = existing.ID, existing.CollectedAt
	if cumulative != write.Cumulative || !reflect.DeepEqual(existing, want) {
		return model.UsageObservation{}, false, app.ErrConflict
	}
	return existing, true, nil
}

func (s *Store) QueryUsage(ctx context.Context, filter app.UsageFilter, authority model.AuthorityRequest, at time.Time) (app.UsageResult, error) {
	if err := s.ensureUsageSchema(ctx); err != nil {
		return app.UsageResult{}, err
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.UsageResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	resolved, err := resolveUsageExecution(ctx, tx, filter.Target)
	if err != nil {
		return app.UsageResult{}, err
	}
	authority.Resource = model.ResourceSelector{Kind: model.ResourceExecution, ExecutionID: resolved.ID}
	decision, err := authorizeTx(ctx, tx, authority, at)
	if err != nil || !decision.Allowed {
		if err == nil {
			err = app.ErrUnauthorized
		}
		return app.UsageResult{}, err
	}
	query := `SELECT id,agent_id,conversation_id,execution_id,attribution_precision,harness,source,source_revision,observed_at,collected_at,counters_json,cost_amount,cost_currency,cost_kind,counter_coverage,cost_coverage,coverage_reason,historical,cumulative
FROM usage_observations u WHERE (cumulative=0 OR NOT EXISTS(SELECT 1 FROM usage_observations newer WHERE newer.source_key=u.source_key AND (newer.observed_at>u.observed_at OR (newer.observed_at=u.observed_at AND newer.collected_at>u.collected_at))))`
	args := []any{}
	if filter.Target.ExecutionID != "" {
		query += ` AND execution_id=?`
		args = append(args, filter.Target.ExecutionID)
	} else {
		query += ` AND conversation_id=?`
		args = append(args, filter.Target.ConversationID)
	}
	if !filter.After.IsZero() {
		query += ` AND observed_at>=?`
		args = append(args, nanos(filter.After))
	}
	if !filter.Before.IsZero() {
		query += ` AND observed_at<?`
		args = append(args, nanos(filter.Before))
	}
	if filter.Cursor != "" {
		cursorAt, cursorID, err := decodeCursor(filter.Cursor)
		if err != nil {
			return app.UsageResult{}, app.ErrInvalid
		}
		query += ` AND (observed_at<? OR (observed_at=? AND id>?))`
		args = append(args, cursorAt, cursorAt, cursorID)
	}
	query += ` ORDER BY observed_at DESC,id ASC LIMIT ?`
	args = append(args, limit+1)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return app.UsageResult{}, err
	}
	defer rows.Close()
	var result app.UsageResult
	for rows.Next() {
		observation, _, err := scanUsage(rows)
		if err != nil {
			return result, err
		}
		result.Observations = append(result.Observations, observation)
	}
	if err = rows.Err(); err != nil {
		return result, err
	}
	if len(result.Observations) > limit {
		last := result.Observations[limit-1]
		result.NextCursor = encodeCursor(nanos(last.ObservedAt), string(last.ID))
		result.Observations = result.Observations[:limit]
	}
	if err = rows.Close(); err != nil {
		return result, err
	}
	return result, tx.Commit()
}

func (s *Store) usageBySourceRevision(ctx context.Context, key, revision string) (model.UsageObservation, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,agent_id,conversation_id,execution_id,attribution_precision,harness,source,source_revision,observed_at,collected_at,counters_json,cost_amount,cost_currency,cost_kind,counter_coverage,cost_coverage,coverage_reason,historical,cumulative FROM usage_observations WHERE source_key=? AND source_revision=?`, key, revision)
	return scanUsage(row)
}

func scanUsage(row scanner) (model.UsageObservation, bool, error) {
	var out model.UsageObservation
	var observed, collected int64
	var counters []byte
	var amount, currency, kind sql.NullString
	var cumulative bool
	if err := row.Scan(&out.ID, &out.Attribution.AgentID, &out.Attribution.ConversationID, &out.Attribution.ExecutionID, &out.Attribution.Precision, &out.Harness, &out.Source, &out.SourceRevision, &observed, &collected, &counters, &amount, &currency, &kind, &out.Coverage.Counters, &out.Coverage.Cost, &out.Coverage.Reason, &out.Historical, &cumulative); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, false, classify(err)
		}
		return out, false, err
	}
	if err := json.Unmarshal(counters, &out.Counters); err != nil {
		return out, false, err
	}
	out.ObservedAt, out.CollectedAt = fromNanos(observed), fromNanos(collected)
	if amount.Valid {
		out.Cost = &model.UsageCost{Amount: amount.String, Currency: currency.String, Kind: model.UsageCostKind(kind.String)}
	}
	return out, cumulative, nil
}

func encodeCursor(at int64, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(at, 10) + "\x00" + id))
}

func decodeCursor(value string) (int64, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return 0, "", err
	}
	parts := strings.SplitN(string(raw), "\x00", 2)
	if len(parts) != 2 || parts[1] == "" {
		return 0, "", fmt.Errorf("invalid cursor")
	}
	at, err := strconv.ParseInt(parts[0], 10, 64)
	return at, parts[1], err
}

var _ app.UsageStore = (*Store)(nil)

func resolveUsageExecution(ctx context.Context, q queryer, target app.UsageTarget) (model.Execution, error) {
	var execution model.Execution
	var err error
	if target.ExecutionID != "" {
		execution, err = scanExecution(q.QueryRowContext(ctx, executionSelect+` WHERE id=?`, target.ExecutionID))
	} else {
		execution, err = scanExecution(q.QueryRowContext(ctx, executionSelect+` WHERE conversation_id=? ORDER BY created_at DESC,id DESC LIMIT 1`, target.ConversationID))
	}
	if err == nil && target.ConversationID != "" && execution.ConversationID != target.ConversationID {
		return model.Execution{}, app.ErrNotFound
	}
	return execution, err
}

func resolveUsageHistory(ctx context.Context, q queryer, id model.ConversationID, expected model.Revision) (app.HistorySelectionRecord, error) {
	row := q.QueryRowContext(ctx, `SELECT conversation_id,harness,title,workspace_id,workspace_hint,archived,availability,metadata_coverage,content_coverage,source_revision,refreshed_at,modified_at,revision,native_namespace,native_reference,native_observed_at,source_token,source_fingerprint,evidence_provider,evidence_version,evidence_payload FROM history_catalog WHERE conversation_id=?`, id)
	var out app.HistorySelectionRecord
	var refreshed, modified, observed int64
	var evidence model.ProviderEvidence
	if err := row.Scan(&out.Entry.ConversationID, &out.Entry.Harness, &out.Entry.Title, &out.Entry.WorkspaceID, &out.Entry.WorkspaceHint, &out.Entry.Archived, &out.Entry.Availability, &out.Entry.Coverage.Metadata, &out.Entry.Coverage.Content, &out.Entry.Coverage.SourceRevision, &refreshed, &modified, &out.Entry.Revision, &out.Source.Native.Namespace, &out.Source.Native.Reference, &observed, &out.Source.SourceToken, &out.Source.SourceFingerprint, &evidence.Provider, &evidence.Version, &evidence.Payload); err != nil {
		return out, classify(err)
	}
	if expected == 0 || expected != out.Entry.Revision {
		return out, app.ErrConflict
	}
	out.Entry.Coverage.RefreshedAt, out.Entry.ModifiedAt = fromNanos(refreshed), fromNanos(modified)
	out.Source.Native.ObservedAt = fromNanos(observed)
	out.Source.Provider, out.Source.ConversationID = out.Entry.Harness, out.Entry.ConversationID
	out.Source.SourceRevision, out.Source.Evidence = out.Entry.Coverage.SourceRevision, evidence
	return out, nil
}
