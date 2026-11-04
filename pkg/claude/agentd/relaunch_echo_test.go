package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// The Codex SSH workaround's command is regenerated per launch; it is not a
// change anyone made.
func TestRelaunchChangesIgnoresTheRegeneratedSSHCommand(t *testing.T) {
	snapshot := func(command string) *sandboxpolicy.Snapshot {
		effective := sandboxpolicy.EmptySnapshot().Effective
		effective.Environment = []sandboxpolicy.EnvironmentEntry{{Name: codexSSHCommandEnv, Value: command}}
		effective.Provenance.Environment = map[string]sandboxpolicy.ProfileSource{
			codexSSHCommandEnv: codexSSHSource()}
		s := sandboxpolicy.NewSnapshot(effective, nil)
		return &s
	}
	assert.Empty(t, relaunchChanges(snapshot("ssh -F /gen/one"), snapshot("ssh -F /gen/two")))
}

func TestRelaunchChangesNamesWithoutValues(t *testing.T) {
	before, after := sandboxpolicy.EmptySnapshot(), sandboxpolicy.EmptySnapshot()
	before.LaunchEnvironment = []sandboxpolicy.EnvironmentEntry{{Name: "API_KEY", Value: "old-value"}}
	after.LaunchEnvironment = []sandboxpolicy.EnvironmentEntry{{Name: "API_KEY", Value: "new-value"}}
	after.Effective.NetworkAccess = "none"
	got := relaunchChanges(&before, &after)
	assert.Contains(t, got, "group environment: changed API_KEY")
	assert.Contains(t, got, "network access: default → none")
	for _, line := range got {
		assert.NotContains(t, line, "value")
	}
	assert.Nil(t, relaunchChanges(nil, &after))
}
