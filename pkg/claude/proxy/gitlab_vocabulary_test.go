package proxy_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/proxy"
)

// gitlab_vocabulary_test.go pins the CLI's completion copies of the GitLab
// proxy's vocabularies to the daemon's gates, for the reason the GitHub pins
// exist: the CLI must not import the daemon, so the lists are copies, and a
// copy drifts.

func TestGLPipelineStatusCompletionMatchesTheGate(t *testing.T) {
	assert.ElementsMatch(t, agentd.GLPipelineStatusesForTest(), proxy.GLPipelineStatusAlternativesForTest(),
		"the CLI's completion vocabulary and the daemon's allow-list have drifted apart")
}

// Order matters for the merge method: an empty value resolves to the gate's
// first entry.
func TestGLMergeMethodCompletionMatchesTheGate(t *testing.T) {
	assert.Equal(t, agentd.GLMergeMethodsForTest(), proxy.GLMergeMethodAlternativesForTest(),
		"the CLI's completion vocabulary and the daemon's allow-list have drifted apart")
}
