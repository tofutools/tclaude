package claude

import (
	"context"
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

func TestAutoMemoryReachesFreshAndContinuedLaunch(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	t.Setenv("CLAUDE_CODE_DISABLE_AUTO_MEMORY", "ambient-conflict")
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
				request := ports.PreparationRequest{Intent: intent, Spec: model.ResolvedExecutionSpec{ExecutionID: id, Attempt: 1, Harness: Name, WorkingDirectory: cwd, Approval: model.ApprovalManual, AutoMemory: enabled, Sandbox: model.SandboxWorkspaceWrite}}
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
					memory, err := os.ReadFile(filepath.Join(cwd, "memory"))
					return err == nil && string(memory) == map[bool]string{false: "1", true: "0"}[enabled]
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
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				_, err = released.Runtime.Stop(ctx, ports.StopRequest{Force: true})
				cancel()
				require.NoError(t, err)
			}
		})
	}
}
