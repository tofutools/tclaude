//go:build linux

package session

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/copilotfixture"
)

const copilotTclaudeLayerSmokeProviderAddress = "198.18.0.1"

// TestCopilotTclaudeLayerNetworkSmoke is the executing evidence behind
// Copilot's Linux host-open constructed-root and private-routed capability
// cells. It runs the real production spawner string, not a helper binary, so
// Copilot's state, cache, executable, workspace, and tclaude coordination
// mounts all have to be sufficient for the CLI to reach its provider and
// persist the pinned session id.
func TestCopilotTclaudeLayerNetworkSmoke(t *testing.T) {
	if os.Getenv(filteredGatewaySmokeEnv) != "1" {
		t.Skip("set TCLAUDE_FILTERED_NETWORK_SMOKE=1 on the executing Linux CI boundary")
	}
	tclaudeBinary := requireFilteredSmokeEnv(t, filteredGatewayTclaudeBinaryEnv)
	tclaudeBinary, err := filepath.Abs(tclaudeBinary)
	require.NoError(t, err)
	bwrapBinary, _, err := ResolveTclaudeLayer(
		sandboxpolicy.NetworkFiltered, sandboxpolicy.RootConstructed)
	require.NoError(t, err)

	previousRelay := tclaudeLayerRelayPrefix
	tclaudeLayerRelayPrefix = func() string {
		return clcommon.ShellQuoteArg(tclaudeBinary) +
			" session " + tclaudeLayerWinchRelayCommand
	}
	t.Cleanup(func() { tclaudeLayerRelayPrefix = previousRelay })

	for _, tc := range []struct {
		name            string
		rules           sandboxpolicy.NetworkRules
		providerAddress string
	}{
		{
			name: "host-network-constructed-root",
			rules: sandboxpolicy.NetworkRules{
				Mode: sandboxpolicy.AccessModeOpen,
			},
			providerAddress: "127.0.0.1",
		},
		{
			name: "private-routed",
			rules: sandboxpolicy.NetworkRules{
				Mode:      sandboxpolicy.AccessModeOpen,
				Namespace: sandboxpolicy.NetworkNamespacePrivate,
			},
			providerAddress: copilotTclaudeLayerSmokeProviderAddress,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dirs := copilotfixture.NewSandboxDirs(t)
			mock := copilotfixture.NewMockProviderAt(t,
				tc.providerAddress+":0",
				[]copilotfixture.Turn{{Text: "MOCK SANDBOX ANSWER"}})
			const sessionID = "32222222-3333-4444-8555-666666666666"

			h := harness.MustGet(harness.CopilotName)
			commandLine := h.Spawn.BuildCommand(harness.SpawnSpec{
				SessionID:     sessionID,
				Name:          "copilot-tclaude-layer-smoke",
				Model:         copilotfixture.MockModel,
				InitialPrompt: "Reply with the text the provider gives you.",
			})
			environment := []sandboxpolicy.EnvironmentEntry{
				{Name: "HOME", Value: dirs.Root},
				{Name: "COPILOT_HOME", Value: dirs.Home},
				{Name: "COPILOT_CACHE_HOME", Value: dirs.Cache},
				{Name: "XDG_CACHE_HOME", Value: dirs.XDGCache},
			}
			require.NoError(t, ValidateTclaudeLayerHarnessPosture(
				h, environment, nil))

			sockets := sandboxpolicy.UnixSocketRules{Mode: sandboxpolicy.AccessModeClosed}
			snapshot := sandboxpolicy.EmptySnapshot()
			snapshot.Effective.Network = &tc.rules
			snapshot.Effective.UnixSockets = &sockets
			spec, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
				HarnessName: harness.CopilotName,
				Cwd:         dirs.WorkDir,
				Snapshot:    &snapshot,
				StateRoot:   dirs.Home,
				Environment: environment,
			})
			require.NoError(t, err)
			wrapped, err := WrapTclaudeLayerSpec(bwrapBinary, spec, commandLine)
			require.NoError(t, err)

			opts := copilotfixture.RunOptions{
				Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache,
				XDGCache: dirs.XDGCache, WorkDir: dirs.WorkDir,
				BaseURL: mock.BaseURL(),
				ExtraEnv: []string{
					"NO_PROXY=" + tc.providerAddress,
					"no_proxy=" + tc.providerAddress,
				},
			}
			result := copilotfixture.RunShell(t, opts, wrapped)
			require.Equal(t, 0, result.ExitCode,
				"production Copilot launch failed\nstdout: %s\nstderr: %s",
				result.Stdout, result.Stderr)
			require.NotEmpty(t, mock.Requests(),
				"the sandboxed production spawner must reach its provider")
			assert.DirExists(t, filepath.Join(dirs.Home, "session-state", sessionID),
				fmt.Sprintf("%s must preserve Copilot session state", tc.name))
		})
	}
}
