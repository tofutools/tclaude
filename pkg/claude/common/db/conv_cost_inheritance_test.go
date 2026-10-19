package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConvCostInheritance(t *testing.T) {
	setupTestDB(t)
	got, err := ConvCostInheritedUntil("conv-a")
	require.NoError(t, err)
	assert.True(t, got.IsZero(), "a conversation without copied history has no boundary")

	first := time.Date(2026, 10, 19, 12, 0, 0, 123, time.UTC)
	require.NoError(t, SetConvCostInheritedUntil("conv-a", first))
	require.NoError(t, SetConvCostInheritedUntil("conv-a", first.Add(-time.Hour)))
	got, err = ConvCostInheritedUntil("conv-a")
	require.NoError(t, err)
	assert.True(t, first.Equal(got), "an earlier boundary never shrinks what is inherited")

	require.NoError(t, DeleteConvCostInheritance("conv-a"))
	got, err = ConvCostInheritedUntil("conv-a")
	require.NoError(t, err)
	assert.True(t, got.IsZero())
	require.Error(t, SetConvCostInheritedUntil("", first))
}
