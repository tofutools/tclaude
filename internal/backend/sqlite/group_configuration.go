package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"time"
)

const groupConfigurationSchema = `CREATE TABLE IF NOT EXISTS group_configurations(group_id TEXT PRIMARY KEY REFERENCES groups(id),profile_id TEXT NOT NULL,revision_id TEXT NOT NULL,content_hash TEXT NOT NULL,revision INTEGER NOT NULL,updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS group_member_requests(scope TEXT NOT NULL,request_id TEXT NOT NULL,intent BLOB NOT NULL,result BLOB NOT NULL,PRIMARY KEY(scope,request_id));`

func readGroupConfiguration(ctx context.Context, q groupReader, id model.GroupID) (model.GroupConfiguration, error) {
	var out model.GroupConfiguration
	var ref model.ConfigurationProfileRef
	var updated int64
	var environment []byte
	err := q.QueryRowContext(ctx, `SELECT g.id,COALESCE(c.profile_id,''),COALESCE(c.revision_id,''),COALESCE(c.content_hash,''),COALESCE(c.revision,0),COALESCE(c.updated_at,0),COALESCE(c.environment_json,'{}') FROM groups g LEFT JOIN group_configurations c ON c.group_id=g.id WHERE g.id=? AND g.tombstoned=0`, id).Scan(&out.GroupID, &ref.ProfileID, &ref.RevisionID, &ref.ContentHash, &out.Revision, &updated, &environment)
	if err != nil {
		return out, classify(err)
	}
	if err := json.Unmarshal(environment, &out.Environment); err != nil {
		return out, err
	}
	if ref.ProfileID != "" {
		out.Profile = &ref
	}
	if updated != 0 {
		out.UpdatedAt = fromNanos(updated)
	}
	return out, nil
}
func (s *Store) GroupConfiguration(ctx context.Context, id model.GroupID) (model.GroupConfiguration, error) {
	return readGroupConfiguration(ctx, s.db, id)
}
func (s *Store) SetGroupConfiguration(ctx context.Context, in app.SetGroupConfigurationRequest, at time.Time) (model.GroupConfiguration, error) {
	var out model.GroupConfiguration
	if in.Principal.Kind != model.PrincipalOperator {
		return out, app.ErrUnauthorized
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	out, err = readGroupConfiguration(ctx, tx, in.GroupID)
	if err != nil {
		return out, err
	}
	if out.Revision != in.ExpectedRevision {
		return out, app.ErrConflict
	}
	if err = requireActiveConfigurationProfileTx(ctx, tx, in.Profile); err != nil {
		return out, err
	}
	var ref model.ConfigurationProfileRef
	if in.Profile != nil {
		ref = *in.Profile
	}
	if in.Environment.Validate() != nil {
		return out, app.ErrInvalid
	}
	out.Environment = in.Environment.Clone()
	out.Profile = in.Profile
	out.Revision++
	out.UpdatedAt = at
	if _, err = tx.ExecContext(ctx, `INSERT INTO group_configurations(environment_json,group_id,profile_id,revision_id,content_hash,revision,updated_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(group_id) DO UPDATE SET environment_json=excluded.environment_json,profile_id=excluded.profile_id,revision_id=excluded.revision_id,content_hash=excluded.content_hash,revision=excluded.revision,updated_at=excluded.updated_at`, environmentJSON(in.Environment), in.GroupID, ref.ProfileID, ref.RevisionID, ref.ContentHash, out.Revision, nanos(at)); err != nil {
		return out, err
	}
	if err = bumpTx(ctx, tx); err != nil {
		return out, err
	}
	return out, tx.Commit()
}
func groupMemberIntent(in app.CreateGroupMemberRequest) []byte {
	data, _ := json.Marshal(struct {
		Environment                    model.Environment `json:",omitempty"`
		GroupID                        model.GroupID
		ID                             model.AgentID
		Name                           string
		GroupRevision, DefaultRevision model.Revision
		Labels                         *model.AgentDisplayLabels `json:",omitempty"`
	}{in.Environment, in.GroupID, in.ID, in.Name, in.ExpectedGroupRevision, in.ExpectedDefaultRevision, in.Labels})
	return data
}
func findGroupMemberAdmission(ctx context.Context, q groupReader, in app.CreateGroupMemberRequest) (app.GroupMemberResult, bool, error) {
	var out app.GroupMemberResult
	var prior, data []byte
	err := q.QueryRowContext(ctx, `SELECT intent,result FROM group_member_requests WHERE scope=? AND request_id=?`, requestScope(in.Context.Principal), in.Context.RequestID).Scan(&prior, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return out, false, nil
	}
	if err != nil {
		return out, false, err
	}
	if string(prior) != string(groupMemberIntent(in)) {
		return out, true, app.ErrConflict
	}
	err = json.Unmarshal(data, &out)
	out.Repeated = true
	return out, true, err
}
func (s *Store) FindGroupMemberAdmission(ctx context.Context, in app.CreateGroupMemberRequest) (app.GroupMemberResult, bool, error) {
	if in.Context.Principal.Kind != model.PrincipalOperator {
		return app.GroupMemberResult{}, false, app.ErrUnauthorized
	}
	return findGroupMemberAdmission(ctx, s.db, in)
}
func (s *Store) AdmitGroupMember(ctx context.Context, in app.CreateGroupMemberRequest, agent model.Agent, at time.Time) (app.GroupMemberResult, error) {
	var out app.GroupMemberResult
	if agent.ID != in.ID || agent.Name != in.Name || agent.PrimaryExecutionID != "" || agent.Lifecycle != model.AgentActive {
		return out, app.ErrInvalid
	}
	if in.Context.Principal.Kind != model.PrincipalOperator {
		return out, app.ErrUnauthorized
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	if prior, found, err := findGroupMemberAdmission(ctx, tx, in); found || err != nil {
		return prior, err
	}
	group, err := readGroup(ctx, tx, in.GroupID)
	if err != nil {
		return out, err
	}
	defaults, err := readGroupConfiguration(ctx, tx, in.GroupID)
	if err != nil {
		return out, err
	}
	if group.Revision != in.ExpectedGroupRevision || defaults.Revision != in.ExpectedDefaultRevision || defaults.Profile == nil || agent.ConfigurationProfile == nil || *defaults.Profile != *agent.ConfigurationProfile || len(group.Members) >= 1024 {
		return out, app.ErrConflict
	}
	var data []byte
	err = tx.QueryRowContext(ctx, `SELECT record FROM configuration_profile_revisions WHERE profile_id=? AND revision_id=?`, defaults.Profile.ProfileID, defaults.Profile.RevisionID).Scan(&data)
	if err != nil {
		return out, classify(err)
	}
	var revision model.ConfigurationProfileRevision
	if err = json.Unmarshal(data, &revision); err != nil {
		return out, err
	}
	expected := revision.Desired
	expected.HostSandbox = model.SandboxInGroup(expected.HostSandbox, in.GroupID)
	expected.Environment, err = model.MergeEnvironment(defaults.Environment, expected.Environment, in.Environment)
	if err != nil {
		return out, app.ErrInvalid
	}
	if !expected.Equal(agent.Desired) || revision.Ref != *defaults.Profile {
		return out, app.ErrConflict
	}
	if err = createAgentTx(ctx, tx, agent); err != nil {
		return out, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO group_members(group_id,agent_id,position) SELECT ?,?,COALESCE(MAX(position)+1,0) FROM group_members WHERE group_id=?`, group.ID, agent.ID, group.ID); err != nil {
		return out, classify(err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE groups SET revision=revision+1,updated_at=? WHERE id=?`, nanos(at), group.ID); err != nil {
		return out, err
	}
	if err = requireGroupCapacity(ctx, tx, group.ID); err != nil {
		return out, err
	}
	group.Members = append(group.Members, agent.ID)
	group.Revision++
	group.UpdatedAt = at
	out = app.GroupMemberResult{Agent: agent, Group: group}
	data, err = json.Marshal(out)
	if err != nil {
		return out, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO group_member_requests(scope,request_id,intent,result) VALUES(?,?,?,?)`, requestScope(in.Context.Principal), in.Context.RequestID, groupMemberIntent(in), data); err != nil {
		return out, err
	}
	if err = bumpTx(ctx, tx); err != nil {
		return out, err
	}
	return out, tx.Commit()
}
