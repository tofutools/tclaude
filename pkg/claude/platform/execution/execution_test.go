package execution

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewIDUsesLegacyLaunchGenerationRepresentation(t *testing.T) {
	id := NewID()
	reparsed, err := ParseID(id.String())
	require.NoError(t, err)
	assert.Equal(t, id, reparsed)
}

func TestNewIDRNGFailureStillAllocatesFreshAttempts(t *testing.T) {
	previous := randomRead
	randomRead = func([]byte) (int, error) { return 0, errors.New("rng unavailable") }
	t.Cleanup(func() { randomRead = previous })

	first := NewID()
	second := NewID()
	require.NotEqual(t, first, second)
	_, err := ParseID(first.String())
	require.NoError(t, err)
	_, err = ParseID(second.String())
	require.NoError(t, err)
}

func TestParseIDRejectsNonCompatibilityRepresentations(t *testing.T) {
	for _, raw := range []string{
		"", "abc", "exe_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "gggggggggggggggggggggggggggggggg",
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := ParseID(raw)
			require.Error(t, err)
		})
	}
}
