package hub_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/federation/client"
	"github.com/tofutools/tclaude/pkg/federation/hub"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

func TestRotationHelloTransfersAdmissionAndOpenHubCannotReviveOld(t *testing.T) {
	_, st, url := newHub(t, hub.Config{Open: true, IdentityRotationWindow: time.Millisecond})
	old, _ := proto.NewIdentity()
	next, _ := proto.NewIdentity()
	require.NoError(t, st.Admit(old.ID(), "fleet"))
	predecessor := startPeer(t, url, "old", "", old)
	eventually(t, "old connected", func() bool { return predecessor.status().State == client.StateConnected })
	certificate, err := proto.NewRotation(old, next, "", 1, time.Now(), 0)
	require.NoError(t, err)
	// Stage public evidence as an already observed predecessor notice; the
	// successor's real hello below must perform the admission transfer.
	require.NoError(t, st.ObserveRotations([]proto.Rotation{certificate}, old.ID(), time.Now().Add(-time.Minute), time.Millisecond))
	successor, err := client.New(client.Options{URL: url, Identity: next, RotationChain: []proto.Rotation{certificate}, MaxBackoff: 20 * time.Millisecond})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { successor.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	eventually(t, "successor admitted", func() bool { return successor.Status().State == client.StateConnected })
	require.Equal(t, []string{"fleet"}, successor.Status().Spaces)
	eventually(t, "old refused despite open mode", func() bool { return predecessor.status().State == client.StateRefused })
	retired, err := st.Get(old.ID())
	require.NoError(t, err)
	require.True(t, retired.Revoked)
	require.Equal(t, 1, successor.Status().IdentityRotationVersion)
}

func TestHubExplicitRecoveryRequiresNoOldKeyAndRetainsSpaces(t *testing.T) {
	h, st, url := newHub(t, hub.Config{Open: true})
	old, _ := proto.NewIdentity()
	next, _ := proto.NewIdentity()
	require.NoError(t, st.Admit(old.ID(), "fleet"))
	predecessor := startPeer(t, url, "old", "", old)
	eventually(t, "old connected", func() bool { return predecessor.status().State == client.StateConnected })
	require.NoError(t, st.RecoverIdentity(old.ID(), next.ID(), time.Now()))
	h.RefreshPolicy()
	successor := startPeer(t, url, "replacement", "", next)
	eventually(t, "replacement connected", func() bool { return successor.status().State == client.StateConnected })
	require.Equal(t, []string{"fleet"}, successor.status().Spaces)
	eventually(t, "retired key refused", func() bool { return predecessor.status().State == client.StateRefused })
	require.NoError(t, st.RevokeOldIdentity(old.ID(), time.Now()))
	h.RefreshPolicy()
	require.Equal(t, client.StateConnected, successor.status().State, "revoking predecessor leaves confirmed replacement active")
}
