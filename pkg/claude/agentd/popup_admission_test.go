package agentd

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApprovalBodyHoldingIsSharedFairly(t *testing.T) {
	a := &approvalRegistry{pending: map[string]*approvalRequest{}}
	limits := func(total, perCaller, free int64) func() {
		oldTotal, oldCaller, oldFree := maxApprovalHeldBytes, maxApprovalHeldBytesPerCaller, approvalBodyFreeBytes
		maxApprovalHeldBytes, maxApprovalHeldBytesPerCaller, approvalBodyFreeBytes = total, perCaller, free
		return func() {
			maxApprovalHeldBytes, maxApprovalHeldBytesPerCaller, approvalBodyFreeBytes = oldTotal, oldCaller, oldFree
		}
	}
	t.Cleanup(limits(100, 60, 10))
	body := func(n int) *bytes.Reader { return bytes.NewReader(make([]byte, n)) }
	admit := func(caller string) *approvalTicket {
		ticket, refused := a.admit(caller)
		require.NotNil(t, ticket, refused)
		return ticket
	}

	greedy := admit("greedy")
	got, err := a.readBody(greedy, body(60))
	require.NoError(t, err)
	assert.Len(t, got, 60, "the whole body comes back")
	assert.Equal(t, int64(50), greedy.held, "the free allowance is not charged")

	_, err = a.readBody(admit("greedy"), body(90))
	assert.ErrorIs(t, err, errApprovalBodyTooLarge, "over one caller's share on its own")
	_, err = a.readBody(admit("greedy"), body(30))
	assert.ErrorIs(t, err, errApprovalBodyBusy, "the caller's own waiting requests fill its share")

	other := admit("other")
	_, err = a.readBody(other, body(60))
	require.NoError(t, err, "a share is kept for another caller")
	third := admit("third")
	_, err = a.readBody(third, body(30))
	assert.ErrorIs(t, err, errApprovalBodyBusy, "the pool is full, for now")
	_, err = a.readBody(admit("small"), body(10))
	assert.NoError(t, err, "a small body gets through a full pool")

	held := a.admission.held
	a.release(other)
	assert.Equal(t, held-50, a.admission.held, "bytes come back with the slot")
	_, err = a.readBody(third, body(30))
	assert.NoError(t, err, "and room with them")
}
