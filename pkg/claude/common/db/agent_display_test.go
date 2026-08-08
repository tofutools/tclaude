package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAgentDisplayTitlePrecedence(t *testing.T) {
	row := &ConvIndexRow{
		CustomTitle: "custom",
		Summary:     "summary",
		FirstPrompt: "prompt",
	}
	assert.Equal(t, "custom", AgentDisplayTitle(row, "actor"))

	row.CustomTitle = ""
	assert.Equal(t, "actor", AgentDisplayTitle(row, "actor"))
	assert.Equal(t, "summary", AgentDisplayTitle(row, ""))

	row.Summary = ""
	assert.Equal(t, "prompt", AgentDisplayTitle(row, ""))
	assert.Empty(t, AgentDisplayTitle(nil, ""))
}
