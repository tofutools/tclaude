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
// so the probes run as the agent would. pi runs against tclaude's walled agent
// directory: its auth.json copy is writable, while the operator's agent
// directory stays read-only, existing files and missing ones alike, even
// through the walled directory's links. A path outside the grants is
// invisible, pi persists the session it was launched with in the operator's
// store, and tclaude's status extension loads from its read-only mount and
// reaches the hook callback.
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
	extension, err := harness.MaterializePiExtension()
	require.NoError(t, err)

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
			t.Setenv(harness.PiAgentDirEnvVar, agentDir)
			// The hook callback the extension runs, standing in for tclaude:
			// it records each payload in the workspace, which the wall leaves
			// writable.
			payloads := filepath.Join(workDir, "payloads.jsonl")
			recorder := filepath.Join(workDir, "hook-recorder")
			require.NoError(t, os.WriteFile(recorder, []byte("#!/bin/sh\n[ \"$1 $2\" = \"session hook-callback\" ] || exit 64\ncat >> "+
				clcommon.ShellQuoteArg(payloads)+"\n"), 0o700))

			mock := pifixture.NewMockAt(t, tc.providerAddress+":0")
			models := `{"providers":{"mock":{"baseUrl":"` + mock.URL + `/v1","api":"openai-completions",` +
				`"apiKey":"mock","models":[{"id":"mock-1","name":"Mock One","contextWindow":64000,` +
				`"maxTokens":4096,"reasoning":false,"input":["text"]}]}}}`
			settings := `{"defaultProvider":"mock"}`
			for name, content := range map[string]string{
				"models.json": models, "settings.json": settings, "auth.json": piSmokeAuth,
			} {
				require.NoError(t, os.WriteFile(filepath.Join(agentDir, name), []byte(content), 0o600))
			}

			// Each marker is computed by the shell, so the command text the
			// mock echoes back can never satisfy an assertion on its own.
			q := clcommon.ShellQuoteArg
			mock.Command = strings.Join([]string{
				"cat " + q(decoy) + " >/dev/null 2>&1 && echo decoy_$((6*7))",
				"printf '{}' > \"$PI_CODING_AGENT_DIR/auth.json\" && echo walledauth_$((6*7))",
				"printf '{}' > " + q(filepath.Join(agentDir, "auth.json")) + " 2>/dev/null && echo hostauth_$((6*7))",
				"echo x >> " + q(filepath.Join(agentDir, "models.json")) + " 2>/dev/null && echo models_$((6*7))",
				"printf '{}' > " + q(filepath.Join(agentDir, "mcp.json")) + " 2>/dev/null && echo mcp_$((6*7))",
				"echo x >> \"$PI_CODING_AGENT_DIR/settings.json\" 2>/dev/null && echo settings_$((6*7))",
				"mkdir -p \"$PI_CODING_AGENT_DIR/extensions/planted\" 2>/dev/null && echo extensions_$((6*7))",
				"echo probe_$((6*7))",
			}, "; ")

			const sessionID = "42222222-3333-4444-8555-666666666666"
			walled, err := harness.PreparePiWalledAgentDir(sessionID)
			require.NoError(t, err)
			t.Cleanup(func() { _ = os.RemoveAll(walled) })
			h := harness.MustGet(harness.PiName)
			commandLine := h.Spawn.BuildCommand(harness.SpawnSpec{
				SessionID:             sessionID,
				Model:                 "mock/mock-1",
				ApprovalPolicy:        harness.PiApprovalAuto,
				ExecutablePath:        resolved.Path,
				ExecutableInterpreter: resolved.Interpreter,
				StatusExtensionPath:   extension,
				TclaudeExecutable:     recorder,
				HarnessStateDir:       walled,
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
				StateRoot:        walled,
				Environment:      environment,
				HarnessReadPaths: append(append([]string(nil), resolved.ReadPaths...), filepath.Dir(extension)),
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
			assert.Contains(t, result, "walledauth_42", "the walled auth.json copy is writable, so a refresh lands")
			for _, marker := range []string{"decoy_42", "hostauth_42", "models_42", "mcp_42", "settings_42", "extensions_42"} {
				assert.NotContains(t, result, marker)
			}
			raw, err := os.ReadFile(filepath.Join(agentDir, "models.json"))
			require.NoError(t, err)
			assert.Equal(t, models, string(raw), "models.json stayed read-only")
			raw, err = os.ReadFile(filepath.Join(agentDir, "auth.json"))
			require.NoError(t, err)
			assert.Equal(t, piSmokeAuth, string(raw), "the operator's auth.json stayed read-only")
			assert.NoFileExists(t, filepath.Join(agentDir, "mcp.json"), "a missing config file cannot be created")
			assert.NoDirExists(t, filepath.Join(agentDir, "extensions"))

			var sessionFile string
			_ = filepath.WalkDir(filepath.Join(agentDir, "sessions"), func(path string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() && strings.HasSuffix(path, "_"+sessionID+".jsonl") {
					sessionFile = path
				}
				return nil
			})
			assert.NotEmpty(t, sessionFile, "pi persisted the session it was launched with")
			recorded, err := os.ReadFile(payloads)
			require.NoError(t, err, "the status extension must load inside the wall and run the hook callback:\n%s", out)
			assert.Contains(t, string(recorded), sessionID, "the extension reports the launched session")
		})
	}
}

// piSmokeAuth is a valid auth.json the probe's overwrite would change.
const piSmokeAuth = `{"unused":{"type":"api_key","key":"unused"}}`

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
