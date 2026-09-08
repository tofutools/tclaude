package migration

import (
	"testing"

	"github.com/stretchr/testify/require"
	sourcev228 "github.com/tofutools/tclaude/internal/backend/migration/source/v228"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/sandboxpolicy"
)

func TestLegacySandboxDecoderKeepsNetworkAndLiteralMeaning(t *testing.T) {
	for _, tc := range []struct {
		name, legacy, network string
		baseline              model.SandboxNetworkBaseline
		closedSocket          bool
	}{
		{"legacy none", "none", "", model.SandboxNetworkDeny, true},
		{"legacy internet", "internet", "", model.SandboxNetworkAllow, false},
		{"new deny does not invent socket closure", "", `{"baseline":"deny","deny":[{"domain":"example.com","include_subdomains":true,"ports":[443]}]}`, model.SandboxNetworkDeny, false},
		{"legacy list", "", `{"mode":"list","allow":[{"host":"example.com"}]}`, model.SandboxNetworkDeny, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := legacySandboxPolicy(sourcev228.Row{Values: map[string]any{"network_access": tc.legacy, "network_json": tc.network, "environment_json": `[{"name":"VALUE","value":"line one\n$(literal)"}]`}})
			require.NoError(t, err)
			require.NoError(t, sandboxpolicy.Validate(got))
			require.Equal(t, tc.baseline, got.Network.Baseline)
			require.Equal(t, "line one\n$(literal)", got.Environment["VALUE"])
			if tc.closedSocket {
				require.Equal(t, "closed", got.UnixSockets.Mode)
			} else {
				require.Nil(t, got.UnixSockets)
			}
		})
	}
}

func TestLegacySandboxDecoderRefusesUnrepresentableFields(t *testing.T) {
	for _, tc := range []struct{ field, value string }{
		{"environment_json", `[{"name":"VALUE","value":"one"},{"name":"VALUE","value":"two"}]`},
		{"network_json", `{"mode":"closed","allow":[{"host":"example.com"}]}`},
		{"network_json", `{"baseline":"deny","mode":"closed"}`},
		{"network_json", `{"baseline":"deny","future_authority":true}`},
		{"filesystem_json", `[{"path":"/a","access":"read","future_authority":"secret"}]`},
		{"resource_limits_json", `{"cpu":"0.25"}`},
		{"resource_limits_json", `{"memory":"1MiB","memory_bytes":2097152}`},
		{"tmpfs_json", `[{"path":"/scratch","size_bytes":12}]`},
		{"pre_launch_json", `[{"name":"setup","script":"exit 91","future_hook":true}]`},
		{"future_column", `"unknown authority"`},
	} {
		t.Run(tc.field+tc.value, func(t *testing.T) {
			_, _, err := legacySandboxPolicy(sourcev228.Row{Values: map[string]any{tc.field: tc.value}})
			require.Error(t, err)
		})
	}
}
