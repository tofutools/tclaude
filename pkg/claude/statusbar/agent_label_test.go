package statusbar

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tofutools/tclaude/pkg/claude/common/grouplabel"
)

func TestAgentLabelText(t *testing.T) {
	assert.Equal(t, colorDim+"t/"+colorReset+"lead", agentLabelText(grouplabel.Label{Prefix: "t", Name: "lead"}))
	assert.Equal(t, "loner", agentLabelText(grouplabel.Label{Name: "loner"}))
	assert.Equal(t, "le ad", agentLabelText(grouplabel.Label{Name: "le\x1bad"}), "no control character reaches the terminal")
}

// A brokered render carries the agent's label back the same as the direct
// path reads it.
func TestFactsFromBroker_CarriesTheAgentLabel(t *testing.T) {
	facts := factsFromBroker(renderRequest{}, BrokeredRenderResponse{AgentPrefix: "c", AgentName: "lead"})
	assert.Equal(t, "c/lead", facts.Agent.String())
}
