package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestFreeFloatingAgentName(t *testing.T) {
	created := time.Date(2026, time.July, 28, 12, 17, 33, 0, time.FixedZone("CEST", 2*60*60))
	got := FreeFloatingAgentName(created, "agt_f3e10b1d99999999")

	assert.Equal(t, "session-20260728-101733-f3e10b1d", got)
	assert.True(t, IsFreeFloatingAgentName(got))
	assert.False(t, IsFreeFloatingAgentName("explicit-worker"))
}
