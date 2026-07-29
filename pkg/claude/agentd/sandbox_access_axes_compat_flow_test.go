package agentd_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestLegacyEnvelopeVersionsRenderByteIdenticallyAcrossBothNetworkPostures(t *testing.T) {
	f := newFlow(t)
	for _, access := range []sandboxpolicy.NetworkAccess{
		sandboxpolicy.NetworkAccessInternet,
		sandboxpolicy.NetworkAccessNone,
	} {
		name := "legacy-" + string(access)
		expectedEffective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{
			Explicit: &sandboxpolicy.Profile{Name: name, NetworkAccess: access},
		})
		require.NoError(t, err)
		expectedPlan, err := sandboxpolicy.RenderMountPlan(expectedEffective)
		require.NoError(t, err)
		expectedBytes, err := json.Marshal(expectedPlan)
		require.NoError(t, err)

		for version := 1; version <= 9; version++ {
			rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles/import", map[string]any{
				"format":         "tclaude-sandbox-profiles",
				"format_version": version,
				"on_conflict":    "overwrite",
				"profiles": []map[string]any{{
					"name": name, "filesystem": []any{}, "environment": []any{},
					"network_access": access,
				}},
			})
			require.Equalf(t, http.StatusOK, rec.Code,
				"%s format_version %d body=%s", access, version, rec.Body.String())

			snapshot, err := db.ResolveEffectiveSandboxSnapshot(0, name)
			require.NoError(t, err)
			assert.Nilf(t, snapshot.Effective.Network,
				"format_version %d must not materialize a new network axis", version)
			assert.Nilf(t, snapshot.Effective.UnixSockets,
				"format_version %d must not materialize a socket axis", version)
			actualPlan, err := sandboxpolicy.RenderMountPlan(snapshot.Effective)
			require.NoError(t, err)
			actualBytes, err := json.Marshal(actualPlan)
			require.NoError(t, err)
			assert.Equalf(t, string(expectedBytes), string(actualBytes),
				"format_version %d changed legacy %s rendering", version, access)
		}
	}
}

func TestLegacyCompatibilityDirectionsNeverInvertAccess(t *testing.T) {
	for _, tc := range []struct {
		access  sandboxpolicy.NetworkAccess
		network sandboxpolicy.AccessMode
		sockets sandboxpolicy.AccessMode
		posture sandboxpolicy.NetworkPosture
	}{
		{sandboxpolicy.NetworkAccessInternet, sandboxpolicy.AccessModeOpen,
			sandboxpolicy.AccessModeUnset, sandboxpolicy.NetworkHostOpen},
		{sandboxpolicy.NetworkAccessNone, sandboxpolicy.AccessModeClosed,
			sandboxpolicy.AccessModeClosed, sandboxpolicy.NetworkIsolatedWithAgentd},
	} {
		t.Run(string(tc.access), func(t *testing.T) {
			axes, err := sandboxpolicy.DeriveAccessAxes(sandboxpolicy.Profile{
				Name: "legacy", NetworkAccess: tc.access,
			})
			require.NoError(t, err)
			assert.Equal(t, tc.network, axes.Network.Mode)
			assert.Equal(t, tc.sockets, axes.UnixSockets.Mode)
			posture, err := sandboxpolicy.NetworkPostureForRules(axes.Network)
			require.NoError(t, err)
			assert.Equal(t, tc.posture, posture)
		})
	}
}

func TestSandboxProfileEnforcementPredictionIsOrderedAndCannotGateLaunch(t *testing.T) {
	f := newFlow(t)
	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name": "socket-wall", "filesystem": []any{}, "environment": []any{},
		"network":      map[string]any{"mode": "open"},
		"unix_sockets": map[string]any{"mode": "closed"},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	rec = profileReq(t, f, http.MethodGet,
		"/v1/sandbox-profiles/socket-wall/enforcement?"+
			"for=tclaude-layer%2Fclaude%2Flinux&"+
			"for=harness-builtin%2Fcodex%2Fdarwin", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var got struct {
		Profile string `json:"profile"`
		Targets []struct {
			Implementation string                      `json:"implementation"`
			Harness        string                      `json:"harness"`
			Platform       string                      `json:"platform"`
			Predicted      bool                        `json:"predicted"`
			Axes           harness.PredictedAccessAxes `json:"axes"`
			Caveat         string                      `json:"caveat"`
		} `json:"targets"`
	}
	testharness.DecodeJSON(t, rec, &got)
	assert.Equal(t, "socket-wall", got.Profile)
	require.Len(t, got.Targets, 2)
	assert.Equal(t, "tclaude-layer", got.Targets[0].Implementation)
	assert.Equal(t, "linux", got.Targets[0].Platform)
	assert.True(t, got.Targets[0].Predicted)
	assert.Equal(t, harness.AccessPredictionRefused,
		got.Targets[0].Axes.UnixSockets.Outcome)
	assert.Contains(t, got.Targets[0].Axes.UnixSockets.Detail,
		`unix_sockets "closed" cannot be enforced with open network access`)
	assert.Equal(t, "harness-builtin", got.Targets[1].Implementation)
	assert.Equal(t, harness.AccessPredictionEnforced,
		got.Targets[1].Axes.UnixSockets.Outcome)

	rec = profileReq(t, f, http.MethodGet,
		"/v1/sandbox-profiles/socket-wall/enforcement?for=tclaude-layer%2Fclaude%2Fwindows", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "platform: linux, darwin")

	rec = profileReq(t, f, http.MethodGet,
		"/v1/sandbox-profiles/socket-wall/enforcement?for=harness-builtin%2Fopencode%2Flinux", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "has no built-in OS sandbox",
		"OpenCode's TCL-793 refusal must happen before any prediction is returned")

	rec = profileReq(t, f, http.MethodGet,
		"/v1/sandbox-profiles/socket-wall/enforcement?for=stacked%2Fclaude%2Fdarwin", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "stacked sandbox prediction is supported only on linux")
}

func TestSandboxProfileEnforcementPredictsFilesystemEnvironmentAndAgentDirectories(t *testing.T) {
	f := newFlow(t)
	parent := t.TempDir()
	child := filepath.Join(parent, "workspace")
	require.NoError(t, os.Mkdir(child, 0o755))
	var err error
	parent, err = filepath.EvalSymlinks(parent)
	require.NoError(t, err)
	child, err = filepath.EvalSymlinks(child)
	require.NoError(t, err)
	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name": "split-policy",
		"filesystem": []any{
			map[string]any{"path": parent, "access": "deny"},
			map[string]any{"path": child, "access": "write"},
		},
		"environment":       []any{map[string]any{"name": "POLICY_OWNER", "value": "preview"}},
		"agent_directories": []any{"GOCACHE"},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	rec = profileReq(t, f, http.MethodGet,
		"/v1/sandbox-profiles/split-policy/enforcement?"+
			"for=harness-builtin%2Fcodex%2Fdarwin&"+
			"for=harness-builtin%2Fclaude%2Flinux&"+
			"for=tclaude-layer%2Fclaude%2Flinux", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var got struct {
		Targets []struct {
			Axes harness.PredictedAccessAxes `json:"axes"`
		} `json:"targets"`
	}
	testharness.DecodeJSON(t, rec, &got)
	require.Len(t, got.Targets, 3)

	assert.Equal(t, harness.AccessPredictionRefused, got.Targets[0].Axes.Filesystem.Outcome)
	assert.Contains(t, got.Targets[0].Axes.Filesystem.Detail, "denied parent mask dominates")
	assert.Contains(t, got.Targets[0].Axes.Filesystem.Detail, child)
	assert.Equal(t, harness.AccessPredictionEnforced, got.Targets[0].Axes.Environment.Outcome)
	assert.Contains(t, got.Targets[0].Axes.Environment.Detail, "shell_environment_policy")
	assert.Equal(t, harness.AccessPredictionRefused, got.Targets[0].Axes.AgentDirectories.Outcome,
		"a launch-wide filesystem refusal also refuses its generated writable directories")

	assert.Equal(t, harness.AccessPredictionEnforcedPartial, got.Targets[1].Axes.Filesystem.Outcome)
	assert.Contains(t, got.Targets[1].Axes.Filesystem.Detail, "Read/Write/Edit")
	assert.Equal(t, harness.AccessPredictionEnforced, got.Targets[1].Axes.AgentDirectories.Outcome)

	assert.Equal(t, harness.AccessPredictionEnforced, got.Targets[2].Axes.Filesystem.Outcome)
	assert.Contains(t, got.Targets[2].Axes.Filesystem.Detail, "process scope")
	assert.Contains(t, got.Targets[2].Axes.Filesystem.Detail, "narrower read/write carve-out")
}

func TestSandboxProfileEnforcementIncludesGeneratedAgentDirectoryCarveOut(t *testing.T) {
	f := newFlow(t)
	realCacheHome := t.TempDir()
	cacheLink := filepath.Join(t.TempDir(), "cache-link")
	require.NoError(t, os.Symlink(realCacheHome, cacheLink))
	t.Setenv("XDG_CACHE_HOME", cacheLink)
	cache := tclcommon.CacheDir()
	require.NoError(t, os.MkdirAll(cache, 0o755))
	var err error
	cache, err = filepath.EvalSymlinks(cache)
	require.NoError(t, err)
	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name":              "private-cache-under-deny",
		"filesystem":        []any{map[string]any{"path": cache, "access": "deny"}},
		"environment":       []any{},
		"agent_directories": []any{"GOCACHE"},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	rec = profileReq(t, f, http.MethodGet,
		"/v1/sandbox-profiles/private-cache-under-deny/enforcement?"+
			"for=harness-builtin%2Fcodex%2Fdarwin", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var got struct {
		Targets []struct {
			Axes harness.PredictedAccessAxes `json:"axes"`
		} `json:"targets"`
	}
	testharness.DecodeJSON(t, rec, &got)
	require.Len(t, got.Targets, 1)
	assert.Equal(t, harness.AccessPredictionRefused, got.Targets[0].Axes.Filesystem.Outcome)
	assert.Contains(t, got.Targets[0].Axes.Filesystem.Detail,
		filepath.Join(cache, "agent-dirs", "predicted-agent", "GOCACHE"),
		"the evaluator models the launch-generated writable path, not only authored rows")
	assert.Equal(t, harness.AccessPredictionRefused, got.Targets[0].Axes.AgentDirectories.Outcome)
}

func TestSandboxProfileDraftEnforcementDetectsCrossScopeFilesystemCarveOut(t *testing.T) {
	f := newFlow(t)
	_, err := db.CreateAgentGroup("crew", "")
	require.NoError(t, err)
	parent := t.TempDir()
	child := filepath.Join(parent, "workspace")
	require.NoError(t, os.Mkdir(child, 0o755))
	parent, err = filepath.EvalSymlinks(parent)
	require.NoError(t, err)
	child, err = filepath.EvalSymlinks(child)
	require.NoError(t, err)
	for _, body := range []map[string]any{
		{
			"name":        "deny-parent",
			"filesystem":  []any{map[string]any{"path": parent, "access": "deny"}},
			"environment": []any{},
		},
		{
			"name":              "group-reopen",
			"filesystem":        []any{map[string]any{"path": child, "access": "write"}},
			"environment":       []any{map[string]any{"name": "POLICY_OWNER", "value": "crew"}},
			"agent_directories": []any{"GOCACHE"},
		},
	} {
		rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", body)
		require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	}
	require.NoError(t, db.SetGlobalSandboxProfile("deny-parent"))
	_, err = db.SetAgentGroupSandboxProfile("crew", "group-reopen")
	require.NoError(t, err)
	groupProfile, err := db.GetSandboxProfile("group-reopen")
	require.NoError(t, err)
	require.NotNil(t, groupProfile)

	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profile-enforcement", map[string]any{
		"draft": map[string]any{
			"id":                groupProfile.ID,
			"name":              groupProfile.Name,
			"filesystem":        []any{map[string]any{"path": child, "access": "write"}},
			"environment":       []any{map[string]any{"name": "POLICY_OWNER", "value": "crew"}},
			"agent_directories": []any{"GOCACHE"},
		},
		"targets": []any{map[string]any{
			"implementation": "harness-builtin", "harness": "codex", "platform": "darwin",
		}},
	})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var got struct {
		Targets []struct {
			Axes harness.PredictedAccessAxes `json:"axes"`
		} `json:"targets"`
		Contexts []struct {
			Filesystem       []sandboxpolicy.FilesystemGrant `json:"filesystem"`
			Environment      []string                        `json:"environment"`
			AgentDirectories []string                        `json:"agent_directories"`
		} `json:"contexts"`
	}
	testharness.DecodeJSON(t, rec, &got)
	require.Len(t, got.Targets, 1)
	assert.Equal(t, harness.AccessPredictionRefused, got.Targets[0].Axes.Filesystem.Outcome,
		"the evaluator must see the denied parent and narrower grant even though they come from different scopes")
	assert.Contains(t, got.Targets[0].Axes.Filesystem.Detail, child)
	require.Len(t, got.Contexts, 1)
	assert.ElementsMatch(t, []sandboxpolicy.FilesystemGrant{
		{Path: parent, Access: sandboxpolicy.AccessDeny},
		{Path: child, Access: sandboxpolicy.AccessWrite},
	}, got.Contexts[0].Filesystem)
	assert.Equal(t, []string{"POLICY_OWNER"}, got.Contexts[0].Environment)
	assert.Equal(t, []string{"GOCACHE"}, got.Contexts[0].AgentDirectories)

	rec = profileReq(t, f, http.MethodPost, "/v1/sandbox-profile-enforcement", map[string]any{
		"draft": map[string]any{
			"id":                groupProfile.ID,
			"name":              groupProfile.Name,
			"filesystem":        []any{map[string]any{"path": child, "access": "write"}},
			"environment":       []any{map[string]any{"name": "POLICY_OWNER", "value": "crew"}},
			"agent_directories": []any{"GOCACHE"},
		},
		"targets": []any{map[string]any{
			"implementation": "tclaude-layer", "harness": "opencode", "platform": "linux",
		}},
	})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var openCode struct {
		Targets []struct {
			Target struct {
				Implementation string `json:"implementation"`
				Harness        string `json:"harness"`
				Sandbox        string `json:"sandbox"`
			} `json:"target"`
			Axes harness.PredictedAccessAxes `json:"axes"`
		} `json:"targets"`
	}
	testharness.DecodeJSON(t, rec, &openCode)
	require.Len(t, openCode.Targets, 1)
	assert.Equal(t, "tclaude-layer", openCode.Targets[0].Target.Implementation)
	assert.Equal(t, harness.OpenCodeName, openCode.Targets[0].Target.Harness)
	assert.Equal(t, harness.OpenCodeSandboxTclaudeLayer, openCode.Targets[0].Target.Sandbox,
		"OpenCode access-control is not a sandbox and must never label this prediction")
	assert.Equal(t, harness.AccessPredictionEnforced, openCode.Targets[0].Axes.Filesystem.Outcome,
		"the tclaude layer preserves the narrower write carve-out under the denied parent")

	globalProfile, err := db.GetSandboxProfile("deny-parent")
	require.NoError(t, err)
	require.NotNil(t, globalProfile)
	rec = profileReq(t, f, http.MethodPost, "/v1/sandbox-profile-enforcement", map[string]any{
		"draft": map[string]any{
			"id":           globalProfile.ID,
			"name":         globalProfile.Name,
			"filesystem":   []any{map[string]any{"path": parent, "access": "deny"}},
			"environment":  []any{},
			"network":      map[string]any{},
			"unix_sockets": map[string]any{},
		},
		"targets": []any{map[string]any{
			"implementation": "harness-builtin", "harness": "codex", "platform": "darwin",
		}},
	})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	testharness.DecodeJSON(t, rec, &got)
	require.Len(t, got.Targets, 1)
	assert.Equal(t, harness.AccessPredictionRefused, got.Targets[0].Axes.Filesystem.Outcome,
		"editing the global half must still evaluate each group's narrower directory rules")
	require.Len(t, got.Contexts, 1)
	assert.ElementsMatch(t, []sandboxpolicy.FilesystemGrant{
		{Path: parent, Access: sandboxpolicy.AccessDeny},
		{Path: child, Access: sandboxpolicy.AccessWrite},
	}, got.Contexts[0].Filesystem)
}

func TestSandboxProfileDraftEnforcementUsesResolvedDefaultSandboxMode(t *testing.T) {
	f := newFlow(t)
	_, err := db.CreateAgentGroup("crew", "")
	require.NoError(t, err)
	globalID, err := db.CreateSpawnProfile(&db.SpawnProfile{
		Name: "claude-off", Harness: harness.DefaultName,
		Sandbox: harness.ClaudeSandboxOff,
	})
	require.NoError(t, err)
	require.NoError(t, db.SetDashboardPref(globalDefaultProfilePrefKey, "claude-off"))
	require.NoError(t, db.SetDashboardPref(
		"tclaude.dash.default_profile_id", fmt.Sprint(globalID),
	))
	parent := t.TempDir()
	body := map[string]any{
		"draft": map[string]any{
			"name":        "resolved-mode",
			"filesystem":  []any{map[string]any{"path": parent, "access": "deny"}},
			"environment": []any{},
		},
	}
	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profile-enforcement", body)
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var got struct {
		Targets []struct {
			Target struct {
				Harness string `json:"harness"`
				Sandbox string `json:"sandbox"`
			} `json:"target"`
			ResolvedBy string                      `json:"resolved_by"`
			Axes       harness.PredictedAccessAxes `json:"axes"`
		} `json:"targets"`
	}
	testharness.DecodeJSON(t, rec, &got)
	require.Len(t, got.Targets, 1)
	assert.Equal(t, harness.DefaultName, got.Targets[0].Target.Harness)
	assert.Equal(t, harness.ClaudeSandboxOff, got.Targets[0].Target.Sandbox)
	assert.Contains(t, got.Targets[0].ResolvedBy, "claude-off")
	assert.Equal(t, harness.AccessPredictionRefused, got.Targets[0].Axes.Filesystem.Outcome)

	globalID, err = db.CreateSpawnProfile(&db.SpawnProfile{
		Name: "codex-workspace", Harness: harness.CodexName,
		Sandbox: harness.SandboxWorkspaceWrite,
	})
	require.NoError(t, err)
	require.NoError(t, db.SetDashboardPref(globalDefaultProfilePrefKey, "codex-workspace"))
	require.NoError(t, db.SetDashboardPref(
		"tclaude.dash.default_profile_id", fmt.Sprint(globalID),
	))
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{
		Name: "codex-group-harness", Harness: harness.CodexName,
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, setGroupProfile(t, f, "crew", "codex-group-harness").Code)
	body["context"] = map[string]any{"group": "crew"}
	rec = profileReq(t, f, http.MethodPost, "/v1/sandbox-profile-enforcement", body)
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	testharness.DecodeJSON(t, rec, &got)
	require.Len(t, got.Targets, 1)
	assert.Equal(t, harness.CodexName, got.Targets[0].Target.Harness)
	assert.Equal(t, harness.SandboxWorkspaceWrite, got.Targets[0].Target.Sandbox)
	assert.Contains(t, got.Targets[0].ResolvedBy, "codex-group-harness")
	assert.Contains(t, got.Targets[0].ResolvedBy, "codex-workspace")
	assert.Equal(t, harness.AccessPredictionRefused, got.Targets[0].Axes.Filesystem.Outcome)

	_, err = db.CreateSpawnProfile(&db.SpawnProfile{
		Name: "opencode-layered", Harness: harness.OpenCodeName,
		Sandbox:               harness.OpenCodeSandboxAccessControl,
		SandboxImplementation: "tclaude-layer",
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, setGroupProfile(t, f, "crew", "opencode-layered").Code)
	rec = profileReq(t, f, http.MethodPost, "/v1/sandbox-profile-enforcement", body)
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	testharness.DecodeJSON(t, rec, &got)
	require.Len(t, got.Targets, 1)
	assert.Equal(t, harness.OpenCodeName, got.Targets[0].Target.Harness)
	assert.Equal(t, harness.OpenCodeSandboxTclaudeLayer, got.Targets[0].Target.Sandbox,
		"the resolved target must name the real OpenCode OS boundary, not its soft command filter")
	assert.Contains(t, got.Targets[0].ResolvedBy, "opencode-layered")
	assert.Equal(t, harness.AccessPredictionEnforced, got.Targets[0].Axes.Filesystem.Outcome)
}

func TestSandboxProfileDraftEnforcementPredictsAllGlobalAssignmentContexts(t *testing.T) {
	f := newFlow(t)
	for i := range 11 {
		_, err := db.CreateAgentGroup(fmt.Sprintf("crew-%02d", i), "")
		require.NoError(t, err)
	}
	parent := t.TempDir()
	child := filepath.Join(parent, "workspace")
	require.NoError(t, os.Mkdir(child, 0o755))
	var err error
	parent, err = filepath.EvalSymlinks(parent)
	require.NoError(t, err)
	child, err = filepath.EvalSymlinks(child)
	require.NoError(t, err)
	for _, body := range []map[string]any{
		{
			"name":        "global-deny",
			"filesystem":  []any{map[string]any{"path": parent, "access": "deny"}},
			"environment": []any{},
		},
		{
			"name":        "last-group-reopen",
			"filesystem":  []any{map[string]any{"path": child, "access": "write"}},
			"environment": []any{},
		},
	} {
		rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", body)
		require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	}
	require.NoError(t, db.SetGlobalSandboxProfile("global-deny"))
	_, err = db.SetAgentGroupSandboxProfile("crew-10", "last-group-reopen")
	require.NoError(t, err)
	globalProfile, err := db.GetSandboxProfile("global-deny")
	require.NoError(t, err)
	require.NotNil(t, globalProfile)

	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profile-enforcement", map[string]any{
		"draft": map[string]any{
			"id":           globalProfile.ID,
			"name":         globalProfile.Name,
			"filesystem":   []any{map[string]any{"path": parent, "access": "deny"}},
			"environment":  []any{},
			"network":      map[string]any{},
			"unix_sockets": map[string]any{},
		},
		"targets": []any{map[string]any{
			"implementation": "harness-builtin", "harness": "codex", "platform": "darwin",
		}},
	})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var got struct {
		Targets []struct {
			Axes        harness.PredictedAccessAxes   `json:"axes"`
			ContextAxes []harness.PredictedAccessAxes `json:"context_axes"`
		} `json:"targets"`
		Contexts []struct {
			Context map[string]string `json:"context"`
		} `json:"contexts"`
		RemainingContexts int `json:"remaining_contexts"`
	}
	testharness.DecodeJSON(t, rec, &got)
	require.Len(t, got.Targets, 1)
	assert.Equal(t, harness.AccessPredictionRefused, got.Targets[0].Axes.Filesystem.Outcome,
		"the omitted eleventh display context must still participate in aggregate prediction")
	assert.Contains(t, got.Targets[0].Axes.Filesystem.Detail, child)
	assert.Len(t, got.Contexts, 10)
	require.Len(t, got.Targets[0].ContextAxes, 10)
	for _, axes := range got.Targets[0].ContextAxes {
		assert.Equal(t, harness.AccessPredictionEnforced, axes.Filesystem.Outcome,
			"each displayed assignment should carry its own non-carve-out verdict")
	}
	assert.Equal(t, 1, got.RemainingContexts)
	for _, context := range got.Contexts {
		assert.NotEqual(t, "crew-10", context.Context["group_name"])
	}
}

func TestSandboxProfileDraftEnforcementSeparatesPredictionFromCompositionContexts(t *testing.T) {
	f := newFlow(t)
	_, err := db.CreateAgentGroup("crew", "")
	require.NoError(t, err)
	for _, body := range []map[string]any{
		{
			"name": "global-net", "filesystem": []any{}, "environment": []any{},
			"network": map[string]any{"mode": "list", "allow": []any{
				map[string]any{"domain": "github.com"},
			}},
		},
		{
			"name": "group-net", "filesystem": []any{}, "environment": []any{},
			"network": map[string]any{"mode": "list", "allow": []any{
				map[string]any{"domain": "api.anthropic.com"},
			}},
		},
	} {
		rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", body)
		require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	}
	require.NoError(t, db.SetGlobalSandboxProfile("global-net"))
	_, err = db.SetAgentGroupSandboxProfile("crew", "group-net")
	require.NoError(t, err)
	groupProfile, err := db.GetSandboxProfile("group-net")
	require.NoError(t, err)
	require.NotNil(t, groupProfile)

	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profile-enforcement", map[string]any{
		"draft": map[string]any{
			"id": groupProfile.ID, "name": "renamed-in-editor",
			"filesystem": []any{}, "environment": []any{},
			"network": map[string]any{"mode": "list", "allow": []any{
				map[string]any{"domain": "api.anthropic.com"},
			}},
		},
		"targets": []any{map[string]any{
			"implementation": "tclaude-layer", "harness": "claude", "platform": "linux",
		}},
	})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var got struct {
		Targets []struct {
			Predicted   bool                          `json:"predicted"`
			Axes        harness.PredictedAccessAxes   `json:"axes"`
			ContextAxes []harness.PredictedAccessAxes `json:"context_axes"`
		} `json:"targets"`
		Contexts []struct {
			Context map[string]string            `json:"context"`
			Network sandboxpolicy.NetworkRules   `json:"network"`
			Notices []sandboxpolicy.AccessNotice `json:"notices"`
		} `json:"contexts"`
	}
	testharness.DecodeJSON(t, rec, &got)
	require.Len(t, got.Targets, 1)
	assert.True(t, got.Targets[0].Predicted)
	assert.Equal(t, harness.AccessPredictionEnforced, got.Targets[0].Axes.Network.Outcome)
	assert.Contains(t, got.Targets[0].Axes.Network.Detail, "Prerequisite-conditional")
	require.Len(t, got.Targets[0].ContextAxes, 1)
	assert.Equal(t, harness.AccessPredictionEnforced,
		got.Targets[0].ContextAxes[0].Network.Outcome)
	assert.Contains(t, got.Targets[0].ContextAxes[0].Network.Detail,
		"Prerequisite-conditional")
	require.Len(t, got.Contexts, 1)
	assert.Equal(t, "crew", got.Contexts[0].Context["group_name"])
	assert.Equal(t, "renamed-in-editor", got.Contexts[0].Context["group"],
		"assignment roles follow the stable profile ID through an in-progress rename")
	assert.Equal(t, sandboxpolicy.AccessModeList, got.Contexts[0].Network.Mode)
	assert.Empty(t, got.Contexts[0].Network.Allow)
	require.Len(t, got.Contexts[0].Notices, 1)
	assert.Equal(t, sandboxpolicy.AccessNoticeClassComposition, got.Contexts[0].Notices[0].Class)
	assert.Equal(t, sandboxpolicy.AccessNoticeReasonEmptyIntersection, got.Contexts[0].Notices[0].Reason)
	assert.ElementsMatch(t, []string{`global "global-net"`, `group "renamed-in-editor"`},
		got.Contexts[0].Notices[0].Tiers)

	rec = profileReq(t, f, http.MethodPut, "/v1/groups/crew/sandbox-profile",
		map[string]any{"name": "group-net"})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var assignment struct {
		Notices []sandboxpolicy.AccessNotice `json:"notices"`
	}
	testharness.DecodeJSON(t, rec, &assignment)
	require.Len(t, assignment.Notices, 1)
	assert.Contains(t, assignment.Notices[0].Detail, `group "crew"`)

	_, err = db.CreateAgentGroup("second", "")
	require.NoError(t, err)
	_, err = db.SetAgentGroupSandboxProfile("second", "group-net")
	require.NoError(t, err)
	rec = profileReq(t, f, http.MethodPut, "/v1/sandbox-profile-default",
		map[string]any{"name": "global-net"})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	testharness.DecodeJSON(t, rec, &assignment)
	require.Len(t, assignment.Notices, 2,
		"global assignment checks every group's assigned profile")
	assert.Contains(t, assignment.Notices[0].Detail, "group")

	rec = profileReq(t, f, http.MethodPost, "/v1/sandbox-profile-enforcement", map[string]any{
		"draft": map[string]any{
			"name": "default-target-draft", "filesystem": []any{}, "environment": []any{},
			"network": map[string]any{"mode": "closed"},
		},
	})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var defaulted struct {
		Targets []struct {
			Target struct {
				Implementation string `json:"implementation"`
				Harness        string `json:"harness"`
				Platform       string `json:"platform"`
			} `json:"target"`
			ResolvedBy string `json:"resolved_by"`
			Predicted  bool   `json:"predicted"`
		} `json:"targets"`
	}
	testharness.DecodeJSON(t, rec, &defaulted)
	require.Len(t, defaulted.Targets, 1)
	assert.Equal(t, "harness-builtin", defaulted.Targets[0].Target.Implementation)
	assert.Equal(t, "claude", defaulted.Targets[0].Target.Harness)
	assert.Equal(t, runtime.GOOS, defaulted.Targets[0].Target.Platform)
	assert.Equal(t, "harness default", defaulted.Targets[0].ResolvedBy)
	assert.True(t, defaulted.Targets[0].Predicted)
}

func TestSandboxProfileDraftEnforcementDistinguishesDarwinLocalAndMixedLists(t *testing.T) {
	f := newFlow(t)
	for _, tc := range []struct {
		name     string
		allow    []any
		platform string
		outcome  string
		detail   string
	}{
		{
			name: "darwin-local",
			allow: []any{
				map[string]any{"loopback": true},
			},
			platform: "darwin",
			outcome:  harness.AccessPredictionEnforced,
			detail:   "host-loopback",
		},
		{
			name: "darwin-local-model-apis",
			allow: []any{
				map[string]any{"loopback": true},
				map[string]any{"domain": "api.anthropic.com", "ports": []int{443}},
				map[string]any{"domain": "api.openai.com", "ports": []int{443}},
			},
			platform: "darwin",
			outcome:  harness.AccessPredictionNotEnforced,
			detail:   "all outbound connections are permitted",
		},
		{
			name: "linux-local-model-apis",
			allow: []any{
				map[string]any{"loopback": true},
				map[string]any{"domain": "api.anthropic.com", "ports": []int{443}},
				map[string]any{"domain": "api.openai.com", "ports": []int{443}},
			},
			platform: "linux",
			outcome:  harness.AccessPredictionEnforcedPartial,
			detail:   "Prerequisite-conditional",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profile-enforcement", map[string]any{
				"draft": map[string]any{
					"name": tc.name, "filesystem": []any{}, "environment": []any{},
					"network": map[string]any{
						"mode": "list", "allow": tc.allow,
					},
				},
				"targets": []any{map[string]any{
					"implementation": "tclaude-layer",
					"harness":        "claude",
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
			assert.Equal(t, tc.outcome, got.Targets[0].Axes.Network.Outcome)
			assert.Contains(t, got.Targets[0].Axes.Network.Detail, tc.detail)
		})
	}
}

func TestSandboxProfileDraftEnforcementProjectsMaterializedPackRows(t *testing.T) {
	f := newFlow(t)
	request := func(t *testing.T, harnessName string) []harness.PredictedNetworkEntry {
		t.Helper()
		rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profile-enforcement", map[string]any{
			"draft": map[string]any{
				"name": "pack-preview", "filesystem": []any{}, "environment": []any{},
				"network": map[string]any{
					"baseline": "deny",
					"packs":    []string{"net-local", "net-anthropic", "net-openai-codex"},
				},
			},
			"targets": []any{map[string]any{
				"implementation": "tclaude-layer",
				"harness":        harnessName,
				"platform":       "linux",
			}},
		})
		require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		var got struct {
			Targets []struct {
				NetworkEntries []harness.PredictedNetworkEntry `json:"network_entries"`
			} `json:"targets"`
		}
		testharness.DecodeJSON(t, rec, &got)
		require.Len(t, got.Targets, 1)
		return got.Targets[0].NetworkEntries
	}

	claudeRows := request(t, "claude")
	require.Len(t, claudeRows, 3)
	for _, row := range claudeRows {
		assert.Equal(t, harness.AccessPredictionEnforcedPartial, row.Outcome)
		assert.Contains(t, row.Detail, "Prerequisite-conditional prediction")
	}

	openCodeRows := request(t, "opencode")
	require.Len(t, openCodeRows, 3)
	for _, row := range openCodeRows {
		assert.Equal(t, harness.AccessPredictionRefused, row.Outcome,
			"the exact built-in local/model combination refuses as a whole")
		assert.Contains(t, row.Detail, "TCL-826")
	}

	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profile-enforcement", map[string]any{
		"draft": map[string]any{
			"name": "authored-aliases", "filesystem": []any{}, "environment": []any{},
			"network": map[string]any{
				"baseline": "deny",
				"allow": []any{
					map[string]any{"domain": "API.EXAMPLE.COM", "ports": []int{443, 443}},
					map[string]any{"cidr": "192.0.2.9/24"},
				},
			},
		},
		"targets": []any{map[string]any{
			"implementation": "tclaude-layer", "harness": "claude", "platform": "linux",
		}},
	})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var aliases struct {
		Targets []struct {
			NetworkEntries []harness.PredictedNetworkEntry `json:"network_entries"`
		} `json:"targets"`
	}
	testharness.DecodeJSON(t, rec, &aliases)
	require.Len(t, aliases.Targets, 1)
	require.Len(t, aliases.Targets[0].NetworkEntries, 2)
	allKeys := []string{}
	for _, row := range aliases.Targets[0].NetworkEntries {
		allKeys = append(allKeys, row.Keys...)
	}
	assert.Contains(t, allKeys, `{"domain":"API.EXAMPLE.COM","ports":[443]}`)
	assert.Contains(t, allKeys, `{"cidr":"192.0.2.9/24"}`)
}

func TestGlobalSandboxAssignmentReportsIntrinsicCompositionOnce(t *testing.T) {
	f := newFlow(t)
	for _, body := range []map[string]any{
		{
			"name": "left", "filesystem": []any{}, "environment": []any{},
			"network": map[string]any{"mode": "list", "allow": []any{
				map[string]any{"domain": "left.example"},
			}},
		},
		{
			"name": "right", "filesystem": []any{}, "environment": []any{},
			"network": map[string]any{"mode": "list", "allow": []any{
				map[string]any{"domain": "right.example"},
			}},
		},
		{
			"name": "self-empty", "filesystem": []any{}, "environment": []any{},
			"includes": []any{"left", "right"},
		},
		{
			"name": "group-open", "filesystem": []any{}, "environment": []any{},
			"network": map[string]any{"mode": "open"},
		},
	} {
		rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", body)
		require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	}

	assign := func() []sandboxpolicy.AccessNotice {
		rec := profileReq(t, f, http.MethodPut, "/v1/sandbox-profile-default",
			map[string]any{"name": "self-empty"})
		require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		var got struct {
			Notices []sandboxpolicy.AccessNotice `json:"notices"`
		}
		testharness.DecodeJSON(t, rec, &got)
		return got.Notices
	}

	notices := assign()
	require.Len(t, notices, 1, "the profile's intrinsic empty intersection is reported without a group")
	assert.Equal(t, "network", notices[0].Axis)
	assert.NotContains(t, notices[0].Detail, "group ")

	_, err := db.CreateAgentGroup("crew", "")
	require.NoError(t, err)
	_, err = db.SetAgentGroupSandboxProfile("crew", "group-open")
	require.NoError(t, err)
	notices = assign()
	require.Len(t, notices, 1,
		"an already-empty global axis is not repeated as a misleading group-context warning")
	assert.NotContains(t, notices[0].Detail, "group ")
}

func TestSandboxProfileImportCannotExpressAwayAgentdSocketFloor(t *testing.T) {
	f := newFlow(t)
	modes := []sandboxpolicy.AccessMode{
		sandboxpolicy.AccessModeOpen,
		sandboxpolicy.AccessModeClosed,
		sandboxpolicy.AccessModeList,
	}
	floor := []string{
		agentipc.CanonicalSocketPath(),
		agentipc.LegacyHomeSocketPath(),
		agentipc.LegacySocketPath(),
	}
	for _, networkMode := range modes {
		for _, socketMode := range modes {
			name := fmt.Sprintf("floor-%s-%s", networkMode, socketMode)
			rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles/import", map[string]any{
				"format": "tclaude-sandbox-profiles", "format_version": 7,
				"profiles": []map[string]any{{
					"name":    name,
					"network": map[string]any{"mode": networkMode},
					"unix_sockets": map[string]any{
						"mode": socketMode, "allow": []any{},
						"agentd_socket_floor": []any{},
					},
				}},
			})
			require.Equalf(t, http.StatusOK, rec.Code,
				"%s/%s body=%s", networkMode, socketMode, rec.Body.String())
			stored, err := db.GetSandboxProfile(name)
			require.NoError(t, err)
			require.NotNil(t, stored)
			axes, err := sandboxpolicy.DeriveAccessAxes(sandboxpolicy.Profile{
				Name: stored.Name, NetworkAccess: stored.NetworkAccess,
				Network: stored.Network, UnixSockets: stored.UnixSockets,
			})
			require.NoError(t, err)
			access := sandboxpolicy.ResolveUnixSocketAccess(axes.UnixSockets)
			for _, path := range floor {
				assert.Containsf(t, access.AllowedPaths, path,
					"%s/%s import removed agentd floor %q", networkMode, socketMode, path)
			}
		}
	}
}

func TestEmptyIntersectionIsAWarningOnSaveAndImportNeverARefusal(t *testing.T) {
	f := newFlow(t)
	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name": "github", "network": map[string]any{
			"mode": "list", "allow": []map[string]any{{"host": "github.com"}},
		},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	rec = profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name": "npm", "includes": []string{"github"},
		"network": map[string]any{
			"mode": "list", "allow": []map[string]any{{"host": "registry.npmjs.org"}},
		},
	})
	require.Equalf(t, http.StatusCreated, rec.Code,
		"an empty effective allow set warns but never blocks save: %s", rec.Body.String())
	var saved struct {
		Notices []sandboxpolicy.AccessNotice `json:"notices"`
	}
	testharness.DecodeJSON(t, rec, &saved)
	require.Len(t, saved.Notices, 1)
	assert.Equal(t, sandboxpolicy.AccessNoticeClassComposition, saved.Notices[0].Class)
	assert.Equal(t, []string{"github", "npm"}, saved.Notices[0].Tiers)

	rec = profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles/import", map[string]any{
		"format": "tclaude-sandbox-profiles", "format_version": 7,
		"on_conflict": "overwrite",
		"profiles": []map[string]any{
			{
				"name": "github", "network": map[string]any{
					"mode": "list", "allow": []map[string]any{{"host": "github.com"}},
				},
			},
			{
				"name": "npm", "includes": []string{"github"},
				"network": map[string]any{
					"mode": "list", "allow": []map[string]any{{"host": "registry.npmjs.org"}},
				},
			},
		},
	})
	require.Equalf(t, http.StatusOK, rec.Code,
		"an empty effective allow set warns but never blocks import: %s", rec.Body.String())
	var imported struct {
		Warnings []string `json:"warnings"`
	}
	testharness.DecodeJSON(t, rec, &imported)
	require.NotEmpty(t, imported.Warnings)
	assert.Contains(t, imported.Warnings[len(imported.Warnings)-1], "github ∩ npm")
}

func TestSpawnAccessPlannerWarnsAndRefusesThroughExistingChannels(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	t.Cleanup(agentd.SetTclaudeLayerHostAvailabilityForTest(func() error { return nil }))
	t.Cleanup(agentd.SetTclaudeLayerAccessVerdictForTest(func(
		_ string, posture sandboxpolicy.NetworkPosture,
	) (harness.LaunchOSSandbox, error) {
		return harness.LaunchOSSandbox{
			State: "on", Source: fmt.Sprintf("test live tclaude-layer verdict for %v", posture),
		}, nil
	}))

	create := func(name string, network, sockets map[string]any) {
		body := map[string]any{"name": name, "network": network}
		if sockets != nil {
			body["unix_sockets"] = sockets
		}
		rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", body)
		require.Equalf(t, http.StatusCreated, rec.Code, "%s body=%s", name, rec.Body.String())
	}
	create("socket-list",
		map[string]any{"mode": "open"},
		map[string]any{"mode": "list", "allow": []map[string]any{{"path": "/tmp/service.sock"}}})
	create("socket-closed",
		map[string]any{"mode": "open"},
		map[string]any{"mode": "closed"})
	create("ambient-sockets",
		map[string]any{"mode": "closed"},
		map[string]any{"mode": "open"})
	create("network-only",
		map[string]any{"mode": "closed"},
		nil)

	warned := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "warned", "approval": "bypassPermissions",
		"sandbox_profile":        "socket-list",
		"sandbox_implementation": "tclaude-layer",
	})
	require.Equalf(t, http.StatusOK, warned.Code, "body=%s", warned.Raw)
	var spawn agent.SpawnResponse
	require.NoError(t, json.Unmarshal(warned.Raw, &spawn))
	require.NotNil(t, spawn.Resolved)
	require.NotEmpty(t, spawn.Resolved.Warnings)
	warningLiteral := "host-open network on Linux tclaude-layer"
	closedLiteral := `unix_sockets \"closed\" cannot be enforced with open network access on Linux tclaude-layer`
	ambientLiteral := `unix_sockets \"open\" cannot preserve ambient host socket visibility with closed network access`
	if runtime.GOOS == "darwin" {
		warningLiteral = "tclaude-layer Seatbelt (process scope)"
		closedLiteral = `unix_sockets \"closed\" is not yet enforceable with open network access on macOS tclaude-layer`
		ambientLiteral = `ambient unix-socket access is not yet enforceable under closed network access on macOS tclaude-layer`
	}
	assert.Contains(t, spawn.Resolved.Warnings[len(spawn.Resolved.Warnings)-1],
		warningLiteral)
	persisted, err := db.AgentEffectiveSandboxConfigForConv(spawn.ConvID)
	require.NoError(t, err)
	require.NotNil(t, persisted)
	require.NotEmpty(t, persisted.Effective.AccessNotices)
	assert.Equal(t, sandboxpolicy.AccessNoticeClassDegradation,
		persisted.Effective.AccessNotices[len(persisted.Effective.AccessNotices)-1].Class)

	for _, tc := range []struct {
		profile string
		want    string
	}{
		{"socket-closed", closedLiteral},
		{"ambient-sockets", ambientLiteral},
	} {
		refused := f.AsHuman().SpawnWith("crew", map[string]any{
			"name": "refused-" + tc.profile, "approval": "bypassPermissions",
			"sandbox_profile":        tc.profile,
			"sandbox_implementation": "tclaude-layer",
		})
		require.Equalf(t, http.StatusUnprocessableEntity, refused.Code,
			"%s body=%s", tc.profile, refused.Raw)
		assert.Contains(t, string(refused.Raw), tc.want)
	}

	legacyBehavior := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "network-only", "approval": "bypassPermissions",
		"sandbox_profile":        "network-only",
		"sandbox_implementation": "tclaude-layer",
	})
	require.Equalf(t, http.StatusOK, legacyBehavior.Code,
		"unset sockets plus closed network preserves today's isolated launch: %s",
		legacyBehavior.Raw)
}
