package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestApplyOpenCodeAttachEnvironmentPinsDaemonHandoff(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/private/data")
	t.Setenv("XDG_CACHE_HOME", "/private/cache")
	t.Setenv("XDG_CONFIG_HOME", "/private/config")
	t.Setenv("XDG_STATE_HOME", "/private/state")
	environment := map[string]string{
		"XDG_DATA_HOME":       "/profile/data",
		"XDG_CACHE_HOME":      "/profile/cache",
		"XDG_CONFIG_HOME":     "/profile/config",
		"XDG_STATE_HOME":      "/profile/state",
		"OPENCODE_CONFIG_DIR": "/must-not-be-injected",
	}

	applyOpenCodeAttachEnvironment(environment)

	assert.Equal(t, "/private/data", environment["XDG_DATA_HOME"])
	assert.Equal(t, "/private/cache", environment["XDG_CACHE_HOME"])
	assert.Equal(t, "/private/config", environment["XDG_CONFIG_HOME"])
	assert.Equal(t, "/private/state", environment["XDG_STATE_HOME"])
	assert.Empty(t, environment["OPENCODE_CONFIG_DIR"])
}
