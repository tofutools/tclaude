package copilot

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

func TestNativeApprovalModesReachFreshAndContinuedLaunch(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	for _, tc := range []struct {
		mode   model.ApprovalMode
		native string
	}{
		{model.ApprovalInherit, ""}, {model.ApprovalAllowTools, "--allow-all-tools"}, {model.ApprovalYolo, "--yolo"},
	} {
		mode := tc.mode
		t.Run(string(mode), func(t *testing.T) {
			root, err := os.MkdirTemp("/tmp", "tcl-copilot-modes-")
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
				request := ports.PreparationRequest{Intent: intent, Spec: model.ResolvedExecutionSpec{ExecutionID: id, Attempt: 1, Harness: Name, WorkingDirectory: cwd, Approval: mode, Sandbox: model.SandboxUnconfined}}
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
					if e != nil {
						return false
					}
					if tc.native == "" {
						return !strings.Contains(string(raw), "--allow-all-tools") && !strings.Contains(string(raw), "--yolo") && !strings.Contains(string(raw), "--no-ask-user")
					}
					return strings.Contains(string(raw), tc.native+"\n") && strings.Contains(string(raw), "--no-ask-user\n")
				}, 3*time.Second, 10*time.Millisecond)
				if intent == ports.StartContinue {
					raw, err := os.ReadFile(filepath.Join(cwd, "argv"))
					require.NoError(t, err)
					require.Contains(t, string(raw), "--resume="+nativeID+"\n")
				}
				prior = released.Evidence
				record, err := decodeEvidence(prior)
				require.NoError(t, err)
				nativeID = record.NativeID
				raw, err := os.ReadFile(filepath.Join(cwd, "argv"))
				require.NoError(t, err)
				require.NotContains(t, string(raw), "--allow-all-paths")
				require.NotContains(t, string(raw), "--allow-all-urls")
				if mode == model.ApprovalYolo {
					require.NotContains(t, string(raw), "--allow-all-tools")
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				_, err = released.Runtime.Stop(ctx, ports.StopRequest{Force: true})
				cancel()
				require.NoError(t, err)
			}
		})
	}
}
