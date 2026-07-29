package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Retiring an agent must take its standing orders with it, in the same
// transaction that revokes its grants. An order is durable guidance that keeps
// arriving on every boundary; a retired agent whose orders kept firing would
// keep instructing live agents after it lost the authority to instruct anyone.
func TestStandingOrder_RetireOwnerDisablesOrders(t *testing.T) {
	setupTestDB(t)
	const owner = "standing-retire-owner"
	agentID, _, err := EnsureAgentForConv(owner, "test")
	require.NoError(t, err)
	require.NotEmpty(t, agentID)

	o := sampleOrder("owned-by-retiree")
	o.OwnerAgent = agentID
	o.OwnerConv = owner
	id, err := InsertStandingOrder(o)
	require.NoError(t, err)
	before, err := GetStandingOrder(id)
	require.NoError(t, err)
	require.NotNil(t, before)

	out, err := RetireAgentAuthorizationByConv(owner, "human", "test")
	require.NoError(t, err)
	require.True(t, out.Retired)
	assert.Equal(t, int64(1), out.StandingOrdersDisabled,
		"the retire outcome reports what it disabled, so the caller can tell the operator")

	got, err := GetStandingOrder(id)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.False(t, got.Enabled)
	assert.Equal(t, before.Revision, got.Revision,
		"retirement must not re-arm the delivery cadence")
	assert.Equal(t, before.RowVersion+1, got.RowVersion,
		"retirement must invalidate stale dashboard writers")
	assert.NotEqual(t, before.UpdatedAt, got.UpdatedAt,
		"retirement keeps its audit timestamp current")
	assert.Equal(t, StandingDisabledReasonAgentRetired, got.DisabledReason,
		"the marker records WHY, so a later reinstate can tell this apart from a hand-disabled order")
}

// Reinstating an agent must not resurrect its orders.
//
// This is the asymmetry that matters: retirement is a judgement about the
// agent, and undoing it restores the agent's ability to act, not automatically
// everything it had previously set running. Guidance that starts re-injecting
// itself into other agents' turns because an unrelated reinstate happened is a
// silent re-authorization nobody asked for; the operator re-enables what they
// still want.
func TestStandingOrder_ReinstateDoesNotResurrectOrders(t *testing.T) {
	setupTestDB(t)
	const owner = "standing-reinstate-owner"
	agentID, _, err := EnsureAgentForConv(owner, "test")
	require.NoError(t, err)

	o := sampleOrder("survives-reinstate")
	o.OwnerAgent = agentID
	o.OwnerConv = owner
	id, err := InsertStandingOrder(o)
	require.NoError(t, err)

	_, err = RetireAgentAuthorizationByConv(owner, "human", "test")
	require.NoError(t, err)
	reinstated, err := ReinstateAgentByID(agentID)
	require.NoError(t, err)
	require.True(t, reinstated)

	got, err := GetStandingOrder(id)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.False(t, got.Enabled, "reinstate restores the agent, not its standing orders")
	assert.Equal(t, StandingDisabledReasonAgentRetired, got.DisabledReason)

	// The read path the hook uses agrees with the stored row. A resurrection
	// bug that only showed up here — an enabled row the ledger then acts on —
	// is the one that would actually deliver text to a live agent.
	live, err := ListEnabledStandingOrdersForEvent(StandingTriggerSessionStart)
	require.NoError(t, err)
	for _, l := range live {
		assert.NotEqual(t, id, l.ID, "a retired owner's order must not appear in the delivery read path")
	}
}

// Retiring twice must not inflate the count. The UPDATE is guarded on the row
// not already being in the target state, so a repeat retire reports zero.
func TestStandingOrder_RetireIsIdempotent(t *testing.T) {
	setupTestDB(t)
	const owner = "standing-retire-twice"
	agentID, _, err := EnsureAgentForConv(owner, "test")
	require.NoError(t, err)

	o := sampleOrder("retire-twice")
	o.OwnerAgent = agentID
	o.OwnerConv = owner
	_, err = InsertStandingOrder(o)
	require.NoError(t, err)

	first, err := RetireAgentAuthorizationByConv(owner, "human", "test")
	require.NoError(t, err)
	require.Equal(t, int64(1), first.StandingOrdersDisabled)

	reinstated, err := ReinstateAgentByID(agentID)
	require.NoError(t, err)
	require.True(t, reinstated)

	second, err := RetireAgentAuthorizationByConv(owner, "human", "test")
	require.NoError(t, err)
	assert.Zero(t, second.StandingOrdersDisabled,
		"the order was already disabled with this reason; re-retiring changes nothing")
}

// Retirement is scoped to the retiree. Another agent's orders — including ones
// aimed at the same group — are untouched.
func TestStandingOrder_RetireLeavesOtherOwnersAlone(t *testing.T) {
	setupTestDB(t)
	const retiree = "standing-retire-scope-a"
	const bystander = "standing-retire-scope-b"
	retireeID, _, err := EnsureAgentForConv(retiree, "test")
	require.NoError(t, err)
	bystanderID, _, err := EnsureAgentForConv(bystander, "test")
	require.NoError(t, err)

	a := sampleOrder("scope-retiree")
	a.OwnerAgent, a.OwnerConv = retireeID, retiree
	_, err = InsertStandingOrder(a)
	require.NoError(t, err)

	b := sampleOrder("scope-bystander")
	b.OwnerAgent, b.OwnerConv = bystanderID, bystander
	otherID, err := InsertStandingOrder(b)
	require.NoError(t, err)

	out, err := RetireAgentAuthorizationByConv(retiree, "human", "test")
	require.NoError(t, err)
	assert.Equal(t, int64(1), out.StandingOrdersDisabled)

	got, err := GetStandingOrder(otherID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, got.Enabled, "a bystander's order is not collateral of someone else's retirement")
}

// Deleting a group must sweep the orders that targeted it, and their ledger
// rows with them. Leaving them behind would strand rows pointing at a group id
// that no longer exists — and a later group reusing that id would inherit
// somebody else's guidance.
func TestStandingOrder_DeleteGroupSweepsOrdersAndLedger(t *testing.T) {
	setupTestDB(t)
	gID, err := CreateAgentGroup("standing-sweep-group", "")
	require.NoError(t, err)

	o := sampleOrder("group-scoped")
	o.TargetKind = StandingTargetGroup
	o.GroupID = gID
	id, err := InsertStandingOrder(o)
	require.NoError(t, err)
	_, err = RecordStandingDelivery(&StandingDelivery{
		OrderID: id, OrderRevision: 1, TargetConv: "someone",
		Epoch: "e1", Outcome: StandingOutcomeDelivered,
		Transport: StandingTransportHookContext, Harness: DefaultHarness,
	})
	require.NoError(t, err)

	require.NoError(t, DeleteAgentGroup("standing-sweep-group"))

	got, err := GetStandingOrder(id)
	require.NoError(t, err)
	assert.Nil(t, got, "a group-targeted order does not outlive its group")

	deliveries, err := ListStandingDeliveries(id, 10)
	require.NoError(t, err)
	assert.Empty(t, deliveries, "the ledger rows go with the order they describe")
}
