package reflink

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeOptional(t *testing.T) {
	url, label, err := NormalizeOptional(" https://example.com/project ", " Project ")
	require.NoError(t, err)
	assert.Equal(t, "https://example.com/project", url)
	assert.Equal(t, "Project", label)

	url, label, err = NormalizeOptional(" ", "orphaned label")
	require.NoError(t, err)
	assert.Empty(t, url)
	assert.Empty(t, label)

	for _, rawURL := range []string{
		"javascript:alert(1)",
		"data:text/html,unsafe",
		"file:///tmp/project",
		"https://",
		"https://example.com/" + strings.Repeat("x", MaxURLLen),
	} {
		_, _, err = NormalizeOptional(rawURL, "")
		assert.Error(t, err, rawURL)
	}
	_, _, err = NormalizeOptional("https://example.com", strings.Repeat("x", MaxLabelLen+1))
	assert.Error(t, err)
}
