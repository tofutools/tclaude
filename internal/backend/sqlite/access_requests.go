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
)

func (s *Store) initializeAccessRequests(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS access_requests (
  id TEXT PRIMARY KEY, decision_id TEXT NOT NULL UNIQUE,
  request_scope TEXT NOT NULL, request_id TEXT NOT NULL,
  requester_kind TEXT NOT NULL, requester_agent_id TEXT NOT NULL DEFAULT '',
  requester_execution_id TEXT NOT NULL, requester_generation INTEGER NOT NULL,
  subject_kind TEXT NOT NULL, subject_id TEXT NOT NULL,
  action TEXT NOT NULL, resource_kind TEXT NOT NULL, resource_id TEXT NOT NULL DEFAULT '',
  requested_configuration_json BLOB, bounds_json BLOB NOT NULL, reason TEXT NOT NULL,
  requested_lifetime INTEGER NOT NULL, expires_at INTEGER NOT NULL, state TEXT NOT NULL,
  grant_id TEXT NOT NULL UNIQUE, revision INTEGER NOT NULL,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
  UNIQUE(request_scope, request_id)
);
CREATE INDEX IF NOT EXISTS access_requests_subject
  ON access_requests(subject_kind, subject_id, created_at, id);
CREATE TABLE IF NOT EXISTS access_request_decisions (
  decision_id TEXT PRIMARY KEY REFERENCES access_requests(decision_id),
  request_scope TEXT NOT NULL, request_id TEXT NOT NULL,
  expected_window_revision INTEGER NOT NULL, answer TEXT NOT NULL, reason TEXT NOT NULL,
  actor_kind TEXT NOT NULL, actor_agent_id TEXT NOT NULL DEFAULT '',
  actor_execution_id TEXT NOT NULL DEFAULT '', actor_automation_run TEXT NOT NULL DEFAULT '',
  submitted_at INTEGER NOT NULL,
  UNIQUE(request_scope, request_id)
);`)
	return err
}

func (s *Store) CreateAccessRequest(ctx context.Context, request model.AccessRequest, at time.Time) (app.AccessRequestResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.AccessRequestResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err = expireAccessRequestsTx(ctx, tx, at); err != nil {
		return app.AccessRequestResult{}, err
	}
	var priorID model.AccessRequestID
	err = tx.QueryRowContext(ctx, `SELECT id FROM access_requests WHERE request_scope=? AND request_id=?`, requestScope(request.Requester), request.RequestID).Scan(&priorID)
	if err == nil {
		prior, readErr := accessRequestRecord(ctx, tx, priorID)
		if readErr != nil {
			return app.AccessRequestResult{}, readErr
		}
		if !sameAccessRequestIntent(prior.Request, request) {
			return app.AccessRequestResult{}, app.ErrConflict
		}
		if err = tx.Commit(); err != nil {
			return app.AccessRequestResult{}, err
		}
		prior.Repeated = true
		return prior, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return app.AccessRequestResult{}, err
	}
	if request.Requester.Kind != model.PrincipalExecution || request.RequestedLifetime < time.Second || request.RequestedLifetime > app.MaxAccessRequestLifetime || !request.ExpiresAt.After(at) {
		return app.AccessRequestResult{}, app.ErrInvalid
	}
	if err = request.ID.Validate(); err != nil {
		return app.AccessRequestResult{}, app.ErrInvalid
	}
	if err = request.DecisionID.Validate(); err != nil {
		return app.AccessRequestResult{}, app.ErrInvalid
	}
	if err = request.GrantID.Validate(); err != nil {
		return app.AccessRequestResult{}, app.ErrInvalid
	}
	subject, err := authoritySubject(ctx, tx, request.Requester, at)
	if err != nil {
		return app.AccessRequestResult{}, err
	}
	request.Subject = subject
	if !validResourceSelector(request.Resource) || app.AccessRequestConfigurationRequired(request.Action) && request.RequestedConfiguration == nil || !configurationMatches(request.Bounds, request.RequestedConfiguration) {
		return app.AccessRequestResult{}, app.ErrInvalid
	}
	grant := accessGrant(request, at)
	if err = app.ValidateAuthorityGrant(grant); err != nil {
		return app.AccessRequestResult{}, err
	}
	decision, err := authorizeTx(ctx, tx, model.AuthorityRequest{Principal: request.Requester, Action: request.Action, Resource: request.Resource, RequestedConfiguration: request.RequestedConfiguration}, at)
	if err != nil {
		return app.AccessRequestResult{}, err
	}
	if decision.Allowed {
		return app.AccessRequestResult{}, app.ErrConflict
	}
	requestedConfiguration, err := optionalDesiredConfigurationJSON(request.RequestedConfiguration)
	if err != nil {
		return app.AccessRequestResult{}, err
	}
	bounds, err := json.Marshal(request.Bounds)
	if err != nil {
		return app.AccessRequestResult{}, err
	}
	sk, sid := subjectParts(request.Subject)
	rk, rid := resourceParts(request.Resource)
	_, err = tx.ExecContext(ctx, `INSERT INTO access_requests(
id,decision_id,request_scope,request_id,requester_kind,requester_agent_id,requester_execution_id,requester_generation,
subject_kind,subject_id,action,resource_kind,resource_id,requested_configuration_json,bounds_json,reason,
requested_lifetime,expires_at,state,grant_id,revision,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		request.ID, request.DecisionID, requestScope(request.Requester), request.RequestID,
		request.Requester.Kind, request.Requester.AgentID, request.Requester.ExecutionID, request.Requester.Generation,
		sk, sid, request.Action, rk, rid, requestedConfiguration, bounds, request.Reason,
		int64(request.RequestedLifetime), nanos(request.ExpiresAt), request.State, request.GrantID,
		request.Revision, nanos(request.CreatedAt), nanos(request.UpdatedAt))
	if err != nil {
		return app.AccessRequestResult{}, classify(err)
	}
	if err = bumpTx(ctx, tx); err != nil {
		return app.AccessRequestResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return app.AccessRequestResult{}, err
	}
	return s.AccessRequest(ctx, request.ID, model.OperatorPrincipal(), at)
}

func (s *Store) ListAccessRequests(ctx context.Context, principal model.Principal, pendingOnly bool, at time.Time) ([]app.AccessRequestResult, error) {
	if principal.Kind != model.PrincipalOperator && principal.Kind != model.PrincipalExecution {
		return nil, app.ErrUnauthorized
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err = expireAccessRequestsTx(ctx, tx, at); err != nil {
		return nil, err
	}
	query := `SELECT id FROM access_requests`
	var args []any
	if principal.Kind != model.PrincipalOperator {
		subject, subjectErr := authoritySubject(ctx, tx, principal, at)
		if subjectErr != nil {
			return nil, subjectErr
		}
		sk, sid := subjectParts(subject)
		query += ` WHERE subject_kind=? AND subject_id=?`
		args = append(args, sk, sid)
	}
	if pendingOnly {
		if len(args) == 0 {
			query += ` WHERE`
		} else {
			query += ` AND`
		}
		query += ` state=?`
		args = append(args, model.AccessRequestPending)
	}
	query += ` ORDER BY created_at,id`
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var ids []model.AccessRequestID
	for rows.Next() {
		var id model.AccessRequestID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	result := make([]app.AccessRequestResult, 0, len(ids))
	for _, id := range ids {
		record, readErr := accessRequestRecord(ctx, tx, id)
		if readErr != nil {
			return nil, readErr
		}
		result = append(result, record)
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) AccessRequest(ctx context.Context, id model.AccessRequestID, principal model.Principal, at time.Time) (app.AccessRequestResult, error) {
	if principal.Kind != model.PrincipalOperator && principal.Kind != model.PrincipalExecution {
		return app.AccessRequestResult{}, app.ErrUnauthorized
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.AccessRequestResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err = expireAccessRequestsTx(ctx, tx, at); err != nil {
		return app.AccessRequestResult{}, err
	}
	record, err := accessRequestRecord(ctx, tx, id)
	if err != nil {
		return app.AccessRequestResult{}, err
	}
	if principal.Kind != model.PrincipalOperator {
		subject, subjectErr := authoritySubject(ctx, tx, principal, at)
		if subjectErr != nil {
			return app.AccessRequestResult{}, subjectErr
		}
		if subject != record.Request.Subject {
			return app.AccessRequestResult{}, app.ErrNotFound
		}
	}
	if err = tx.Commit(); err != nil {
		return app.AccessRequestResult{}, err
	}
	return record, nil
}

func (s *Store) DecideAccessRequest(ctx context.Context, submission model.AccessDecisionSubmission, at time.Time) (app.AccessRequestResult, error) {
	if submission.Actor.Kind != model.PrincipalOperator {
		return app.AccessRequestResult{}, app.ErrUnauthorized
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.AccessRequestResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var priorDecision model.DecisionID
	var priorAnswer, priorReason string
	err = tx.QueryRowContext(ctx, `SELECT decision_id,answer,reason FROM access_request_decisions WHERE request_scope=? AND request_id=?`, requestScope(submission.Actor), submission.RequestID).Scan(&priorDecision, &priorAnswer, &priorReason)
	if err == nil {
		if priorDecision != submission.DecisionID || priorAnswer != submission.Answer || priorReason != submission.Reason {
			return app.AccessRequestResult{}, app.ErrConflict
		}
		record, readErr := accessRequestRecordByDecision(ctx, tx, submission.DecisionID)
		if readErr != nil {
			return app.AccessRequestResult{}, readErr
		}
		if err = tx.Commit(); err != nil {
			return app.AccessRequestResult{}, err
		}
		record.Repeated = true
		return record, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return app.AccessRequestResult{}, err
	}
	record, err := accessRequestRecordByDecision(ctx, tx, submission.DecisionID)
	if err != nil {
		return app.AccessRequestResult{}, err
	}
	request := record.Request
	if request.State != model.AccessRequestPending || request.Revision != submission.ExpectedWindowRevision {
		return app.AccessRequestResult{}, app.ErrConflict
	}
	if !request.ExpiresAt.After(at) {
		if err = expireAccessRequestTx(ctx, tx, request.ID, at); err != nil {
			return app.AccessRequestResult{}, err
		}
		if err = tx.Commit(); err != nil {
			return app.AccessRequestResult{}, err
		}
		return app.AccessRequestResult{}, app.ErrConflict
	}
	if !accessRequestSubjectCurrent(ctx, tx, request, at) {
		return app.AccessRequestResult{}, app.ErrConflict
	}
	state := model.AccessRequestDenied
	if submission.Answer == model.AccessAnswerApprove {
		grant := accessGrant(request, at)
		if err = app.ValidateAuthorityGrant(grant); err != nil || !validResourceSelector(grant.Resource) || !configurationMatches(grant.Bounds, request.RequestedConfiguration) {
			return app.AccessRequestResult{}, app.ErrInvalid
		}
		bounds, marshalErr := json.Marshal(grant.Bounds)
		if marshalErr != nil {
			return app.AccessRequestResult{}, marshalErr
		}
		sk, sid := subjectParts(grant.Subject)
		rk, rid := resourceParts(grant.Resource)
		_, err = tx.ExecContext(ctx, `INSERT INTO authority_grants(id,subject_kind,subject_id,action,resource_kind,resource_id,bounds_json,expires_at,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,1,?,?)`, grant.ID, sk, sid, grant.Action, rk, rid, bounds, nanos(*grant.ExpiresAt), nanos(grant.CreatedAt), nanos(grant.UpdatedAt))
		if err != nil {
			return app.AccessRequestResult{}, classify(err)
		}
		state = model.AccessRequestApproved
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO access_request_decisions(decision_id,request_scope,request_id,expected_window_revision,answer,reason,actor_kind,actor_agent_id,actor_execution_id,actor_automation_run,submitted_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, submission.DecisionID, requestScope(submission.Actor), submission.RequestID, submission.ExpectedWindowRevision, submission.Answer, submission.Reason, submission.Actor.Kind, submission.Actor.AgentID, submission.Actor.ExecutionID, submission.Actor.AutomationRun, nanos(submission.SubmittedAt))
	if err != nil {
		return app.AccessRequestResult{}, classify(err)
	}
	updated, err := tx.ExecContext(ctx, `UPDATE access_requests SET state=?,revision=revision+1,updated_at=? WHERE id=? AND state=? AND revision=?`, state, nanos(at), request.ID, model.AccessRequestPending, submission.ExpectedWindowRevision)
	if err != nil {
		return app.AccessRequestResult{}, err
	}
	if count, _ := updated.RowsAffected(); count != 1 {
		return app.AccessRequestResult{}, app.ErrConflict
	}
	if err = bumpTx(ctx, tx); err != nil {
		return app.AccessRequestResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return app.AccessRequestResult{}, err
	}
	return s.AccessRequest(ctx, request.ID, model.OperatorPrincipal(), at)
}

func accessRequestRecordByDecision(ctx context.Context, q queryer, id model.DecisionID) (app.AccessRequestResult, error) {
	var requestID model.AccessRequestID
	if err := q.QueryRowContext(ctx, `SELECT id FROM access_requests WHERE decision_id=?`, id).Scan(&requestID); err != nil {
		return app.AccessRequestResult{}, classify(err)
	}
	return accessRequestRecord(ctx, q, requestID)
}

func accessRequestRecord(ctx context.Context, q queryer, id model.AccessRequestID) (app.AccessRequestResult, error) {
	request, err := scanAccessRequest(q.QueryRowContext(ctx, `SELECT id,decision_id,request_id,requester_kind,requester_agent_id,requester_execution_id,requester_generation,subject_kind,subject_id,action,resource_kind,resource_id,requested_configuration_json,bounds_json,reason,requested_lifetime,expires_at,state,grant_id,revision,created_at,updated_at FROM access_requests WHERE id=?`, id))
	if err != nil {
		return app.AccessRequestResult{}, err
	}
	decision := model.AccessDecision{ID: request.DecisionID, Kind: model.DecisionAccess, AccessRequestID: request.ID, SourceRevision: 1, PermittedAnswers: []string{model.AccessAnswerApprove, model.AccessAnswerDeny}, ExpiresAt: request.ExpiresAt, Revision: request.Revision}
	switch request.State {
	case model.AccessRequestPending:
		decision.State = model.DecisionOpen
	case model.AccessRequestExpired:
		decision.State = model.DecisionExpired
	default:
		decision.State = model.DecisionAnswered
		var submitted int64
		var actorKind model.PrincipalKind
		submission := model.AccessDecisionSubmission{DecisionID: request.DecisionID}
		err = q.QueryRowContext(ctx, `SELECT request_id,expected_window_revision,answer,reason,actor_kind,actor_agent_id,actor_execution_id,actor_automation_run,submitted_at FROM access_request_decisions WHERE decision_id=?`, request.DecisionID).Scan(&submission.RequestID, &submission.ExpectedWindowRevision, &submission.Answer, &submission.Reason, &actorKind, &submission.Actor.AgentID, &submission.Actor.ExecutionID, &submission.Actor.AutomationRun, &submitted)
		if err != nil {
			return app.AccessRequestResult{}, classify(err)
		}
		submission.Actor.Kind = actorKind
		submission.SubmittedAt = fromNanos(submitted)
		if submission.Actor.Kind != model.PrincipalOperator || submission.RequestID.Validate() != nil || submission.ExpectedWindowRevision != decision.SourceRevision || strings.TrimSpace(submission.Reason) == "" || submission.Answer == model.AccessAnswerApprove && request.State != model.AccessRequestApproved || submission.Answer == model.AccessAnswerDeny && request.State != model.AccessRequestDenied || submission.Answer != model.AccessAnswerApprove && submission.Answer != model.AccessAnswerDeny {
			return app.AccessRequestResult{}, app.ErrInvalid
		}
		decision.Submission = &submission
	}
	return app.AccessRequestResult{Request: request, Decision: decision}, nil
}

func scanAccessRequest(row scanner) (model.AccessRequest, error) {
	var request model.AccessRequest
	var subjectKind, subjectID, resourceKind, resourceID string
	var requestedConfiguration, bounds []byte
	var lifetime, expires, created, updated int64
	err := row.Scan(&request.ID, &request.DecisionID, &request.RequestID, &request.Requester.Kind, &request.Requester.AgentID, &request.Requester.ExecutionID, &request.Requester.Generation, &subjectKind, &subjectID, &request.Action, &resourceKind, &resourceID, &requestedConfiguration, &bounds, &request.Reason, &lifetime, &expires, &request.State, &request.GrantID, &request.Revision, &created, &updated)
	if err != nil {
		return request, classify(err)
	}
	request.Subject, request.Resource = makeSubject(subjectKind, subjectID), makeResource(resourceKind, resourceID)
	if !validResourceSelector(request.Resource) {
		return request, app.ErrInvalid
	}
	if len(requestedConfiguration) > 0 {
		request.RequestedConfiguration = new(model.DesiredConfiguration)
		if err = json.Unmarshal(requestedConfiguration, request.RequestedConfiguration); err != nil {
			return request, err
		}
	}
	if err = json.Unmarshal(bounds, &request.Bounds); err != nil {
		return request, err
	}
	request.RequestedLifetime = time.Duration(lifetime)
	request.ExpiresAt, request.CreatedAt, request.UpdatedAt = fromNanos(expires), fromNanos(created), fromNanos(updated)
	if !validStoredAccessRequest(request) {
		return request, app.ErrInvalid
	}
	return request, nil
}

func expireAccessRequestsTx(ctx context.Context, tx *sql.Tx, at time.Time) error {
	result, err := tx.ExecContext(ctx, `UPDATE access_requests SET state=?,revision=revision+1,updated_at=? WHERE state=? AND expires_at<=?`, model.AccessRequestExpired, nanos(at), model.AccessRequestPending, nanos(at))
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count > 0 {
		return bumpTx(ctx, tx)
	}
	return nil
}

func expireAccessRequestTx(ctx context.Context, tx *sql.Tx, id model.AccessRequestID, at time.Time) error {
	result, err := tx.ExecContext(ctx, `UPDATE access_requests SET state=?,revision=revision+1,updated_at=? WHERE id=? AND state=? AND expires_at<=?`, model.AccessRequestExpired, nanos(at), id, model.AccessRequestPending, nanos(at))
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 1 {
		return bumpTx(ctx, tx)
	}
	return app.ErrConflict
}

func accessGrant(request model.AccessRequest, at time.Time) model.AuthorityGrant {
	expires := request.ExpiresAt
	return model.AuthorityGrant{ID: request.GrantID, Subject: request.Subject, Action: request.Action, Resource: request.Resource, Bounds: request.Bounds, ExpiresAt: &expires, Revision: 1, CreatedAt: at, UpdatedAt: at}
}

func optionalDesiredConfigurationJSON(value *model.DesiredConfiguration) ([]byte, error) {
	if value == nil {
		return nil, nil
	}
	return json.Marshal(value)
}

func validStoredAccessRequest(request model.AccessRequest) bool {
	if request.ID.Validate() != nil || request.DecisionID.Validate() != nil || request.RequestID.Validate() != nil || request.GrantID.Validate() != nil {
		return false
	}
	if request.Requester.Kind != model.PrincipalExecution || request.Requester.ExecutionID.Validate() != nil || request.Requester.Generation == 0 {
		return false
	}
	if request.Subject.Kind == model.AuthorityAgent {
		if request.Subject.AgentID.Validate() != nil || request.Requester.AgentID != request.Subject.AgentID {
			return false
		}
	} else if request.Subject.Kind == model.AuthorityExecution {
		if request.Subject.ExecutionID.Validate() != nil || request.Requester.AgentID != "" || request.Requester.ExecutionID != request.Subject.ExecutionID {
			return false
		}
	} else {
		return false
	}
	if request.Action == "" || !validResourceSelector(request.Resource) || strings.TrimSpace(request.Reason) == "" || len(request.Reason) > 1024 {
		return false
	}
	if request.RequestedLifetime < time.Second || request.RequestedLifetime > app.MaxAccessRequestLifetime || request.ExpiresAt.Sub(request.CreatedAt) != request.RequestedLifetime || request.UpdatedAt.Before(request.CreatedAt) {
		return false
	}
	switch request.State {
	case model.AccessRequestPending, model.AccessRequestApproved, model.AccessRequestDenied, model.AccessRequestExpired:
	default:
		return false
	}
	return (!app.AccessRequestConfigurationRequired(request.Action) || request.RequestedConfiguration != nil) && configurationMatches(request.Bounds, request.RequestedConfiguration) && app.ValidateAuthorityGrant(accessGrant(request, request.UpdatedAt)) == nil
}

func sameAccessRequestIntent(left, right model.AccessRequest) bool {
	return left.RequestID == right.RequestID && left.Requester.Kind == right.Requester.Kind && left.Requester.AgentID == right.Requester.AgentID && left.Requester.ExecutionID == right.Requester.ExecutionID && left.Action == right.Action && left.Resource == right.Resource && reflect.DeepEqual(left.RequestedConfiguration, right.RequestedConfiguration) && reflect.DeepEqual(left.Bounds, right.Bounds) && left.Reason == right.Reason && left.RequestedLifetime == right.RequestedLifetime
}

func accessRequestSubjectCurrent(ctx context.Context, q queryer, request model.AccessRequest, at time.Time) bool {
	access, err := executionAccessRow(q.QueryRowContext(ctx, accessSelect+` WHERE execution_id=?`, request.Requester.ExecutionID))
	if err != nil || access.State != model.ExecutionAccessActive || !at.Before(access.ExpiresAt) || access.AgentID != request.Requester.AgentID {
		return false
	}
	var state model.ExecutionState
	if err = q.QueryRowContext(ctx, `SELECT state FROM executions WHERE id=?`, request.Requester.ExecutionID).Scan(&state); err != nil || state == model.ExecutionExited || state == model.ExecutionFailed || state == model.ExecutionUnknown {
		return false
	}
	if request.Subject.Kind == model.AuthorityAgent {
		var primary model.ExecutionID
		var lifecycle model.AgentLifecycleState
		return q.QueryRowContext(ctx, `SELECT primary_execution_id,lifecycle_state FROM agents WHERE id=?`, request.Subject.AgentID).Scan(&primary, &lifecycle) == nil && primary == request.Requester.ExecutionID && lifecycle == model.AgentActive
	}
	return request.Subject.Kind == model.AuthorityExecution && request.Subject.ExecutionID == request.Requester.ExecutionID && access.AgentID == ""
}
