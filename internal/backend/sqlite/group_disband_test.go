package sqlite

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"path/filepath"
	"testing"
	"time"
)

func TestGroupDisbandRequiresSettledWorkAndFencesFreshAdmission(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "db"))
	require.NoError(t, err)
	defer s.Close()
	at := time.Now().UTC()
	group := model.Group{ID: "group", Name: "Group", Revision: 1, CreatedAt: at, UpdatedAt: at}
	require.NoError(t, s.CreateGroup(ctx, group, model.ConfigurationBounds{}))
	run := model.WorkRun{ID: "run", RequestID: "run-request", Requester: model.OperatorPrincipal(), Scope: model.WorkScope{GroupID: group.ID}, State: model.WorkRunRunning, ControlState: model.WorkControlActive, Revision: 1, CreatedAt: at, UpdatedAt: at, Graph: &model.WorkGraph{}}
	_, _, err = s.CreateGraphWorkRun(ctx, run, nil)
	require.NoError(t, err)
	request := app.DisbandGroupRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "remove"}, ID: group.ID, ExpectedRevision: 1}
	_, err = s.DisbandGroup(ctx, request, at)
	require.ErrorIs(t, err, app.ErrConflict)
	require.Contains(t, err.Error(), "run")
	_, err = s.db.ExecContext(ctx, `UPDATE work_runs SET state='failed',control_state='draining' WHERE id='run'`)
	require.NoError(t, err)
	_, err = s.DisbandGroup(ctx, request, at)
	require.ErrorIs(t, err, app.ErrConflict)
	_, err = s.db.ExecContext(ctx, `UPDATE work_runs SET control_state='settled' WHERE id='run'`)
	require.NoError(t, err)
	_, err = s.DisbandGroup(ctx, request, at)
	require.NoError(t, err)
	retained, err := s.WorkRun(ctx, "run")
	require.NoError(t, err)
	require.Equal(t, group.ID, retained.Run.Scope.GroupID)
	run.ID = "new-run"
	run.RequestID = "new-request"
	_, _, err = s.CreateGraphWorkRun(ctx, run, nil)
	require.ErrorIs(t, err, app.ErrConflict)
	_, err = s.WorkRun(ctx, "new-run")
	require.ErrorIs(t, err, app.ErrNotFound)
	_, _, err = s.CreateTeamDeployment(ctx, model.TeamDeployment{ID: "new-team", TargetKind: model.TeamTargetExistingGroup}, group, nil, nil, model.OperatorPrincipal(), "new-team", "digest", at)
	require.ErrorIs(t, err, app.ErrNotFound)
}
