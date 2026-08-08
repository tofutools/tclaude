package harness

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCopilotVirtualCostFromNanoAIU(t *testing.T) {
	total := int64(43_000_000_000)
	cost, ok := CopilotVirtualCostFromNanoAIU(&total)
	assert.True(t, ok)
	assert.InDelta(t, 43.0, cost.Credits, 1e-12)
	assert.InDelta(t, 0.43, cost.USD, 1e-12)
}

func TestCopilotVirtualCostFromNanoAIUAbsentOrZeroIsAbsent(t *testing.T) {
	zero := int64(0)
	negative := int64(-1)
	for _, total := range []*int64{nil, &zero, &negative} {
		cost, ok := CopilotVirtualCostFromNanoAIU(total)
		assert.False(t, ok)
		assert.Zero(t, cost.USD)
		assert.Zero(t, cost.Credits)
	}
}

func TestCopilotVirtualCreditsFromUSD(t *testing.T) {
	assert.InDelta(t, 43.0, CopilotVirtualCreditsFromUSD(0.43), 1e-12)
	assert.Zero(t, CopilotVirtualCreditsFromUSD(0))
	assert.Zero(t, CopilotVirtualCreditsFromUSD(-1))
}
