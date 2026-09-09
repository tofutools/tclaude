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
		Labels                         *model.AgentDisplayLabels   `json:",omitempty"`
		ConfigurationOverrides         *model.ConfigurationOptions `json:",omitempty"`
	}{in.Environment, in.GroupID, in.ID, in.Name, in.ExpectedGroupRevision, in.ExpectedDefaultRevision, in.Labels, in.ConfigurationOverrides})
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
func authorizeGroupMember(ctx context.Context, q queryer, in app.CreateGroupMemberRequest, desired *model.DesiredConfiguration, at time.Time) error {
	decision, err := authorizeTx(ctx, q, model.AuthorityRequest{Principal: in.Context.Principal, Action: model.ActionCreateGroupMember, Resource: model.ResourceSelector{Kind: model.ResourceGroup, GroupID: in.GroupID}, RequestedConfiguration: desired}, at)
	if err != nil {
		return err
	}
	if !decision.Allowed {
		return app.ErrUnauthorized
	}
	return nil
}
func (s *Store) FindGroupMemberAdmission(ctx context.Context, in app.CreateGroupMemberRequest, at time.Time) (app.GroupMemberResult, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.GroupMemberResult{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if err = authorizeGroupMember(ctx, tx, in, nil, at); err != nil {
		return app.GroupMemberResult{}, false, err
	}
	prior, found, err := findGroupMemberAdmission(ctx, tx, in)
	if err == nil && found {
		err = authorizeGroupMember(ctx, tx, in, &prior.Agent.Desired, at)
	}
	return prior, found, err
}
func (s *Store) AdmitGroupMember(ctx context.Context, admission app.GroupMemberAdmission) (app.GroupMemberResult, error) {
	in, agent, at := admission.Request, admission.Agent, admission.At
	var out app.GroupMemberResult
	if agent.ID != in.ID || agent.Name != in.Name || agent.PrimaryExecutionID != "" || agent.Lifecycle != model.AgentActive {
		return out, app.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	if err = authorizeGroupMember(ctx, tx, in, nil, at); err != nil {
		return out, err
	}
	if prior, found, err := findGroupMemberAdmission(ctx, tx, in); found || err != nil {
		if err == nil {
			err = authorizeGroupMember(ctx, tx, in, &prior.Agent.Desired, at)
		}
		return prior, err
	}
	if err = authorizeGroupMember(ctx, tx, in, &agent.Desired, at); err != nil {
		return out, err
	}
	group, err := readGroup(ctx, tx, in.GroupID)
	if err != nil {
		return out, err
	}
	defaults, err := readGroupConfiguration(ctx, tx, in.GroupID)
	if err != nil {
		return out, err
	}
	if group.Revision != in.ExpectedGroupRevision || defaults.Revision != in.ExpectedDefaultRevision || defaults.Profile == nil || agent.ConfigurationProfile == nil || defaults.Profile.ProfileID != agent.ConfigurationProfile.ProfileID || len(group.Members) >= 1024 {
		return out, app.ErrConflict
	}
	var data []byte
	err = tx.QueryRowContext(ctx, `SELECT record FROM configuration_profiles WHERE id=?`, agent.ConfigurationProfile.ProfileID).Scan(&data)
	if err != nil {
		return out, classify(err)
	}
	var profile model.ConfigurationProfile
	if err = json.Unmarshal(data, &profile); err != nil {
		return out, err
	}
	if profile.Archived || profile.CurrentRevisionID != agent.ConfigurationProfile.RevisionID {
		return out, app.ErrConflict
	}
	err = tx.QueryRowContext(ctx, `SELECT record FROM configuration_profile_revisions WHERE profile_id=? AND revision_id=?`, agent.ConfigurationProfile.ProfileID, agent.ConfigurationProfile.RevisionID).Scan(&data)
	if err != nil {
		return out, classify(err)
	}
	var revision model.ConfigurationProfileRevision
	if err = json.Unmarshal(data, &revision); err != nil {
		return out, err
	}
	expected := revision.Desired
	if revision.Options != nil || in.ConfigurationOverrides != nil {
		if admission.Configuration.Selected != revision.Ref {
			return out, app.ErrConflict
		}
		if revision.Options != nil {
			if err := requireProfileResolutionCurrent(ctx, tx, admission.Configuration); err != nil {
				return out, err
			}
		}
		expected = admission.Configuration.Desired
	}
	expected.HostSandbox = model.SandboxInGroup(expected.HostSandbox, in.GroupID)
	expected.Environment, err = model.MergeEnvironment(defaults.Environment, expected.Environment, in.Environment)
	if err != nil {
		return out, app.ErrInvalid
	}
	if !expected.Equal(agent.Desired) || revision.Ref != *agent.ConfigurationProfile {
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
