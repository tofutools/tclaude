package db

import (
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
	"time"
)

func TestModelProxyLeasePinsRequestPeerWorkerGenerationAndSlidesIdle(t *testing.T) {
	setupTestDB(t)
	l := ModelProxyLease{ID: "lease", Peer: "receiver", Request: "request", Kind: "spawn", Proxy: "main", IdleSeconds: 86400}
	require.NoError(t, IssueModelProxyLease(l))
	l.Worker, l.Session, l.Generation = "worker", "session", "generation"
	for _, change := range []func(*ModelProxyLease){func(x *ModelProxyLease) { x.Peer = "other" }, func(x *ModelProxyLease) { x.Request = "other" }, func(x *ModelProxyLease) { x.Kind = "teleport" }, func(x *ModelProxyLease) { x.Proxy = "other" }} {
		bad := l
		change(&bad)
		require.ErrorIs(t, ActivateModelProxyLease(bad), ErrModelProxyRefused)
	}
	require.NoError(t, ActivateModelProxyLease(l))
	ResetForTest()
	require.NoError(t, ActivateModelProxyLease(l))
	for _, change := range []func(*ModelProxyLease){func(x *ModelProxyLease) { x.Worker = "other" }, func(x *ModelProxyLease) { x.Session = "other" }, func(x *ModelProxyLease) { x.Generation = "other" }} {
		bad := l
		change(&bad)
		require.ErrorIs(t, ActivateModelProxyLease(bad), ErrModelProxyRefused)
	}
	d, err := Open()
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE model_proxy_leases SET touched_at=?`, dbTime(time.Now().Add(-23*time.Hour)))
	require.NoError(t, err)
	require.NoError(t, CheckModelProxyLease(l.ID, l.Peer, l.Proxy, l.Session, l.Generation, true))
	got, err := GetModelProxyLease(l.ID)
	require.NoError(t, err)
	require.WithinDuration(t, time.Now(), got.TouchedAt, time.Second)
	_, err = d.Exec(`UPDATE model_proxy_leases SET touched_at=?`, dbTime(time.Now().Add(-25*time.Hour)))
	require.NoError(t, err)
	require.ErrorIs(t, CheckModelProxyLease(l.ID, l.Peer, l.Proxy, l.Session, l.Generation, true), ErrModelProxyRefused)
	require.ErrorIs(t, ActivateModelProxyLease(l), ErrModelProxyRefused)
	_, err = d.Exec(`UPDATE model_proxy_leases SET touched_at=?`, dbTime(time.Now()))
	require.NoError(t, err)
	require.NoError(t, RevokeModelProxyLease(l.ID, l.Peer))
	require.ErrorIs(t, ActivateModelProxyLease(l), ErrModelProxyRefused)
}

func TestModelProxyWorkerLeaseCannotBeReassigned(t *testing.T) {
	setupTestDB(t)
	l := ModelProxyWorkerLease{Worker: "worker", Gateway: "payer", Lease: "lease", Request: "request", Kind: "teleport", Proxy: "main"}
	require.NoError(t, RecordModelProxyWorkerLease(l))
	ResetForTest()
	require.NoError(t, RecordModelProxyWorkerLease(l))
	l.Lease = "other"
	require.ErrorIs(t, RecordModelProxyWorkerLease(l), ErrModelProxyRefused)
}

func TestModelProxyWorkerLeaseCloseSurvivesUntilAcknowledged(t *testing.T) {
	setupTestDB(t)
	l := ModelProxyWorkerLease{Worker: "worker", Gateway: "payer", Lease: "lease", Request: "request", Kind: "spawn", Proxy: "main"}
	require.NoError(t, RecordModelProxyWorkerLease(l))
	require.NoError(t, SaveSession(&SessionRow{ID: "launch", Status: "idle", ExitLaunchGeneration: "one"}))
	require.NoError(t, BindModelProxyLaunch("launch", "main@payer", strings.Repeat("a", 64)))
	require.NoError(t, SetModelProxyLaunchLease("launch", "one", l.Lease))
	stale, err := StaleModelProxyWorkerLeases()
	require.NoError(t, err)
	require.Empty(t, stale)
	require.NoError(t, ForgetClosedModelProxyWorkerLease(l.Lease, l.Gateway))
	got, err := GetModelProxyWorkerLease(l.Worker)
	require.NoError(t, err)
	require.NotNil(t, got, "a premature ack cannot remove a live binding")
	d, err := Open()
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE sessions SET exit_callback_generation='two' WHERE id='launch'`)
	require.NoError(t, err)
	ResetForTest()
	stale, err = StaleModelProxyWorkerLeases()
	require.NoError(t, err)
	require.Equal(t, []ModelProxyWorkerLease{l}, stale)
	require.NoError(t, ForgetClosedModelProxyWorkerLease(l.Lease, "other"))
	got, err = GetModelProxyWorkerLease(l.Worker)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NoError(t, ForgetClosedModelProxyWorkerLease(l.Lease, l.Gateway))
	got, err = GetModelProxyWorkerLease(l.Worker)
	require.NoError(t, err)
	require.Nil(t, got)
}
