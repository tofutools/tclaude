package agentd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Suppressed recovery has stopped, so the brief asks for the operator instead
// of saying tclaude has it in hand.
func TestFleetAttentionSaysWhenRecoveryNeedsTheOperator(t *testing.T) {
	a := fleetAgent{AgentID: "agt_a", Title: "worker"}
	detail := func(status, reason string) string {
		items := fleetAgentAttention(a, agentState{RecoveryStatus: status, RecoveryReason: reason}, time.Now())
		require.Len(t, items, 1)
		assert.Equal(t, fleetRecovering, items[0].Kind)
		return items[0].Detail
	}
	assert.Equal(t, "is being recovered (backoff): crash loop", detail(db.AgentRecoveryStatusBackoff, "crash loop"))
	suppressed := detail(db.AgentRecoveryStatusSuppressed, "operator stopped it")
	assert.NotContains(t, suppressed, "being recovered")
	assert.Contains(t, suppressed, "needs you")
	assert.Contains(t, suppressed, "operator stopped it")
}
