package db

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSpendAlerts_ConcurrentClaims(t *testing.T) {
	setupTestDB(t)
	var inserted atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			ok, err := InsertSpendAlert("daily", "2026-10-15", 80, &HumanMessage{FromTitle: "Spend limits", Body: "80% warning"})
			assert.NoError(t, err)
			if ok {
				inserted.Add(1)
			}
		})
	}
	wg.Wait()
	assert.Equal(t, int32(1), inserted.Load())
	messages, err := ListHumanMessagesPage(HumanMessageFilter{}, 0, MaxHumanMessagesPage)
	require.NoError(t, err)
	assert.Len(t, messages, 1)
}
