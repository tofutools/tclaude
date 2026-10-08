package hub

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

func TestHubRotationPreservesSpacesAndRevokesPredecessor(t *testing.T) {
	st, err := OpenStore(filepath.Join(t.TempDir(), "hub.sqlite"))
	require.NoError(t, err)
	defer func() { _ = st.Close() }()
	old, _ := proto.NewIdentity()
	next, _ := proto.NewIdentity()
	require.NoError(t, st.Admit(old.ID(), "private"))
	require.NoError(t, st.RecordSeen(old.ID(), old.Pub, "mac", "test", time.Now()))
	now := time.Now()
	r, err := proto.NewRotation(old, next, "", 1, now, time.Minute)
	require.NoError(t, err)
	require.NoError(t, st.ObserveRotations([]proto.Rotation{r}, old.ID(), now, time.Minute))
	require.NoError(t, st.ObserveRotations([]proto.Rotation{r}, next.ID(), now.Add(30*time.Second), time.Minute))
	p, err := st.Get(next.ID())
	require.NoError(t, err)
	require.Nil(t, p)
	require.NoError(t, st.ObserveRotations([]proto.Rotation{r}, next.ID(), now.Add(2*time.Minute), time.Minute))
	p, err = st.Get(next.ID())
	require.NoError(t, err)
	require.False(t, p.Revoked)
	require.Equal(t, []string{"private"}, p.Spaces)
	p, err = st.Get(old.ID())
	require.NoError(t, err)
	require.True(t, p.Revoked)
	retired, err := st.IdentityRetired(old.ID())
	require.NoError(t, err)
	require.True(t, retired)
	chain, err := st.RotationChain(next.ID())
	require.NoError(t, err)
	require.Len(t, chain, 1)
	require.NoError(t, st.ObserveRotations(chain, next.ID(), now.Add(time.Hour), time.Minute))
}

func TestHubRotationConflictingSuccessorsRequireRecovery(t *testing.T) {
	st, err := OpenStore(filepath.Join(t.TempDir(), "hub.sqlite"))
	require.NoError(t, err)
	defer func() { _ = st.Close() }()
	old, _ := proto.NewIdentity()
	a, _ := proto.NewIdentity()
	b, _ := proto.NewIdentity()
	require.NoError(t, st.Admit(old.ID()))
	now := time.Now()
	ra, err := proto.NewRotation(old, a, "", 1, now, time.Minute)
	require.NoError(t, err)
	rb, err := proto.NewRotation(old, b, "", 1, now, time.Minute)
	require.NoError(t, err)
	require.NoError(t, st.ObserveRotations([]proto.Rotation{ra}, old.ID(), now, time.Minute))
	require.Error(t, st.ObserveRotations([]proto.Rotation{rb}, b.ID(), now.Add(time.Hour), time.Minute))
	require.Error(t, st.ObserveRotations([]proto.Rotation{ra}, a.ID(), now.Add(time.Hour), time.Minute))
	p, err := st.Get(a.ID())
	require.NoError(t, err)
	require.Nil(t, p)
}
