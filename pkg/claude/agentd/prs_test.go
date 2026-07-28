package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGitHubPRRefFromURL(t *testing.T) {
	tests := []struct {
		name   string
		rawURL string
		repo   string
		number int
		ok     bool
	}{
		{
			name:   "canonical",
			rawURL: "https://github.com/tofutools/tclaude/pull/800",
			repo:   "tofutools/tclaude",
			number: 800,
			ok:     true,
		},
		{
			name:   "trailing path is still the same PR",
			rawURL: "https://github.com/TofuTools/TClaude/pull/800/files",
			repo:   "TofuTools/TClaude",
			number: 800,
			ok:     true,
		},
		{
			name:   "non GitHub host",
			rawURL: "https://gitlab.com/tofutools/tclaude/pull/800",
		},
		{
			name:   "not a PR path",
			rawURL: "https://github.com/tofutools/tclaude/issues/800",
		},
		{
			name:   "zero PR number",
			rawURL: "https://github.com/tofutools/tclaude/pull/0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := githubPRRefFromURL(tt.rawURL)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.repo, got.repo)
			assert.Equal(t, tt.number, got.number)
		})
	}
}
