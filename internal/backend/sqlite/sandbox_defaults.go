package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func (s *Store) SandboxDefaults(ctx context.Context) (model.SandboxDefaults, error) {
	return readSandboxDefaults(ctx, s.db)
}

func readSandboxDefaults(ctx context.Context, q groupReader) (model.SandboxDefaults, error) {
	var out model.SandboxDefaults
	var data []byte
	err := q.QueryRowContext(ctx, `SELECT record FROM sandbox_defaults WHERE id=1`).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(data, &out)
	return out, err
}

func (s *Store) SaveSandboxDefaults(ctx context.Context, in app.SaveSandboxDefaultsRequest) (model.SandboxDefaults, error) {
	var out model.SandboxDefaults
	if err := app.ValidateSandboxDefaults(in); err != nil {
		return out, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	intent, err := json.Marshal(struct {
		Global   model.SandboxProfileID
		Groups   map[model.GroupID]model.SandboxProfileID
		Revision model.Revision
	}{in.Global, in.Groups, in.ExpectedRevision})
	if err != nil {
		return out, err
	}
	var prior, data []byte
	err = tx.QueryRowContext(ctx, `SELECT intent,result FROM sandbox_defaults_requests WHERE scope=? AND request_id=?`, requestScope(in.Context.Principal), in.Context.RequestID).Scan(&prior, &data)
	if err == nil {
		if string(prior) != string(intent) {
			return out, app.ErrConflict
		}
		err = json.Unmarshal(data, &out)
		return out, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	err = tx.QueryRowContext(ctx, `SELECT record FROM sandbox_defaults WHERE id=1`).Scan(&data)
	if err == nil {
		if err = json.Unmarshal(data, &out); err != nil {
			return out, err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	if out.Revision != in.ExpectedRevision {
		return out, app.ErrConflict
	}
	requireProfile := func(id model.SandboxProfileID) error {
		var archived bool
		err := tx.QueryRowContext(ctx, `SELECT archived FROM sandbox_profiles WHERE id=?`, id).Scan(&archived)
		if err != nil {
			return classify(err)
		}
		if archived {
			return app.ErrConflict
		}
		return nil
	}
	if in.Global != "" {
		if err = requireProfile(in.Global); err != nil {
			return out, err
		}
	}
	for group, profile := range in.Groups {
		var id string
		if err = tx.QueryRowContext(ctx, `SELECT id FROM groups WHERE id=? AND tombstoned=0`, group).Scan(&id); err != nil {
			return out, classify(err)
		}
		if err = requireProfile(profile); err != nil {
			return out, err
		}
	}
	out = model.SandboxDefaults{Global: in.Global, Groups: in.Groups, Revision: out.Revision + 1}
	data, err = json.Marshal(out)
	if err != nil {
		return out, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO sandbox_defaults(id,record) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET record=excluded.record`, data); err != nil {
		return out, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO sandbox_defaults_requests(scope,request_id,intent,result) VALUES(?,?,?,?)`, requestScope(in.Context.Principal), in.Context.RequestID, intent, data); err != nil {
		return out, err
	}
	if err = bumpTx(ctx, tx); err != nil {
		return out, err
	}
	return out, tx.Commit()
}

// Lifecycle cleanup drops assignments to removed registry entries atomically.
// Existing execution snapshots and explicit agent choices are not rewritten.
func clearSandboxDefaultAssignments(ctx context.Context, tx *sql.Tx, profile model.SandboxProfileID, group model.GroupID) error {
	defaults, err := readSandboxDefaults(ctx, tx)
	if err != nil {
		return err
	}
	changed := false
	if profile != "" && defaults.Global == profile {
		defaults.Global = ""
		changed = true
	}
	for id, selected := range defaults.Groups {
		if id == group || profile != "" && selected == profile {
			delete(defaults.Groups, id)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	defaults.Revision++
	data, err := json.Marshal(defaults)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE sandbox_defaults SET record=? WHERE id=1`, data)
	return err
}

func (s *Store) SandboxGroupsForAgent(ctx context.Context, id model.AgentID) ([]model.GroupID, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT g.id FROM group_members m JOIN groups g ON g.id=m.group_id WHERE m.agent_id=? AND g.tombstoned=0 ORDER BY g.id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var groups []model.GroupID
	for rows.Next() {
		var group model.GroupID
		if err = rows.Scan(&group); err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

// SandboxGroupDisbanded distinguishes retired provenance from an invalid ID.
func (s *Store) SandboxGroupDisbanded(ctx context.Context, id model.GroupID) (bool, error) {
	var removed bool
	err := s.db.QueryRowContext(ctx, `SELECT tombstoned FROM groups WHERE id=?`, id).Scan(&removed)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return removed, err
}
