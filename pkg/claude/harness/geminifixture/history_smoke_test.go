package geminifixture

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// Exercise the native CLI after both remints, not just tclaude's transcript reader.
func TestGeminiPortableHistoryNativeRoundTrip(t *testing.T) {
	w := NewWorld(t)
	homeProject := w.Project
	source := uuid.NewString()
	w.ask(harness.AskSpec{SessionID: source, Prompt: "original home question"}, TrustWorkspaceEnv).RequireOK(t)
	history := gemini().History
	require.NotNil(t, history)
	raw, err := history.Export(source, homeProject)
	require.NoError(t, err)
	w.Project = testutil.CanonicalTempDir(t)
	visitor, cleanup, err := history.Import(raw, source, w.Project)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	w.ask(harness.AskSpec{ResumeID: visitor, Prompt: "new visitor question"}, TrustWorkspaceEnv).RequireOK(t)
	calls := w.Mock.ModelCalls()
	require.Contains(t, calls[len(calls)-1].Body, "original home question")
	require.Contains(t, calls[len(calls)-1].Body, w.Mock.Reply)
	raw, err = history.Export(visitor, w.Project)
	require.NoError(t, err)
	w.Project = homeProject
	returned, cleanup, err := history.Import(raw, visitor, w.Project)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	w.ask(harness.AskSpec{ResumeID: returned, Prompt: "returned home question"}, TrustWorkspaceEnv).RequireOK(t)
	calls = w.Mock.ModelCalls()
	require.Contains(t, calls[len(calls)-1].Body, "original home question")
	require.Contains(t, calls[len(calls)-1].Body, "new visitor question")
	require.Contains(t, calls[len(calls)-1].Body, w.Mock.Reply)
}
