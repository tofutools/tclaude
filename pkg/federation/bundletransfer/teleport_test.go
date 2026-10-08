package bundletransfer

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"testing"
	"time"
)

func TestTeleportCredentialModes(t *testing.T) {
	for _, mode := range []string{"", "local", "proxy:main@home", "proxy:gateway-1@inst_abc"} {
		require.True(t, ValidCredentials(mode), mode)
	}
	for _, mode := range []string{"remote", "proxy:", "proxy:a@", "proxy:@b", "proxy:a@b@c", "proxy:a@b\n", "proxy:a/../b@peer"} {
		require.False(t, ValidCredentials(mode), mode)
	}
}
func TestTeleportProvenanceRejectsDiscontinuousAndFutureHops(t *testing.T) {
	a, err := proto.NewIdentity()
	require.NoError(t, err)
	b, err := proto.NewIdentity()
	require.NoError(t, err)
	in := TeleportIntent{Version: 1, Chain: proto.NewEnvelopeID(), OriginInstance: a.ID(), OriginAgent: "agt_origin000000000000000000", SourceAgent: "agt_origin000000000000000000", SourceConv: "019fe740-43a4-7023-b8ae-1ee64459f2a1", Hops: []TeleportHop{{Offer: proto.NewEnvelopeID(), FromInstance: a.ID(), FromAgent: "agt_origin000000000000000000", ToInstance: b.ID(), ToGroup: "team", At: time.Now()}}}
	require.NoError(t, in.Validate())
	bad := in
	bad.Hops = append([]TeleportHop{}, in.Hops...)
	bad.Hops[0].At = time.Now().Add(time.Hour)
	require.Error(t, bad.Validate())
	bad = in
	bad.OriginInstance = b.ID()
	require.ErrorContains(t, bad.Validate(), "origin")
	bad = in
	bad.Hops = append(append([]TeleportHop{}, in.Hops...), in.Hops[0])
	require.ErrorContains(t, bad.Validate(), "discontinuous")
}
