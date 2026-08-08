package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/agent"
)

// The Copilot drive acquisition LOG is the last line of defence for a spawn
// caller that renders nothing.
//
// It is not redundant with the launch echo TCL-1097 added, even though it is
// derived from it: the echo only reaches a person on callers that render it,
// and the scribe summon (measured — TCL-1104) renders none of it. Deleting this
// as "already covered by the echo" would move the scribe from logged to silent
// inside a change whose whole purpose is more disclosure.
//
// What this test does NOT prove is that executeSpawn still CALLS it — that is
// asserted by the comment at the call site rather than by a guard, because
// capturing slog output across the flow harness would couple every spawn test
// to a global logger swap. If you are here because you are removing the call,
// the condition to satisfy first is TCL-1104: every surface renders the echo.
func TestCopilotDriveAcquisitionLog(t *testing.T) {
	ambient := &agent.ResolvedLaunch{CopilotAPI: agent.ResolvedField{
		Value: "api", Source: `group default profile "copilot-api-on"`}}
	assert.Equal(t, `copilot_api: api (group default profile "copilot-api-on")`,
		copilotDriveAcquisitionLog(ambient),
		"an acquisition from a tier nobody typed must be logged, naming the tier")

	// The three silences, each for its own reason.
	assert.Empty(t, copilotDriveAcquisitionLog(&agent.ResolvedLaunch{
		CopilotAPI: agent.ResolvedField{Value: "api", Source: agent.ProvExplicit}}),
		"the caller asked for the drive; telling them they asked is noise")
	assert.Empty(t, copilotDriveAcquisitionLog(&agent.ResolvedLaunch{
		CopilotAPI: agent.ResolvedField{Value: "", Source: agent.ProvHarnessDefault}}),
		"send-keys is the known-good default and has nothing to disclose")
	assert.Empty(t, copilotDriveAcquisitionLog(nil),
		"a caller with no echo at all must not panic on the way to saying nothing")
}
