package federationcmd

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPeersSelfRowIsDisplayOnly(t *testing.T) {
	st := &status{InstanceID: "inst_local", Name: "desk", Fingerprint: "local-fp", Peers: []statusPeer{{InstanceID: "inst_peer", Trusted: true, Level: "restricted"}}}
	var out bytes.Buffer
	require.Zero(t, printPeers(&jsonParam{}, st, &out))
	require.Contains(t, out.String(), "this node")
	require.Contains(t, out.String(), "local-fp")
	require.Contains(t, out.String(), "self")
	out.Reset()
	require.Zero(t, printPeers(&jsonParam{JSON: true}, st, &out))
	var rows []statusPeer
	require.NoError(t, json.Unmarshal(out.Bytes(), &rows))
	require.Len(t, rows, 2)
	require.True(t, rows[0].Self)
	require.False(t, rows[0].Trusted)
	require.Len(t, st.Peers, 1, "self never enters the remote peer directory")
}
