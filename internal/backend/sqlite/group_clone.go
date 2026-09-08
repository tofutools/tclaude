package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"reflect"
	"time"
)

const groupCloneSchema = `CREATE TABLE IF NOT EXISTS group_clone_requests(scope TEXT NOT NULL,request_id TEXT NOT NULL,intent BLOB NOT NULL,result BLOB NOT NULL,PRIMARY KEY(scope,request_id));`

func groupCloneIntent(in app.CloneGroupRequest) []byte {
	in.Context = app.RequestContext{}
	b, _ := json.Marshal(in)
	return b
}
func findGroupClone(ctx context.Context, q groupReader, in app.CloneGroupRequest) (app.CloneGroupResult, bool, error) {
	var out app.CloneGroupResult
	if in.Context.Principal.Kind != model.PrincipalOperator {
		return out, false, app.ErrUnauthorized
	}
	var intent, result []byte
	err := q.QueryRowContext(ctx, `SELECT intent,result FROM group_clone_requests WHERE scope=? AND request_id=?`, requestScope(in.Context.Principal), in.Context.RequestID).Scan(&intent, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return out, false, nil
	}
	if err != nil {
		return out, false, err
	}
	if !bytes.Equal(intent, groupCloneIntent(in)) {
		return out, false, app.ErrConflict
	}
	if err = json.Unmarshal(result, &out); err != nil {
		return out, false, err
	}
	out.Repeated = true
	return out, true, nil
}
func (s *Store) FindGroupClone(ctx context.Context, in app.CloneGroupRequest) (app.CloneGroupResult, bool, error) {
	return findGroupClone(ctx, s.db, in)
}
func (s *Store) AdmitGroupClone(ctx context.Context, in app.CloneGroupRequest, clones []model.Agent, at time.Time) (app.CloneGroupResult, error) {
	var out app.CloneGroupResult
	if in.Context.Principal.Kind != model.PrincipalOperator {
		return out, app.ErrUnauthorized
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	if prior, found, err := findGroupClone(ctx, tx, in); found || err != nil {
		return prior, err
	}
	source, err := readGroup(ctx, tx, in.SourceID)
	if err != nil {
		return out, err
	}
	if source.Revision != in.ExpectedGroupRevision {
		return out, app.ErrConflict
	}
	copied := map[model.AgentID]model.Agent{}
	for _, clone := range clones {
		if _, exists := copied[clone.CloneSourceAgentID]; exists {
			return out, app.ErrInvalid
		}
		copied[clone.CloneSourceAgentID] = clone
	}
	members := map[model.AgentID]model.AgentID{}
	var targetMembers []model.AgentID
	if in.CopyMembers {
		if len(source.Members) != len(in.ExpectedMembers) {
			return out, app.ErrConflict
		}
		for _, id := range source.Members {
			agent, err := scanAgent(tx.QueryRowContext(ctx, agentSelect+` WHERE id=?`, id))
			if err != nil {
				return out, err
			}
			if agent.Revision != in.ExpectedMembers[id] {
				return out, app.ErrConflict
			}
			if agent.Lifecycle != model.AgentActive {
				continue
			}
			expected := agent.Desired
			expected.HostSandbox = model.SandboxInGroup(expected.HostSandbox, in.ID)
			clone, ok := copied[id]
			if !ok || clone.ID.Validate() != nil || clone.ID == id || clone.Name != agent.Name || clone.TaskReference != agent.TaskReference || !clone.Desired.Equal(expected) || clone.Notifications != agent.Notifications || !reflect.DeepEqual(clone.ConfigurationProfile, agent.ConfigurationProfile) || clone.PrimaryExecutionID != "" || clone.ParentAgentID != "" || clone.Lifecycle != model.AgentActive || clone.Revision != 1 {
				return out, app.ErrConflict
			}
			if err = createAgentTx(ctx, tx, clone); err != nil {
				return out, err
			}
			members[id] = clone.ID
			targetMembers = append(targetMembers, clone.ID)
		}
	}
	if len(members) != len(clones) || !model.ValidGroupCapacity(in.MaxActiveMembers) || (in.MaxActiveMembers > 0 && int64(len(members)) > in.MaxActiveMembers) {
		return out, app.ErrConflict
	}
	group := model.Group{ID: in.ID, Name: in.Name, Members: targetMembers, Details: source.Details, MaxActiveMembers: in.MaxActiveMembers, Revision: 1, CreatedAt: at, UpdatedAt: at}
	if _, err = tx.ExecContext(ctx, `INSERT INTO groups(id,name,owner_agent_id,revision,created_at,updated_at) VALUES(?,?,'',1,?,?)`, group.ID, group.Name, nanos(at), nanos(at)); err != nil {
		return out, classify(err)
	}
	for i, id := range group.Members {
		if _, err = tx.ExecContext(ctx, `INSERT INTO group_members(group_id,agent_id,position) VALUES(?,?,?)`, group.ID, id, i); err != nil {
			return out, classify(err)
		}
	}
	if group.Details != nil {
		data, err := json.Marshal(group.Details)
		if err != nil {
			return out, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO group_details(group_id,record) VALUES(?,?)`, group.ID, data); err != nil {
			return out, err
		}
	}
	if group.MaxActiveMembers > 0 {
		if _, err = tx.ExecContext(ctx, `INSERT INTO group_capacity(group_id,max_active_members) VALUES(?,?)`, group.ID, group.MaxActiveMembers); err != nil {
			return out, err
		}
	}
	if in.CopyDefault {
		defaults, err := readGroupConfiguration(ctx, tx, in.SourceID)
		if err != nil {
			return out, err
		}
		if defaults.Revision != in.ExpectedDefaultRevision || defaults.Profile == nil && len(defaults.Environment) == 0 {
			return out, app.ErrConflict
		}
		if err = requireActiveConfigurationProfileTx(ctx, tx, defaults.Profile); err != nil {
			return out, err
		}
		var ref model.ConfigurationProfileRef
		if defaults.Profile != nil {
			ref = *defaults.Profile
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO group_configurations(environment_json,group_id,profile_id,revision_id,content_hash,revision,updated_at) VALUES(?,?,?,?,?,1,?)`, environmentJSON(defaults.Environment), group.ID, ref.ProfileID, ref.RevisionID, ref.ContentHash, nanos(at)); err != nil {
			return out, err
		}
	}
	out = app.CloneGroupResult{Group: group, Members: members}
	data, err := json.Marshal(out)
	if err != nil {
		return out, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO group_clone_requests(scope,request_id,intent,result) VALUES(?,?,?,?)`, requestScope(in.Context.Principal), in.Context.RequestID, groupCloneIntent(in), data); err != nil {
		return out, err
	}
	if err = bumpTx(ctx, tx); err != nil {
		return out, err
	}
	return out, tx.Commit()
}
