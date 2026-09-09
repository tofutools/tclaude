package app

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDirectoryTrustReleaseRechecksBeforeConsumingDurablePermit(t *testing.T) {
	calls := 0
	permit := &releasePermit{beforeConsume: func(context.Context) error { calls++; return ErrUnauthorized }}
	// No store is installed: a rejected physical-directory recheck must return
	// before durable release can be consumed or the provider may start a child.
	require.ErrorIs(t, permit.Consume(context.Background()), ErrUnauthorized)
	require.Equal(t, 1, calls)
	require.False(t, permit.consumed.Load())
}
