package agentd_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// mountPathFlowHome gives the flow its own HOME so the protected-root wall and
// "~" expansion resolve inside the test tree.
func mountPathFlowHome(t *testing.T, dirs ...string) []string {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("HOME", home)
	out := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		full := filepath.Join(home, dir)
		require.NoError(t, os.MkdirAll(full, 0o755))
		out = append(out, full)
	}
	return out
}

// TestSandboxProfileMountPathRoundTripsThroughTheRegistry is the authoring
// round-trip: a mount_path survives create → read unchanged, with the host path
// still canonicalized as the authority-bearing side. The dashboard editor sends
// exactly this shape.
func TestSandboxProfileMountPathRoundTripsThroughTheRegistry(t *testing.T) {
	f := newFlow(t)
	dirs := mountPathFlowHome(t, "datasets")

	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name": "mounted",
		"filesystem": []any{
			map[string]any{"path": dirs[0], "access": "read", "mount_path": "/data"},
		},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	rec = profileReq(t, f, http.MethodGet, "/v1/sandbox-profiles/mounted", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var got struct {
		Filesystem []sandboxpolicy.FilesystemGrant `json:"filesystem"`
	}
	testharness.DecodeJSON(t, rec, &got)
	assert.Equal(t, []sandboxpolicy.FilesystemGrant{
		{Path: dirs[0], Access: sandboxpolicy.AccessRead, MountPath: "/data"},
	}, got.Filesystem)
}

func TestSandboxProfileMountPathOnDenyIsRejected(t *testing.T) {
	f := newFlow(t)
	dirs := mountPathFlowHome(t, "secret")

	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name": "bad",
		"filesystem": []any{
			map[string]any{"path": dirs[0], "access": "deny", "mount_path": "/data"},
		},
	})
	require.Equalf(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "mount_path is not allowed on a deny rule")
}

func TestSandboxProfileMountPathCollisionIsRejected(t *testing.T) {
	f := newFlow(t)
	dirs := mountPathFlowHome(t, "one", "two")

	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name": "collide",
		"filesystem": []any{
			map[string]any{"path": dirs[0], "access": "read", "mount_path": "/data"},
			map[string]any{"path": dirs[1], "access": "read", "mount_path": "/data"},
		},
	})
	require.Equalf(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "is claimed by two different host paths")
}

// TestSandboxProfileMountPathEnforcementPreviewIsCapabilityHonest is the
// effective-preview half of the empirical-evidence ruling: the cell claims
// enforcement only where a mount namespace exists, and every other surface is
// bucketed Refused with the projection spelled out rather than quietly
// downgraded to a host-path mount.
func TestSandboxProfileMountPathEnforcementPreviewIsCapabilityHonest(t *testing.T) {
	f := newFlow(t)
	dirs := mountPathFlowHome(t, "datasets")
	draft := map[string]any{
		"name": "mounted",
		"filesystem": []any{
			map[string]any{"path": dirs[0], "access": "read", "mount_path": "/data"},
		},
	}

	for _, tc := range []struct {
		name           string
		implementation string
		harnessName    string
		platform       string
		outcome        string
	}{
		{"linux tclaude-layer", "tclaude-layer", harness.DefaultName, "linux", harness.AccessPredictionEnforced},
		{"linux stacked", "stacked", harness.DefaultName, "linux", harness.AccessPredictionEnforced},
		{"darwin tclaude-layer", "tclaude-layer", harness.DefaultName, "darwin", harness.AccessPredictionRefused},
		{"linux harness-builtin", "harness-builtin", harness.DefaultName, "linux", harness.AccessPredictionRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profile-enforcement", map[string]any{
				"draft": draft,
				"targets": []any{map[string]any{
					"implementation": tc.implementation,
					"harness":        tc.harnessName,
					"platform":       tc.platform,
				}},
			})
			require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
			var got struct {
				Targets []struct {
					Axes harness.PredictedAccessAxes `json:"axes"`
				} `json:"targets"`
			}
			testharness.DecodeJSON(t, rec, &got)
			require.Len(t, got.Targets, 1)
			assert.Equal(t, tc.outcome, got.Targets[0].Axes.Filesystem.Outcome)
			assert.Contains(t, got.Targets[0].Axes.Filesystem.Detail, "/data",
				"the preview must name the sandbox path the rule projects onto")
			if tc.outcome == harness.AccessPredictionRefused {
				assert.Contains(t, got.Targets[0].Axes.Filesystem.Detail, "mount namespace",
					"the refusal must name the missing capability")
			}
		})
	}
}
