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

func TestQuestionTimeoutReachesFreshAndContinuedLaunch(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	for _, mode := range []model.AskUserQuestionTimeout{"", "inherit", "never", "60s", "5m", "10m"} {
		t.Run("timeout_"+string(mode), func(t *testing.T) {
			root, err := os.MkdirTemp("/tmp", "tcl-claude-modes-")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
			executable := filepath.Join(root, "fixture")
			require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$PWD/argv\"\nwhile IFS= read -r line; do :; done\n"), 0700))
			provider, err := New(Config{Executable: executable, PrivateRoot: root, NativeHome: filepath.Join(root, "native")})
			require.NoError(t, err)
			var prior model.ProviderEvidence
			var nativeID string
			for _, intent := range []ports.StartIntent{ports.StartFresh, ports.StartContinue} {
				cwd := filepath.Join(root, string(intent))
				require.NoError(t, os.MkdirAll(cwd, 0700))
				id := model.ExecutionID("execution_" + string(intent))
				request := ports.PreparationRequest{Intent: intent, Spec: model.ResolvedExecutionSpec{ExecutionID: id, Attempt: 1, Harness: Name, WorkingDirectory: cwd, Approval: model.ApprovalManual, AskUserQuestionTimeout: mode, Sandbox: model.SandboxWorkspaceWrite}}
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
					return true
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
				args := strings.Split(string(raw), "\n")
				count := 0
				for i, arg := range args {
					if arg != "--settings" {
						continue
					}
					count++
					var settings map[string]any
					require.NoError(t, json.Unmarshal([]byte(args[i+1]), &settings))
					if mode == "" || mode == "inherit" {
						require.NotContains(t, settings, "askUserQuestionTimeout")
					} else {
						require.Equal(t, string(mode), settings["askUserQuestionTimeout"])
					}
					require.Contains(t, settings, "hooks")
					require.Contains(t, settings, "statusLine")
				}
				require.Equal(t, 1, count)

				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				_, err = released.Runtime.Stop(ctx, ports.StopRequest{Force: true})
				cancel()
				require.NoError(t, err)
			}
		})
	}
}
