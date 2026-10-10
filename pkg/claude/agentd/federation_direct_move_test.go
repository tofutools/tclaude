package agentd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestDirectMoveAuthoritySurvivesItsOwnAppliedReceipt(t *testing.T) {
	t.Setenv("HOME", testutil.CanonicalTempDir(t))
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	id, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, db.TrustFederationPeer(db.FederationPeer{InstanceID: id.ID(), PubKey: id.Pub, TrustLevel: db.FederationTrustUnrestricted}))
	group, err := db.CreateAgentGroup("receiver", "")
	require.NoError(t, err)
	d := bundletransfer.New(bundletransfer.Agent, []byte("fixture"), "Move", time.Now().Add(time.Hour))
	d.Group = "receiver"
	d.Move = &bundletransfer.MoveIntent{SourceAgent: db.NewAgentID(), SourceConv: "019fe740-43a4-7023-b8ae-1ee64459f2a1", DirectIfAllowed: true}
	o := &db.FederationBundleOffer{Direction: "in", Peer: id.ID(), Descriptor: d, State: "ready", GroupID: group}
	_, err = db.InsertFederationBundleOffer(*o, bundletransfer.Agent)
	require.NoError(t, err)
	a, err := directMoveAuthority(o)
	require.NoError(t, err)
	require.NoError(t, a.check())
	require.NoError(t, db.ReserveFederationBundleImport(o.Peer, d.ID, a.record.TargetAgent))
	require.NoError(t, db.SetFederationBundleOfferState("in", o.Peer, d.ID, "applied", ""))
	require.NoError(t, a.check(), "a deferred launch keeps its authority after the HTTP import receipt")
	other := *a.record
	other.TargetAgent = db.NewAgentID()
	require.Error(t, (&teleportLandingAuthority{record: &other, direct: o}).check(), "an applied receipt cannot authorize a different target")
	peer, err := db.GetFederationPeer(o.Peer)
	require.NoError(t, err)
	peer.TrustLevel = db.FederationTrustRestricted
	require.NoError(t, db.TrustFederationPeer(*peer))
	require.Error(t, a.check(), "the admitted continuation still rechecks live receiver authority")
}
