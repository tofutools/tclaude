package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOutboxStatusTextDistinguishesOfflineDiscard(t *testing.T) {
	assert.Equal(t, "discarded while offline", outboxStatusText(inboxEntry{
		Delivered:        false,
		NudgeDiscardedAt: "2026-07-16T12:00:00Z",
	}))
	assert.Equal(t, "discarded offline · read", outboxStatusText(inboxEntry{
		Read:             true,
		NudgeDiscardedAt: "2026-07-16T12:00:00Z",
	}))
	assert.Equal(t, "delivered", outboxStatusText(inboxEntry{Delivered: true}))
	assert.Equal(t, "delivered · read", outboxStatusText(inboxEntry{Delivered: true, Read: true}))
	assert.Equal(t, "pending", outboxStatusText(inboxEntry{}))
}
