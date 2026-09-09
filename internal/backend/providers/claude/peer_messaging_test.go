package claude

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func TestPeerMessagingReachesFreshAndContinuedLaunch(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}

	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[enabled], func(t *testing.T) {
			root, err := os.MkdirTemp("/tmp", "tcl-claude-modes-")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
			executable := filepath.Join(root, "fixture")
			require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$PWD/argv\"\nprintf '%s' \"$CLAUDE_CODE_DISABLE_AUTO_MEMORY\" > \"$PWD/memory\"\nwhile IFS= read -r line; do :; done\n"), 0700))
			provider, err := New(Config{Executable: executable, PrivateRoot: root, NativeHome: filepath.Join(root, "native")})
			require.NoError(t, err)
			var prior model.ProviderEvidence
			var nativeID string
			for _, intent := range []ports.StartIntent{ports.StartFresh, ports.StartContinue} {
				cwd := filepath.Join(root, string(intent))
				require.NoError(t, os.MkdirAll(cwd, 0700))
				id := model.ExecutionID("execution_" + string(intent))
				request := ports.PreparationRequest{Intent: intent, Spec: model.ResolvedExecutionSpec{ExecutionID: id, Attempt: 1, Harness: Name, WorkingDirectory: cwd, Approval: model.ApprovalManual, PeerMessaging: enabled, Sandbox: model.SandboxWorkspaceWrite}}
				if intent == ports.StartContinue {
					request.Continuation = &model.NativeConversationEvidence{Namespace: NativeNamespace, Reference: nativeID}
					request.PriorEvidence = prior
				}
				prepared, err := provider.Prepare(context.Background(), request)
				require.NoError(t, err)
				released, err := prepared.Release(context.Background(), &testPermit{execution: id})
				require.NoError(t, err)
				t.Cleanup(func() {
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					defer cancel()
					_, _ = released.Runtime.Stop(ctx, ports.StopRequest{Force: true})
				})
				require.Eventually(t, func() bool {
					raw, e := os.ReadFile(filepath.Join(cwd, "argv"))
					if e != nil || !strings.Contains(string(raw), "--settings\n") {
						return false
					}
					return strings.Contains(string(raw), "crossSessionInbound") || enabled
				}, 3*time.Second, 10*time.Millisecond)
				if intent == ports.StartContinue {
					raw, err := os.ReadFile(filepath.Join(cwd, "argv"))
					require.NoError(t, err)
					require.Contains(t, string(raw), "--resume\n"+nativeID+"\n")
				}
				prior = released.Evidence
				record, err := decodeEvidence(prior)
				require.NoError(t, err)
				nativeID = record.NativeID
				raw, err := os.ReadFile(filepath.Join(cwd, "argv"))
				require.NoError(t, err)
				require.Contains(t, string(raw), `"allowUnsandboxedCommands":false`)
				require.Contains(t, string(raw), `"enabled":true`)
				lines := strings.Split(string(raw), "\n")
				var settings map[string]any
				for i, arg := range lines {
					if arg == "--settings" {
						require.NoError(t, json.Unmarshal([]byte(lines[i+1]), &settings))
						break
					}
				}
				require.NotEmpty(t, settings["hooks"])
				if enabled {
					require.NotContains(t, settings, "crossSessionInbound")
					require.NotContains(t, settings, "isolatePeerMachines")
					require.NotContains(t, settings, "permissions")
				} else {
					require.Equal(t, "refuse", settings["crossSessionInbound"])
					require.Equal(t, true, settings["isolatePeerMachines"])
					require.Equal(t, map[string]any{"deny": []any{"ListAgents"}}, settings["permissions"])
				}

				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				_, err = released.Runtime.Stop(ctx, ports.StopRequest{Force: true})
				cancel()
				require.NoError(t, err)
			}
		})
	}
}
