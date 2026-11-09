package agent

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

func TestUpdateConfigSetKeys(t *testing.T) {
	s := config.SelfUpdateConfig{Method: "release"}.Resolved()
	require.NoError(t, applyUpdateConfigKey(&s, "window", "22:30 - 01:00"))
	require.Equal(t, "22:30", s.Schedule.WindowStart)
	require.Equal(t, "01:00", s.Schedule.WindowEnd)
	require.NoError(t, applyUpdateConfigKey(&s, "setup-flags", "--install-agent-skills, install-utility-skills,"))
	require.Equal(t, []string{"install-agent-skills", "install-utility-skills"}, s.PostUpdateSetup.Flags)
	require.NoError(t, applyUpdateConfigKey(&s, "post-setup", "on"))
	require.True(t, s.PostUpdateSetup.Enabled)
	require.NoError(t, applyUpdateConfigKey(&s, "method", "none"))
	require.Empty(t, s.Method)
	require.Error(t, applyUpdateConfigKey(&s, "update-on-start", "maybe"))
	require.ErrorContains(t, applyUpdateConfigKey(&s, "command", "x"), "unknown key")
	require.Empty(t, s.Validate())

	var out bytes.Buffer
	printUpdateSettings(&updateSettingsView{Settings: s, InstallDir: "/home/me/.local/bin"}, &out)
	require.Contains(t, out.String(), "Post-update setup:  on: tclaude setup --yes --install-agent-skills --install-utility-skills")
	require.Contains(t, out.String(), "Schedule:           off")
}
