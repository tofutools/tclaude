package db

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFederationJobsDurableIdentityAndTransitions(t *testing.T) {
	setupTestDB(t)
	j := FederationJob{ID: "job1", Direction: "in", Peer: "peer", Fingerprint: "content", State: "pending", Request: json.RawMessage(`{"command":"echo hi"}`), WorkerID: NewAgentID(), CallerAgent: "caller", ExpiresAt: time.Now().Add(time.Hour)}
	require.NoError(t, InsertFederationJob(&j))
	require.Error(t, InsertFederationJob(&j))
	require.NoError(t, TransitionFederationJob(j.ID, "pending", "preparing", nil))
	require.Error(t, TransitionFederationJob(j.ID, "pending", "preparing", nil))
	rows, e := ListFederationJobs(true)
	require.NoError(t, e)
	require.Len(t, rows, 1)
	require.Equal(t, j.WorkerID, rows[0].WorkerID)
	require.Equal(t, "caller", rows[0].CallerAgent)
	require.WithinDuration(t, j.ExpiresAt, rows[0].ExpiresAt, time.Microsecond)
	require.NoError(t, TransitionFederationJob(j.ID, "preparing", "completed", json.RawMessage(`{"exit_code":0}`)))
	rows, e = ListFederationJobs(true)
	require.NoError(t, e)
	require.Empty(t, rows)
	saved, e := GetFederationJob(j.ID)
	require.NoError(t, e)
	require.Equal(t, "completed", saved.State)
	require.Equal(t, j.Fingerprint, saved.Fingerprint)
}
func TestFederationRepoRevisionAndDisablePreserveIdentity(t *testing.T) {
	setupTestDB(t)
	r := FederationRepo{Name: "project", Enabled: true}
	require.NoError(t, SaveFederationRepo(&r))
	original := r.ID
	stale := r
	require.NoError(t, SaveFederationRepo(&r))
	require.Error(t, SaveFederationRepo(&stale))
	require.NoError(t, DisableFederationRepo(r.Name))
	saved, e := GetFederationRepo(original)
	require.NoError(t, e)
	require.False(t, saved.Enabled)
	require.Equal(t, original, saved.ID)
	require.Equal(t, int64(3), saved.Revision)
}
