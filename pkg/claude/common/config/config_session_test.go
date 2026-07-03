package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The tmux-name style must be nil-safe and fail closed: anything that isn't
// exactly "dir" resolves to the historical id-prefix style, so a typo in
// config.json can never change launch behavior.
func TestResolvedTmuxNameStyle(t *testing.T) {
	assert.Equal(t, TmuxNameStyleID, (*Config)(nil).ResolvedTmuxNameStyle(), "nil config")
	assert.Equal(t, TmuxNameStyleID, (&Config{}).ResolvedTmuxNameStyle(), "absent session block")
	assert.Equal(t, TmuxNameStyleID, (&Config{Session: &SessionConfig{}}).ResolvedTmuxNameStyle(), "empty style")
	assert.Equal(t, TmuxNameStyleID, (&Config{Session: &SessionConfig{TmuxNameStyle: "bogus"}}).ResolvedTmuxNameStyle(), "unknown style fails closed")
	assert.Equal(t, TmuxNameStyleID, (&Config{Session: &SessionConfig{TmuxNameStyle: "Dir"}}).ResolvedTmuxNameStyle(), "styles are case-sensitive slugs")
	assert.Equal(t, TmuxNameStyleDir, (&Config{Session: &SessionConfig{TmuxNameStyle: "dir"}}).ResolvedTmuxNameStyle())
}
