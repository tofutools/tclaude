package transport

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestPublicSandboxPreviewObservesPathsWithoutPublishingOrCreating(t *testing.T) {
	root := t.TempDir()
	private := filepath.Join(root, "private")
	require.NoError(t, os.Mkdir(private, 0700))
	store, err := sqlite.Open(filepath.Join(private, "backend.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	inspector, err := host.NewSandboxPathInspector([]string{private})
	require.NoError(t, err)
	service := app.New(store, providers.NewRegistry()).WithSandboxPathInspector(inspector)
	handler := testHandler(t, service)
	parent, err := service.SaveSandboxProfile(context.Background(), app.SaveSandboxProfileRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "parent"}, ID: "sandbox_parent", Name: "Parent", Policy: model.SandboxPolicy{Filesystem: []model.SandboxFilesystemRule{{HostPath: private, Access: model.SandboxFilesystemRead}}}})
	require.NoError(t, err)
	missing := filepath.Join(root, "not-created")
	policy := model.SandboxPolicy{Includes: []model.SandboxProfileRef{parent.Revision.Ref}, Filesystem: []model.SandboxFilesystemRule{{HostPath: missing, Access: model.SandboxFilesystemWrite}}, PreLaunch: []model.SandboxSetupBlock{{Name: "setup", Script: "exit 91"}}}
	body, err := json.Marshal(struct {
		Policy model.SandboxPolicy `json:"policy"`
	}{policy})
	require.NoError(t, err)
	response := request(handler, "POST", "/v2/sandbox-profiles/preview", string(body), testCredential)
	require.Equal(t, 200, response.Code, response.Body.String())
	var preview app.SandboxPolicyPreview
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &preview))
	require.Len(t, preview.Includes, 1)
	require.Equal(t, parent.Revision.Ref, preview.Includes[0].Ref)
	require.Len(t, preview.Paths, 2)
	require.Equal(t, "refused", preview.Paths[0].Observation.State)
	require.Equal(t, &parent.Revision.Ref, preview.Paths[0].Source)
	require.Equal(t, "missing", preview.Paths[1].Observation.State)
	require.Nil(t, preview.Paths[1].Source)
	require.NoDirExists(t, missing)
	catalog, err := service.ListSandboxProfiles(context.Background(), model.OperatorPrincipal(), true)
	require.NoError(t, err)
	require.Len(t, catalog, 1)
	agentHandler, err := NewHandler(service, fixedCaller{model.AgentPrincipal("agent")})
	require.NoError(t, err)
	require.Equal(t, 403, request(agentHandler, "POST", "/v2/sandbox-profiles/preview", string(body), "").Code)
}
