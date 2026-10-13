//go:build linux

package session

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/pifixture"
)

// TestPiTclaudeLayerSmoke is the executing evidence that pi works inside
// tclaude's sandbox, its only boundary. It runs the production pi spawner
// string, with the launch executable resolved as `session new` resolves it,
// in a constructed root on the host network and in the private routed
// namespace. pi answers a mock provider's tool call with its own bash tool,
// so the probes run as the agent would: the agent directory's auth.json is
// writable, the read-only floor holds over models.json, settings.json and
// extensions/, a path outside the grants is invisible, and pi persists the
// session it was launched with.
func TestPiTclaudeLayerSmoke(t *testing.T) {
	if os.Getenv(filteredGatewaySmokeEnv) != "1" {
		t.Skip("set TCLAUDE_FILTERED_NETWORK_SMOKE=1 on the executing Linux CI boundary")
	}
	tclaudeBinary := requireFilteredSmokeEnv(t, filteredGatewayTclaudeBinaryEnv)
	tclaudeBinary, err := filepath.Abs(tclaudeBinary)
	require.NoError(t, err)
	bwrapBinary, _, err := ResolveTclaudeLayer(
		sandboxpolicy.NetworkFiltered, sandboxpolicy.RootConstructed)
	require.NoError(t, err)
	resolved, err := harness.ResolvePiLaunchExecutable()
	require.NoError(t, err, "the CI job installs the pinned pi")

	previousRelay := tclaudeLayerRelayPrefix
	tclaudeLayerRelayPrefix = func() string {
		return clcommon.ShellQuoteArg(tclaudeBinary) +
			" session " + tclaudeLayerWinchRelayCommand
	}
	t.Cleanup(func() { tclaudeLayerRelayPrefix = previousRelay })
	prepareStackedSmokeControlPlane(t)

	for _, tc := range []struct {
		name            string
		rules           sandboxpolicy.NetworkRules
		providerAddress string
	}{
		{
			name:            "host-network-constructed-root",
			rules:           sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeOpen},
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
			root := newPiTclaudeLayerSmokeRoot(t)
			agentDir := filepath.Join(root, "agent")
			workDir := filepath.Join(root, "work")
			decoy := filepath.Join(root, "other-harness-home", "secret")
			for _, dir := range []string{agentDir, workDir, filepath.Dir(decoy)} {
				require.NoError(t, os.MkdirAll(dir, 0o700))
			}
			require.NoError(t, os.WriteFile(decoy, []byte("secret"), 0o600))

			mock := pifixture.NewMockAt(t, tc.providerAddress+":0")
			models := `{"providers":{"mock":{"baseUrl":"` + mock.URL + `/v1","api":"openai-completions",` +
				`"apiKey":"mock","models":[{"id":"mock-1","name":"Mock One","contextWindow":64000,` +
				`"maxTokens":4096,"reasoning":false,"input":["text"]}]}}}`
			settings := `{"defaultProvider":"mock"}`
			for name, content := range map[string]string{
				"models.json": models, "settings.json": settings, "auth.json": `{"stale":true}`,
			} {
				require.NoError(t, os.WriteFile(filepath.Join(agentDir, name), []byte(content), 0o600))
			}

			// Each marker is computed by the shell, so the command text the
			// mock echoes back can never satisfy an assertion on its own.
			q := clcommon.ShellQuoteArg
			mock.Command = strings.Join([]string{
				"cat " + q(decoy) + " >/dev/null 2>&1 && echo decoy_$((6*7))",
				"printf '{}' > " + q(filepath.Join(agentDir, "auth.json")) + " && echo auth_$((6*7))",
				"echo x >> " + q(filepath.Join(agentDir, "models.json")) + " 2>/dev/null && echo models_$((6*7))",
				"echo x >> " + q(filepath.Join(agentDir, "settings.json")) + " 2>/dev/null && echo settings_$((6*7))",
				"mkdir " + q(filepath.Join(agentDir, "extensions", "planted")) + " 2>/dev/null && echo extensions_$((6*7))",
				"echo probe_$((6*7))",
			}, "; ")

			const sessionID = "42222222-3333-4444-8555-666666666666"
			h := harness.MustGet(harness.PiName)
			commandLine := h.Spawn.BuildCommand(harness.SpawnSpec{
				SessionID:             sessionID,
				Model:                 "mock/mock-1",
				ApprovalPolicy:        harness.PiApprovalAuto,
				ExecutablePath:        resolved.Path,
				ExecutableInterpreter: resolved.Interpreter,
				// One turn, then exit: the smoke needs no pane.
				ExtraArgs:     []string{"--print"},
				InitialPrompt: "TOOL: run the probe",
			})
			environment := []sandboxpolicy.EnvironmentEntry{
				{Name: "HOME", Value: root},
				{Name: harness.PiAgentDirEnvVar, Value: agentDir},
			}

			sockets := sandboxpolicy.UnixSocketRules{Mode: sandboxpolicy.AccessModeClosed}
			snapshot := sandboxpolicy.EmptySnapshot()
			snapshot.Effective.Network = &tc.rules
			snapshot.Effective.UnixSockets = &sockets
			spec, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
				HarnessName:      harness.PiName,
				Cwd:              workDir,
				Snapshot:         &snapshot,
				StateRoot:        agentDir,
				Environment:      environment,
				HarnessReadPaths: resolved.ReadPaths,
			})
			require.NoError(t, err)
			wrapped, err := WrapTclaudeLayerSpec(bwrapBinary, spec, commandLine)
			require.NoError(t, err)

			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/bin/sh", "-c", wrapped)
			cmd.Dir = workDir
			cmd.Env = []string{
				"HOME=" + root,
				"PATH=" + os.Getenv("PATH"),
				harness.PiAgentDirEnvVar + "=" + agentDir,
				"NO_PROXY=" + tc.providerAddress,
				"no_proxy=" + tc.providerAddress,
			}
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "production pi launch failed:\n%s", out)
			require.GreaterOrEqual(t, mock.Calls(), 2,
				"pi must reach its provider, run the tool and send the result back:\n%s", out)

			result := mock.LastMessages()
			assert.Contains(t, result, "probe_42", "the probe ran inside the sandbox")
			assert.Contains(t, result, "auth_42", "pi's login stays writable, so a token refresh lands")
			for _, marker := range []string{"decoy_42", "models_42", "settings_42", "extensions_42"} {
				assert.NotContains(t, result, marker)
			}
			raw, err := os.ReadFile(filepath.Join(agentDir, "models.json"))
			require.NoError(t, err)
			assert.Equal(t, models, string(raw), "the floor kept models.json read-only")
			assert.NoDirExists(t, filepath.Join(agentDir, "extensions", "planted"))

			var sessionFile string
			_ = filepath.WalkDir(filepath.Join(agentDir, "sessions"), func(path string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() && strings.HasSuffix(path, "_"+sessionID+".jsonl") {
					sessionFile = path
				}
				return nil
			})
			assert.NotEmpty(t, sessionFile, "pi persisted the session it was launched with")
		})
	}
}

func newPiTclaudeLayerSmokeRoot(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	base := filepath.Join(home, ".cache")
	require.NoError(t, os.MkdirAll(base, 0o700))
	root, err := os.MkdirTemp(base, "tclaude-pi-layer-smoke-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	root, err = filepath.EvalSymlinks(root)
	require.NoError(t, err)
	return root
}
