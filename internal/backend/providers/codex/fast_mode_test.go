package codex

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

func TestFastModeReachesFreshAndContinuedLaunch(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	for _, mode := range []model.FastMode{"", model.FastModeOn, model.FastModeOff} {
		t.Run(string(mode), func(t *testing.T) {
			root, err := os.MkdirTemp("/tmp", "tcl-codex-modes-")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
			executable := filepath.Join(root, "fixture")
			require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\nif [ \"$3\" = --help ]; then exit 0; fi\nprintf '%s\\n' \"$@\" > \"$PWD/argv\"\nwhile IFS= read -r line; do :; done\n"), 0700))
			provider, err := New(Config{Executable: executable, PrivateRoot: root, NativeHome: filepath.Join(root, "native")})
			require.NoError(t, err)
			var prior model.ProviderEvidence
			var nativeID string
			for _, intent := range []ports.StartIntent{ports.StartFresh, ports.StartContinue} {
				cwd := filepath.Join(root, string(intent))
				require.NoError(t, os.MkdirAll(cwd, 0700))
				id := model.ExecutionID("execution_" + string(intent))
				request := ports.PreparationRequest{Intent: intent, Spec: model.ResolvedExecutionSpec{ExecutionID: id, Attempt: 1, Harness: Name, WorkingDirectory: cwd, Approval: model.ApprovalOnRequest, FastMode: mode, Sandbox: model.SandboxReadOnly}}
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
					return e == nil && strings.Contains(string(raw), "-a\non-request\n-s\nread-only\n")
				}, 3*time.Second, 10*time.Millisecond)
				rawArgs, err := os.ReadFile(filepath.Join(cwd, "argv"))
				require.NoError(t, err)
				switch mode {
				case "":
					require.NotContains(t, string(rawArgs), "service_tier=")
				case model.FastModeOn:
					require.Contains(t, string(rawArgs), "service_tier=\"fast\"")
				case model.FastModeOff:
					require.Contains(t, string(rawArgs), "service_tier=\"default\"")
				}
				if intent == ports.StartContinue {
					raw, err := os.ReadFile(filepath.Join(cwd, "argv"))
					require.NoError(t, err)
					require.Contains(t, string(raw), "resume\n"+nativeID+"\n")
				}
				runtime := released.Runtime.(*Runtime)
				if intent == ports.StartFresh {
					runtime.observations = &observationSink{}
					nativeID = "00000000-0000-4000-8000-000000000001"
					writeHookEvent(t, runtime.spool.Directory(), sessionStartEvent{SessionID: nativeID, HookEventName: "SessionStart", Source: "startup"})
					_, err = runtime.Observe(context.Background())
					require.NoError(t, err)
					require.Equal(t, nativeID, runtime.nativeID)
				}
				prior, err = runtime.providerEvidence()
				require.NoError(t, err)
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				_, err = released.Runtime.Stop(ctx, ports.StopRequest{Force: true})
				cancel()
				require.NoError(t, err)
			}
		})
	}
}
