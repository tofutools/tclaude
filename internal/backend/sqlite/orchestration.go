package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func (s *Store) SaveDefinition(ctx context.Context, definition model.Definition, revision model.DefinitionRevision, expected model.Revision) (app.DefinitionRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.DefinitionRecord{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var priorDefinition model.DefinitionID
	var priorHash string
	lookupErr := tx.QueryRowContext(ctx, `SELECT definition_id,content_hash FROM definition_revisions WHERE request_scope=? AND request_id=?`, requestScope(revision.Author), revision.RequestID).Scan(&priorDefinition, &priorHash)
	if lookupErr == nil {
		if priorDefinition != definition.ID || priorHash != revision.ContentHash {
			return app.DefinitionRecord{}, app.ErrConflict
		}
		if err = tx.Commit(); err != nil {
			return app.DefinitionRecord{}, err
		}
		return s.Definition(ctx, definition.ID)
	}
	if !errors.Is(lookupErr, sql.ErrNoRows) {
		return app.DefinitionRecord{}, lookupErr
	}
	now := nanos(definition.UpdatedAt)
	if expected == 0 {
		definition.Revision, revision.Number = 1, 1
		_, err = tx.ExecContext(ctx, `INSERT INTO definitions(id,name,kind,head_revision_id,tombstoned,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, definition.ID, definition.Name, definition.Kind, revision.ID, definition.Tombstoned, definition.Revision, nanos(definition.CreatedAt), now)
	} else {
		var kind model.DefinitionKind
		if err = tx.QueryRowContext(ctx, `SELECT kind FROM definitions WHERE id=?`, definition.ID).Scan(&kind); err != nil {
			return app.DefinitionRecord{}, classify(err)
		}
		if kind != definition.Kind {
			return app.DefinitionRecord{}, app.ErrConflict
		}
		result, updateErr := tx.ExecContext(ctx, `UPDATE definitions SET name=?,head_revision_id=?,tombstoned=?,revision=revision+1,updated_at=? WHERE id=? AND revision=?`, definition.Name, revision.ID, definition.Tombstoned, now, definition.ID, expected)
		if updateErr != nil {
			return app.DefinitionRecord{}, updateErr
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return app.DefinitionRecord{}, app.ErrConflict
		}
		definition.Revision, revision.Number = expected+1, expected+1
	}
	if err != nil {
		return app.DefinitionRecord{}, classify(err)
	}
	parameters, _ := json.Marshal(revision.Parameters)
	team, _ := json.Marshal(revision.Team)
	process, _ := json.Marshal(revision.Process)
	dependencies, _ := json.Marshal(revision.Dependencies)
	author, _ := json.Marshal(revision.Author)
	if _, err = tx.ExecContext(ctx, `INSERT INTO definition_revisions(id,definition_id,number,request_scope,request_id,content_hash,schema_version,compiler_version,source,parameters_json,team_json,process_json,dependencies_json,author_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, revision.ID, revision.DefinitionID, revision.Number, requestScope(revision.Author), revision.RequestID, revision.ContentHash, revision.SchemaVersion, revision.CompilerVersion, revision.Source, parameters, nullableJSON(revision.Team, team), nullableJSON(revision.Process, process), dependencies, author, nanos(revision.CreatedAt)); err != nil {
		return app.DefinitionRecord{}, classify(err)
	}
	if err = bumpTx(ctx, tx); err != nil {
		return app.DefinitionRecord{}, err
	}
	if err = tx.Commit(); err != nil {
		return app.DefinitionRecord{}, err
	}
	return s.Definition(ctx, definition.ID)
}

func (s *Store) Definition(ctx context.Context, id model.DefinitionID) (app.DefinitionRecord, error) {
	var record app.DefinitionRecord
	var created, updated int64
	err := s.db.QueryRowContext(ctx, `SELECT id,name,kind,head_revision_id,tombstoned,revision,created_at,updated_at FROM definitions WHERE id=?`, id).Scan(&record.Definition.ID, &record.Definition.Name, &record.Definition.Kind, &record.Definition.HeadRevisionID, &record.Definition.Tombstoned, &record.Definition.Revision, &created, &updated)
	if err != nil {
		return record, classify(err)
	}
	record.Definition.CreatedAt, record.Definition.UpdatedAt = fromNanos(created), fromNanos(updated)
	record.Head, err = s.DefinitionRevision(ctx, record.Definition.HeadRevisionID)
	return record, err
}

func (s *Store) DefinitionRevision(ctx context.Context, id model.DefinitionRevisionID) (model.DefinitionRevision, error) {
	var revision model.DefinitionRevision
	var parameters, team, process, dependencies, author []byte
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT id,definition_id,number,request_id,content_hash,schema_version,compiler_version,source,parameters_json,team_json,process_json,dependencies_json,author_json,created_at FROM definition_revisions WHERE id=?`, id).Scan(&revision.ID, &revision.DefinitionID, &revision.Number, &revision.RequestID, &revision.ContentHash, &revision.SchemaVersion, &revision.CompilerVersion, &revision.Source, &parameters, &team, &process, &dependencies, &author, &created)
	if err != nil {
		return revision, classify(err)
	}
	if err = json.Unmarshal(parameters, &revision.Parameters); err != nil {
		return revision, err
	}
	if len(team) > 0 && string(team) != "null" {
		revision.Team = new(model.TeamDefinition)
		if err = json.Unmarshal(team, revision.Team); err != nil {
			return revision, err
		}
	}
	if len(process) > 0 && string(process) != "null" {
		revision.Process = new(model.ProcessDefinition)
		if err = json.Unmarshal(process, revision.Process); err != nil {
			return revision, err
		}
	}
	if err = json.Unmarshal(dependencies, &revision.Dependencies); err != nil {
		return revision, err
	}
	if err = json.Unmarshal(author, &revision.Author); err != nil {
		return revision, err
	}
	revision.CreatedAt = fromNanos(created)
	return revision, nil
}

func (s *Store) ListDefinitions(ctx context.Context, kind model.DefinitionKind, includeTombstoned bool) ([]model.Definition, error) {
	query := `SELECT id,name,kind,head_revision_id,tombstoned,revision,created_at,updated_at FROM definitions WHERE (?='' OR kind=?) AND (? OR tombstoned=0) ORDER BY name,id`
	rows, err := s.db.QueryContext(ctx, query, kind, kind, includeTombstoned)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var definitions []model.Definition
	for rows.Next() {
		var definition model.Definition
		var created, updated int64
		if err = rows.Scan(&definition.ID, &definition.Name, &definition.Kind, &definition.HeadRevisionID, &definition.Tombstoned, &definition.Revision, &created, &updated); err != nil {
			return nil, err
		}
		definition.CreatedAt, definition.UpdatedAt = fromNanos(created), fromNanos(updated)
		definitions = append(definitions, definition)
	}
	return definitions, rows.Err()
}

func (s *Store) SaveProgramProfile(ctx context.Context, profile model.ProgramProfile, revision model.ProgramProfileRevision, expected model.Revision) (app.ProgramProfileRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.ProgramProfileRecord{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var priorProfile model.ProgramProfileID
	var priorRevision model.ProgramProfileRevisionID
	var priorHash string
	lookupErr := tx.QueryRowContext(ctx, `SELECT profile_id,id,content_hash FROM program_profile_revisions WHERE request_scope=? AND request_id=?`, requestScope(revision.Author), revision.RequestID).Scan(&priorProfile, &priorRevision, &priorHash)
	if lookupErr == nil {
		if priorProfile != profile.ID || priorHash != revision.ContentHash {
			return app.ProgramProfileRecord{}, app.ErrConflict
		}
		if err = tx.Commit(); err != nil {
			return app.ProgramProfileRecord{}, err
		}
		storedProfile, readErr := s.programProfile(ctx, priorProfile)
		if readErr != nil {
			return app.ProgramProfileRecord{}, readErr
		}
		storedRevision, readErr := s.ProgramProfileRevision(ctx, priorRevision)
		return app.ProgramProfileRecord{Profile: storedProfile, Head: storedRevision}, readErr
	}
	if !errors.Is(lookupErr, sql.ErrNoRows) {
		return app.ProgramProfileRecord{}, lookupErr
	}
	if expected == 0 {
		profile.Revision, revision.Number = 1, 1
		_, err = tx.ExecContext(ctx, `INSERT INTO program_profiles(id,name,head_revision_id,tombstoned,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, profile.ID, profile.Name, revision.ID, profile.Tombstoned, profile.Revision, nanos(profile.CreatedAt), nanos(profile.UpdatedAt))
	} else {
		result, updateErr := tx.ExecContext(ctx, `UPDATE program_profiles SET name=?,head_revision_id=?,tombstoned=?,revision=revision+1,updated_at=? WHERE id=? AND revision=?`, profile.Name, revision.ID, profile.Tombstoned, nanos(profile.UpdatedAt), profile.ID, expected)
		if updateErr != nil {
			return app.ProgramProfileRecord{}, updateErr
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return app.ProgramProfileRecord{}, app.ErrConflict
		}
		profile.Revision, revision.Number = expected+1, expected+1
	}
	if err != nil {
		return app.ProgramProfileRecord{}, classify(err)
	}
	args, _ := json.Marshal(revision.ArgumentPrefix)
	environment, _ := json.Marshal(revision.Environment)
	authority, _ := json.Marshal(revision.EffectAuthority)
	author, _ := json.Marshal(revision.Author)
	if _, err = tx.ExecContext(ctx, `INSERT INTO program_profile_revisions(id,profile_id,number,request_scope,request_id,content_hash,executable,argument_prefix_json,environment_json,working_directory,sandbox,timeout_ns,output_limit_bytes,effect_authority_json,author_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, revision.ID, revision.ProfileID, revision.Number, requestScope(revision.Author), revision.RequestID, revision.ContentHash, revision.Executable, args, environment, revision.WorkingDirectory, revision.Sandbox, int64(revision.Timeout), revision.OutputLimitBytes, authority, author, nanos(revision.CreatedAt)); err != nil {
		return app.ProgramProfileRecord{}, classify(err)
	}
	if err = bumpTx(ctx, tx); err != nil {
		return app.ProgramProfileRecord{}, err
	}
	if err = tx.Commit(); err != nil {
		return app.ProgramProfileRecord{}, err
	}
	storedProfile, err := s.programProfile(ctx, profile.ID)
	if err != nil {
		return app.ProgramProfileRecord{}, err
	}
	storedRevision, err := s.ProgramProfileRevision(ctx, revision.ID)
	return app.ProgramProfileRecord{Profile: storedProfile, Head: storedRevision}, err
}

func (s *Store) programProfile(ctx context.Context, id model.ProgramProfileID) (model.ProgramProfile, error) {
	var profile model.ProgramProfile
	var created, updated int64
	err := s.db.QueryRowContext(ctx, `SELECT id,name,head_revision_id,tombstoned,revision,created_at,updated_at FROM program_profiles WHERE id=?`, id).Scan(&profile.ID, &profile.Name, &profile.HeadRevisionID, &profile.Tombstoned, &profile.Revision, &created, &updated)
	if err != nil {
		return profile, classify(err)
	}
	profile.CreatedAt, profile.UpdatedAt = fromNanos(created), fromNanos(updated)
	return profile, nil
}

func (s *Store) ProgramProfile(ctx context.Context, id model.ProgramProfileID) (app.ProgramProfileRecord, error) {
	profile, err := s.programProfile(ctx, id)
	if err != nil {
		return app.ProgramProfileRecord{}, err
	}
	revision, err := s.ProgramProfileRevision(ctx, profile.HeadRevisionID)
	return app.ProgramProfileRecord{Profile: profile, Head: revision}, err
}

func (s *Store) ListProgramProfiles(ctx context.Context, includeTombstoned bool) ([]model.ProgramProfile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,head_revision_id,tombstoned,revision,created_at,updated_at FROM program_profiles WHERE (? OR tombstoned=0) ORDER BY name,id`, includeTombstoned)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var profiles []model.ProgramProfile
	for rows.Next() {
		var profile model.ProgramProfile
		var created, updated int64
		if err = rows.Scan(&profile.ID, &profile.Name, &profile.HeadRevisionID, &profile.Tombstoned, &profile.Revision, &created, &updated); err != nil {
			return nil, err
		}
		profile.CreatedAt, profile.UpdatedAt = fromNanos(created), fromNanos(updated)
		profiles = append(profiles, profile)
	}
	return profiles, rows.Err()
}

func (s *Store) ProgramProfileRevision(ctx context.Context, id model.ProgramProfileRevisionID) (model.ProgramProfileRevision, error) {
	var revision model.ProgramProfileRevision
	var args, environment, authority, author []byte
	var timeout, created int64
	err := s.db.QueryRowContext(ctx, `SELECT id,profile_id,number,request_id,content_hash,executable,argument_prefix_json,environment_json,working_directory,sandbox,timeout_ns,output_limit_bytes,effect_authority_json,author_json,created_at FROM program_profile_revisions WHERE id=?`, id).Scan(&revision.ID, &revision.ProfileID, &revision.Number, &revision.RequestID, &revision.ContentHash, &revision.Executable, &args, &environment, &revision.WorkingDirectory, &revision.Sandbox, &timeout, &revision.OutputLimitBytes, &authority, &author, &created)
	if err != nil {
		return revision, classify(err)
	}
	if err = json.Unmarshal(args, &revision.ArgumentPrefix); err != nil {
		return revision, err
	}
	if err = json.Unmarshal(environment, &revision.Environment); err != nil {
		return revision, err
	}
	if err = json.Unmarshal(authority, &revision.EffectAuthority); err != nil {
		return revision, err
	}
	if err = json.Unmarshal(author, &revision.Author); err != nil {
		return revision, err
	}
	revision.Timeout, revision.CreatedAt = time.Duration(timeout), fromNanos(created)
	return revision, nil
}

func (s *Store) CreateGraphWorkRun(ctx context.Context, run model.WorkRun, windows []model.DecisionWindow) (app.WorkRunRecord, bool, error) {
	if existing, err := s.WorkRunByRequest(ctx, run.Requester, run.RequestID); err == nil {
		if existing.Run.ID != run.ID || !reflect.DeepEqual(existing.Run.Graph, run.Graph) || !reflect.DeepEqual(existing.Run.Parameters, run.Parameters) {
			return app.WorkRunRecord{}, false, app.ErrConflict
		}
		return existing, true, nil
	} else if !errors.Is(err, app.ErrNotFound) {
		return app.WorkRunRecord{}, false, err
	}
	requester, _ := json.Marshal(run.Requester)
	authority, _ := json.Marshal(run.Authority)
	delegation, _ := json.Marshal(run.Delegation)
	spec, _ := json.Marshal(run.Spec)
	graph, _ := json.Marshal(run.Graph)
	closure, _ := json.Marshal(run.DefinitionClosure)
	parameters, _ := json.Marshal(run.Parameters)
	scope, _ := json.Marshal(run.Scope)
	programs, _ := json.Marshal(run.AuthorizedPrograms)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.WorkRunRecord{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if err = requirePendingAutomationAction(ctx, tx, run.Requester, model.AutomationStartWork, run.Scope.DeploymentID, run.CreatedAt); err != nil {
		return app.WorkRunRecord{}, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO work_runs(id,request_scope,request_id,requester_json,authority_json,delegation_json,spec_json,state,worker_execution_id,cancellation_requested,cancellation_reason,revision,created_at,updated_at,graph_json,definition_closure_json,parameters_json,scope_json,authorized_programs_json,control_state,outcome,deadline) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, run.ID, requestScope(run.Requester), run.RequestID, requester, authority, delegation, spec, run.State, run.WorkerExecutionID, run.CancellationRequested, run.CancellationReason, run.Revision, nanos(run.CreatedAt), nanos(run.UpdatedAt), graph, closure, parameters, scope, programs, run.ControlState, run.Outcome, nanos(run.Deadline))
	if err != nil {
		return app.WorkRunRecord{}, false, classify(err)
	}
	for _, attempt := range run.NodeAttempts {
		performer, _ := json.Marshal(attempt.Performer)
		var retryAt, settledAt any
		if attempt.RetryAt != nil {
			retryAt = nanos(*attempt.RetryAt)
		}
		if attempt.SettledAt != nil {
			settledAt = nanos(*attempt.SettledAt)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO work_node_attempts(work_run_id,node_id,activation_id,attempt,issuance_id,state,performer_json,operation_id,execution_id,ready_at,retry_at,deadline,retry_budget,join_winner,decision_id,outcome,detail,created_at,updated_at,settled_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, attempt.Ref.RunID, attempt.Ref.NodeID, attempt.Ref.ActivationID, attempt.Ref.Attempt, attempt.Ref.IssuanceID, attempt.State, performer, attempt.OperationID, attempt.ExecutionID, nanos(attempt.ReadyAt), retryAt, nanos(attempt.Deadline), attempt.RetryBudget, attempt.JoinWinner, attempt.DecisionID, attempt.Outcome, attempt.Detail, nanos(attempt.CreatedAt), nanos(attempt.UpdatedAt), settledAt)
		if err != nil {
			return app.WorkRunRecord{}, false, classify(err)
		}
	}
	for _, window := range windows {
		if err = insertDecisionWindow(ctx, tx, window); err != nil {
			return app.WorkRunRecord{}, false, err
		}
	}
	if err = bumpTx(ctx, tx); err != nil {
		return app.WorkRunRecord{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return app.WorkRunRecord{}, false, err
	}
	record, err := s.WorkRun(ctx, run.ID)
	return record, false, err
}

func (s *Store) Decision(ctx context.Context, id model.DecisionID) (app.DecisionRecord, error) {
	return decisionRecord(ctx, s.db, id)
}

func (s *Store) PendingDecisions(ctx context.Context) ([]app.DecisionRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM decision_windows WHERE state=? ORDER BY expires_at,id`, model.DecisionOpen)
	if err != nil {
		return nil, err
	}
	var ids []model.DecisionID
	for rows.Next() {
		var id model.DecisionID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	records := make([]app.DecisionRecord, 0, len(ids))
	for _, id := range ids {
		record, readErr := s.Decision(ctx, id)
		if readErr != nil {
			return nil, readErr
		}
		records = append(records, record)
	}
	return records, nil
}

func (s *Store) SubmitDecision(ctx context.Context, submission model.DecisionSubmission, authority model.AuthorityRequest, at time.Time) (app.DecisionRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.DecisionRecord{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var priorDecision model.DecisionID
	var priorAnswer, priorReason string
	var priorEvidence []byte
	lookupErr := tx.QueryRowContext(ctx, `SELECT decision_id,answer,reason,evidence_refs_json FROM decision_submissions WHERE request_scope=? AND request_id=?`, requestScope(submission.Actor), submission.RequestID).Scan(&priorDecision, &priorAnswer, &priorReason, &priorEvidence)
	if lookupErr == nil {
		var refs []model.WorkEvidenceID
		if err = json.Unmarshal(priorEvidence, &refs); err != nil {
			return app.DecisionRecord{}, err
		}
		if priorDecision != submission.DecisionID || priorAnswer != submission.Answer || priorReason != submission.Reason || !reflect.DeepEqual(refs, submission.EvidenceRefs) {
			return app.DecisionRecord{}, app.ErrConflict
		}
		if err = tx.Commit(); err != nil {
			return app.DecisionRecord{}, err
		}
		return s.Decision(ctx, submission.DecisionID)
	}
	if !errors.Is(lookupErr, sql.ErrNoRows) {
		return app.DecisionRecord{}, lookupErr
	}
	record, err := decisionRecord(ctx, tx, submission.DecisionID)
	if err != nil {
		return app.DecisionRecord{}, err
	}
	if record.Window.State != model.DecisionOpen || record.Window.Revision != submission.ExpectedWindowRevision {
		return app.DecisionRecord{}, app.ErrConflict
	}
	if !record.Window.ExpiresAt.After(at) {
		if _, err = tx.ExecContext(ctx, `UPDATE decision_windows SET state=?,revision=revision+1,updated_at=? WHERE id=? AND state=? AND revision=?`, model.DecisionExpired, nanos(at), submission.DecisionID, model.DecisionOpen, submission.ExpectedWindowRevision); err != nil {
			return app.DecisionRecord{}, err
		}
		if err = bumpTx(ctx, tx); err != nil {
			return app.DecisionRecord{}, err
		}
		if err = tx.Commit(); err != nil {
			return app.DecisionRecord{}, err
		}
		return app.DecisionRecord{}, app.ErrConflict
	}
	allowedAnswer := false
	for _, answer := range record.Window.PermittedAnswers {
		allowedAnswer = allowedAnswer || answer == submission.Answer
	}
	if !allowedAnswer {
		return app.DecisionRecord{}, app.ErrInvalid
	}
	authorityDecision, err := authorizeTx(ctx, tx, authority, at)
	if err != nil {
		return app.DecisionRecord{}, err
	}
	if !authorityDecision.Allowed || !decisionAudienceAllows(record.Window.Audience, submission.Actor, authorityDecision) {
		return app.DecisionRecord{}, app.ErrUnauthorized
	}
	evidence, _ := json.Marshal(submission.EvidenceRefs)
	actor, _ := json.Marshal(submission.Actor)
	if _, err = tx.ExecContext(ctx, `INSERT INTO decision_submissions(decision_id,request_scope,request_id,expected_window_revision,answer,reason,evidence_refs_json,actor_json,submitted_at) VALUES(?,?,?,?,?,?,?,?,?)`, submission.DecisionID, requestScope(submission.Actor), submission.RequestID, submission.ExpectedWindowRevision, submission.Answer, submission.Reason, evidence, actor, nanos(submission.SubmittedAt)); err != nil {
		return app.DecisionRecord{}, classify(err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE decision_windows SET state=?,revision=revision+1,updated_at=? WHERE id=? AND state=? AND revision=?`, model.DecisionAnswered, nanos(at), submission.DecisionID, model.DecisionOpen, submission.ExpectedWindowRevision)
	if err != nil {
		return app.DecisionRecord{}, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return app.DecisionRecord{}, app.ErrConflict
	}
	if err = bumpTx(ctx, tx); err != nil {
		return app.DecisionRecord{}, err
	}
	if err = tx.Commit(); err != nil {
		return app.DecisionRecord{}, err
	}
	return s.Decision(ctx, submission.DecisionID)
}

func (s *Store) ApplyGraphTransition(ctx context.Context, transition app.GraphTransition) (app.WorkRunRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.WorkRunRecord{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if transition.Evidence != nil {
		var priorID model.WorkEvidenceID
		var priorRun model.WorkRunID
		var priorNode model.WorkNodeID
		var priorActivation model.WorkActivationID
		var priorAttempt uint32
		var priorIssuance model.WorkIssuanceID
		err = tx.QueryRowContext(ctx, `SELECT id,work_run_id,node_id,activation_id,attempt,issuance_id FROM work_node_evidence WHERE request_scope=? AND request_id=?`, requestScope(transition.Evidence.Reporter), transition.Evidence.RequestID).Scan(&priorID, &priorRun, &priorNode, &priorActivation, &priorAttempt, &priorIssuance)
		if err == nil {
			ref := transition.Evidence.Attempt
			if priorRun != ref.RunID || priorNode != ref.NodeID || priorActivation != ref.ActivationID || priorAttempt != ref.Attempt || priorIssuance != ref.IssuanceID {
				return app.WorkRunRecord{}, app.ErrConflict
			}
			if err = tx.Commit(); err != nil {
				return app.WorkRunRecord{}, err
			}
			return s.WorkRun(ctx, priorRun)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return app.WorkRunRecord{}, err
		}
	}
	var authorityDecision model.AuthorityDecision
	if transition.Authority.Action != "" {
		decision, authorizeErr := authorizeTx(ctx, tx, transition.Authority, transition.At)
		if authorizeErr != nil {
			return app.WorkRunRecord{}, authorizeErr
		}
		if !decision.Allowed {
			return app.WorkRunRecord{}, app.ErrUnauthorized
		}
		authorityDecision = decision
	}
	additionalDecisions := make([]model.AuthorityDecision, len(transition.AdditionalAuthority))
	for i, request := range transition.AdditionalAuthority {
		decision, authorizeErr := authorizeTx(ctx, tx, request, transition.At)
		if authorizeErr != nil {
			return app.WorkRunRecord{}, authorizeErr
		}
		if !decision.Allowed {
			return app.WorkRunRecord{}, app.ErrUnauthorized
		}
		additionalDecisions[i] = decision
	}
	var currentRevision model.Revision
	var currentState model.WorkRunState
	var cancelled bool
	var deadline sql.NullInt64
	if err = tx.QueryRowContext(ctx, `SELECT revision,state,cancellation_requested,deadline FROM work_runs WHERE id=?`, transition.WorkRunID).Scan(&currentRevision, &currentState, &cancelled, &deadline); err != nil {
		return app.WorkRunRecord{}, classify(err)
	}
	if currentRevision != transition.ExpectedRevision || (cancelled && (transition.Operation != nil || transition.Execution != nil || len(transition.Activations) > 0)) {
		return app.WorkRunRecord{}, app.ErrConflict
	}
	if deadline.Valid && transition.At.After(fromNanos(deadline.Int64)) && transition.RunState != model.WorkRunFailed && transition.RunState != model.WorkRunCancelled {
		return app.WorkRunRecord{}, app.ErrConflict
	}
	if transition.Execution != nil {
		if transition.Execution.AgentID != "" {
			var agentRevision model.Revision
			var primary model.ExecutionID
			var lifecycle model.AgentLifecycleState
			if err = tx.QueryRowContext(ctx, `SELECT revision,primary_execution_id,lifecycle_state FROM agents WHERE id=?`, transition.Execution.AgentID).Scan(&agentRevision, &primary, &lifecycle); err != nil {
				return app.WorkRunRecord{}, classify(err)
			}
			if transition.AgentExpected == 0 || agentRevision != transition.AgentExpected || lifecycle != model.AgentActive {
				return app.WorkRunRecord{}, app.ErrConflict
			}
			if primary != "" {
				var primaryState model.ExecutionState
				if err = tx.QueryRowContext(ctx, `SELECT state FROM executions WHERE id=?`, primary).Scan(&primaryState); err != nil {
					return app.WorkRunRecord{}, classify(err)
				}
				if primaryState != model.ExecutionExited && primaryState != model.ExecutionFailed {
					return app.WorkRunRecord{}, app.ErrConflict
				}
				if _, err = tx.ExecContext(ctx, `UPDATE execution_accesses SET state=?,revoked_at=?,revision=revision+1 WHERE execution_id=? AND state NOT IN (?,?)`, model.ExecutionAccessRevoked, nanos(transition.At), primary, model.ExecutionAccessRevoked, model.ExecutionAccessExpired); err != nil {
					return app.WorkRunRecord{}, err
				}
			}
			if err = insertConversationAndAssociation(ctx, tx, transition.Execution.AgentID, transition.Execution.ConversationID, transition.At); err != nil {
				return app.WorkRunRecord{}, err
			}
		}
		if err = insertExecution(ctx, tx, *transition.Execution); err != nil {
			return app.WorkRunRecord{}, err
		}
		if transition.Access != nil {
			if err = insertExecutionAccess(ctx, tx, *transition.Access); err != nil {
				return app.WorkRunRecord{}, err
			}
		}
		if transition.Execution.AgentID != "" {
			result, updateErr := tx.ExecContext(ctx, `UPDATE agents SET primary_execution_id=?,revision=revision+1,updated_at=? WHERE id=? AND revision=?`, transition.Execution.ID, nanos(transition.At), transition.Execution.AgentID, transition.AgentExpected)
			if updateErr != nil {
				return app.WorkRunRecord{}, updateErr
			}
			if count, _ := result.RowsAffected(); count != 1 {
				return app.WorkRunRecord{}, app.ErrConflict
			}
		}
	}
	if transition.Operation != nil {
		if err = insertOperation(ctx, tx, *transition.Operation); err != nil {
			return app.WorkRunRecord{}, err
		}
		if transition.Authority.Action != "" {
			if err = insertOperationAuthority(ctx, tx, transition.Operation.ID, transition.Authority, authorityDecision); err != nil {
				return app.WorkRunRecord{}, err
			}
		}
		for i, request := range transition.AdditionalAuthority {
			if err = insertAdditionalOperationAuthority(ctx, tx, transition.Operation.ID, i, request, additionalDecisions[i]); err != nil {
				return app.WorkRunRecord{}, err
			}
		}
		if transition.Execution == nil && transition.Authority.Action != "" {
			if _, err = tx.ExecContext(ctx, `INSERT INTO effect_permits(operation_id) VALUES(?)`, transition.Operation.ID); err != nil {
				return app.WorkRunRecord{}, classify(err)
			}
		}
	}
	if transition.WorkspaceUse != nil {
		use := transition.WorkspaceUse
		if _, err = tx.ExecContext(ctx, `INSERT INTO workspace_uses(id,workspace_id,execution_id,work_run_id,created_at) VALUES(?,?,?,?,?)`, use.ID, use.WorkspaceID, use.ExecutionID, use.WorkRunID, nanos(use.CreatedAt)); err != nil {
			return app.WorkRunRecord{}, classify(err)
		}
	}
	for _, update := range transition.Updates {
		var issuance model.WorkIssuanceID
		var state model.WorkNodeAttemptState
		var decisionID model.DecisionID
		err = tx.QueryRowContext(ctx, `SELECT issuance_id,state,decision_id FROM work_node_attempts WHERE work_run_id=? AND node_id=? AND activation_id=? AND attempt=?`, update.Ref.RunID, update.Ref.NodeID, update.Ref.ActivationID, update.Ref.Attempt).Scan(&issuance, &state, &decisionID)
		if err != nil {
			return app.WorkRunRecord{}, classify(err)
		}
		if issuance != update.Ref.IssuanceID || terminalNodeAttempt(state) {
			return app.WorkRunRecord{}, app.ErrConflict
		}
		var settled any
		if terminalNodeAttempt(update.State) {
			settled = nanos(transition.At)
		}
		newIssuance := update.Ref.IssuanceID
		if update.NewIssuanceID != "" {
			newIssuance = update.NewIssuanceID
		}
		result, updateErr := tx.ExecContext(ctx, `UPDATE work_node_attempts SET issuance_id=?,operation_id=CASE WHEN ?='' THEN operation_id ELSE ? END,execution_id=CASE WHEN ?='' THEN execution_id ELSE ? END,decision_id=CASE WHEN ?='' THEN decision_id ELSE ? END,state=?,outcome=?,detail=?,updated_at=?,settled_at=? WHERE work_run_id=? AND node_id=? AND activation_id=? AND attempt=? AND issuance_id=?`, newIssuance, update.OperationID, update.OperationID, update.ExecutionID, update.ExecutionID, update.DecisionID, update.DecisionID, update.State, update.Outcome, update.Detail, nanos(transition.At), settled, update.Ref.RunID, update.Ref.NodeID, update.Ref.ActivationID, update.Ref.Attempt, update.Ref.IssuanceID)
		if updateErr != nil {
			return app.WorkRunRecord{}, updateErr
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return app.WorkRunRecord{}, app.ErrConflict
		}
		if decisionID != "" && terminalNodeAttempt(update.State) {
			if _, err = tx.ExecContext(ctx, `UPDATE decision_windows SET state=?,revision=revision+1,updated_at=? WHERE id=? AND state=?`, model.DecisionExpired, nanos(transition.At), decisionID, model.DecisionOpen); err != nil {
				return app.WorkRunRecord{}, err
			}
		}
	}
	if transition.Evidence != nil {
		evidence := transition.Evidence
		reporter, _ := json.Marshal(evidence.Reporter)
		var passed any
		if evidence.Passed != nil {
			passed = *evidence.Passed
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO work_node_evidence(id,request_scope,request_id,work_run_id,node_id,activation_id,attempt,issuance_id,reporter_json,kind,artifact_revision,passed,disposition,detail,recorded_at,revision) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, evidence.ID, requestScope(evidence.Reporter), evidence.RequestID, evidence.Attempt.RunID, evidence.Attempt.NodeID, evidence.Attempt.ActivationID, evidence.Attempt.Attempt, evidence.Attempt.IssuanceID, reporter, evidence.Kind, evidence.ArtifactRevision, passed, evidence.Disposition, evidence.Detail, nanos(evidence.RecordedAt), evidence.Revision)
		if err != nil {
			return app.WorkRunRecord{}, classify(err)
		}
	}
	for _, attempt := range transition.Activations {
		performer, _ := json.Marshal(attempt.Performer)
		var retryAt, settledAt any
		if attempt.RetryAt != nil {
			retryAt = nanos(*attempt.RetryAt)
		}
		if attempt.SettledAt != nil {
			settledAt = nanos(*attempt.SettledAt)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO work_node_attempts(work_run_id,node_id,activation_id,attempt,issuance_id,state,performer_json,operation_id,execution_id,ready_at,retry_at,deadline,retry_budget,join_winner,decision_id,outcome,detail,created_at,updated_at,settled_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, attempt.Ref.RunID, attempt.Ref.NodeID, attempt.Ref.ActivationID, attempt.Ref.Attempt, attempt.Ref.IssuanceID, attempt.State, performer, attempt.OperationID, attempt.ExecutionID, nanos(attempt.ReadyAt), retryAt, nanos(attempt.Deadline), attempt.RetryBudget, attempt.JoinWinner, attempt.DecisionID, attempt.Outcome, attempt.Detail, nanos(attempt.CreatedAt), nanos(attempt.UpdatedAt), settledAt)
		if err != nil {
			return app.WorkRunRecord{}, classify(err)
		}
	}
	for _, window := range transition.DecisionWindows {
		if err = insertDecisionWindow(ctx, tx, window); err != nil {
			return app.WorkRunRecord{}, err
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE work_runs SET state=?,control_state=?,outcome=?,revision=revision+1,updated_at=? WHERE id=? AND revision=?`, transition.RunState, transition.ControlState, transition.RunOutcome, nanos(transition.At), transition.WorkRunID, transition.ExpectedRevision)
	if err != nil {
		return app.WorkRunRecord{}, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return app.WorkRunRecord{}, app.ErrConflict
	}
	if currentState != transition.RunState {
		if err = insertTerminalWorkFactTx(ctx, tx, transition.WorkRunID, transition.RunState, transition.At); err != nil {
			return app.WorkRunRecord{}, err
		}
	}
	if err = bumpTx(ctx, tx); err != nil {
		return app.WorkRunRecord{}, err
	}
	if err = tx.Commit(); err != nil {
		return app.WorkRunRecord{}, err
	}
	return s.WorkRun(ctx, transition.WorkRunID)
}

func terminalNodeAttempt(state model.WorkNodeAttemptState) bool {
	switch state {
	case model.NodeAttemptSucceeded, model.NodeAttemptFailed, model.NodeAttemptWaived, model.NodeAttemptSuppressed:
		return true
	default:
		return false
	}
}

type decisionQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func decisionRecord(ctx context.Context, query decisionQuery, id model.DecisionID) (app.DecisionRecord, error) {
	var record app.DecisionRecord
	var audience, answers, evidence []byte
	var expires, created, updated int64
	err := query.QueryRowContext(ctx, `SELECT id,kind,source_revision,work_run_id,node_id,activation_id,attempt,issuance_id,audience_json,question,permitted_answers_json,evidence_refs_json,expires_at,state,revision,created_at,updated_at FROM decision_windows WHERE id=?`, id).Scan(&record.Window.ID, &record.Window.Kind, &record.Window.SourceRevision, &record.Window.Attempt.RunID, &record.Window.Attempt.NodeID, &record.Window.Attempt.ActivationID, &record.Window.Attempt.Attempt, &record.Window.Attempt.IssuanceID, &audience, &record.Window.Question, &answers, &evidence, &expires, &record.Window.State, &record.Window.Revision, &created, &updated)
	if err != nil {
		return record, classify(err)
	}
	if err = unmarshalMany([][]byte{audience, answers, evidence}, []any{&record.Window.Audience, &record.Window.PermittedAnswers, &record.Window.EvidenceRefs}); err != nil {
		return record, err
	}
	record.Window.ExpiresAt, record.Window.CreatedAt, record.Window.UpdatedAt = fromNanos(expires), fromNanos(created), fromNanos(updated)
	var requestID model.RequestID
	var expected model.Revision
	var answer, reason string
	var submissionEvidence, actor []byte
	var submitted int64
	err = query.QueryRowContext(ctx, `SELECT request_id,expected_window_revision,answer,reason,evidence_refs_json,actor_json,submitted_at FROM decision_submissions WHERE decision_id=?`, id).Scan(&requestID, &expected, &answer, &reason, &submissionEvidence, &actor, &submitted)
	if err == nil {
		submission := &model.DecisionSubmission{RequestID: requestID, DecisionID: id, ExpectedWindowRevision: expected, Answer: answer, Reason: reason, SubmittedAt: fromNanos(submitted)}
		if err = json.Unmarshal(submissionEvidence, &submission.EvidenceRefs); err != nil {
			return record, err
		}
		if err = json.Unmarshal(actor, &submission.Actor); err != nil {
			return record, err
		}
		record.Submission = submission
	} else if !errors.Is(err, sql.ErrNoRows) {
		return record, err
	}
	return record, nil
}

func insertDecisionWindow(ctx context.Context, tx *sql.Tx, window model.DecisionWindow) error {
	audience, _ := json.Marshal(window.Audience)
	answers, _ := json.Marshal(window.PermittedAnswers)
	evidence, _ := json.Marshal(window.EvidenceRefs)
	_, err := tx.ExecContext(ctx, `INSERT INTO decision_windows(id,kind,source_revision,work_run_id,node_id,activation_id,attempt,issuance_id,audience_json,question,permitted_answers_json,evidence_refs_json,expires_at,state,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, window.ID, window.Kind, window.SourceRevision, window.Attempt.RunID, window.Attempt.NodeID, window.Attempt.ActivationID, window.Attempt.Attempt, window.Attempt.IssuanceID, audience, window.Question, answers, evidence, nanos(window.ExpiresAt), window.State, window.Revision, nanos(window.CreatedAt), nanos(window.UpdatedAt))
	return classify(err)
}

func decisionAudienceAllows(audience []model.DecisionAudience, principal model.Principal, decision model.AuthorityDecision) bool {
	subject := principal.Authority
	if principal.Kind == model.PrincipalOperator {
		subject = model.AuthoritySubject{Kind: model.AuthorityOperator}
	}
	if principal.Kind == model.PrincipalAgent {
		subject = model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: principal.AgentID}
	}
	for _, candidate := range audience {
		if candidate.Subject == subject {
			return true
		}
		if candidate.RoleID != "" && decision.SourceKind == model.AuthorityRole && decision.SourceID == string(candidate.RoleID) {
			return true
		}
	}
	return false
}

func (s *Store) SaveAutomationRule(ctx context.Context, rule model.AutomationRule, revision model.AutomationRuleRevision, expected model.Revision) (app.AutomationRuleRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.AutomationRuleRecord{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var priorRule model.AutomationRuleID
	var priorHash string
	lookupErr := tx.QueryRowContext(ctx, `SELECT rule_id,content_hash FROM automation_rule_revisions WHERE request_scope=? AND request_id=?`, requestScope(revision.Author), revision.RequestID).Scan(&priorRule, &priorHash)
	if lookupErr == nil {
		if priorRule != rule.ID || priorHash != revision.ContentHash {
			return app.AutomationRuleRecord{}, app.ErrConflict
		}
		if err = tx.Commit(); err != nil {
			return app.AutomationRuleRecord{}, err
		}
		return s.AutomationRule(ctx, rule.ID)
	}
	if !errors.Is(lookupErr, sql.ErrNoRows) {
		return app.AutomationRuleRecord{}, lookupErr
	}
	if expected == 0 {
		rule.Revision, revision.Number = 1, 1
		_, err = tx.ExecContext(ctx, `INSERT INTO automation_rules(id,name,head_revision_id,enabled,tombstoned,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, rule.ID, rule.Name, revision.ID, rule.Enabled, rule.Tombstoned, rule.Revision, nanos(rule.CreatedAt), nanos(rule.UpdatedAt))
	} else {
		result, updateErr := tx.ExecContext(ctx, `UPDATE automation_rules SET name=?,head_revision_id=?,enabled=?,tombstoned=?,revision=revision+1,updated_at=? WHERE id=? AND revision=?`, rule.Name, revision.ID, rule.Enabled, rule.Tombstoned, nanos(rule.UpdatedAt), rule.ID, expected)
		if updateErr != nil {
			return app.AutomationRuleRecord{}, updateErr
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return app.AutomationRuleRecord{}, app.ErrConflict
		}
		rule.Revision, revision.Number = expected+1, expected+1
	}
	if err != nil {
		return app.AutomationRuleRecord{}, classify(err)
	}
	owner, _ := json.Marshal(revision.Owner)
	delegation, _ := json.Marshal(revision.Delegation)
	condition, _ := json.Marshal(revision.Condition)
	action, _ := json.Marshal(revision.Action)
	policy, _ := json.Marshal(revision.Policy)
	dependencies, _ := json.Marshal(revision.Dependencies)
	author, _ := json.Marshal(revision.Author)
	_, err = tx.ExecContext(ctx, `INSERT INTO automation_rule_revisions(id,rule_id,number,request_scope,request_id,content_hash,owner_json,delegation_json,condition_json,action_json,policy_json,dependencies_json,author_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, revision.ID, revision.RuleID, revision.Number, requestScope(revision.Author), revision.RequestID, revision.ContentHash, owner, delegation, condition, action, policy, dependencies, author, nanos(revision.CreatedAt))
	if err != nil {
		return app.AutomationRuleRecord{}, classify(err)
	}
	if err = bumpTx(ctx, tx); err != nil {
		return app.AutomationRuleRecord{}, err
	}
	if err = tx.Commit(); err != nil {
		return app.AutomationRuleRecord{}, err
	}
	return s.AutomationRule(ctx, rule.ID)
}

func (s *Store) AutomationRule(ctx context.Context, id model.AutomationRuleID) (app.AutomationRuleRecord, error) {
	var record app.AutomationRuleRecord
	var created, updated int64
	err := s.db.QueryRowContext(ctx, `SELECT id,name,head_revision_id,enabled,tombstoned,revision,created_at,updated_at FROM automation_rules WHERE id=?`, id).Scan(&record.Rule.ID, &record.Rule.Name, &record.Rule.HeadRevisionID, &record.Rule.Enabled, &record.Rule.Tombstoned, &record.Rule.Revision, &created, &updated)
	if err != nil {
		return record, classify(err)
	}
	record.Rule.CreatedAt, record.Rule.UpdatedAt = fromNanos(created), fromNanos(updated)
	record.Head, err = s.AutomationRuleRevision(ctx, record.Rule.HeadRevisionID)
	return record, err
}

func (s *Store) AutomationRuleRevision(ctx context.Context, id model.AutomationRuleRevisionID) (model.AutomationRuleRevision, error) {
	var revision model.AutomationRuleRevision
	var owner, delegation, condition, action, policy, dependencies, author []byte
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT id,rule_id,number,request_id,content_hash,owner_json,delegation_json,condition_json,action_json,policy_json,dependencies_json,author_json,created_at FROM automation_rule_revisions WHERE id=?`, id).Scan(&revision.ID, &revision.RuleID, &revision.Number, &revision.RequestID, &revision.ContentHash, &owner, &delegation, &condition, &action, &policy, &dependencies, &author, &created)
	if err != nil {
		return revision, classify(err)
	}
	if err = unmarshalMany([][]byte{owner, delegation, condition, action, policy, dependencies, author}, []any{&revision.Owner, &revision.Delegation, &revision.Condition, &revision.Action, &revision.Policy, &revision.Dependencies, &revision.Author}); err != nil {
		return revision, err
	}
	revision.CreatedAt = fromNanos(created)
	return revision, nil
}

// requirePendingAutomationAction is called from the same SQLite transaction
// that creates a new child effect. Exact retries return before this check, so
// disabling a rule suppresses only actions that have not already been admitted.
func requirePendingAutomationAction(ctx context.Context, tx *sql.Tx, principal model.Principal, expected model.AutomationActionKind, continuationDeployment model.DeploymentID, at time.Time) error {
	if principal.Kind != model.PrincipalAutomation {
		return nil
	}
	occurrenceID := model.OccurrenceID(principal.AutomationRun)
	if err := occurrenceID.Validate(); err != nil {
		return nil
	}
	var state model.OccurrenceState
	var ruleID model.AutomationRuleID
	var eligibleAt, expiresAt int64
	var deploymentID model.DeploymentID
	var requesterJSON, actionJSON []byte
	var enabled, tombstoned bool
	err := tx.QueryRowContext(ctx, `SELECT o.rule_id,o.state,o.eligible_at,o.expires_at,o.deployment_id,o.requester_json,r.enabled,r.tombstoned,rr.action_json
		FROM automation_occurrences o
		JOIN automation_rules r ON r.id=o.rule_id
		JOIN automation_rule_revisions rr ON rr.id=o.rule_revision_id AND rr.rule_id=o.rule_id
		WHERE o.id=?`, occurrenceID).Scan(&ruleID, &state, &eligibleAt, &expiresAt, &deploymentID, &requesterJSON, &enabled, &tombstoned, &actionJSON)
	if errors.Is(err, sql.ErrNoRows) {
		// Automation principals are also used for application-owned, explicitly
		// delegated run fixtures that are not rule occurrences.
		return nil
	}
	if err != nil {
		return app.ErrConflict
	}
	var requester model.Principal
	var action model.AutomationAction
	if json.Unmarshal(requesterJSON, &requester) != nil || json.Unmarshal(actionJSON, &action) != nil || !reflect.DeepEqual(requester, principal) {
		return app.ErrConflict
	}
	if expected == model.AutomationStartWork && action.Kind == model.AutomationDeployTeam && state == model.OccurrenceAdmitted && continuationDeployment != "" && deploymentID == continuationDeployment {
		return nil
	}
	if action.Kind != expected || state != model.OccurrencePending || !enabled || tombstoned || at.Before(fromNanos(eligibleAt)) || !at.Before(fromNanos(expiresAt)) {
		return app.ErrConflict
	}
	var authorityAction model.Action
	switch expected {
	case model.AutomationStartWork:
		authorityAction = model.ActionStartWork
	case model.AutomationDeployTeam:
		authorityAction = model.ActionRunAutomation
	}
	if authorityAction != "" {
		decision, authorizeErr := authorizeTx(ctx, tx, model.AuthorityRequest{Principal: principal, Action: authorityAction, Resource: model.ResourceSelector{Kind: model.ResourceAutomationRule, AutomationRuleID: ruleID}}, at)
		if authorizeErr != nil {
			return authorizeErr
		}
		if !decision.Allowed {
			return app.ErrUnauthorized
		}
	}
	return nil
}

func (s *Store) ListAutomationRules(ctx context.Context, includeTombstoned bool) ([]model.AutomationRule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,head_revision_id,enabled,tombstoned,revision,created_at,updated_at FROM automation_rules WHERE (? OR tombstoned=0) ORDER BY name,id`, includeTombstoned)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rules []model.AutomationRule
	for rows.Next() {
		var rule model.AutomationRule
		var created, updated int64
		if err = rows.Scan(&rule.ID, &rule.Name, &rule.HeadRevisionID, &rule.Enabled, &rule.Tombstoned, &rule.Revision, &created, &updated); err != nil {
			return nil, err
		}
		rule.CreatedAt, rule.UpdatedAt = fromNanos(created), fromNanos(updated)
		rules = append(rules, rule)
	}
	return rules, rows.Err()
}

func (s *Store) MaterializeOccurrence(ctx context.Context, occurrence model.AutomationOccurrence, expectedRule model.Revision) (app.OccurrenceRecord, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.OccurrenceRecord{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	requestScopeValue := requestScope(occurrence.Requester)
	var requestedID model.OccurrenceID
	requestErr := tx.QueryRowContext(ctx, `SELECT id FROM automation_occurrences WHERE request_scope=? AND request_id=?`, requestScopeValue, occurrence.RequestID).Scan(&requestedID)
	if requestErr == nil {
		if err = tx.Commit(); err != nil {
			return app.OccurrenceRecord{}, false, err
		}
		stored, readErr := s.Occurrence(ctx, requestedID)
		if readErr != nil {
			return app.OccurrenceRecord{}, false, readErr
		}
		if requestedID != occurrence.ID || stored.Occurrence.RuleRevisionID != occurrence.RuleRevisionID || stored.Occurrence.SourceOccurrenceKey != occurrence.SourceOccurrenceKey || !reflect.DeepEqual(stored.Occurrence.Recipients, occurrence.Recipients) {
			return app.OccurrenceRecord{}, false, app.ErrConflict
		}
		return stored, true, nil
	}
	if !errors.Is(requestErr, sql.ErrNoRows) {
		return app.OccurrenceRecord{}, false, requestErr
	}
	var currentRevision model.Revision
	var head model.AutomationRuleRevisionID
	var enabled bool
	err = tx.QueryRowContext(ctx, `SELECT revision,head_revision_id,enabled FROM automation_rules WHERE id=?`, occurrence.RuleID).Scan(&currentRevision, &head, &enabled)
	if err != nil {
		return app.OccurrenceRecord{}, false, classify(err)
	}
	if currentRevision != expectedRule || head != occurrence.RuleRevisionID || !enabled {
		return app.OccurrenceRecord{}, false, app.ErrConflict
	}
	var existing model.OccurrenceID
	err = tx.QueryRowContext(ctx, `SELECT id FROM automation_occurrences WHERE rule_revision_id=? AND source_occurrence_key=?`, occurrence.RuleRevisionID, occurrence.SourceOccurrenceKey).Scan(&existing)
	if err == nil {
		if existing != occurrence.ID {
			return app.OccurrenceRecord{}, false, app.ErrConflict
		}
		if err = tx.Commit(); err != nil {
			return app.OccurrenceRecord{}, false, err
		}
		record, readErr := s.Occurrence(ctx, existing)
		return record, true, readErr
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return app.OccurrenceRecord{}, false, err
	}
	if err = applyOccurrenceOverlapTx(ctx, tx, &occurrence); err != nil {
		return app.OccurrenceRecord{}, false, err
	}
	requester, _ := json.Marshal(occurrence.Requester)
	_, err = tx.ExecContext(ctx, `INSERT INTO automation_occurrences(id,rule_id,rule_revision_id,source_occurrence_key,request_scope,request_id,requester_json,parent_occurrence_id,causal_depth,scheduled_at,event_at,eligible_at,expires_at,state,operation_id,work_run_id,deployment_id,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, occurrence.ID, occurrence.RuleID, occurrence.RuleRevisionID, occurrence.SourceOccurrenceKey, requestScopeValue, occurrence.RequestID, requester, occurrence.ParentOccurrenceID, occurrence.CausalDepth, nullableTime(occurrence.ScheduledAt), nullableTime(occurrence.EventAt), nanos(occurrence.EligibleAt), nanos(occurrence.ExpiresAt), occurrence.State, occurrence.OperationID, occurrence.WorkRunID, occurrence.DeploymentID, occurrence.Revision, nanos(occurrence.CreatedAt), nanos(occurrence.UpdatedAt))
	if err != nil {
		return app.OccurrenceRecord{}, false, classify(err)
	}
	for _, recipient := range occurrence.Recipients {
		if _, err = tx.ExecContext(ctx, `INSERT INTO automation_occurrence_recipients(occurrence_id,agent_id,disposition,operation_id,detail) VALUES(?,?,?,?,?)`, occurrence.ID, recipient.AgentID, recipient.Disposition, recipient.OperationID, recipient.Detail); err != nil {
			return app.OccurrenceRecord{}, false, classify(err)
		}
	}
	if err = bumpTx(ctx, tx); err != nil {
		return app.OccurrenceRecord{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return app.OccurrenceRecord{}, false, err
	}
	record, err := s.Occurrence(ctx, occurrence.ID)
	return record, false, err
}

func (s *Store) ScheduleCursor(ctx context.Context, ruleID model.AutomationRuleID) (time.Time, model.Revision, error) {
	var raw string
	var revision model.Revision
	err := s.db.QueryRowContext(ctx, `SELECT source_cursor,revision FROM automation_condition_state WHERE rule_id=?`, ruleID).Scan(&raw, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, 0, nil
	}
	if err != nil {
		return time.Time{}, 0, err
	}
	value, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("decode schedule cursor: %w", err)
	}
	return value.UTC(), revision, nil
}

func (s *Store) AutomationConditionState(ctx context.Context, ruleID model.AutomationRuleID) (model.AutomationConditionState, error) {
	var state model.AutomationConditionState
	var dwellSince, cooldownUntil, debounceAt sql.NullInt64
	var debouncePayload []byte
	var observed int64
	err := s.db.QueryRowContext(ctx, `SELECT rule_id,source_cursor,dwell_episode_id,dwell_since,cooldown_until,debounce_at,debounce_payload,observed_at,revision FROM automation_condition_state WHERE rule_id=?`, ruleID).Scan(&state.RuleID, &state.SourceCursor, &state.DwellEpisodeID, &dwellSince, &cooldownUntil, &debounceAt, &debouncePayload, &observed, &state.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return model.AutomationConditionState{RuleID: ruleID}, nil
	}
	if err != nil {
		return state, err
	}
	state.ObservedAt = fromNanos(observed)
	state.DebouncePayload = debouncePayload
	if dwellSince.Valid {
		value := fromNanos(dwellSince.Int64)
		state.DwellSince = &value
	}
	if cooldownUntil.Valid {
		value := fromNanos(cooldownUntil.Int64)
		state.CooldownUntil = &value
	}
	if debounceAt.Valid {
		value := fromNanos(debounceAt.Int64)
		state.DebounceAt = &value
	}
	return state, nil
}

func (s *Store) AdvanceTrigger(ctx context.Context, state model.AutomationConditionState, occurrence *model.AutomationOccurrence, expectedRule, expectedState model.Revision) (app.OccurrenceRecord, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.OccurrenceRecord{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var currentRule model.Revision
	var head model.AutomationRuleRevisionID
	var enabled bool
	if err = tx.QueryRowContext(ctx, `SELECT revision,head_revision_id,enabled FROM automation_rules WHERE id=?`, state.RuleID).Scan(&currentRule, &head, &enabled); err != nil {
		return app.OccurrenceRecord{}, false, classify(err)
	}
	if currentRule != expectedRule || !enabled || occurrence != nil && head != occurrence.RuleRevisionID {
		return app.OccurrenceRecord{}, false, app.ErrConflict
	}
	var currentState model.Revision
	stateErr := tx.QueryRowContext(ctx, `SELECT revision FROM automation_condition_state WHERE rule_id=?`, state.RuleID).Scan(&currentState)
	var dwellSince, cooldownUntil, debounceAt any
	if state.DwellSince != nil {
		dwellSince = nanos(*state.DwellSince)
	}
	if state.CooldownUntil != nil {
		cooldownUntil = nanos(*state.CooldownUntil)
	}
	if state.DebounceAt != nil {
		debounceAt = nanos(*state.DebounceAt)
	}
	if errors.Is(stateErr, sql.ErrNoRows) {
		if expectedState != 0 {
			return app.OccurrenceRecord{}, false, app.ErrConflict
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO automation_condition_state(rule_id,source_cursor,dwell_episode_id,dwell_since,cooldown_until,debounce_at,debounce_payload,observed_at,revision) VALUES(?,?,?,?,?,?,?,?,1)`, state.RuleID, state.SourceCursor, state.DwellEpisodeID, dwellSince, cooldownUntil, debounceAt, state.DebouncePayload, nanos(state.ObservedAt))
	} else if stateErr != nil {
		return app.OccurrenceRecord{}, false, stateErr
	} else {
		if currentState != expectedState {
			return app.OccurrenceRecord{}, false, app.ErrConflict
		}
		_, err = tx.ExecContext(ctx, `UPDATE automation_condition_state SET source_cursor=?,dwell_episode_id=?,dwell_since=?,cooldown_until=?,debounce_at=?,debounce_payload=?,observed_at=?,revision=revision+1 WHERE rule_id=? AND revision=?`, state.SourceCursor, state.DwellEpisodeID, dwellSince, cooldownUntil, debounceAt, state.DebouncePayload, nanos(state.ObservedAt), state.RuleID, expectedState)
	}
	if err != nil {
		return app.OccurrenceRecord{}, false, classify(err)
	}
	if occurrence != nil {
		if err = applyOccurrenceOverlapTx(ctx, tx, occurrence); err != nil {
			return app.OccurrenceRecord{}, false, err
		}
		requester, _ := json.Marshal(occurrence.Requester)
		_, err = tx.ExecContext(ctx, `INSERT INTO automation_occurrences(id,rule_id,rule_revision_id,source_occurrence_key,request_scope,request_id,requester_json,parent_occurrence_id,causal_depth,scheduled_at,event_at,eligible_at,expires_at,state,operation_id,work_run_id,deployment_id,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, occurrence.ID, occurrence.RuleID, occurrence.RuleRevisionID, occurrence.SourceOccurrenceKey, requestScope(occurrence.Requester), occurrence.RequestID, requester, occurrence.ParentOccurrenceID, occurrence.CausalDepth, nullableTime(occurrence.ScheduledAt), nullableTime(occurrence.EventAt), nanos(occurrence.EligibleAt), nanos(occurrence.ExpiresAt), occurrence.State, occurrence.OperationID, occurrence.WorkRunID, occurrence.DeploymentID, occurrence.Revision, nanos(occurrence.CreatedAt), nanos(occurrence.UpdatedAt))
		if err != nil {
			return app.OccurrenceRecord{}, false, classify(err)
		}
		for _, recipient := range occurrence.Recipients {
			if _, err = tx.ExecContext(ctx, `INSERT INTO automation_occurrence_recipients(occurrence_id,agent_id,disposition,operation_id,detail) VALUES(?,?,?,?,?)`, occurrence.ID, recipient.AgentID, recipient.Disposition, recipient.OperationID, recipient.Detail); err != nil {
				return app.OccurrenceRecord{}, false, classify(err)
			}
		}
	}
	if err = bumpTx(ctx, tx); err != nil {
		return app.OccurrenceRecord{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return app.OccurrenceRecord{}, false, err
	}
	if occurrence == nil {
		return app.OccurrenceRecord{}, false, nil
	}
	record, err := s.Occurrence(ctx, occurrence.ID)
	return record, false, err
}

// AdvanceSchedule owns the schedule cursor and optional occurrence insertion in
// one transaction. A nil occurrence records an intentionally skipped set of
// missed ticks; a non-nil occurrence is the sole coalesced/admitted tick.
func (s *Store) AdvanceSchedule(ctx context.Context, occurrence *model.AutomationOccurrence, ruleID model.AutomationRuleID, expectedRule, expectedCursor model.Revision, consideredThrough time.Time) (app.OccurrenceRecord, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.OccurrenceRecord{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var currentRule model.Revision
	var head model.AutomationRuleRevisionID
	var enabled bool
	if err = tx.QueryRowContext(ctx, `SELECT revision,head_revision_id,enabled FROM automation_rules WHERE id=?`, ruleID).Scan(&currentRule, &head, &enabled); err != nil {
		return app.OccurrenceRecord{}, false, classify(err)
	}
	if currentRule != expectedRule || !enabled || occurrence != nil && head != occurrence.RuleRevisionID {
		return app.OccurrenceRecord{}, false, app.ErrConflict
	}
	var currentCursor string
	var currentCursorRevision model.Revision
	cursorErr := tx.QueryRowContext(ctx, `SELECT source_cursor,revision FROM automation_condition_state WHERE rule_id=?`, ruleID).Scan(&currentCursor, &currentCursorRevision)
	if errors.Is(cursorErr, sql.ErrNoRows) {
		if expectedCursor != 0 {
			return app.OccurrenceRecord{}, false, app.ErrConflict
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO automation_condition_state(rule_id,source_cursor,observed_at,revision) VALUES(?,?,?,1)`, ruleID, consideredThrough.UTC().Format(time.RFC3339Nano), nanos(consideredThrough))
	} else if cursorErr != nil {
		return app.OccurrenceRecord{}, false, cursorErr
	} else {
		if currentCursorRevision != expectedCursor {
			return app.OccurrenceRecord{}, false, app.ErrConflict
		}
		_, err = tx.ExecContext(ctx, `UPDATE automation_condition_state SET source_cursor=?,observed_at=?,revision=revision+1 WHERE rule_id=? AND revision=?`, consideredThrough.UTC().Format(time.RFC3339Nano), nanos(consideredThrough), ruleID, expectedCursor)
	}
	if err != nil {
		return app.OccurrenceRecord{}, false, classify(err)
	}
	if occurrence == nil {
		if err = bumpTx(ctx, tx); err != nil {
			return app.OccurrenceRecord{}, false, err
		}
		return app.OccurrenceRecord{}, false, tx.Commit()
	}
	if err = applyOccurrenceOverlapTx(ctx, tx, occurrence); err != nil {
		return app.OccurrenceRecord{}, false, err
	}
	requester, _ := json.Marshal(occurrence.Requester)
	requestScopeValue := requestScope(occurrence.Requester)
	_, err = tx.ExecContext(ctx, `INSERT INTO automation_occurrences(id,rule_id,rule_revision_id,source_occurrence_key,request_scope,request_id,requester_json,parent_occurrence_id,causal_depth,scheduled_at,event_at,eligible_at,expires_at,state,operation_id,work_run_id,deployment_id,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, occurrence.ID, occurrence.RuleID, occurrence.RuleRevisionID, occurrence.SourceOccurrenceKey, requestScopeValue, occurrence.RequestID, requester, occurrence.ParentOccurrenceID, occurrence.CausalDepth, nullableTime(occurrence.ScheduledAt), nullableTime(occurrence.EventAt), nanos(occurrence.EligibleAt), nanos(occurrence.ExpiresAt), occurrence.State, occurrence.OperationID, occurrence.WorkRunID, occurrence.DeploymentID, occurrence.Revision, nanos(occurrence.CreatedAt), nanos(occurrence.UpdatedAt))
	if err != nil {
		return app.OccurrenceRecord{}, false, classify(err)
	}
	for _, recipient := range occurrence.Recipients {
		if _, err = tx.ExecContext(ctx, `INSERT INTO automation_occurrence_recipients(occurrence_id,agent_id,disposition,operation_id,detail) VALUES(?,?,?,?,?)`, occurrence.ID, recipient.AgentID, recipient.Disposition, recipient.OperationID, recipient.Detail); err != nil {
			return app.OccurrenceRecord{}, false, classify(err)
		}
	}
	if err = bumpTx(ctx, tx); err != nil {
		return app.OccurrenceRecord{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return app.OccurrenceRecord{}, false, err
	}
	record, err := s.Occurrence(ctx, occurrence.ID)
	return record, false, err
}

func applyOccurrenceOverlapTx(ctx context.Context, tx *sql.Tx, occurrence *model.AutomationOccurrence) error {
	var policyJSON []byte
	if err := tx.QueryRowContext(ctx, `SELECT policy_json FROM automation_rule_revisions WHERE id=?`, occurrence.RuleRevisionID).Scan(&policyJSON); err != nil {
		return classify(err)
	}
	var policy model.OccurrencePolicy
	if err := json.Unmarshal(policyJSON, &policy); err != nil {
		return err
	}
	var active uint32
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM automation_occurrences WHERE rule_id=? AND id<>? AND state IN (?,?,?,?)`, occurrence.RuleID, occurrence.ID, model.OccurrencePending, model.OccurrenceAdmitted, model.OccurrencePartial, model.OccurrenceUncertain).Scan(&active); err != nil {
		return err
	}
	switch policy.Overlap {
	case model.OverlapForbid:
		if active > 0 {
			occurrence.State = model.OccurrenceDenied
			for i := range occurrence.Recipients {
				occurrence.Recipients[i].Disposition, occurrence.Recipients[i].Detail = model.RecipientSkipped, "forbidden by active occurrence"
			}
		}
	case model.OverlapAllow:
		if active >= policy.MaxActive {
			occurrence.State = model.OccurrenceParked
		}
	case model.OverlapReplace:
		if active > 0 {
			occurrence.State = model.OccurrenceParked
		}
	}
	return nil
}

func (s *Store) Occurrence(ctx context.Context, id model.OccurrenceID) (app.OccurrenceRecord, error) {
	var record app.OccurrenceRecord
	var scheduled, event sql.NullInt64
	var requester []byte
	var eligible, expires, created, updated int64
	err := s.db.QueryRowContext(ctx, `SELECT id,rule_id,rule_revision_id,source_occurrence_key,request_id,requester_json,parent_occurrence_id,causal_depth,scheduled_at,event_at,eligible_at,expires_at,state,operation_id,work_run_id,deployment_id,revision,created_at,updated_at FROM automation_occurrences WHERE id=?`, id).Scan(&record.Occurrence.ID, &record.Occurrence.RuleID, &record.Occurrence.RuleRevisionID, &record.Occurrence.SourceOccurrenceKey, &record.Occurrence.RequestID, &requester, &record.Occurrence.ParentOccurrenceID, &record.Occurrence.CausalDepth, &scheduled, &event, &eligible, &expires, &record.Occurrence.State, &record.Occurrence.OperationID, &record.Occurrence.WorkRunID, &record.Occurrence.DeploymentID, &record.Occurrence.Revision, &created, &updated)
	if err != nil {
		return record, classify(err)
	}
	if scheduled.Valid {
		record.Occurrence.ScheduledAt = fromNanos(scheduled.Int64)
	}
	if event.Valid {
		record.Occurrence.EventAt = fromNanos(event.Int64)
	}
	if err = json.Unmarshal(requester, &record.Occurrence.Requester); err != nil {
		return record, err
	}
	record.Occurrence.EligibleAt, record.Occurrence.ExpiresAt = fromNanos(eligible), fromNanos(expires)
	record.Occurrence.CreatedAt, record.Occurrence.UpdatedAt = fromNanos(created), fromNanos(updated)
	rows, err := s.db.QueryContext(ctx, `SELECT agent_id,disposition,operation_id,detail FROM automation_occurrence_recipients WHERE occurrence_id=? ORDER BY rowid`, id)
	if err != nil {
		return record, err
	}
	defer rows.Close()
	for rows.Next() {
		var recipient model.OccurrenceRecipient
		if err = rows.Scan(&recipient.AgentID, &recipient.Disposition, &recipient.OperationID, &recipient.Detail); err != nil {
			return record, err
		}
		record.Occurrence.Recipients = append(record.Occurrence.Recipients, recipient)
	}
	return record, rows.Err()
}

func (s *Store) OccurrencesForRule(ctx context.Context, id model.AutomationRuleID) ([]app.OccurrenceRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM automation_occurrences WHERE rule_id=? ORDER BY created_at,id`, id)
	if err != nil {
		return nil, err
	}
	var ids []model.OccurrenceID
	for rows.Next() {
		var occurrenceID model.OccurrenceID
		if err = rows.Scan(&occurrenceID); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, occurrenceID)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	records := make([]app.OccurrenceRecord, 0, len(ids))
	for _, occurrenceID := range ids {
		record, readErr := s.Occurrence(ctx, occurrenceID)
		if readErr != nil {
			return nil, readErr
		}
		records = append(records, record)
	}
	return records, nil
}

func (s *Store) PendingOccurrences(ctx context.Context) ([]app.OccurrenceRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM automation_occurrences WHERE state IN (?,?,?,?,?) ORDER BY eligible_at,id`, model.OccurrencePending, model.OccurrenceAdmitted, model.OccurrencePartial, model.OccurrenceParked, model.OccurrenceUncertain)
	if err != nil {
		return nil, err
	}
	var ids []model.OccurrenceID
	for rows.Next() {
		var id model.OccurrenceID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	records := make([]app.OccurrenceRecord, 0, len(ids))
	for _, id := range ids {
		record, readErr := s.Occurrence(ctx, id)
		if readErr != nil {
			return nil, readErr
		}
		records = append(records, record)
	}
	return records, nil
}

func (s *Store) UpdateOccurrence(ctx context.Context, id model.OccurrenceID, expected model.Revision, state model.OccurrenceState, operationID model.OperationID, workRunID model.WorkRunID, deploymentID model.DeploymentID, recipients []model.OccurrenceRecipient, at time.Time) (app.OccurrenceRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.OccurrenceRecord{}, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE automation_occurrences SET state=?,operation_id=?,work_run_id=?,deployment_id=?,revision=revision+1,updated_at=? WHERE id=? AND revision=?`, state, operationID, workRunID, deploymentID, nanos(at), id, expected)
	if err != nil {
		return app.OccurrenceRecord{}, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return app.OccurrenceRecord{}, app.ErrConflict
	}
	for _, recipient := range recipients {
		if _, err = tx.ExecContext(ctx, `UPDATE automation_occurrence_recipients SET disposition=?,operation_id=?,detail=? WHERE occurrence_id=? AND agent_id=?`, recipient.Disposition, recipient.OperationID, recipient.Detail, id, recipient.AgentID); err != nil {
			return app.OccurrenceRecord{}, err
		}
	}
	if err = bumpTx(ctx, tx); err != nil {
		return app.OccurrenceRecord{}, err
	}
	if err = tx.Commit(); err != nil {
		return app.OccurrenceRecord{}, err
	}
	return s.Occurrence(ctx, id)
}

func nullableJSON(value any, encoded []byte) any {
	if value == nil || reflect.ValueOf(value).IsNil() {
		return nil
	}
	return encoded
}
func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return nanos(value)
}
func unmarshalMany(values [][]byte, targets []any) error {
	for i := range values {
		if err := json.Unmarshal(values[i], targets[i]); err != nil {
			return err
		}
	}
	return nil
}
