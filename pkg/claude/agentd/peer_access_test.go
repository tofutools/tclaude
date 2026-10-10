package agentd

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"net/http/httptest"
	"testing"
)

func TestPeerAccessDecisionCannotBeReplacedAfterConsumption(t *testing.T) {
	req := &approvalRequest{peerAccess: &db.FederationPeerAccessRequest{Slug: PermNodeUpdate, GrantTTLSeconds: 7200}, decision: make(chan approvalOutcome, 1)}
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

func TestPeerAccessDecisionCannotExtendRequestedLifetime(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		requested, approved, status int
	}{
		{"permanent_from_finite", 3600, 0, 400},
		{"longer_from_finite", 3600, 3601, 400},
		{"same", 3600, 3600, 200},
		{"shorter", 3600, 60, 200},
		{"finite_from_permanent", 0, 3600, 200},
		{"permanent", 0, 0, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &approvalRequest{peerAccess: &db.FederationPeerAccessRequest{Slug: PermNodeUpdate, GrantTTLSeconds: tc.requested}, decision: make(chan approvalOutcome, 1)}
			rec := httptest.NewRecorder()
			servePeerAccessDecision(rec, req, "approve", &tc.approved, nil)
			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			if tc.status != 200 {
				require.Empty(t, req.decision)
				require.False(t, req.peerDecisionQueued)
				require.Equal(t, tc.requested, req.peerAccess.GrantTTLSeconds)
			}
		})
	}
}
