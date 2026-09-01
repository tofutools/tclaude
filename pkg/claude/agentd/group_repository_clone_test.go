package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestPrepareGroupRepositoryCloneNormalizesGitHubForms(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "nested", "repo")
	for _, tc := range []struct {
		name, repository, transport, cloneURL string
	}{
		{"short ssh", "acme/payments", "ssh", "git@github.com:acme/payments.git"},
		{"web https", "https://github.com/acme/payments.git", "https", "https://github.com/acme/payments.git"},
		{"scp copied", "git@github.com:acme/payments.git", "ssh", "git@github.com:acme/payments.git"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := prepareGroupRepositoryClone(&groupRepositoryClone{
				Repository: tc.repository, Transport: tc.transport, Destination: destination,
			})
			require.NoError(t, err)
			assert.Equal(t, tc.cloneURL, plan.CloneURL)
			assert.Equal(t, "https://github.com/acme/payments", plan.WebURL)
			assert.Equal(t, "acme/payments", plan.Label)
		})
	}
}

func TestCloneGroupRepositoryCreatesMissingParents(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "one", "two", "repo")
	previous := runGroupRepositoryClone
	t.Cleanup(func() { runGroupRepositoryClone = previous })
	runGroupRepositoryClone = func(_ context.Context, cloneURL, gotDestination string) ([]byte, error) {
		assert.Equal(t, "git@github.com:acme/repo.git", cloneURL)
		assert.Equal(t, destination, gotDestination)
		info, err := os.Stat(filepath.Dir(destination))
		require.NoError(t, err)
		assert.True(t, info.IsDir())
		return nil, os.Mkdir(destination, 0o755)
	}
	require.NoError(t, cloneGroupRepository(&preparedGroupRepositoryClone{
		CloneURL: "git@github.com:acme/repo.git", Destination: destination,
	}))
}

func TestDashboardCreateGroupClonesAndAttachesRepository(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)
	destination := filepath.Join(t.TempDir(), "new", "repo")
	previous := runGroupRepositoryClone
	t.Cleanup(func() { runGroupRepositoryClone = previous })
	runGroupRepositoryClone = func(_ context.Context, _, gotDestination string) ([]byte, error) {
		require.Equal(t, destination, gotDestination)
		return nil, os.Mkdir(gotDestination, 0o755)
	}
	body, err := json.Marshal(map[string]any{
		"name": "repo-team",
		"repository_clone": map[string]any{
			"repository": "acme/repo", "transport": "ssh",
			"destination": destination, "attach": true,
		},
	})
	require.NoError(t, err)
	w := httptest.NewRecorder()
	serveDashboardGroups(w, dashboardRequest(http.MethodPost, "/api/groups", string(body)))
	require.Equal(t, http.StatusCreated, w.Code, "body=%s", w.Body.String())

	group, err := db.GetAgentGroupByName("repo-team")
	require.NoError(t, err)
	require.NotNil(t, group)
	assert.Equal(t, destination, group.DefaultCwd)
	assert.Equal(t, "https://github.com/acme/repo", group.AttachmentURL)
	assert.Equal(t, "acme/repo", group.AttachmentLabel)
}

func TestDashboardCreateGroupCloneFailureKeepsGroupUncreated(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)
	destination := filepath.Join(t.TempDir(), "missing-parent", "repo")
	previous := runGroupRepositoryClone
	t.Cleanup(func() { runGroupRepositoryClone = previous })
	runGroupRepositoryClone = func(context.Context, string, string) ([]byte, error) {
		return []byte("Permission denied (publickey)."), errors.New("exit status 128")
	}
	body, err := json.Marshal(map[string]any{
		"name": "failed-team",
		"repository_clone": map[string]any{
			"repository": "acme/repo", "transport": "ssh", "destination": destination,
		},
	})
	require.NoError(t, err)
	w := httptest.NewRecorder()
	serveDashboardGroups(w, dashboardRequest(http.MethodPost, "/api/groups", string(body)))
	assert.Equal(t, http.StatusBadGateway, w.Code, "body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "Permission denied (publickey)")
	group, err := db.GetAgentGroupByName("failed-team")
	require.NoError(t, err)
	assert.Nil(t, group)
	_, err = os.Stat(filepath.Dir(destination))
	assert.NoError(t, err, "the clone parent is created automatically before git runs")
}

func TestTemplateInstantiateClonesBeforeCreatingGroup(t *testing.T) {
	setupTestDB(t)
	_, err := db.CreateGroupTemplate(&db.GroupTemplate{Name: "empty-template"})
	require.NoError(t, err)
	destination := filepath.Join(t.TempDir(), "template", "repo")
	previous := runGroupRepositoryClone
	t.Cleanup(func() { runGroupRepositoryClone = previous })
	runGroupRepositoryClone = func(_ context.Context, _, gotDestination string) ([]byte, error) {
		require.Equal(t, destination, gotDestination)
		return nil, os.Mkdir(gotDestination, 0o755)
	}
	body, err := json.Marshal(map[string]any{
		"group_name": "template-repo-team",
		"repository_clone": map[string]any{
			"repository": "acme/template-repo", "transport": "https",
			"destination": destination, "attach": true,
		},
	})
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodPost, "/v1/templates/empty-template/instantiate", bytes.NewReader(body))
	r.SetPathValue("name", "empty-template")
	r = asDashboardHumanPeer(r)
	w := httptest.NewRecorder()
	handleTemplateInstantiate(w, r)
	require.Equal(t, http.StatusCreated, w.Code, "body=%s", w.Body.String())

	group, err := db.GetAgentGroupByName("template-repo-team")
	require.NoError(t, err)
	require.NotNil(t, group)
	assert.Equal(t, destination, group.DefaultCwd)
	assert.Equal(t, "https://github.com/acme/template-repo", group.AttachmentURL)
}
