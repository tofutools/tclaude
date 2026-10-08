package db

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"testing"
	"time"
)

func TestFederationBundleOfferQuotaAndReplay(t *testing.T) {
	setupTestDB(t)
	peer, err := proto.NewIdentity()
	require.NoError(t, err)
	makeOffer := func() FederationBundleOffer {
		return FederationBundleOffer{Peer: peer.ID(), Direction: "in", State: "pending", Descriptor: bundletransfer.New(bundletransfer.Config, []byte("payload"), "offer", time.Now().Add(time.Hour))}
	}
	first := makeOffer()
	for i := 0; i < bundletransfer.PendingLimit; i++ {
		o := makeOffer()
		if i == 0 {
			o = first
		}
		ok, err := InsertFederationBundleOffer(o, bundletransfer.Config)
		require.NoError(t, err)
		require.True(t, ok)
	}
	ok, err := InsertFederationBundleOffer(first, bundletransfer.Config)
	require.NoError(t, err)
	require.False(t, ok)
	_, err = InsertFederationBundleOffer(makeOffer(), bundletransfer.Config)
	require.ErrorIs(t, err, ErrOfferQuota)
	require.NoError(t, SetFederationBundleOfferState("in", peer.ID(), first.Descriptor.ID, "declined", ""))
	ok, err = InsertFederationBundleOffer(makeOffer(), bundletransfer.Config)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, SetFederationBundleOfferState("in", peer.ID(), first.Descriptor.ID, "ready", ""))
	got, err := GetFederationBundleOffer("in", peer.ID(), first.Descriptor.ID)
	require.NoError(t, err)
	require.Equal(t, "declined", got.State)
	require.Empty(t, got.Descriptor.Inline)
	require.NoError(t, MarkFederationBundleResultQueued(peer.ID(), first.Descriptor.ID))
	got, err = GetFederationBundleOffer("in", peer.ID(), first.Descriptor.ID)
	require.NoError(t, err)
	require.True(t, got.ResultQueued)
	// Quotas are selected by the local bundle policy, and don't prevent a future
	// agent bundle from exceeding the config kind's 64 MiB allowance.
	kind := bundletransfer.Type{Name: "agent", MaxBytes: 256 << 20, PendingLimit: 2, PendingBytes: 512 << 20}
	o := makeOffer()
	o.Descriptor.Type = kind.Name
	o.Descriptor.Bytes = 200 << 20
	ok, err = InsertFederationBundleOffer(o, kind)
	require.NoError(t, err)
	require.True(t, ok)
	o = makeOffer()
	o.Descriptor.Bytes = bundletransfer.Config.PendingBytes
	_, err = InsertFederationBundleOffer(o, bundletransfer.Config)
	require.ErrorIs(t, err, ErrOfferQuota)
}

func TestFederationAgentOfferLaunchReservation(t *testing.T) {
	setupTestDB(t)
	peer, err := proto.NewIdentity()
	require.NoError(t, err)
	d := bundletransfer.New(bundletransfer.Agent, []byte("archive"), "offer", time.Now().Add(time.Hour))
	d.Group = "receiver"
	o := FederationBundleOffer{Descriptor: d, Peer: peer.ID(), Direction: "in", State: "ready", GroupID: 42, SenderAgent: "remote-agent"}
	_, err = InsertFederationBundleOffer(o, bundletransfer.Agent)
	require.NoError(t, err)
	require.NoError(t, ReserveFederationBundleImport(peer.ID(), d.ID, "reserved-agent"))
	require.Error(t, ReserveFederationBundleImport(peer.ID(), d.ID, "duplicate-agent"))
	released, err := ReleaseUnlaunchedFederationBundleImport(peer.ID(), d.ID, "reserved-agent")
	require.NoError(t, err)
	require.True(t, released)
	require.NoError(t, ReserveFederationBundleImport(peer.ID(), d.ID, "reserved-agent"))
	require.NoError(t, SetFederationBundleLaunchLabel("reserved-agent", "launch-label"))
	released, err = ReleaseUnlaunchedFederationBundleImport(peer.ID(), d.ID, "reserved-agent")
	require.NoError(t, err)
	require.False(t, released, "a possibly late subprocess must not be duplicated")
	got, err := GetFederationBundleOffer("in", peer.ID(), d.ID)
	require.NoError(t, err)
	require.Equal(t, int64(42), got.GroupID)
	require.Equal(t, "remote-agent", got.SenderAgent)
	require.Equal(t, "reserved-agent", got.ImportAgent)
	require.Equal(t, "launch-label", got.ImportLabel)
	require.NoError(t, ClearUnlaunchedFederationBundleLabel("reserved-agent", "stale-label"))
	released, err = ReleaseUnlaunchedFederationBundleImport(peer.ID(), d.ID, "reserved-agent")
	require.NoError(t, err)
	require.False(t, released, "a stale preparation failure cannot clear a later attempt")
	require.NoError(t, ClearUnlaunchedFederationBundleLabel("reserved-agent", "launch-label"))
	released, err = ReleaseUnlaunchedFederationBundleImport(peer.ID(), d.ID, "reserved-agent")
	require.NoError(t, err)
	require.True(t, released, "definite pre-dispatch failures can release their reservation")
}
