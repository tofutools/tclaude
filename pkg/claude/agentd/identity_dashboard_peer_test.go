package agentd

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// A peer's action on an agent is never this node's dashboard human, even
// through a cookie-auth twin that marks its request as the human's.
func TestDashboardHumanPeerSkipsPeerAction(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/agents/x/compact", nil)
	require.Equal(t, classHuman, classify(peerFromContext(asDashboardHumanPeer(r).Context())), "the operator's dashboard request is the human")
	a := &peerActionAuthority{peer: "inst_peer", sourceConv: "conv", permission: PermGroupsMembersCompact}
	r = r.WithContext(context.WithValue(r.Context(), peerActionKey{}, a))
	require.NotEqual(t, classHuman, classify(peerFromContext(asDashboardHumanPeer(r).Context())), "a peer's action stays unidentified")
}
