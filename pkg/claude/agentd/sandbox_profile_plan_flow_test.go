package agentd_test

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestSandboxProfilePlanHypotheticalFlowsThroughMuxWithoutLaunchAuthority(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	workspace := filepath.Join(f.World.HomeDir, "workspace")
	require.NoError(t, os.MkdirAll(workspace, 0o755))
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{
		Name: "workspace",
		Filesystem: []db.SandboxFilesystemGrant{{
			Path: workspace, Access: "write",
		}},
	})
	require.NoError(t, err)

	req := testharness.JSONRequest(t, http.MethodPost, "/v1/sandbox-profile-plan", map[string]any{
		"group":           "crew",
		"sandbox_profile": "workspace",
		"cwd":             workspace,
		"for":             "tclaude-layer/claude/" + runtime.GOOS,
	})
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(req))
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var response struct {
		Source        string `json:"source"`
		RecordedAxes  any    `json:"recorded_axes"`
		PredictedAxes any    `json:"predicted_axes"`
		Plan          struct {
			Applicable bool `json:"applicable"`
			Entries    []struct {
				Class       int    `json:"class"`
				Disposition string `json:"disposition"`
			} `json:"entries"`
		} `json:"plan"`
	}
	testharness.DecodeJSON(t, rec, &response)
	assert.Equal(t, "hypothetical", response.Source)
	assert.Nil(t, response.RecordedAxes, "hypothetical inspection must not masquerade as recorded fact")
	assert.NotNil(t, response.PredictedAxes)
	assert.True(t, response.Plan.Applicable)
	assert.NotEmpty(t, response.Plan.Entries)
	assert.Contains(t, response.Plan.Entries, struct {
		Class       int    `json:"class"`
		Disposition string `json:"disposition"`
	}{Class: 2, Disposition: "present"})

	otherPlatform := "darwin"
	if runtime.GOOS == "darwin" {
		otherPlatform = "linux"
	}
	req = testharness.JSONRequest(t, http.MethodPost, "/v1/sandbox-profile-plan", map[string]any{
		"cwd": "/path/that/exists/only/on/the/requested/host",
		"for": "tclaude-layer/claude/" + otherPlatform,
	})
	rec = testharness.Serve(f.Mux, agentd.AsHumanPeer(req))
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var crossPlatform struct {
		PredictedAxes any `json:"predicted_axes"`
		Plan          struct {
			Applicable bool   `json:"applicable"`
			Reason     string `json:"reason"`
		} `json:"plan"`
	}
	testharness.DecodeJSON(t, rec, &crossPlatform)
	assert.NotNil(t, crossPlatform.PredictedAxes)
	assert.False(t, crossPlatform.Plan.Applicable)
	assert.Contains(t, crossPlatform.Plan.Reason, "requires daemon host platform")
}
