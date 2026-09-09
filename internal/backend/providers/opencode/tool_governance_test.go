package opencode

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestContinuationReappliesToolGovernance(t *testing.T) {
	approval := model.ApprovalDeny
	for _, tools := range []model.ToolGovernance{model.ToolGovernanceAllow, model.ToolGovernanceAsk, model.ToolGovernanceDeny} {
		t.Run(string(tools), func(t *testing.T) {
			root, err := os.MkdirTemp("/tmp", "tclaude-opencode-continuation-")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
			executable := filepath.Join(root, "opencode-fake")
			script := "#!/bin/sh\nexec \"$OPENCODE_TEST_BINARY\" -test.run=TestOpenCodeServerHelper -- \"$@\"\n"
			require.NoError(t, os.WriteFile(executable, []byte(script), 0o700))
			provider, err := New(Config{Executable: executable, PrivateRoot: root,
				Environment: []string{"OPENCODE_TEST_BINARY=" + os.Args[0]}})
			require.NoError(t, err)

			automatic := ports.PreparationRequest{Intent: ports.StartFresh, Observations: &observationSink{}, Spec: model.ResolvedExecutionSpec{
				ExecutionID: "execution_automatic", Harness: Name, WorkingDirectory: root,
				Approval: model.ApprovalAutomatic, Sandbox: model.SandboxUnconfined,
			}}
			firstPrepared, err := provider.Prepare(context.Background(), automatic)
			require.NoError(t, err)
			first, err := firstPrepared.Release(context.Background(), &testPermit{execution: automatic.Spec.ExecutionID, operation: "operation_first"})
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, err = first.Runtime.Stop(ctx, ports.StopRequest{Force: true})
			cancel()
			require.NoError(t, err)

			prior, err := decodeEvidence(first.Evidence)
			require.NoError(t, err)
			supervised := ports.PreparationRequest{
				Intent: ports.StartContinue, Observations: &observationSink{},
				Spec: model.ResolvedExecutionSpec{ExecutionID: "execution_supervised", Harness: Name,
					WorkingDirectory: root, ToolGovernance: tools, Approval: approval, Sandbox: model.SandboxUnconfined},
				Continuation:  &model.NativeConversationEvidence{Namespace: NativeNamespace, Reference: "ses_test"},
				PriorEvidence: first.Evidence,
			}
			secondPrepared, err := provider.Prepare(context.Background(), supervised)
			require.NoError(t, err)
			description := secondPrepared.Describe()
			releaseCtx, releaseCancel := context.WithCancel(context.Background())
			releaseCancel()
			second, err := secondPrepared.Release(releaseCtx, &testPermit{execution: supervised.Spec.ExecutionID, operation: "operation_second"})
			require.Error(t, err)
			require.Equal(t, ports.ReleaseUncertain, second.State)
			require.NotNil(t, second.Runtime)
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_, _ = second.Runtime.Stop(ctx, ports.StopRequest{Force: true})
			})

			var recovered ports.RecoveryResult
			require.Eventually(t, func() bool {
				recovered, err = provider.Recover(context.Background(), ports.RecoveryRequest{
					ExecutionID: supervised.Spec.ExecutionID, Spec: supervised.Spec, Evidence: description.Evidence, Observations: &observationSink{},
				})
				return err == nil && recovered.State == ports.RecoveryControlled
			}, 2*time.Second, 20*time.Millisecond,
				"prepared continuation recovery must enforce policy before returning controlled: %v", err)

			data, err := os.ReadFile(filepath.Join(prior.StateRoot, "data", "permission.json"))
			require.NoError(t, err)
			var rules []permissionRule
			require.NoError(t, json.Unmarshal(data, &rules))
			for _, permission := range []string{"bash", "glob", "grep", "lsp", "task", "skill"} {
				action := ""
				for _, rule := range rules {
					if (rule.Permission == "*" || rule.Permission == permission) && rule.Pattern == "*" {
						action = rule.Action
					}
				}
				require.Equal(t, string(tools), action)
			}
			// Tool grants do not broaden deny approval for edits or web access.
			for _, permission := range []string{"edit", "webfetch"} {
				action := ""
				for _, rule := range rules {
					if (rule.Permission == "*" || rule.Permission == permission) && rule.Pattern == "*" {
						action = rule.Action
					}
				}
				require.Equal(t, "deny", action)
			}
		})
	}
}
