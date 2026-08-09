package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestStatusDetailForDisplayHidesAppServerProvenance(t *testing.T) {
	for _, detail := range []string{
		"app-server snapshot",
		"app-server daemon reconnect",
		"APP-SERVER reconnect",
	} {
		assert.Empty(t, StatusDetailForDisplay(detail), detail)
	}
}

func TestStatusDetailForDisplayPreservesOperationalDetails(t *testing.T) {
	for _, detail := range []string{
		"Bash",
		"rate_limit",
		"1 background shell running",
		"app-server connection refused",
		"app-server approval required",
	} {
		assert.Equal(t, detail, StatusDetailForDisplay(detail), detail)
	}
}
