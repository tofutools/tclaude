package agentd

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"net/http/httptest"
	"testing"
)

func TestPeerAccessDecisionCannotBeReplacedAfterConsumption(t *testing.T) {
	req := &approvalRequest{peerAccess: &db.FederationPeerAccessRequest{Slug: PermNodeUpdate, GrantTTLSeconds: 60}, decision: make(chan approvalOutcome, 1)}
	ttl := 3600
	first := httptest.NewRecorder()
	servePeerAccessDecision(first, req, "approve", &ttl, nil)
	require.Equal(t, 200, first.Code)
	// The waiter has consumed the first channel value, but has not yet read the
	// grant fields. A competing POST must still be refused at this boundary.
	require.Equal(t, outcomeApprove, <-req.decision)
	forever := 0
	second := httptest.NewRecorder()
	servePeerAccessDecision(second, req, "approve", &forever, nil)
	require.Equal(t, 409, second.Code)
	require.Equal(t, 3600, req.peerAccess.GrantTTLSeconds)
}
