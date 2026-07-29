package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type wireSandboxProfile struct {
	Name             string   `json:"name"`
	AgentDirectories []string `json:"agent_directories"`
	NetworkAccess    string   `json:"network_access"`
	Filesystem       []struct {
		Path   string `json:"path"`
		Access string `json:"access"`
	} `json:"filesystem"`
	FilesystemSpellings *struct {
		Version int `json:"version"`
		Rules   []struct {
			ResolvedPath string   `json:"resolved_path"`
			Spellings    []string `json:"spellings"`
		} `json:"rules"`
	} `json:"filesystem_spellings"`
	Environment []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"environment"`
}

func TestSandboxProfilesPayloadReadsAndMutationsRequireDedicatedPermission(t *testing.T) {
	f := newFlow(t)
	const peer = "sandbox-profile-gate-aaaa-bbbb"
	f.HaveConvWithTitle(peer, "peer")
	_, err := db.CreateAgentGroup("exists", "")
	require.NoError(t, err)
	for _, req := range []*http.Request{
		testharness.JSONRequest(t, http.MethodGet, "/v1/sandbox-profile-read-exclusions", nil),
		testharness.JSONRequest(t, http.MethodPost, "/v1/sandbox-profile-enforcement", map[string]any{"draft": map[string]any{"name": "x"}}),
		testharness.JSONRequest(t, http.MethodGet, "/v1/sandbox-profiles", nil),
		testharness.JSONRequest(t, http.MethodGet, "/v1/sandbox-profiles/anything", nil),
		testharness.JSONRequest(t, http.MethodGet, "/v1/sandbox-profiles/export", nil),
		testharness.JSONRequest(t, http.MethodPost, "/v1/sandbox-profiles/import/inspect", nil),
		testharness.JSONRequest(t, http.MethodPut, "/v1/groups/exists/sandbox-profile", map[string]any{"name": "x"}),
		testharness.JSONRequest(t, http.MethodPut, "/v1/groups/missing/sandbox-profile", map[string]any{"name": "x"}),
	} {
		rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(req, peer))
		assert.Equalf(t, http.StatusForbidden, rec.Code, "%s %s body=%s", req.Method, req.URL.Path, rec.Body.String())
	}
}

func TestSandboxProfileReadExclusionCatalog(t *testing.T) {
	f := newFlow(t)
	realHome := t.TempDir()
	linkedHome := filepath.Join(t.TempDir(), "home")
	require.NoError(t, os.Symlink(realHome, linkedHome))
	t.Setenv("HOME", linkedHome)
	canonicalHome, err := filepath.EvalSymlinks(linkedHome)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(linkedHome, ".claude"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(linkedHome, ".tclaude", "data"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(linkedHome, ".claude", "settings.json"), []byte(`{"sandbox":{"enabled":true,"filesystem":{"denyRead":["~/.tclaude/data"],"denyWrite":["~/.tclaude/data"]},"network":{"allowedDomains":["api.example.com"],"allowUnixSockets":["/tmp/example.sock"]}}}`), 0o600))
	rec := profileReq(t, f, http.MethodGet, "/v1/sandbox-profile-read-exclusions", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var catalog struct {
		Version       int `json:"version"`
		Platform      string
		Home          string
		Categories    []map[string]any `json:"categories"`
		Informational []map[string]any `json:"informational"`
		Global        []struct {
			Path      string   `json:"path"`
			Access    string   `json:"access"`
			Harnesses []string `json:"harnesses"`
		} `json:"global_filesystem"`
		GlobalWarnings   []string         `json:"global_config_warnings"`
		GlobalNetwork    []map[string]any `json:"global_network"`
		GlobalSockets    []map[string]any `json:"global_unix_sockets"`
		NetworkTemplates []struct {
			ID      string                            `json:"id"`
			Entries []sandboxpolicy.NetworkAllowEntry `json:"entries"`
			Warning string                            `json:"warning"`
		} `json:"network_templates"`
		NetworkPacks []struct {
			ID      string                            `json:"id"`
			Entries []sandboxpolicy.NetworkAllowEntry `json:"entries"`
			Warning string                            `json:"warning"`
		} `json:"network_packs"`
		SocketTemplates []struct {
			ID string `json:"id"`
		} `json:"socket_templates"`
	}
	testharness.DecodeJSON(t, rec, &catalog)
	assert.Equal(t, 1, catalog.Version)
	assert.NotEmpty(t, catalog.Platform)
	assert.Equal(t, canonicalHome, catalog.Home)
	require.Len(t, catalog.Categories, 7)
	assert.Equal(t, "secrets.ssh", catalog.Categories[0]["id"])
	assert.Equal(t, "home.directory", catalog.Categories[6]["id"])
	assert.Equal(t, []any{canonicalHome}, catalog.Categories[6]["paths"])
	assert.NotEmpty(t, catalog.Informational)
	require.NotEmpty(t, catalog.GlobalNetwork)
	assert.Equal(t, "api.example.com", catalog.GlobalNetwork[0]["entry"].(map[string]any)["domain"])
	require.NotEmpty(t, catalog.GlobalSockets)
	assert.Equal(t, []string{"net-local", "net-anthropic", "net-openai-codex", "net-github", "net-go-modules", "net-npm"},
		[]string{catalog.NetworkPacks[0].ID, catalog.NetworkPacks[1].ID, catalog.NetworkPacks[2].ID, catalog.NetworkPacks[3].ID, catalog.NetworkPacks[4].ID, catalog.NetworkPacks[5].ID})
	assert.Equal(t, []sandboxpolicy.NetworkAllowEntry{{Loopback: true}},
		catalog.NetworkPacks[0].Entries)
	assert.Equal(t, []sandboxpolicy.NetworkAllowEntry{
		{Domain: "api.openai.com", Ports: []int{443}},
	}, catalog.NetworkPacks[2].Entries)
	assert.Contains(t, catalog.NetworkPacks[2].Warning, "ChatGPT-auth Codex is refused")
	assert.Equal(t, []sandboxpolicy.NetworkAllowEntry{{
		Domain: "api.anthropic.com", Ports: []int{443},
	}}, catalog.NetworkPacks[1].Entries)
	assert.Equal(t, catalog.NetworkPacks, catalog.NetworkTemplates,
		"legacy insertion catalog remains a compatibility alias")
	assert.NotContains(t, rec.Body.String(), "net-pypi")
	assert.Equal(t, []string{"sockets-agentd-only", "sockets-ssh-agent"},
		[]string{catalog.SocketTemplates[0].ID, catalog.SocketTemplates[1].ID})
	var privateState *struct {
		Path      string   `json:"path"`
		Access    string   `json:"access"`
		Harnesses []string `json:"harnesses"`
	}
	for i := range catalog.Global {
		if catalog.Global[i].Path == "~/.tclaude/data" && catalog.Global[i].Access == "deny" {
			privateState = &catalog.Global[i]
			break
		}
	}
	require.NotNil(t, privateState)
	assert.Equal(t, []string{"claude", "codex"}, privateState.Harnesses)
	assert.Empty(t, catalog.GlobalWarnings)
}

func TestSandboxProfileDraftPermissionCanOnlySubmitValidatedDraft(t *testing.T) {
	f := newFlow(t)
	const peer = "sandbox-drafter-aaaa-bbbb"
	f.HaveConvWithTitle(peer, "sandbox-scribe")
	require.NoError(t, db.GrantAgentPermission(peer, agentd.PermSandboxProfilesDraft, "test"))
	token := "abcdefghijklmnop"

	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/sandbox-profile-drafts/"+token, map[string]any{
			"profile": map[string]any{
				"name": "proposed", "filesystem": []any{},
				"environment": []map[string]any{{"name": "CACHE_DIR", "value": "/tmp/cache"}},
				"network":     map[string]any{"mode": "list", "allow": []any{}},
			},
		}), peer))
	require.Equalf(t, http.StatusAccepted, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "has not been saved")
	assert.Contains(t, rec.Body.String(), `"class":"composition"`)

	// Draft permission is not policy-management permission: registry reads and
	// all CRUD/assignment surfaces remain forbidden, and no profile was saved.
	for _, req := range []*http.Request{
		testharness.JSONRequest(t, http.MethodGet, "/v1/sandbox-profiles", nil),
		testharness.JSONRequest(t, http.MethodPost, "/v1/sandbox-profiles", map[string]any{"name": "proposed"}),
		testharness.JSONRequest(t, http.MethodPut, "/v1/sandbox-profile-default", map[string]any{"name": "proposed"}),
	} {
		denied := testharness.Serve(f.Mux, agentd.AsAgentPeer(req, peer))
		assert.Equalf(t, http.StatusForbidden, denied.Code, "%s %s body=%s", req.Method, req.URL.Path, denied.Body.String())
	}
	missing := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/sandbox-profiles/proposed", nil)))
	assert.Equal(t, http.StatusNotFound, missing.Code, "draft submission must not persist a profile")
}

func TestSandboxProfilesCRUDValidationAndAssignments(t *testing.T) {
	f := newFlow(t)
	home := os.Getenv("HOME")
	cache := filepath.Join(home, "shared-cache")
	require.NoError(t, os.MkdirAll(cache, 0o755))
	canonicalCache, err := filepath.EvalSymlinks(cache)
	require.NoError(t, err)
	_, err = db.CreateAgentGroup("crew", "")
	require.NoError(t, err)

	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name": "dev-caches",
		"filesystem": []map[string]any{
			{"path": cache, "access": "read"},
			{"path": cache + string(filepath.Separator), "access": "write"},
		},
		"environment": []map[string]any{
			{"name": "GOCACHE", "value": cache},
			{"name": "GOCACHE", "value": cache},
		},
		"agent_directories": []string{"GOLANGCI_LINT_CACHE"},
		"network_access":    "internet",
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "create body=%s", rec.Body.String())

	rec = profileReq(t, f, http.MethodGet, "/v1/sandbox-profiles/dev-caches", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var got wireSandboxProfile
	testharness.DecodeJSON(t, rec, &got)
	require.Len(t, got.Filesystem, 1)
	assert.Equal(t, canonicalCache, got.Filesystem[0].Path)
	assert.Equal(t, "write", got.Filesystem[0].Access)
	require.Len(t, got.Environment, 1)
	assert.Equal(t, "GOCACHE", got.Environment[0].Name)
	assert.Equal(t, []string{"GOLANGCI_LINT_CACHE"}, got.AgentDirectories)
	assert.Equal(t, "internet", got.NetworkAccess)

	for _, body := range []map[string]any{
		{"name": "export"},
		{"name": "bad-network", "network_access": "local-only"},
		{"name": "IMPORT"},
		{"name": "protected", "filesystem": []map[string]any{{"path": filepath.Join(home, ".tclaude", "data"), "access": "write"}}},
		{"name": "reserved", "environment": []map[string]any{{"name": "TCLAUDE_SESSION_ID", "value": "spoof"}}},
		{"name": "conflict", "environment": []map[string]any{{"name": "A", "value": "1"}, {"name": "A", "value": "2"}}},
		{"name": "agent-dir-conflict", "environment": []map[string]any{{"name": "GOCACHE", "value": cache}}, "agent_directories": []string{"GOCACHE"}},
	} {
		rec = profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", body)
		assert.Equalf(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	}

	require.Equal(t, http.StatusOK, profileReq(t, f, http.MethodPut,
		"/v1/sandbox-profile-default", map[string]any{"name": "dev-caches"}).Code)
	require.Equal(t, http.StatusOK, profileReq(t, f, http.MethodPut,
		"/v1/groups/crew/sandbox-profile", map[string]any{"name": "dev-caches"}).Code)

	rec = profileReq(t, f, http.MethodPatch, "/v1/sandbox-profiles/dev-caches", map[string]any{
		"name": "renamed", "filesystem": []map[string]any{{"path": cache, "access": "write"}},
	})
	require.Equalf(t, http.StatusOK, rec.Code, "rename body=%s", rec.Body.String())
	for _, path := range []string{"/v1/sandbox-profile-default", "/v1/groups/crew/sandbox-profile"} {
		rec = profileReq(t, f, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var ref struct {
			Name string `json:"name"`
		}
		testharness.DecodeJSON(t, rec, &ref)
		assert.Equal(t, "renamed", ref.Name)
	}

	require.Equal(t, http.StatusNoContent,
		profileReq(t, f, http.MethodDelete, "/v1/sandbox-profiles/renamed", nil).Code)
	for _, path := range []string{"/v1/sandbox-profile-default", "/v1/groups/crew/sandbox-profile"} {
		rec = profileReq(t, f, http.MethodGet, path, nil)
		var ref struct {
			Name string `json:"name"`
		}
		testharness.DecodeJSON(t, rec, &ref)
		assert.Empty(t, ref.Name, "delete atomically clears assignment at %s", path)
	}
}

func TestSandboxProfilePreviewAndSaveRejectRetargetedRetainedSpelling(t *testing.T) {
	f := newFlow(t)
	home, err := filepath.EvalSymlinks(os.Getenv("HOME"))
	require.NoError(t, err)
	root := filepath.Join(home, "retarget-preview")
	original := filepath.Join(root, "original")
	current := filepath.Join(root, "current")
	alias := filepath.Join(root, "alias")
	require.NoError(t, os.MkdirAll(original, 0o755))
	require.NoError(t, os.MkdirAll(current, 0o755))
	require.NoError(t, os.Symlink(original, alias))

	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name": "retargeted",
		"filesystem": []map[string]any{{
			"path": alias, "access": "read",
		}},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "create body=%s", rec.Body.String())
	rec = profileReq(t, f, http.MethodGet, "/v1/sandbox-profiles/retargeted", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "get body=%s", rec.Body.String())
	var saved map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &saved))
	require.NotNil(t, saved["filesystem_spellings"])

	require.NoError(t, os.Remove(alias))
	require.NoError(t, os.Symlink(current, alias))
	for _, suffix := range []string{"?dry_run=1", ""} {
		rec = profileReq(t, f, http.MethodPatch,
			"/v1/sandbox-profiles/retargeted"+suffix, saved)
		require.Equalf(t, http.StatusBadRequest, rec.Code,
			"retargeted spelling must fail at preview and save: body=%s", rec.Body.String())
		assert.Contains(t, rec.Body.String(), `retargeted`)
		assert.Contains(t, rec.Body.String(), alias)
		assert.Contains(t, rec.Body.String(), original)
		assert.Contains(t, rec.Body.String(), current)
		assert.Contains(t, rec.Body.String(), "re-save the profile to adopt the new target")
		assert.Contains(t, rec.Body.String(), "remove the retained spelling")
	}

	// Omitting the old sidecar and submitting the spelling again is the
	// explicit re-authoring remedy: preview adopts the current target and
	// returns a new sidecar for the CAS-protected save.
	delete(saved, "filesystem_spellings")
	saved["filesystem"] = []map[string]any{{"path": alias, "access": "read"}}
	rec = profileReq(t, f, http.MethodPatch,
		"/v1/sandbox-profiles/retargeted?dry_run=1", saved)
	require.Equalf(t, http.StatusOK, rec.Code, "re-author preview body=%s", rec.Body.String())
	var preview struct {
		After    wireSandboxProfile `json:"after"`
		Revision string             `json:"revision"`
	}
	testharness.DecodeJSON(t, rec, &preview)
	canonicalCurrent, err := filepath.EvalSymlinks(current)
	require.NoError(t, err)
	require.Len(t, preview.After.Filesystem, 1)
	assert.Equal(t, canonicalCurrent, preview.After.Filesystem[0].Path)
	require.NotNil(t, preview.After.FilesystemSpellings)
	require.Len(t, preview.After.FilesystemSpellings.Rules, 1)
	assert.Equal(t, []string{alias}, preview.After.FilesystemSpellings.Rules[0].Spellings)

	rec = profileReq(t, f, http.MethodPatch,
		"/v1/sandbox-profiles/retargeted?revision="+url.QueryEscape(preview.Revision),
		preview.After)
	require.Equalf(t, http.StatusOK, rec.Code, "re-author save body=%s", rec.Body.String())
	rec = profileReq(t, f, http.MethodGet, "/v1/sandbox-profiles/retargeted", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var reauthored wireSandboxProfile
	testharness.DecodeJSON(t, rec, &reauthored)
	require.Len(t, reauthored.Filesystem, 1)
	assert.Equal(t, canonicalCurrent, reauthored.Filesystem[0].Path)
	require.NotNil(t, reauthored.FilesystemSpellings)
	assert.Equal(t, []string{alias}, reauthored.FilesystemSpellings.Rules[0].Spellings)
}

func TestSandboxProfilesExportImportRoundTrip(t *testing.T) {
	f := newFlow(t)
	cache := filepath.Join(os.Getenv("HOME"), "cache")
	require.NoError(t, os.MkdirAll(cache, 0o755))
	canonicalCache, err := filepath.EvalSymlinks(cache)
	require.NoError(t, err)
	_, err = db.CreateAgentGroup("portable-group", "")
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name":           "portable",
		"filesystem":     []map[string]any{{"path": cache, "access": "write"}},
		"environment":    []map[string]any{{"name": "GOCACHE", "value": cache}},
		"network_access": "none",
	}).Code)
	require.Equal(t, http.StatusOK, profileReq(t, f, http.MethodPut,
		"/v1/sandbox-profile-default", map[string]any{"name": "portable"}).Code)
	require.Equal(t, http.StatusOK, profileReq(t, f, http.MethodPut,
		"/v1/groups/portable-group/sandbox-profile", map[string]any{"name": "portable"}).Code)

	rec := profileReq(t, f, http.MethodGet, "/v1/sandbox-profiles/export?name=portable&include_assignments=true", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "export body=%s", rec.Body.String())
	var bundle map[string]any
	testharness.DecodeJSON(t, rec, &bundle)
	assert.Equal(t, "tclaude-sandbox-profiles", bundle["format"])
	// v5 removed read_baseline/read_baseline_exclusions (TCL-623), v6
	// removed break_glass_filesystem (TCL-791), v7 adds independent
	// network and Unix-socket axes, and v8 retains filesystem spellings.
	// Exporting only the newest
	// version keeps an older importer from silently dropping a
	// security-significant field as an unknown key; older versions stay importable.
	assert.Equal(t, float64(8), bundle["format_version"])

	require.Equal(t, http.StatusNoContent,
		profileReq(t, f, http.MethodDelete, "/v1/sandbox-profiles/portable", nil).Code)
	bundle["apply_assignments"] = true
	rec = profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles/import", bundle)
	require.Equalf(t, http.StatusOK, rec.Code, "import body=%s", rec.Body.String())
	rec = profileReq(t, f, http.MethodGet, "/v1/sandbox-profiles/portable", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var got wireSandboxProfile
	testharness.DecodeJSON(t, rec, &got)
	require.Len(t, got.Filesystem, 1)
	assert.Equal(t, canonicalCache, got.Filesystem[0].Path)
	assert.Equal(t, "none", got.NetworkAccess)
	for _, path := range []string{"/v1/sandbox-profile-default", "/v1/groups/portable-group/sandbox-profile"} {
		rec = profileReq(t, f, http.MethodGet, path, nil)
		var ref struct {
			Name string `json:"name"`
		}
		testharness.DecodeJSON(t, rec, &ref)
		assert.Equal(t, "portable", ref.Name)
	}
}

func TestSandboxProfileLocalNetworkShapesRoundTripWithoutSchemaChanges(t *testing.T) {
	f := newFlow(t)
	profiles := []struct {
		name  string
		rules sandboxpolicy.NetworkRules
	}{
		{
			name: "local-access",
			rules: sandboxpolicy.NetworkRules{
				Mode: sandboxpolicy.AccessModeList,
				Allow: []sandboxpolicy.NetworkAllowEntry{{
					Loopback: true,
				}},
			},
		},
		{
			name: "local-model-apis",
			rules: sandboxpolicy.NetworkRules{
				Mode: sandboxpolicy.AccessModeList,
				Allow: []sandboxpolicy.NetworkAllowEntry{
					{Domain: "api.anthropic.com", Ports: []int{443}},
					{Domain: "api.openai.com", Ports: []int{443}},
					{Loopback: true},
				},
			},
		},
	}
	for _, profile := range profiles {
		rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
			"name": profile.name, "filesystem": []any{}, "environment": []any{},
			"network": profile.rules,
		})
		require.Equalf(t, http.StatusCreated, rec.Code, "%s create body=%s", profile.name, rec.Body.String())

		rec = profileReq(t, f, http.MethodGet, "/v1/sandbox-profiles/"+profile.name, nil)
		require.Equalf(t, http.StatusOK, rec.Code, "%s get body=%s", profile.name, rec.Body.String())
		var got struct {
			Network *sandboxpolicy.NetworkRules `json:"network"`
		}
		testharness.DecodeJSON(t, rec, &got)
		require.NotNil(t, got.Network)
		assert.Equal(t, profile.rules, *got.Network)
	}

	rec := profileReq(t, f, http.MethodGet,
		"/v1/sandbox-profiles/export?name=local-access&name=local-model-apis", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "export body=%s", rec.Body.String())
	var bundle map[string]any
	testharness.DecodeJSON(t, rec, &bundle)
	assert.Equal(t, float64(8), bundle["format_version"],
		"the presets use the existing access-axis schema")

	for _, profile := range profiles {
		require.Equal(t, http.StatusNoContent, profileReq(
			t, f, http.MethodDelete, "/v1/sandbox-profiles/"+profile.name, nil).Code)
	}
	rec = profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles/import", bundle)
	require.Equalf(t, http.StatusOK, rec.Code, "import body=%s", rec.Body.String())
	for _, profile := range profiles {
		rec = profileReq(t, f, http.MethodGet, "/v1/sandbox-profiles/"+profile.name, nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var got struct {
			Network *sandboxpolicy.NetworkRules `json:"network"`
		}
		testharness.DecodeJSON(t, rec, &got)
		require.NotNil(t, got.Network)
		assert.Equal(t, profile.rules, *got.Network)
	}
}

func TestSandboxProfilesImportConflictRollsBackWholeBundle(t *testing.T) {
	f := newFlow(t)
	require.Equal(t, http.StatusCreated,
		profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{"name": "already-there"}).Code)
	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles/import", map[string]any{
		"format": "tclaude-sandbox-profiles", "format_version": 1,
		"profiles": []map[string]any{{"name": "would-be-partial"}, {"name": "already-there"}},
	})
	require.Equalf(t, http.StatusConflict, rec.Code, "import body=%s", rec.Body.String())
	rec = profileReq(t, f, http.MethodGet, "/v1/sandbox-profiles/would-be-partial", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, "conflict planning must happen before the first insert")
}

func TestSandboxProfilesImportPreviewWarnsAndImportRetainsMissingPaths(t *testing.T) {
	f := newFlow(t)
	canonicalHome, err := filepath.EvalSymlinks(os.Getenv("HOME"))
	require.NoError(t, err)
	missing := filepath.Join(canonicalHome, "portable-recipient-missing", "cache")
	require.NoError(t, os.RemoveAll(filepath.Dir(missing)))
	bundle := map[string]any{
		"format": "tclaude-sandbox-profiles", "format_version": 1,
		"profiles": []map[string]any{{
			"name":       "portable-missing",
			"filesystem": []map[string]any{{"path": missing, "access": "write"}},
		}},
	}

	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles/import/inspect", bundle)
	require.Equalf(t, http.StatusOK, rec.Code, "inspect body=%s", rec.Body.String())
	var preview struct {
		Profiles []wireSandboxProfile `json:"profiles"`
		Warnings []struct {
			Profile string `json:"profile"`
			Path    string `json:"path"`
			Message string `json:"message"`
		} `json:"warnings"`
	}
	testharness.DecodeJSON(t, rec, &preview)
	require.Len(t, preview.Profiles, 1)
	require.Len(t, preview.Warnings, 1)
	assert.Equal(t, "portable-missing", preview.Warnings[0].Profile)
	assert.Equal(t, missing, preview.Warnings[0].Path)
	assert.Contains(t, preview.Warnings[0].Message, "does not exist locally")

	rec = profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles/import", bundle)
	require.Equalf(t, http.StatusOK, rec.Code, "import body=%s", rec.Body.String())
	var result struct {
		Imported []string `json:"imported"`
		Warnings []string `json:"warnings"`
	}
	testharness.DecodeJSON(t, rec, &result)
	assert.Equal(t, []string{"portable-missing"}, result.Imported)
	require.Len(t, result.Warnings, 1)
	assert.Contains(t, result.Warnings[0], missing)

	rec = profileReq(t, f, http.MethodGet, "/v1/sandbox-profiles/portable-missing", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var stored wireSandboxProfile
	testharness.DecodeJSON(t, rec, &stored)
	require.Len(t, stored.Filesystem, 1)
	assert.Equal(t, missing, stored.Filesystem[0].Path)
}
