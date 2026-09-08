package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

const shellRequestSchema = `CREATE TABLE IF NOT EXISTS shell_requests(scope TEXT NOT NULL,request_id TEXT NOT NULL,operation_id TEXT NOT NULL REFERENCES operations(id),intent BLOB NOT NULL,PRIMARY KEY(scope,request_id));`

func shellIntent(in app.StartShellRequest) []byte {
	data, _ := json.Marshal(struct {
		WorkspaceID model.WorkspaceID
		Revision    model.Revision
		Sandbox     model.SandboxMode
		Group       *model.ShellGroupSelection
		Environment model.Environment       `json:",omitempty"`
		HostSandbox *model.SandboxSelection `json:",omitempty"`
	}{in.WorkspaceID, in.ExpectedRevision, in.Sandbox, in.Group, in.Environment, in.HostSandbox})
	return data
}

func findShellAdmission(ctx context.Context, tx *sql.Tx, in app.StartShellRequest, at time.Time) (app.AdmissionResult, bool, error) {
	var intent []byte
	var id model.OperationID
	err := tx.QueryRowContext(ctx, `SELECT operation_id,intent FROM shell_requests WHERE scope=? AND request_id=?`, requestScope(in.Context.Principal), in.Context.RequestID).Scan(&id, &intent)
	if errors.Is(err, sql.ErrNoRows) {
		return app.AdmissionResult{}, false, nil
	}
	if err != nil {
		return app.AdmissionResult{}, false, err
	}
	if !bytes.Equal(intent, shellIntent(in)) {
		return app.AdmissionResult{}, true, app.ErrConflict
	}
	authority, found, err := operationAuthority(ctx, tx, id)
	if err != nil {
		return app.AdmissionResult{}, true, err
	}
	if !found {
		return app.AdmissionResult{}, true, app.ErrUnauthorized
	}
	authority.Principal = in.Context.Principal
	decision, err := authorizeTx(ctx, tx, authority, at)
	if err != nil {
		return app.AdmissionResult{}, true, err
	}
	if !decision.Allowed {
		return app.AdmissionResult{}, true, app.ErrUnauthorized
	}
	prior, found, err := admissionByRequest(ctx, tx, model.Operation{Principal: in.Context.Principal, RequestID: in.Context.RequestID, Kind: model.OperationStartShell}, "", false)
	if err == nil && (!found || prior.Operation.ID != id) {
		err = app.ErrConflict
	}
	return prior, true, err
}

func (s *Store) FindShellAdmission(ctx context.Context, in app.StartShellRequest, at time.Time) (app.AdmissionResult, bool, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return app.AdmissionResult{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	return findShellAdmission(ctx, tx, in, at)
}

func validateShellRequest(ctx context.Context, tx *sql.Tx, in app.ShellAdmission) error {
	if in.Request == nil { // Compatibility for internal callers that have no authored group/environment.
		if in.Execution.Spec.ShellGroup != nil || len(in.Execution.Spec.Environment) != 0 || in.Execution.Spec.HostSandbox != nil {
			return app.ErrInvalid
		}
		return nil
	}
	req := in.Request
	if req.Context.Principal != in.Operation.Principal || req.Context.RequestID != in.Operation.RequestID || req.WorkspaceID != in.WorkspaceUse.WorkspaceID || req.ExpectedRevision != in.WorkspaceRevision || req.Sandbox != in.Execution.Spec.Sandbox || !model.SameSandboxSelection(req.HostSandbox, in.Execution.Spec.HostSandbox) || !model.SameSandboxSelection(req.HostSandbox, in.Authority.RequestedHostSandbox) || !reflect.DeepEqual(req.Group, in.Execution.Spec.ShellGroup) {
		return app.ErrConflict
	}
	environment := req.Environment.Clone()
	if req.Group != nil {
		if req.Context.Principal.Kind != model.PrincipalOperator {
			return app.ErrUnauthorized
		}
		var revision model.Revision
		if err := tx.QueryRowContext(ctx, `SELECT revision FROM groups WHERE id=? AND tombstoned=0`, req.Group.GroupID).Scan(&revision); err != nil {
			return classify(err)
		}
		if revision != req.Group.Revision {
			return app.ErrConflict
		}
		config, err := readGroupConfiguration(ctx, tx, req.Group.GroupID)
		if err != nil {
			return err
		}
		if config.Revision != req.Group.ConfigurationRevision {
			return app.ErrConflict
		}
		environment, err = model.MergeEnvironment(config.Environment, req.Environment)
		if err != nil {
			return app.ErrInvalid
		}
	}
	if environment.Validate() != nil || !environment.Equal(in.Execution.Spec.Environment) {
		return app.ErrConflict
	}
	if in.Authority.RequestedEnvironment == nil || !environment.Equal(*in.Authority.RequestedEnvironment) {
		return app.ErrUnauthorized
	}
	return nil
}

func shellGroupJSON(group *model.ShellGroupSelection) any {
	if group == nil {
		return nil
	}
	data, _ := json.Marshal(group)
	return data
}
