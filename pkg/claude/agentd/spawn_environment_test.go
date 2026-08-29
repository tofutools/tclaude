package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func TestResolveCommonLaunchEnvironmentPrecedence(t *testing.T) {
	global := &db.SpawnProfile{Name: "global", Environment: []sandboxpolicy.EnvironmentEntry{{Name: "LEVEL", Value: "global"}, {Name: "GLOBAL", Value: "yes"}}}
	groupProfile := &db.SpawnProfile{Name: "group-profile", Environment: []sandboxpolicy.EnvironmentEntry{{Name: "LEVEL", Value: "group-profile"}}}
	named := &db.SpawnProfile{Name: "named", Environment: []sandboxpolicy.EnvironmentEntry{{Name: "LEVEL", Value: "named"}}}
	group := &db.AgentGroup{Name: "team", Environment: []sandboxpolicy.EnvironmentEntry{{Name: "LEVEL", Value: "group"}, {Name: "GROUP", Value: "yes"}}}
	got, err := resolveCommonLaunchEnvironment(group, []launchProfileTier{{profile: named}, {profile: groupProfile}, {profile: global}}, []sandboxpolicy.EnvironmentEntry{{Name: "LEVEL", Value: "explicit"}})
	require.NoError(t, err)
	assert.Equal(t, []sandboxpolicy.EnvironmentEntry{{Name: "GLOBAL", Value: "yes"}, {Name: "GROUP", Value: "yes"}, {Name: "LEVEL", Value: "explicit"}}, got)
}
