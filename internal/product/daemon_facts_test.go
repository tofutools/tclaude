package product

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func TestGitHubCompositionPinsSourceAndKeepsCredentialPrivate(t *testing.T) {
	credential := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(credential, []byte("disposable-secret\n"), 0600))
	sources, err := configuredGitHubSources([]string{"checks=owner/repo#42"}, credential)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	require.Equal(t, "checks", sources[0].SourceID())
	// A mismatched public rule target is refused before any HTTP request.
	_, err = sources[0].CollectAutomationFacts(context.Background(), ports.AutomationFactCollectRequest{Resource: model.AutomationFactResource{Kind: model.FactResourceRepositoryPullReq, Repository: "owner/other", PullRequest: 42}, Now: time.Now(), Limit: 64})
	require.Error(t, err)
	require.NotContains(t, err.Error(), "disposable-secret")
	for _, specs := range [][]string{{"checks=owner/repo#42", "checks=owner/repo#43"}, {"checks=owner/repo#0"}, {"checks=https://example.com#42"}, {"bad\nname=owner/repo#42"}, {"a=o/r#1", "b=o/r#2", "c=o/r#3", "d=o/r#4", "e=o/r#5"}} {
		_, err = configuredGitHubSources(specs, credential)
		require.Error(t, err)
	}
	require.NoError(t, os.Chmod(credential, 0644))
	_, err = configuredGitHubSources([]string{"checks=owner/repo#42"}, credential)
	require.ErrorContains(t, err, "private regular")
	_, err = configuredGitHubSources(nil, credential)
	require.Error(t, err)
}
