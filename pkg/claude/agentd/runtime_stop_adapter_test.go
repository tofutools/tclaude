package agentd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	platformexec "github.com/tofutools/tclaude/pkg/claude/platform/execution"
	platformruntime "github.com/tofutools/tclaude/pkg/claude/platform/runtime"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestRuntimeStopAdapter_NativeTerminalRecipes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		recipe terminalStopRecipe
		want   []string
	}{
		{"claude", claudeStopRecipe(), []string{"Escape", "C-c", "C-c", "C-c", "C-c"}},
		{"codex", codexStopRecipe(), []string{"C-c", "C-c", "C-c", "C-c"}},
		{"copilot", copilotStopRecipe(), []string{"C-c", "C-c", "C-c", "C-c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			world := testharness.New(t)
			previousTmux := clcommon.Default
			clcommon.Default = world.Tmux
			t.Cleanup(func() { clcommon.Default = previousTmux })
			const generation = "11111111111111111111111111111111"
			tmuxName := "runtime-stop-" + tc.name
			world.Tmux.MarkAlive(tmuxName)
			world.Tmux.SetPaneIdentityForTest(tmuxName, "%1", 4242)
			target := &lifecycleTarget{
				attempt:   platformexec.AttemptRef{ExecutionID: platformexec.ID(generation)},
				sessionID: "session-" + tc.name, convID: "conv-" + tc.name,
				tmuxSession: tmuxName, generation: generation, paneID: "%1", panePID: 4242,
				softExitSettled: make(chan struct{}),
			}
			runtime := &terminalStopRuntime{
				key:    platformruntime.AttemptKey{Execution: platformexec.ID(generation)},
				target: target, recipe: tc.recipe,
			}
			ctx, cancel := context.WithCancel(context.Background())
			result := runtime.RequestStop(ctx, platformruntime.StopRequest{
				Retry: platformruntime.RetryBudget{Attempts: 1},
			})
			cancel()
			require.Equal(t, platformruntime.ControlDispatched, result.State)
			var got []string
			for _, sent := range world.Tmux.Sent() {
				got = append(got, sent.Text)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}
