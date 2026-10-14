package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClipProjectionText(t *testing.T) {
	assert.Equal(t, "short", clipProjectionText("  short \n", 8))
	assert.Equal(t, "abcdefg…", clipProjectionText("abcdefghij", 8))
	assert.Equal(t, "åäöåäöå…", clipProjectionText("åäöåäöåäöå", 8), "clipping counts runes, not bytes")
}
