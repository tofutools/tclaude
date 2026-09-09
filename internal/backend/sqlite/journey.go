package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func (s *Store) CatalogHistory(ctx context.Context, harness, sourceName string, writes []app.HistoryCatalogWrite, coverage model.HistoryCoverage, at time.Time) ([]model.HistoryCatalogEntry, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO history_refreshes(harness,source_name,metadata_coverage,content_coverage,source_revision,refreshed_at) VALUES(?,?,?,?,?,?) ON CONFLICT(harness,source_name) DO UPDATE SET metadata_coverage=excluded.metadata_coverage,content_coverage=excluded.content_coverage,source_revision=excluded.source_revision,refreshed_at=excluded.refreshed_at`, harness, sourceName, coverage.Metadata, coverage.Content, coverage.SourceRevision, nanos(coverage.RefreshedAt)); err != nil {
		return nil, err
	}
	if coverage.Metadata == model.HistoryCoverageComplete {
		if _, err := tx.ExecContext(ctx, `UPDATE history_catalog SET availability=?,revision=revision+1 WHERE harness=? AND source_name=?`, model.HistoryAbsent, harness, sourceName); err != nil {
			return nil, err
		}
	}
	for _, write := range writes {
		var id model.ConversationID
		err := tx.QueryRowContext(ctx, `SELECT conversation_id FROM history_catalog WHERE harness=? AND native_namespace=? AND native_reference=?`, write.Entry.Harness, write.Native.Namespace, write.Native.Reference).Scan(&id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if id == "" {
			id = write.Entry.ConversationID
			if _, err := tx.ExecContext(ctx, `INSERT INTO conversations(id,revision,created_at,updated_at) VALUES(?,1,?,?)`, id, nanos(at), nanos(at)); err != nil {
				return nil, classify(err)
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO history_catalog(conversation_id,harness,source_name,title,workspace_id,workspace_hint,archived,availability,metadata_coverage,content_coverage,source_revision,refreshed_at,modified_at,native_namespace,native_reference,native_observed_at,source_token,source_fingerprint,evidence_provider,evidence_version,evidence_payload,revision) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1)`, id, write.Entry.Harness, sourceName, write.Entry.Title, write.Entry.WorkspaceID, write.Entry.WorkspaceHint, write.Entry.Archived, write.Entry.Availability, write.Entry.Coverage.Metadata, write.Entry.Coverage.Content, write.Entry.Coverage.SourceRevision, nanos(write.Entry.Coverage.RefreshedAt), nanos(write.Entry.ModifiedAt), write.Native.Namespace, write.Native.Reference, nanos(write.Native.ObservedAt), write.SourceToken, write.SourceFingerprint, write.Evidence.Provider, write.Evidence.Version, write.Evidence.Payload)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE history_catalog SET source_name=?,title=?,workspace_id=?,workspace_hint=?,availability=?,metadata_coverage=?,content_coverage=?,source_revision=?,refreshed_at=?,modified_at=?,native_observed_at=?,source_token=?,source_fingerprint=?,evidence_provider=?,evidence_version=?,evidence_payload=?,revision=revision+1 WHERE conversation_id=?`, sourceName, write.Entry.Title, write.Entry.WorkspaceID, write.Entry.WorkspaceHint, write.Entry.Availability, write.Entry.Coverage.Metadata, write.Entry.Coverage.Content, write.Entry.Coverage.SourceRevision, nanos(write.Entry.Coverage.RefreshedAt), nanos(write.Entry.ModifiedAt), nanos(write.Native.ObservedAt), write.SourceToken, write.SourceFingerprint, write.Evidence.Provider, write.Evidence.Version, write.Evidence.Payload, id)
		}
		if err != nil {
			return nil, err
		}
		for _, point := range write.Points {
			_, err := tx.ExecContext(ctx, `INSERT INTO history_points(id,conversation_id,kind,provider_token,occurred_at,revision) VALUES(?,?,?,?,?,1) ON CONFLICT(conversation_id,provider_token) DO UPDATE SET kind=excluded.kind,occurred_at=excluded.occurred_at,revision=history_points.revision+1`, point.Point.ID, id, point.Point.Kind, point.Token, nanos(point.Point.OccurredAt))
			if err != nil {
				return nil, classify(err)
			}
		}
	}
	if err := bumpTx(ctx, tx); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	result, err := s.searchHistory(ctx, app.HistorySearchFilter{Harness: harness}, sourceName)
	return result.Entries, err
}

func (s *Store) SearchHistory(ctx context.Context, filter app.HistorySearchFilter) (app.HistorySearchResult, error) {
	return s.searchHistory(ctx, filter, "")
}

func (s *Store) searchHistory(ctx context.Context, filter app.HistorySearchFilter, sourceName string) (app.HistorySearchResult, error) {
	query := `SELECT conversation_id,harness,title,workspace_id,workspace_hint,archived,availability,metadata_coverage,content_coverage,source_revision,refreshed_at,modified_at,revision FROM history_catalog WHERE 1=1`
	var args []any
	if filter.Harness != "" {
		query += ` AND harness=?`
		args = append(args, filter.Harness)
	}
	if sourceName != "" {
		query += ` AND source_name=?`
		args = append(args, sourceName)
	}
	if filter.WorkspaceID != "" {
		query += ` AND workspace_id=?`
		args = append(args, filter.WorkspaceID)
	}
	if filter.Archived != nil {
		query += ` AND archived=?`
		args = append(args, *filter.Archived)
	}
	if strings.TrimSpace(filter.Query) != "" {
		query += ` AND (title LIKE ? OR search_text LIKE ?)`
		term := "%" + strings.TrimSpace(filter.Query) + "%"
		args = append(args, term, term)
	}
	query += ` ORDER BY modified_at DESC,conversation_id`
	result := app.HistorySearchResult{Coverage: model.HistoryCoverage{Metadata: model.HistoryCoverageUnknown, Content: model.HistoryCoverageUnknown}}
	coverageSeen := false
	coverageQuery := `SELECT metadata_coverage,content_coverage,source_revision,refreshed_at FROM history_refreshes WHERE 1=1`
	var coverageArgs []any
	if filter.Harness != "" {
		coverageQuery += ` AND harness=?`
		coverageArgs = append(coverageArgs, filter.Harness)
	}
	if sourceName != "" {
		coverageQuery += ` AND source_name=?`
		coverageArgs = append(coverageArgs, sourceName)
	}
	coverageRows, coverageErr := s.db.QueryContext(ctx, coverageQuery, coverageArgs...)
	if coverageErr != nil {
		return result, coverageErr
	}
	for coverageRows.Next() {
		var metadata, content model.HistoryCoverageState
		var revision string
		var refreshed int64
		if err := coverageRows.Scan(&metadata, &content, &revision, &refreshed); err != nil {
			coverageRows.Close()
			return result, err
		}
		if !coverageSeen {
			result.Coverage.Metadata, result.Coverage.Content = metadata, content
			coverageSeen = true
		} else {
			result.Coverage.Metadata = mergeCoverage(result.Coverage.Metadata, metadata)
			result.Coverage.Content = mergeCoverage(result.Coverage.Content, content)
		}
		result.Coverage.RefreshedAt = later(result.Coverage.RefreshedAt, fromNanos(refreshed))
		if result.Coverage.SourceRevision == "" {
			result.Coverage.SourceRevision = revision
		} else if result.Coverage.SourceRevision != revision {
			result.Coverage.SourceRevision = ""
		}
	}
	if err := coverageRows.Close(); err != nil {
		return result, err
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return app.HistorySearchResult{}, err
	}
	defer rows.Close()
	for rows.Next() {
		entry, err := scanHistoryEntry(rows)
		if err != nil {
			return result, err
		}
		result.Entries = append(result.Entries, entry)
	}
	return result, rows.Err()
}

func (s *Store) ResolveHistory(ctx context.Context, selection model.HistorySelection) (app.HistorySelectionRecord, error) {
	row := s.db.QueryRowContext(ctx, `SELECT conversation_id,harness,title,workspace_id,workspace_hint,archived,availability,metadata_coverage,content_coverage,source_revision,refreshed_at,modified_at,revision,native_namespace,native_reference,native_observed_at,source_token,source_fingerprint,evidence_provider,evidence_version,evidence_payload FROM history_catalog WHERE conversation_id=?`, selection.ConversationID)
	var out app.HistorySelectionRecord
	var refreshed, modified, observed int64
	var evidence model.ProviderEvidence
	if err := row.Scan(&out.Entry.ConversationID, &out.Entry.Harness, &out.Entry.Title, &out.Entry.WorkspaceID, &out.Entry.WorkspaceHint, &out.Entry.Archived, &out.Entry.Availability, &out.Entry.Coverage.Metadata, &out.Entry.Coverage.Content, &out.Entry.Coverage.SourceRevision, &refreshed, &modified, &out.Entry.Revision, &out.Source.Native.Namespace, &out.Source.Native.Reference, &observed, &out.Source.SourceToken, &out.Source.SourceFingerprint, &evidence.Provider, &evidence.Version, &evidence.Payload); err != nil {
		return out, classify(err)
	}
	out.Entry.Coverage.RefreshedAt, out.Entry.ModifiedAt = fromNanos(refreshed), fromNanos(modified)
	out.Source.Native.ObservedAt = fromNanos(observed)
	out.Source.Provider = out.Entry.Harness
	out.Source.ConversationID = out.Entry.ConversationID
	out.Source.SourceRevision = out.Entry.Coverage.SourceRevision
	out.Source.Evidence = evidence
	if selection.ExpectedConversationRevision == 0 || selection.ExpectedConversationRevision != out.Entry.Revision {
		return out, app.ErrConflict
	}
	if selection.PointID != "" {
		var point model.HistoryPoint
		var token string
		var occurred int64
		if err := s.db.QueryRowContext(ctx, `SELECT id,conversation_id,kind,provider_token,occurred_at,revision FROM history_points WHERE id=? AND conversation_id=?`, selection.PointID, selection.ConversationID).Scan(&point.ID, &point.ConversationID, &point.Kind, &token, &occurred, &point.Revision); err != nil {
			return out, classify(err)
		}
		point.OccurredAt = fromNanos(occurred)
		if selection.ExpectedPointRevision == 0 || selection.ExpectedPointRevision != point.Revision {
			return out, app.ErrConflict
		}
		out.Point = &point
		out.Source.Point = &ports.ProviderHistoryPoint{Token: token, Kind: point.Kind, OccurredAt: point.OccurredAt}
	}
	return out, nil
}

func (s *Store) HistoryPoints(ctx context.Context, id model.ConversationID) ([]model.HistoryPoint, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,conversation_id,kind,occurred_at,revision FROM history_points WHERE conversation_id=? ORDER BY occurred_at,id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var points []model.HistoryPoint
	for rows.Next() {
		var point model.HistoryPoint
		var occurred int64
		if err := rows.Scan(&point.ID, &point.ConversationID, &point.Kind, &occurred, &point.Revision); err != nil {
			return nil, err
		}
		point.OccurredAt = fromNanos(occurred)
		points = append(points, point)
	}
	return points, rows.Err()
}

func (s *Store) IndexHistoryRead(ctx context.Context, id model.ConversationID, text string, coverage model.HistoryCoverage, at time.Time) (model.HistoryCatalogEntry, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE history_catalog SET search_text=?,content_coverage=?,source_revision=?,refreshed_at=?,revision=revision+1 WHERE conversation_id=?`, text, coverage.Content, coverage.SourceRevision, nanos(at), id)
	if err != nil {
		return model.HistoryCatalogEntry{}, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return model.HistoryCatalogEntry{}, app.ErrNotFound
	}
	if err := s.bump(ctx); err != nil {
		return model.HistoryCatalogEntry{}, err
	}
	var entry model.HistoryCatalogEntry
	err = scanHistoryEntryInto(s.db.QueryRowContext(ctx, `SELECT conversation_id,harness,title,workspace_id,workspace_hint,archived,availability,metadata_coverage,content_coverage,source_revision,refreshed_at,modified_at,revision FROM history_catalog WHERE conversation_id=?`, id), &entry)
	return entry, err
}

func (s *Store) SetHistoryMetadata(ctx context.Context, id model.ConversationID, expected model.Revision, title string, archived bool, requestID model.RequestID, authority model.AuthorityRequest, at time.Time) (model.HistoryCatalogEntry, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.HistoryCatalogEntry{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var priorID model.ConversationID
	var priorTitle string
	var priorArchived bool
	err = tx.QueryRowContext(ctx, `SELECT conversation_id,title,archived FROM history_metadata_requests WHERE request_scope=? AND request_id=?`, requestScope(authority.Principal), requestID).Scan(&priorID, &priorTitle, &priorArchived)
	if err == nil {
		if priorID != id || priorTitle != title || priorArchived != archived {
			return model.HistoryCatalogEntry{}, app.ErrConflict
		}
		if err = tx.Commit(); err != nil {
			return model.HistoryCatalogEntry{}, err
		}
		var entry model.HistoryCatalogEntry
		err = scanHistoryEntryInto(s.db.QueryRowContext(ctx, `SELECT conversation_id,harness,title,workspace_id,workspace_hint,archived,availability,metadata_coverage,content_coverage,source_revision,refreshed_at,modified_at,revision FROM history_catalog WHERE conversation_id=?`, id), &entry)
		return entry, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return model.HistoryCatalogEntry{}, err
	}
	decision, err := authorizeTx(ctx, tx, authority, at)
	if err != nil {
		return model.HistoryCatalogEntry{}, err
	}
	if !decision.Allowed {
		return model.HistoryCatalogEntry{}, app.ErrUnauthorized
	}
	result, err := tx.ExecContext(ctx, `UPDATE history_catalog SET title=?,archived=?,revision=revision+1 WHERE conversation_id=? AND revision=?`, title, archived, id, expected)
	if err != nil {
		return model.HistoryCatalogEntry{}, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return model.HistoryCatalogEntry{}, app.ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO history_metadata_requests(request_scope,request_id,conversation_id,title,archived) VALUES(?,?,?,?,?)`, requestScope(authority.Principal), requestID, id, title, archived); err != nil {
		return model.HistoryCatalogEntry{}, classify(err)
	}
	if err = bumpTx(ctx, tx); err != nil {
		return model.HistoryCatalogEntry{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.HistoryCatalogEntry{}, err
	}
	var entry model.HistoryCatalogEntry
	err = scanHistoryEntryInto(s.db.QueryRowContext(ctx, `SELECT conversation_id,harness,title,workspace_id,workspace_hint,archived,availability,metadata_coverage,content_coverage,source_revision,refreshed_at,modified_at,revision FROM history_catalog WHERE conversation_id=?`, id), &entry)
	return entry, err
}

func mergeCoverage(current, next model.HistoryCoverageState) model.HistoryCoverageState {
	if current == model.HistoryCoverageUnknown {
		return next
	}
	if current == model.HistoryCoveragePartial || next == model.HistoryCoveragePartial {
		return model.HistoryCoveragePartial
	}
	if current == model.HistoryCoverageUnknown || next == model.HistoryCoverageUnknown {
		return model.HistoryCoverageUnknown
	}
	return model.HistoryCoverageComplete
}

func scanHistoryEntry(row scanner) (model.HistoryCatalogEntry, error) {
	var entry model.HistoryCatalogEntry
	err := scanHistoryEntryInto(row, &entry)
	return entry, err
}
func scanHistoryEntryInto(row scanner, entry *model.HistoryCatalogEntry) error {
	var refreshed, modified int64
	err := row.Scan(&entry.ConversationID, &entry.Harness, &entry.Title, &entry.WorkspaceID, &entry.WorkspaceHint, &entry.Archived, &entry.Availability, &entry.Coverage.Metadata, &entry.Coverage.Content, &entry.Coverage.SourceRevision, &refreshed, &modified, &entry.Revision)
	entry.Coverage.RefreshedAt, entry.ModifiedAt = fromNanos(refreshed), fromNanos(modified)
	return classify(err)
}

func (s *Store) RegisterWorkspace(ctx context.Context, workspace model.Workspace) error {
	intent, _ := json.Marshal(workspace.Intent)
	observation, _ := json.Marshal(workspace.Observation)
	_, err := s.db.ExecContext(ctx, `INSERT INTO workspaces(id,intent_json,state,observation_json,resource_owner,resource_version,resource_payload,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, workspace.ID, intent, workspace.State, observation, workspace.Resource.Owner, workspace.Resource.Version, workspace.Resource.Payload, workspace.Revision, nanos(workspace.CreatedAt), nanos(workspace.UpdatedAt))
	if err != nil {
		return classify(err)
	}
	return s.bump(ctx)
}

func (s *Store) Workspace(ctx context.Context, id model.WorkspaceID) (model.Workspace, error) {
	return scanWorkspace(s.db.QueryRowContext(ctx, `SELECT id,intent_json,state,observation_json,resource_owner,resource_version,resource_payload,revision,created_at,updated_at FROM workspaces WHERE id=?`, id))
}

func scanWorkspace(row scanner) (model.Workspace, error) {
	var workspace model.Workspace
	var intent, observation []byte
	var created, updated int64
	if err := row.Scan(&workspace.ID, &intent, &workspace.State, &observation, &workspace.Resource.Owner, &workspace.Resource.Version, &workspace.Resource.Payload, &workspace.Revision, &created, &updated); err != nil {
		return workspace, classify(err)
	}
	if err := json.Unmarshal(intent, &workspace.Intent); err != nil {
		return workspace, err
	}
	if err := json.Unmarshal(observation, &workspace.Observation); err != nil {
		return workspace, err
	}
	workspace.CreatedAt, workspace.UpdatedAt = fromNanos(created), fromNanos(updated)
	return workspace, nil
}

func (s *Store) AdmitWorkspaceEffect(ctx context.Context, in app.WorkspaceEffectAdmission) (app.WorkspaceEffectAdmissionResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.WorkspaceEffectAdmissionResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if repeated, ok, err := resourceAdmissionByRequest(ctx, tx, in.Operation, in.Workspace); err != nil {
		return app.WorkspaceEffectAdmissionResult{}, err
	} else if ok {
		_ = tx.Commit()
		return repeated, nil
	}
	decision, err := authorizeTx(ctx, tx, in.Authority, in.Operation.CreatedAt)
	if err != nil || !decision.Allowed {
		if err == nil {
			err = app.ErrUnauthorized
		}
		return app.WorkspaceEffectAdmissionResult{}, err
	}
	// Reserve removal in the same transaction that excludes new execution uses.
	if in.Operation.Kind == model.OperationRemoveWorkspace {
		if err := requireNoWorkspaceRemovalTx(ctx, tx, in.Workspace.ID); err != nil {
			return app.WorkspaceEffectAdmissionResult{}, err
		}
		var uses int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM workspace_uses WHERE workspace_id=? AND released_at IS NULL`, in.Workspace.ID).Scan(&uses); err != nil {
			return app.WorkspaceEffectAdmissionResult{}, err
		}
		if uses != 0 {
			return app.WorkspaceEffectAdmissionResult{}, app.ErrConflict
		}
		result, err := tx.ExecContext(ctx, `UPDATE workspaces SET state=?,revision=revision+1,updated_at=? WHERE id=? AND revision=? AND state=?`, model.WorkspacePending, nanos(in.Operation.CreatedAt), in.Workspace.ID, in.Workspace.Revision, model.WorkspaceAvailable)
		if err != nil {
			return app.WorkspaceEffectAdmissionResult{}, err
		}
		if n, _ := result.RowsAffected(); n != 1 {
			return app.WorkspaceEffectAdmissionResult{}, app.ErrConflict
		}
	}
	var existingWorkspace model.WorkspaceID
	if err := tx.QueryRowContext(ctx, `SELECT id FROM workspaces WHERE id=?`, in.Workspace.ID).Scan(&existingWorkspace); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return app.WorkspaceEffectAdmissionResult{}, err
		}
		intent, _ := json.Marshal(in.Workspace.Intent)
		observation, _ := json.Marshal(in.Workspace.Observation)
		if _, err := tx.ExecContext(ctx, `INSERT INTO workspaces(id,intent_json,state,observation_json,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, in.Workspace.ID, intent, in.Workspace.State, observation, in.Workspace.Revision, nanos(in.Workspace.CreatedAt), nanos(in.Workspace.UpdatedAt)); err != nil {
			return app.WorkspaceEffectAdmissionResult{}, classify(err)
		}
	}
	if err := insertOperation(ctx, tx, in.Operation); err != nil {
		return app.WorkspaceEffectAdmissionResult{}, err
	}
	if err := insertOperationAuthority(ctx, tx, in.Operation.ID, in.Authority, decision); err != nil {
		return app.WorkspaceEffectAdmissionResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO effect_permits(operation_id) VALUES(?)`, in.Operation.ID); err != nil {
		return app.WorkspaceEffectAdmissionResult{}, err
	}
	if err := bumpTx(ctx, tx); err != nil {
		return app.WorkspaceEffectAdmissionResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return app.WorkspaceEffectAdmissionResult{}, err
	}
	return app.WorkspaceEffectAdmissionResult{Operation: in.Operation, Workspace: in.Workspace}, nil
}

func resourceAdmissionByRequest(ctx context.Context, tx *sql.Tx, operation model.Operation, requested model.Workspace) (app.WorkspaceEffectAdmissionResult, bool, error) {
	var existing model.OperationID
	err := tx.QueryRowContext(ctx, `SELECT id FROM operations WHERE request_scope=? AND request_id=?`, requestScope(operation.Principal), operation.RequestID).Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		return app.WorkspaceEffectAdmissionResult{}, false, nil
	}
	if err != nil {
		return app.WorkspaceEffectAdmissionResult{}, false, err
	}
	stored, err := operationTx(ctx, tx, existing)
	if err != nil {
		return app.WorkspaceEffectAdmissionResult{}, false, err
	}
	if stored.Kind != operation.Kind || !sameRequester(stored.Principal, operation.Principal) {
		return app.WorkspaceEffectAdmissionResult{}, false, app.ErrConflict
	}
	var workspaceID model.WorkspaceID
	if err := tx.QueryRowContext(ctx, `SELECT resource_id FROM operation_authority WHERE operation_id=?`, existing).Scan(&workspaceID); err != nil {
		return app.WorkspaceEffectAdmissionResult{}, false, err
	}
	workspace, err := scanWorkspace(tx.QueryRowContext(ctx, `SELECT id,intent_json,state,observation_json,resource_owner,resource_version,resource_payload,revision,created_at,updated_at FROM workspaces WHERE id=?`, workspaceID))
	if err != nil {
		return app.WorkspaceEffectAdmissionResult{}, false, err
	}
	if workspace.ID != requested.ID || ((operation.Kind == model.OperationCreateWorkspace || operation.Kind == model.OperationRestoreWorkspace) && !reflect.DeepEqual(workspace.Intent, requested.Intent)) {
		return app.WorkspaceEffectAdmissionResult{}, false, app.ErrConflict
	}
	return app.WorkspaceEffectAdmissionResult{Operation: stored, Workspace: workspace, Repeated: true}, true, nil
}

func (s *Store) CompleteWorkspaceEffect(ctx context.Context, in app.WorkspaceEffectCompletion) (model.Workspace, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Workspace{}, err
	}
	defer func() { _ = tx.Rollback() }()
	opState := model.OperationFailed
	switch in.Disposition {
	case ports.EffectAccepted:
		opState = model.OperationSucceeded
	case ports.EffectUnknown:
		opState = model.OperationUncertain
	case ports.EffectRefused, ports.EffectUnsupported:
		opState = model.OperationRefused
	}
	result, err := tx.ExecContext(ctx, `UPDATE operations SET state=?,detail=?,revision=revision+1,updated_at=? WHERE id=? AND state=?`, opState, in.Detail, nanos(in.At), in.OperationID, model.OperationAdmitted)
	if err != nil {
		return model.Workspace{}, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return model.Workspace{}, app.ErrConflict
	}
	observation, _ := json.Marshal(in.Observation)
	result, err = tx.ExecContext(ctx, `UPDATE workspaces SET state=?,observation_json=?,resource_owner=?,resource_version=?,resource_payload=?,revision=revision+1,updated_at=? WHERE id=?`, in.State, observation, in.Resource.Owner, in.Resource.Version, in.Resource.Payload, nanos(in.At), in.WorkspaceID)
	if err != nil {
		return model.Workspace{}, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return model.Workspace{}, app.ErrConflict
	}
	if err := bumpTx(ctx, tx); err != nil {
		return model.Workspace{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Workspace{}, err
	}
	return s.Workspace(ctx, in.WorkspaceID)
}

func (s *Store) ActiveWorkspaceUses(ctx context.Context, id model.WorkspaceID) ([]model.WorkspaceUse, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,workspace_id,execution_id,work_run_id,released_at,created_at FROM workspace_uses WHERE workspace_id=? AND released_at IS NULL ORDER BY created_at`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.WorkspaceUse
	for rows.Next() {
		var use model.WorkspaceUse
		var released sql.NullInt64
		var created int64
		if err := rows.Scan(&use.ID, &use.WorkspaceID, &use.ExecutionID, &use.WorkRunID, &released, &created); err != nil {
			return nil, err
		}
		use.CreatedAt = fromNanos(created)
		if released.Valid {
			v := fromNanos(released.Int64)
			use.ReleasedAt = &v
		}
		out = append(out, use)
	}
	return out, rows.Err()
}

func (s *Store) WorkspaceUseForExecution(ctx context.Context, executionID model.ExecutionID) (model.WorkspaceUse, error) {
	var use model.WorkspaceUse
	var released sql.NullInt64
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT id,workspace_id,execution_id,work_run_id,released_at,created_at FROM workspace_uses WHERE execution_id=?`, executionID).Scan(&use.ID, &use.WorkspaceID, &use.ExecutionID, &use.WorkRunID, &released, &created)
	if err != nil {
		return use, classify(err)
	}
	use.CreatedAt = fromNanos(created)
	if released.Valid {
		at := fromNanos(released.Int64)
		use.ReleasedAt = &at
	}
	return use, nil
}

func (s *Store) ReleaseWorkspaceUse(ctx context.Context, id model.WorkspaceUseID, executionID model.ExecutionID, at time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE workspace_uses SET released_at=? WHERE id=? AND execution_id=? AND released_at IS NULL`, nanos(at), id, executionID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return app.ErrConflict
	}
	return s.bump(ctx)
}

func (s *Store) UpdateWorkspaceObservation(ctx context.Context, id model.WorkspaceID, expected model.Revision, state model.WorkspaceState, observation model.WorkspaceObservation, resource model.WorkspaceResourceEvidence, at time.Time) (model.Workspace, error) {
	encoded, _ := json.Marshal(observation)
	result, err := s.db.ExecContext(ctx, `UPDATE workspaces SET state=?,observation_json=?,resource_owner=?,resource_version=?,resource_payload=?,revision=revision+1,updated_at=? WHERE id=? AND revision=?`, state, encoded, resource.Owner, resource.Version, resource.Payload, nanos(at), id, expected)
	if err != nil {
		return model.Workspace{}, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return model.Workspace{}, app.ErrConflict
	}
	if err := s.bump(ctx); err != nil {
		return model.Workspace{}, err
	}
	return s.Workspace(ctx, id)
}

func (s *Store) AcquireHistoryUse(ctx context.Context, claim model.HistoryUseClaim) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO history_use_claims(id,conversation_id,point_id,operation_id,work_run_id,source_revision,source_fingerprint,state,revision,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, claim.ID, claim.ConversationID, claim.PointID, claim.OperationID, claim.WorkRunID, claim.SourceRevision, claim.SourceFingerprint, claim.State, claim.Revision, nanos(claim.CreatedAt))
	if err != nil {
		return classify(err)
	}
	return s.bump(ctx)
}

func (s *Store) SettleHistoryUse(ctx context.Context, id model.HistoryUseID, expected model.Revision, state model.HistoryUseState, at time.Time) (model.HistoryUseClaim, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE history_use_claims SET state=?,revision=revision+1,settled_at=CASE WHEN ?=? THEN ? ELSE NULL END WHERE id=? AND revision=? AND state IN (?,?)`, state, state, model.HistoryUseReleased, nanos(at), id, expected, model.HistoryUseHeld, model.HistoryUseUncertain)
	if err != nil {
		return model.HistoryUseClaim{}, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return model.HistoryUseClaim{}, app.ErrConflict
	}
	var claim model.HistoryUseClaim
	var created int64
	var settled sql.NullInt64
	err = s.db.QueryRowContext(ctx, `SELECT id,conversation_id,point_id,operation_id,work_run_id,source_revision,source_fingerprint,state,revision,created_at,settled_at FROM history_use_claims WHERE id=?`, id).Scan(&claim.ID, &claim.ConversationID, &claim.PointID, &claim.OperationID, &claim.WorkRunID, &claim.SourceRevision, &claim.SourceFingerprint, &claim.State, &claim.Revision, &created, &settled)
	if err != nil {
		return claim, classify(err)
	}
	claim.CreatedAt = fromNanos(created)
	if settled.Valid {
		value := fromNanos(settled.Int64)
		claim.SettledAt = &value
	}
	if err := s.bump(ctx); err != nil {
		return claim, err
	}
	return claim, nil
}

func (s *Store) HistoryUse(ctx context.Context, id model.HistoryUseID) (model.HistoryUseClaim, error) {
	var claim model.HistoryUseClaim
	var created int64
	var settled sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT id,conversation_id,point_id,operation_id,work_run_id,source_revision,source_fingerprint,state,revision,created_at,settled_at FROM history_use_claims WHERE id=?`, id).Scan(&claim.ID, &claim.ConversationID, &claim.PointID, &claim.OperationID, &claim.WorkRunID, &claim.SourceRevision, &claim.SourceFingerprint, &claim.State, &claim.Revision, &created, &settled)
	if err != nil {
		return claim, classify(err)
	}
	claim.CreatedAt = fromNanos(created)
	if settled.Valid {
		value := fromNanos(settled.Int64)
		claim.SettledAt = &value
	}
	return claim, nil
}

func (s *Store) CreateWorkRun(ctx context.Context, run model.WorkRun, claim *model.HistoryUseClaim) (model.WorkRun, bool, error) {
	requestID := run.RequestID
	scope := requestScope(run.Requester)
	var existing model.WorkRunID
	err := s.db.QueryRowContext(ctx, `SELECT id FROM work_runs WHERE request_scope=? AND request_id=?`, scope, requestID).Scan(&existing)
	if err == nil {
		stored, e := s.WorkRun(ctx, existing)
		if e == nil && (stored.Run.ID != run.ID || !reflect.DeepEqual(stored.Run.Spec, run.Spec) || !reflect.DeepEqual(stored.Run.Requester, run.Requester)) {
			return model.WorkRun{}, false, app.ErrConflict
		}
		return stored.Run, true, e
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return model.WorkRun{}, false, err
	}
	requester, _ := json.Marshal(run.Requester)
	authority, _ := json.Marshal(run.Authority)
	delegation, _ := json.Marshal(run.Delegation)
	spec, _ := json.Marshal(run.Spec)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.WorkRun{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `INSERT INTO work_runs(id,request_scope,request_id,requester_json,authority_json,delegation_json,spec_json,state,worker_execution_id,cancellation_requested,cancellation_reason,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, run.ID, scope, requestID, requester, authority, delegation, spec, run.State, run.WorkerExecutionID, run.CancellationRequested, run.CancellationReason, run.Revision, nanos(run.CreatedAt), nanos(run.UpdatedAt))
	if err != nil {
		return model.WorkRun{}, false, classify(err)
	}
	for _, attempt := range run.Attempts {
		var settled any
		if attempt.SettledAt != nil {
			settled = nanos(*attempt.SettledAt)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO work_attempts(work_run_id,step,attempt,operation_id,state,detail,started_at,settled_at) VALUES(?,?,?,?,?,?,?,?)`, run.ID, attempt.Step, attempt.Attempt, attempt.OperationID, attempt.State, attempt.Detail, nanos(attempt.StartedAt), settled); err != nil {
			return model.WorkRun{}, false, err
		}
	}
	if run.WorkspaceUseID != "" {
		if _, err = tx.ExecContext(ctx, `INSERT INTO workspace_uses(id,workspace_id,work_run_id,created_at) VALUES(?,?,?,?)`, run.WorkspaceUseID, run.Spec.WorkspaceID, run.ID, nanos(run.CreatedAt)); err != nil {
			return model.WorkRun{}, false, err
		}
	}
	if claim != nil {
		if _, err = tx.ExecContext(ctx, `INSERT INTO history_use_claims(id,conversation_id,point_id,operation_id,work_run_id,source_revision,source_fingerprint,state,revision,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, claim.ID, claim.ConversationID, claim.PointID, claim.OperationID, claim.WorkRunID, claim.SourceRevision, claim.SourceFingerprint, claim.State, claim.Revision, nanos(claim.CreatedAt)); err != nil {
			return model.WorkRun{}, false, classify(err)
		}
	}
	if err = bumpTx(ctx, tx); err != nil {
		return model.WorkRun{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return model.WorkRun{}, false, err
	}
	return run, false, nil
}

func (s *Store) WorkRun(ctx context.Context, id model.WorkRunID) (app.WorkRunRecord, error) {
	var record app.WorkRunRecord
	var requester, authority, delegation, spec []byte
	var graph, closure, parameters, scope, programs []byte
	var deadline sql.NullInt64
	var created, updated int64
	err := s.db.QueryRowContext(ctx, `SELECT id,request_id,requester_json,authority_json,delegation_json,spec_json,state,worker_execution_id,cancellation_requested,cancellation_reason,revision,created_at,updated_at,graph_json,definition_closure_json,parameters_json,scope_json,authorized_programs_json,control_state,outcome,deadline FROM work_runs WHERE id=?`, id).Scan(&record.Run.ID, &record.Run.RequestID, &requester, &authority, &delegation, &spec, &record.Run.State, &record.Run.WorkerExecutionID, &record.Run.CancellationRequested, &record.Run.CancellationReason, &record.Run.Revision, &created, &updated, &graph, &closure, &parameters, &scope, &programs, &record.Run.ControlState, &record.Run.Outcome, &deadline)
	if err != nil {
		return record, classify(err)
	}
	if err = json.Unmarshal(requester, &record.Run.Requester); err != nil {
		return record, err
	}
	if err = json.Unmarshal(authority, &record.Run.Authority); err != nil {
		return record, err
	}
	if len(delegation) > 0 && string(delegation) != "null" {
		record.Run.Delegation = new(model.AutomationDelegation)
		if err = json.Unmarshal(delegation, record.Run.Delegation); err != nil {
			return record, err
		}
	}
	if err = json.Unmarshal(spec, &record.Run.Spec); err != nil {
		return record, err
	}
	if len(graph) > 0 && string(graph) != "null" {
		record.Run.Graph = new(model.WorkGraph)
		if err = json.Unmarshal(graph, record.Run.Graph); err != nil {
			return record, err
		}
	}
	if len(closure) > 0 {
		if err = json.Unmarshal(closure, &record.Run.DefinitionClosure); err != nil {
			return record, err
		}
	}
	if len(parameters) > 0 {
		if err = json.Unmarshal(parameters, &record.Run.Parameters); err != nil {
			return record, err
		}
	}
	if len(scope) > 0 {
		if err = json.Unmarshal(scope, &record.Run.Scope); err != nil {
			return record, err
		}
	}
	if len(programs) > 0 {
		if err = json.Unmarshal(programs, &record.Run.AuthorizedPrograms); err != nil {
			return record, err
		}
	}
	if deadline.Valid {
		record.Run.Deadline = fromNanos(deadline.Int64)
	}
	record.Run.CreatedAt, record.Run.UpdatedAt = fromNanos(created), fromNanos(updated)
	_ = s.db.QueryRowContext(ctx, `SELECT id FROM workspace_uses WHERE work_run_id=? AND released_at IS NULL`, id).Scan(&record.Run.WorkspaceUseID)
	_ = s.db.QueryRowContext(ctx, `SELECT id FROM history_use_claims WHERE work_run_id=? ORDER BY created_at DESC LIMIT 1`, id).Scan(&record.Run.HistoryUseID)
	rows, err := s.db.QueryContext(ctx, `SELECT step,attempt,operation_id,state,detail,started_at,settled_at FROM work_attempts WHERE work_run_id=? ORDER BY rowid`, id)
	if err != nil {
		return record, err
	}
	for rows.Next() {
		var a model.WorkStepAttempt
		var started int64
		var settled sql.NullInt64
		if err := rows.Scan(&a.Step, &a.Attempt, &a.OperationID, &a.State, &a.Detail, &started, &settled); err != nil {
			rows.Close()
			return record, err
		}
		a.StartedAt = fromNanos(started)
		if settled.Valid {
			v := fromNanos(settled.Int64)
			a.SettledAt = &v
		}
		record.Run.Attempts = append(record.Run.Attempts, a)
	}
	rows.Close()
	nrows, err := s.db.QueryContext(ctx, `SELECT node_id,activation_id,attempt,issuance_id,state,performer_json,operation_id,execution_id,ready_at,retry_at,deadline,retry_budget,join_winner,decision_id,outcome,detail,created_at,updated_at,settled_at FROM work_node_attempts WHERE work_run_id=? ORDER BY created_at,node_id,activation_id,attempt`, id)
	if err != nil {
		return record, err
	}
	for nrows.Next() {
		var attempt model.WorkNodeAttempt
		var performer []byte
		var readyAt, deadlineAt, createdAt, updatedAt int64
		var retryAt, settledAt sql.NullInt64
		attempt.Ref.RunID = id
		if err = nrows.Scan(&attempt.Ref.NodeID, &attempt.Ref.ActivationID, &attempt.Ref.Attempt, &attempt.Ref.IssuanceID, &attempt.State, &performer, &attempt.OperationID, &attempt.ExecutionID, &readyAt, &retryAt, &deadlineAt, &attempt.RetryBudget, &attempt.JoinWinner, &attempt.DecisionID, &attempt.Outcome, &attempt.Detail, &createdAt, &updatedAt, &settledAt); err != nil {
			nrows.Close()
			return record, err
		}
		if len(performer) > 0 && string(performer) != "null" {
			attempt.Performer = new(model.Performer)
			if err = json.Unmarshal(performer, attempt.Performer); err != nil {
				nrows.Close()
				return record, err
			}
		}
		attempt.ReadyAt, attempt.Deadline = fromNanos(readyAt), fromNanos(deadlineAt)
		attempt.CreatedAt, attempt.UpdatedAt = fromNanos(createdAt), fromNanos(updatedAt)
		if retryAt.Valid {
			value := fromNanos(retryAt.Int64)
			attempt.RetryAt = &value
		}
		if settledAt.Valid {
			value := fromNanos(settledAt.Int64)
			attempt.SettledAt = &value
		}
		record.Run.NodeAttempts = append(record.Run.NodeAttempts, attempt)
	}
	if err = nrows.Close(); err != nil {
		return record, err
	}
	nodeEvidenceRows, err := s.db.QueryContext(ctx, `SELECT id,request_id,node_id,activation_id,attempt,issuance_id,reporter_json,kind,artifact_revision,passed,disposition,detail,recorded_at,revision FROM work_node_evidence WHERE work_run_id=? ORDER BY recorded_at,id`, id)
	if err != nil {
		return record, err
	}
	for nodeEvidenceRows.Next() {
		var evidence model.WorkNodeEvidence
		var reporter []byte
		var passed sql.NullBool
		var recordedAt int64
		evidence.Attempt.RunID = id
		if err = nodeEvidenceRows.Scan(&evidence.ID, &evidence.RequestID, &evidence.Attempt.NodeID, &evidence.Attempt.ActivationID, &evidence.Attempt.Attempt, &evidence.Attempt.IssuanceID, &reporter, &evidence.Kind, &evidence.ArtifactRevision, &passed, &evidence.Disposition, &evidence.Detail, &recordedAt, &evidence.Revision); err != nil {
			nodeEvidenceRows.Close()
			return record, err
		}
		if err = json.Unmarshal(reporter, &evidence.Reporter); err != nil {
			nodeEvidenceRows.Close()
			return record, err
		}
		if passed.Valid {
			value := passed.Bool
			evidence.Passed = &value
		}
		evidence.RecordedAt = fromNanos(recordedAt)
		record.NodeEvidence = append(record.NodeEvidence, evidence)
	}
	if err = nodeEvidenceRows.Close(); err != nil {
		return record, err
	}
	erows, err := s.db.QueryContext(ctx, `SELECT id,request_id,work_run_id,step,attempt,kind,reporter_json,artifact_revision,passed,detail,recorded_at,revision FROM work_evidence WHERE work_run_id=? ORDER BY recorded_at`, id)
	if err != nil {
		return record, err
	}
	for erows.Next() {
		var e model.WorkEvidence
		var reporter []byte
		var passed sql.NullBool
		var at int64
		if err := erows.Scan(&e.ID, &e.RequestID, &e.WorkRunID, &e.Step, &e.Attempt, &e.Kind, &reporter, &e.ArtifactRevision, &passed, &e.Detail, &at, &e.Revision); err != nil {
			erows.Close()
			return record, err
		}
		_ = json.Unmarshal(reporter, &e.Reporter)
		if passed.Valid {
			v := passed.Bool
			e.Passed = &v
		}
		e.RecordedAt = fromNanos(at)
		record.Evidence = append(record.Evidence, e)
	}
	erows.Close()
	var d model.WorkDecision
	var decider []byte
	var at int64
	err = s.db.QueryRowContext(ctx, `SELECT work_run_id,request_id,step,attempt,decision,decider_json,reason,decided_at,revision FROM work_decisions WHERE work_run_id=?`, id).Scan(&d.WorkRunID, &d.RequestID, &d.Step, &d.Attempt, &d.Decision, &decider, &d.Reason, &at, &d.Revision)
	if err == nil {
		_ = json.Unmarshal(decider, &d.Decider)
		d.DecidedAt = fromNanos(at)
		record.Decision = &d
	} else if !errors.Is(err, sql.ErrNoRows) {
		return record, err
	}
	decisionRows, err := s.db.QueryContext(ctx, `SELECT id FROM decision_windows WHERE work_run_id=? ORDER BY created_at,id`, id)
	if err != nil {
		return record, err
	}
	var decisionIDs []model.DecisionID
	for decisionRows.Next() {
		var decisionID model.DecisionID
		if err = decisionRows.Scan(&decisionID); err != nil {
			decisionRows.Close()
			return record, err
		}
		decisionIDs = append(decisionIDs, decisionID)
	}
	if err = decisionRows.Close(); err != nil {
		return record, err
	}
	for _, decisionID := range decisionIDs {
		decisionRecord, readErr := s.Decision(ctx, decisionID)
		if readErr != nil {
			return record, readErr
		}
		record.Decisions = append(record.Decisions, decisionRecord.Window)
	}
	return record, nil
}

func (s *Store) WorkRunByRequest(ctx context.Context, principal model.Principal, requestID model.RequestID) (app.WorkRunRecord, error) {
	var id model.WorkRunID
	err := s.db.QueryRowContext(ctx, `SELECT id FROM work_runs WHERE request_scope=? AND request_id=?`, requestScope(principal), requestID).Scan(&id)
	if err != nil {
		return app.WorkRunRecord{}, classify(err)
	}
	return s.WorkRun(ctx, id)
}

func (s *Store) PendingWorkRuns(ctx context.Context) ([]app.WorkRunRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM work_runs WHERE state IN (?,?,?,?) OR (state IN (?,?) AND control_state=?) ORDER BY created_at`, model.WorkRunPending, model.WorkRunRunning, model.WorkRunWaiting, model.WorkRunUncertain, model.WorkRunFailed, model.WorkRunCancelled, model.WorkControlDraining)
	if err != nil {
		return nil, err
	}
	var ids []model.WorkRunID
	for rows.Next() {
		var id model.WorkRunID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	out := make([]app.WorkRunRecord, 0, len(ids))
	for _, id := range ids {
		record, err := s.WorkRun(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, nil
}

func (s *Store) RecordWorkProgress(ctx context.Context, progress app.WorkProgress) (app.WorkRunRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.WorkRunRecord{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var settled any
	if progress.AttemptState == model.WorkAttemptSucceeded || progress.AttemptState == model.WorkAttemptFailed || progress.AttemptState == model.WorkAttemptUncertain {
		settled = nanos(progress.At)
	}
	result, err := tx.ExecContext(ctx, `UPDATE work_attempts SET operation_id=?,state=?,detail=?,settled_at=? WHERE work_run_id=? AND step=? AND attempt=? AND state IN (?,?)`, progress.OperationID, progress.AttemptState, progress.Detail, settled, progress.WorkRunID, progress.Step, progress.Attempt, model.WorkAttemptPending, model.WorkAttemptRunning)
	if err != nil {
		return app.WorkRunRecord{}, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return app.WorkRunRecord{}, app.ErrConflict
	}
	result, err = tx.ExecContext(ctx, `UPDATE work_runs SET state=?,worker_execution_id=CASE WHEN ?='' THEN worker_execution_id ELSE ? END,revision=revision+1,updated_at=? WHERE id=? AND revision=?`, progress.RunState, progress.WorkerExecutionID, progress.WorkerExecutionID, nanos(progress.At), progress.WorkRunID, progress.ExpectedRevision)
	if err != nil {
		return app.WorkRunRecord{}, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return app.WorkRunRecord{}, app.ErrConflict
	}
	if progress.WorkerExecutionID != "" {
		if _, err = tx.ExecContext(ctx, `UPDATE workspace_uses SET execution_id=? WHERE work_run_id=? AND released_at IS NULL`, progress.WorkerExecutionID, progress.WorkRunID); err != nil {
			return app.WorkRunRecord{}, err
		}
	}
	if progress.Step == model.WorkStepLaunchWorker {
		claimState := model.HistoryUseReleased
		var settledAt any = nanos(progress.At)
		if progress.AttemptState == model.WorkAttemptUncertain {
			claimState = model.HistoryUseUncertain
			settledAt = nil
		}
		if _, err = tx.ExecContext(ctx, `UPDATE history_use_claims SET state=?,revision=revision+1,settled_at=? WHERE work_run_id=? AND state=?`, claimState, settledAt, progress.WorkRunID, model.HistoryUseHeld); err != nil {
			return app.WorkRunRecord{}, err
		}
	}
	if err = bumpTx(ctx, tx); err != nil {
		return app.WorkRunRecord{}, err
	}
	if err = tx.Commit(); err != nil {
		return app.WorkRunRecord{}, err
	}
	return s.WorkRun(ctx, progress.WorkRunID)
}

func (s *Store) RecordWorkEvidence(ctx context.Context, e model.WorkEvidence, expected model.Revision, authority model.AuthorityRequest, at time.Time) (app.WorkRunRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.WorkRunRecord{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var existingRun model.WorkRunID
	var existingStep model.WorkStep
	var existingAttempt uint64
	var existingKind model.WorkEvidenceKind
	var existingArtifact, existingDetail string
	var existingPassed sql.NullBool
	err = tx.QueryRowContext(ctx, `SELECT work_run_id,step,attempt,kind,artifact_revision,passed,detail FROM work_evidence WHERE request_scope=? AND request_id=?`, requestScope(e.Reporter), e.RequestID).Scan(&existingRun, &existingStep, &existingAttempt, &existingKind, &existingArtifact, &existingPassed, &existingDetail)
	if err == nil {
		passedMatches := (e.Passed == nil && !existingPassed.Valid) || (e.Passed != nil && existingPassed.Valid && *e.Passed == existingPassed.Bool)
		if existingRun != e.WorkRunID || existingStep != e.Step || existingAttempt != e.Attempt || existingKind != e.Kind || existingArtifact != e.ArtifactRevision || !passedMatches || existingDetail != e.Detail {
			return app.WorkRunRecord{}, app.ErrConflict
		}
		if err = tx.Commit(); err != nil {
			return app.WorkRunRecord{}, err
		}
		return s.WorkRun(ctx, e.WorkRunID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return app.WorkRunRecord{}, err
	}
	decision, err := authorizeTx(ctx, tx, authority, at)
	if err != nil || !decision.Allowed {
		if err == nil {
			err = app.ErrUnauthorized
		}
		return app.WorkRunRecord{}, err
	}
	reporter, _ := json.Marshal(e.Reporter)
	var passed any
	if e.Passed != nil {
		passed = *e.Passed
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO work_evidence(id,request_scope,request_id,work_run_id,step,attempt,kind,reporter_json,artifact_revision,passed,detail,recorded_at,revision) SELECT ?,?,?,?,?,?,?,?,?,?,?,?,1 WHERE EXISTS(SELECT 1 FROM work_runs r JOIN work_attempts a ON a.work_run_id=r.id WHERE r.id=? AND r.revision=? AND r.state=? AND a.step=? AND a.attempt=? AND a.step=? AND a.state=?)`, e.ID, requestScope(e.Reporter), e.RequestID, e.WorkRunID, e.Step, e.Attempt, e.Kind, reporter, e.ArtifactRevision, passed, e.Detail, nanos(e.RecordedAt), e.WorkRunID, expected, model.WorkRunWaiting, e.Step, e.Attempt, model.WorkStepAwaitEvidence, model.WorkAttemptPending)
	if err != nil {
		return app.WorkRunRecord{}, classify(err)
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return app.WorkRunRecord{}, app.ErrConflict
	}
	result, err = tx.ExecContext(ctx, `UPDATE work_runs SET state=?,revision=revision+1,updated_at=? WHERE id=? AND revision=?`, model.WorkRunWaiting, nanos(at), e.WorkRunID, expected)
	if err != nil {
		return app.WorkRunRecord{}, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return app.WorkRunRecord{}, app.ErrConflict
	}
	if err = bumpTx(ctx, tx); err != nil {
		return app.WorkRunRecord{}, err
	}
	if err = tx.Commit(); err != nil {
		return app.WorkRunRecord{}, err
	}
	return s.WorkRun(ctx, e.WorkRunID)
}

func (s *Store) DecideWork(ctx context.Context, d model.WorkDecision, expected model.Revision, authority model.AuthorityRequest, at time.Time) (app.WorkRunRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.WorkRunRecord{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var existingRun model.WorkRunID
	var existingDecision model.WorkDecisionKind
	var existingStep model.WorkStep
	var existingAttempt uint64
	var existingReason string
	err = tx.QueryRowContext(ctx, `SELECT work_run_id,step,attempt,decision,reason FROM work_decisions WHERE request_scope=? AND request_id=?`, requestScope(d.Decider), d.RequestID).Scan(&existingRun, &existingStep, &existingAttempt, &existingDecision, &existingReason)
	if err == nil {
		if existingRun != d.WorkRunID || existingStep != d.Step || existingAttempt != d.Attempt || existingDecision != d.Decision || existingReason != d.Reason {
			return app.WorkRunRecord{}, app.ErrConflict
		}
		if err = tx.Commit(); err != nil {
			return app.WorkRunRecord{}, err
		}
		return s.WorkRun(ctx, d.WorkRunID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return app.WorkRunRecord{}, err
	}
	decision, err := authorizeTx(ctx, tx, authority, at)
	if err != nil || !decision.Allowed {
		if err == nil {
			err = app.ErrUnauthorized
		}
		return app.WorkRunRecord{}, err
	}
	state := model.WorkRunFailed
	switch d.Decision {
	case model.WorkDecisionAccept:
		state = model.WorkRunSucceeded
	case model.WorkDecisionCancel:
		state = model.WorkRunCancelled
	}
	decider, _ := json.Marshal(d.Decider)
	_, err = tx.ExecContext(ctx, `INSERT INTO work_decisions(work_run_id,request_scope,request_id,step,attempt,decision,decider_json,reason,decided_at,revision) VALUES(?,?,?,?,?,?,?,?,?,1)`, d.WorkRunID, requestScope(d.Decider), d.RequestID, d.Step, d.Attempt, d.Decision, decider, d.Reason, nanos(d.DecidedAt))
	if err != nil {
		return app.WorkRunRecord{}, classify(err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE work_runs SET state=?,revision=revision+1,updated_at=? WHERE id=? AND revision=? AND state=?`, state, nanos(at), d.WorkRunID, expected, model.WorkRunWaiting)
	if err != nil {
		return app.WorkRunRecord{}, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return app.WorkRunRecord{}, app.ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE workspace_uses SET released_at=? WHERE work_run_id=? AND released_at IS NULL AND (execution_id='' OR EXISTS(SELECT 1 FROM executions e WHERE e.id=workspace_uses.execution_id AND e.state IN (?,?)))`, nanos(at), d.WorkRunID, model.ExecutionExited, model.ExecutionFailed); err != nil {
		return app.WorkRunRecord{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE history_use_claims SET state=?,revision=revision+1,settled_at=? WHERE work_run_id=? AND state=?`, model.HistoryUseReleased, nanos(at), d.WorkRunID, model.HistoryUseHeld); err != nil {
		return app.WorkRunRecord{}, err
	}
	if err = bumpTx(ctx, tx); err != nil {
		return app.WorkRunRecord{}, err
	}
	if err = tx.Commit(); err != nil {
		return app.WorkRunRecord{}, err
	}
	return s.WorkRun(ctx, d.WorkRunID)
}

func (s *Store) CancelWork(ctx context.Context, id model.WorkRunID, expected model.Revision, reason string, requestID model.RequestID, authority model.AuthorityRequest, at time.Time) (app.WorkRunRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.WorkRunRecord{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var existingRun model.WorkRunID
	var existingReason string
	err = tx.QueryRowContext(ctx, `SELECT id,cancellation_reason FROM work_runs WHERE cancel_request_scope=? AND cancel_request_id=?`, requestScope(authority.Principal), requestID).Scan(&existingRun, &existingReason)
	if err == nil {
		if existingRun != id || existingReason != reason {
			return app.WorkRunRecord{}, app.ErrConflict
		}
		if err = tx.Commit(); err != nil {
			return app.WorkRunRecord{}, err
		}
		return s.WorkRun(ctx, id)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return app.WorkRunRecord{}, err
	}
	decision, err := authorizeTx(ctx, tx, authority, at)
	if err != nil || !decision.Allowed {
		if err == nil {
			err = app.ErrUnauthorized
		}
		return app.WorkRunRecord{}, err
	}
	var priorState model.WorkRunState
	if err = tx.QueryRowContext(ctx, `SELECT state FROM work_runs WHERE id=? AND revision=?`, id, expected).Scan(&priorState); err != nil {
		return app.WorkRunRecord{}, classify(err)
	}
	nextState := model.WorkRunCancelled
	if priorState == model.WorkRunUncertain {
		nextState = model.WorkRunUncertain
	} else {
		decider, _ := json.Marshal(authority.Principal)
		if _, err = tx.ExecContext(ctx, `INSERT INTO work_decisions(work_run_id,request_scope,request_id,step,attempt,decision,decider_json,reason,decided_at,revision) VALUES(?,?,?,?,?,?,?,?,?,1)`, id, requestScope(authority.Principal), requestID, model.WorkStepEvaluate, 1, model.WorkDecisionCancel, decider, reason, nanos(at)); err != nil {
			return app.WorkRunRecord{}, classify(err)
		}
	}
	controlState := model.WorkControlSettled
	var activeGraphAttempts int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_node_attempts WHERE work_run_id=? AND state IN (?,?,?)`, id, model.NodeAttemptAdmitted, model.NodeAttemptRunning, model.NodeAttemptUncertain).Scan(&activeGraphAttempts); err != nil {
		return app.WorkRunRecord{}, err
	}
	if activeGraphAttempts > 0 || priorState == model.WorkRunUncertain {
		controlState = model.WorkControlDraining
	}
	result, err := tx.ExecContext(ctx, `UPDATE work_runs SET state=?,control_state=?,outcome=?,cancellation_requested=1,cancellation_reason=?,cancel_request_scope=?,cancel_request_id=?,revision=revision+1,updated_at=? WHERE id=? AND revision=? AND state IN (?,?,?,?)`, nextState, controlState, model.WorkOutcomeCancelled, reason, requestScope(authority.Principal), requestID, nanos(at), id, expected, model.WorkRunPending, model.WorkRunRunning, model.WorkRunWaiting, model.WorkRunUncertain)
	if err != nil {
		return app.WorkRunRecord{}, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return app.WorkRunRecord{}, app.ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE work_node_attempts SET state=?,outcome=?,detail=?,updated_at=?,settled_at=? WHERE work_run_id=? AND state IN (?,?,?,?)`, model.NodeAttemptSuppressed, model.WorkOutcomeCancelled, "suppressed by cancellation", nanos(at), nanos(at), id, model.NodeAttemptReady, model.NodeAttemptRetryWait, model.NodeAttemptBlocked, model.NodeAttemptWaiting); err != nil {
		return app.WorkRunRecord{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE decision_windows SET state=?,revision=revision+1,updated_at=? WHERE work_run_id=? AND state=?`, model.DecisionExpired, nanos(at), id, model.DecisionOpen); err != nil {
		return app.WorkRunRecord{}, err
	}
	if priorState != model.WorkRunUncertain {
		if _, err = tx.ExecContext(ctx, `UPDATE workspace_uses SET released_at=? WHERE work_run_id=? AND released_at IS NULL AND (execution_id='' OR EXISTS(SELECT 1 FROM executions e WHERE e.id=workspace_uses.execution_id AND e.state IN (?,?)))`, nanos(at), id, model.ExecutionExited, model.ExecutionFailed); err != nil {
			return app.WorkRunRecord{}, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE history_use_claims SET state=?,revision=revision+1,settled_at=? WHERE work_run_id=? AND state=?`, model.HistoryUseReleased, nanos(at), id, model.HistoryUseHeld); err != nil {
			return app.WorkRunRecord{}, err
		}
	}
	if err = bumpTx(ctx, tx); err != nil {
		return app.WorkRunRecord{}, err
	}
	if err = tx.Commit(); err != nil {
		return app.WorkRunRecord{}, err
	}
	return s.WorkRun(ctx, id)
}

func (s *Store) ResolveWorkUncertainty(ctx context.Context, d model.WorkDecision, expected model.Revision, authority model.AuthorityRequest, at time.Time) (app.WorkRunRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.WorkRunRecord{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var existingRun model.WorkRunID
	var existingReason string
	err = tx.QueryRowContext(ctx, `SELECT work_run_id,reason FROM work_decisions WHERE request_scope=? AND request_id=?`, requestScope(d.Decider), d.RequestID).Scan(&existingRun, &existingReason)
	if err == nil {
		if existingRun != d.WorkRunID || existingReason != d.Reason {
			return app.WorkRunRecord{}, app.ErrConflict
		}
		if err = tx.Commit(); err != nil {
			return app.WorkRunRecord{}, err
		}
		return s.WorkRun(ctx, d.WorkRunID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return app.WorkRunRecord{}, err
	}
	decision, err := authorizeTx(ctx, tx, authority, at)
	if err != nil {
		return app.WorkRunRecord{}, err
	}
	if !decision.Allowed {
		return app.WorkRunRecord{}, app.ErrUnauthorized
	}
	decider, _ := json.Marshal(d.Decider)
	if _, err = tx.ExecContext(ctx, `INSERT INTO work_decisions(work_run_id,request_scope,request_id,step,attempt,decision,decider_json,reason,decided_at,revision) VALUES(?,?,?,?,?,?,?,?,?,1)`, d.WorkRunID, requestScope(d.Decider), d.RequestID, d.Step, d.Attempt, d.Decision, decider, d.Reason, nanos(d.DecidedAt)); err != nil {
		return app.WorkRunRecord{}, classify(err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE work_runs SET state=?,revision=revision+1,updated_at=? WHERE id=? AND revision=? AND state=?`, model.WorkRunFailed, nanos(at), d.WorkRunID, expected, model.WorkRunUncertain)
	if err != nil {
		return app.WorkRunRecord{}, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return app.WorkRunRecord{}, app.ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE workspace_uses SET released_at=? WHERE work_run_id=? AND released_at IS NULL`, nanos(at), d.WorkRunID); err != nil {
		return app.WorkRunRecord{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE history_use_claims SET state=?,revision=revision+1,settled_at=? WHERE work_run_id=? AND state IN (?,?)`, model.HistoryUseReleased, nanos(at), d.WorkRunID, model.HistoryUseHeld, model.HistoryUseUncertain); err != nil {
		return app.WorkRunRecord{}, err
	}
	if err = bumpTx(ctx, tx); err != nil {
		return app.WorkRunRecord{}, err
	}
	if err = tx.Commit(); err != nil {
		return app.WorkRunRecord{}, err
	}
	return s.WorkRun(ctx, d.WorkRunID)
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func (s *Store) AdmitShell(ctx context.Context, in app.ShellAdmission) (app.AdmissionResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.AdmissionResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if in.Request != nil {
		if prior, found, err := findShellAdmission(ctx, tx, *in.Request, in.Operation.CreatedAt); err != nil || found {
			return prior, err
		}
	}
	decision, err := authorizeTx(ctx, tx, in.Authority, in.Operation.CreatedAt)
	if err != nil {
		return app.AdmissionResult{}, err
	}
	if !decision.Allowed {
		return app.AdmissionResult{}, app.ErrUnauthorized
	}
	if repeated, ok, err := admissionByRequest(ctx, tx, in.Operation, "", false); err != nil {
		return app.AdmissionResult{}, err
	} else if ok {
		if repeated.Execution.Workload != model.ExecutionWorkloadShell || repeated.Execution.Spec.WorkingDirectory != in.Execution.Spec.WorkingDirectory || repeated.Execution.Spec.Sandbox != in.Execution.Spec.Sandbox || !model.SameSandboxSelection(repeated.Execution.Spec.HostSandbox, in.Execution.Spec.HostSandbox) || !repeated.Execution.Spec.Environment.Equal(in.Execution.Spec.Environment) || !reflect.DeepEqual(repeated.Execution.Spec.ShellGroup, in.Execution.Spec.ShellGroup) {
			return app.AdmissionResult{}, app.ErrConflict
		}
		var workspaceID model.WorkspaceID
		if err := tx.QueryRowContext(ctx, `SELECT workspace_id FROM workspace_uses WHERE execution_id=?`, repeated.Execution.ID).Scan(&workspaceID); err != nil {
			return app.AdmissionResult{}, classify(err)
		}
		if workspaceID != in.WorkspaceUse.WorkspaceID {
			return app.AdmissionResult{}, app.ErrConflict
		}
		_ = tx.Commit()
		return repeated, nil
	}
	if err := validateShellRequest(ctx, tx, in); err != nil {
		return app.AdmissionResult{}, err
	}
	var revision model.Revision
	var state model.WorkspaceState
	if err := tx.QueryRowContext(ctx, `SELECT revision,state FROM workspaces WHERE id=?`, in.WorkspaceUse.WorkspaceID).Scan(&revision, &state); err != nil {
		return app.AdmissionResult{}, classify(err)
	}
	if state != model.WorkspaceAvailable || revision != in.WorkspaceRevision {
		return app.AdmissionResult{}, app.ErrConflict
	}
	if err := insertExecution(ctx, tx, in.Execution); err != nil {
		return app.AdmissionResult{}, err
	}
	if err := insertOperation(ctx, tx, in.Operation); err != nil {
		return app.AdmissionResult{}, err
	}
	if err := insertOperationAuthority(ctx, tx, in.Operation.ID, in.Authority, decision); err != nil {
		return app.AdmissionResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO workspace_uses(id,workspace_id,execution_id,created_at) VALUES(?,?,?,?)`, in.WorkspaceUse.ID, in.WorkspaceUse.WorkspaceID, in.Execution.ID, nanos(in.WorkspaceUse.CreatedAt)); err != nil {
		return app.AdmissionResult{}, classify(err)
	}
	if in.Request != nil {
		if _, err := tx.ExecContext(ctx, `INSERT INTO shell_requests(scope,request_id,operation_id,intent) VALUES(?,?,?,?)`, requestScope(in.Operation.Principal), in.Operation.RequestID, in.Operation.ID, shellIntent(*in.Request)); err != nil {
			return app.AdmissionResult{}, classify(err)
		}
	}
	if err := bumpTx(ctx, tx); err != nil {
		return app.AdmissionResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return app.AdmissionResult{}, err
	}
	return app.AdmissionResult{Operation: in.Operation, Execution: in.Execution}, nil
}

func (s *Store) RecordShellPrepared(ctx context.Context, executionID model.ExecutionID, operationID model.OperationID, evidence ports.ShellResourceEvidence, at time.Time) (model.Execution, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Execution{}, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE executions SET state=?,shell_evidence_owner=?,shell_evidence_version=?,shell_evidence_payload=?,revision=revision+1,updated_at=? WHERE id=? AND workload_kind=? AND state=?`, model.ExecutionPrepared, evidence.Owner, evidence.Version, evidence.Payload, nanos(at), executionID, model.ExecutionWorkloadShell, model.ExecutionReserved)
	if err != nil {
		return model.Execution{}, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return model.Execution{}, app.ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO release_permits(execution_id,operation_id) VALUES(?,?)`, executionID, operationID); err != nil {
		return model.Execution{}, classify(err)
	}
	if err = bumpTx(ctx, tx); err != nil {
		return model.Execution{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.Execution{}, err
	}
	return s.Execution(ctx, executionID)
}

func (s *Store) CompleteShell(ctx context.Context, in app.OperationCompletion, evidence ports.ShellResourceEvidence) (app.AdmissionResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.AdmissionResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := completeOperationTx(ctx, tx, in); err != nil {
		return app.AdmissionResult{}, err
	}
	if evidence.Owner != "" {
		if _, err = tx.ExecContext(ctx, `UPDATE executions SET shell_evidence_owner=?,shell_evidence_version=?,shell_evidence_payload=? WHERE id=?`, evidence.Owner, evidence.Version, evidence.Payload, in.ExecutionID); err != nil {
			return app.AdmissionResult{}, err
		}
	}
	if in.UpdateExecutionState && (in.ExecutionState == model.ExecutionExited || in.ExecutionState == model.ExecutionFailed) {
		if _, err = tx.ExecContext(ctx, `UPDATE workspace_uses SET released_at=? WHERE execution_id=? AND released_at IS NULL`, nanos(in.At), in.ExecutionID); err != nil {
			return app.AdmissionResult{}, err
		}
	}
	if err = bumpTx(ctx, tx); err != nil {
		return app.AdmissionResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return app.AdmissionResult{}, err
	}
	return s.OperationResult(ctx, in.OperationID)
}

func (s *Store) ShellRecovery(ctx context.Context, executionID model.ExecutionID) (app.ShellRecoveryRecord, error) {
	var record app.ShellRecoveryRecord
	err := s.db.QueryRowContext(ctx, `SELECT u.workspace_id,e.shell_evidence_owner,e.shell_evidence_version,e.shell_evidence_payload FROM executions e JOIN workspace_uses u ON u.execution_id=e.id AND u.released_at IS NULL WHERE e.id=? AND e.workload_kind=?`, executionID, model.ExecutionWorkloadShell).Scan(&record.WorkspaceID, &record.Evidence.Owner, &record.Evidence.Version, &record.Evidence.Payload)
	return record, classify(err)
}

func (s *Store) RecordShellRecovery(ctx context.Context, executionID model.ExecutionID, state model.ExecutionState, evidence ports.ShellResourceEvidence, at time.Time) (model.Execution, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Execution{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var current model.ExecutionState
	if err = tx.QueryRowContext(ctx, `SELECT state FROM executions WHERE id=? AND workload_kind=?`, executionID, model.ExecutionWorkloadShell).Scan(&current); err != nil {
		return model.Execution{}, classify(err)
	}
	if current == model.ExecutionExited || current == model.ExecutionFailed {
		state = current
		evidence = ports.ShellResourceEvidence{}
	}

	result, err := tx.ExecContext(ctx, `UPDATE executions SET state=?,shell_evidence_owner=CASE WHEN ?='' THEN shell_evidence_owner ELSE ? END,shell_evidence_version=CASE WHEN ?='' THEN shell_evidence_version ELSE ? END,shell_evidence_payload=CASE WHEN ?='' THEN shell_evidence_payload ELSE ? END,revision=revision+1,updated_at=? WHERE id=? AND workload_kind=?`, state, evidence.Owner, evidence.Owner, evidence.Owner, evidence.Version, evidence.Owner, evidence.Payload, nanos(at), executionID, model.ExecutionWorkloadShell)
	if err != nil {
		return model.Execution{}, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return model.Execution{}, app.ErrConflict
	}
	if state == model.ExecutionExited || state == model.ExecutionFailed {
		if _, err = tx.ExecContext(ctx, `UPDATE execution_accesses SET state=?,revoked_at=?,revision=revision+1 WHERE execution_id=? AND state NOT IN (?,?)`, model.ExecutionAccessRevoked, nanos(at), executionID, model.ExecutionAccessRevoked, model.ExecutionAccessExpired); err != nil {
			return model.Execution{}, err
		}

		if _, err = tx.ExecContext(ctx, `UPDATE workspace_uses SET released_at=? WHERE execution_id=? AND released_at IS NULL`, nanos(at), executionID); err != nil {
			return model.Execution{}, err
		}
	}
	if err = bumpTx(ctx, tx); err != nil {
		return model.Execution{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.Execution{}, err
	}
	return s.Execution(ctx, executionID)
}

// Inspection may refresh an observation while a removal is still running;
// the durable operation, not that observation, owns the exclusion interval.
func requireNoWorkspaceRemovalTx(ctx context.Context, tx *sql.Tx, id model.WorkspaceID) error {
	var pending int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM operations o JOIN operation_authority a ON a.operation_id=o.id WHERE a.resource_kind=? AND a.resource_id=? AND o.kind=? AND o.state IN (?,?)`, model.ResourceWorkspace, id, model.OperationRemoveWorkspace, model.OperationAdmitted, model.OperationRunning).Scan(&pending)
	if err != nil {
		return err
	}
	if pending != 0 {
		return app.ErrConflict
	}
	return nil
}
