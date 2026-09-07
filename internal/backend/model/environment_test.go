package model

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEnvironmentLiteralCompositionAndRuntimeNamespace(t *testing.T) {
	group := Environment{"SHARED": "group", "GROUP_ONLY": "yes"}
	profile := Environment{"SHARED": "profile", "EMPTY": "old"}
	explicit := Environment{"SHARED": "literal $HOME\nwith=equals", "EMPTY": ""}
	merged, err := MergeEnvironment(group, profile, explicit)
	require.NoError(t, err)
	require.Equal(t, []string{"EMPTY=", "GROUP_ONLY=yes", "SHARED=literal $HOME\nwith=equals"}, merged.Entries())
	require.Equal(t, "group", group["SHARED"])
	merged["SHARED"] = "changed"
	require.Equal(t, "literal $HOME\nwith=equals", explicit["SHARED"])
	for _, name := range []string{"HOME", "PATH", "BASH_ENV", "LD_PRELOAD", "DYLD_INSERT_LIBRARIES", "CODEX_HOME", "COPILOT_HOME", "OPENCODE_SERVER_PASSWORD", "TCLAUDE_BACKEND_CREDENTIAL_FILE", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "XDG_DATA_HOME"} {
		require.Error(t, (Environment{name: "override"}).Validate(), name)
	}
	for _, invalid := range []Environment{{"": "v"}, {"1NAME": "v"}, {"A=B": "v"}, {"A": "\x00"}, {"A": string([]byte{255})}, {"A": strings.Repeat("x", 16385)}} {
		require.Error(t, invalid.Validate())
	}
	require.True(t, Environment(nil).Equal(Environment{}))
}
