package agentd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLiveRunSeanceHarness_SanitizesAuthorityAndCapturesAnswer(t *testing.T) {
	t.Setenv(humanTokenEnvVar, "operator-secret")
	t.Setenv("TCLAUDE_IGNORE_HOOKS", "wrong")
	t.Setenv("TCLAUDE_AGENT_HINT", "wrong")

	result := liveRunSeanceHarness(context.Background(), SeanceExecPlan{
		Argv: []string{
			"sh", "-c",
			`printf '%s|%s|%s|%s' "${TCLAUDE_HUMAN_TOKEN-unset}" "$TCLAUDE_IGNORE_HOOKS" "$TCLAUDE_AGENT_HINT" "$POLICY_OWNER"`,
		},
		Cwd: t.TempDir(),
		Environment: map[string]string{
			"POLICY_OWNER":         "predecessor",
			humanTokenEnvVar:       "smuggled-secret",
			"TCLAUDE_IGNORE_HOOKS": "wrong-again",
			"TCLAUDE_AGENT_HINT":   "wrong-again",
		},
	})
	require.NoError(t, result.Err)
	assert.True(t, result.Started)
	assert.Equal(t, "unset|true|1|predecessor", result.Stdout)
	assert.Empty(t, result.Stderr)
	assert.False(t, result.StdoutTruncated)
}

func TestLiveRunSeanceHarness_DistinguishesInitAndExitFailures(t *testing.T) {
	t.Run("initialization", func(t *testing.T) {
		result := liveRunSeanceHarness(context.Background(), SeanceExecPlan{
			Argv: []string{"definitely-not-a-real-tclaude-seance-binary"},
			Cwd:  t.TempDir(),
		})
		require.Error(t, result.Err)
		assert.False(t, result.Started)
	})

	t.Run("started then exited", func(t *testing.T) {
		result := liveRunSeanceHarness(context.Background(), SeanceExecPlan{
			Argv: []string{"sh", "-c", "printf 'harness failed' >&2; exit 7"},
			Cwd:  t.TempDir(),
		})
		require.Error(t, result.Err)
		assert.True(t, result.Started)
		assert.Equal(t, "harness failed", result.Stderr)
	})
}

func TestLiveRunSeanceHarness_CancelsOnAnswerLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result := liveRunSeanceHarness(ctx, SeanceExecPlan{
		Argv: []string{"sh", "-c", "head -c 300000 /dev/zero | tr '\\0' x"},
		Cwd:  t.TempDir(),
	})
	require.Error(t, result.Err)
	assert.True(t, result.Started)
	assert.True(t, result.StdoutTruncated)
	assert.Len(t, result.Stdout, maxSeanceAnswerBytes)
	assert.True(t, strings.HasPrefix(result.Stdout, "xxxx"))
}
