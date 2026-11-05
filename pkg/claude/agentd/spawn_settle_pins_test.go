package agentd

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// cancelingSpawner forks nothing and cancels the launch's pending reservation,
// as an operator canceling the pending row would, before any conv-id appears.
type cancelingSpawner struct{ label string }

func (s *cancelingSpawner) SpawnNew(args clcommon.SpawnArgs) error {
	s.label = args.Label
	return db.DeletePendingSpawn(args.Label)
}

func (s *cancelingSpawner) SpawnResume(clcommon.SpawnArgs) error { return nil }

// An async spawn whose pending reservation is canceled before its response is
// refused as an identity conflict, rather than answered as a pending spawn
// that no longer exists.
func TestAsyncSpawnCanceledReservationIsAnIdentityConflict(t *testing.T) {
	setupTestDB(t)
	groupID, err := db.CreateAgentGroup("canceled", "")
	require.NoError(t, err)
	spawner := &cancelingSpawner{}
	previous := Spawn
	Spawn = spawner
	t.Cleanup(func() { Spawn = previous })
	_, failure := executeSpawn(&db.AgentGroup{ID: groupID, Name: "canceled"}, spawnParams{
		Name: "canceled-worker", Harness: "codex", Cwd: testutil.CanonicalTempDir(t),
		Async: true, Timeout: time.Millisecond,
	})
	require.NotNil(t, failure)
	require.Equal(t, http.StatusConflict, failure.Status, failure.Msg)
	require.Equal(t, "identity", failure.Kind)
	require.Equal(t, "pending spawn "+spawner.label+" was canceled before its response", failure.Msg)
}
