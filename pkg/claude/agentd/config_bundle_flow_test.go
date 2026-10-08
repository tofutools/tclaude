package agentd_test

import (
	"encoding/json"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/configbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestConfigBundleRoundTripAndConflict(t *testing.T) {
	f := newFlow(t)
	role := &db.Role{Name: "portable-role", Brief: "Review the change."}
	_, err := db.CreateRole(role)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, profileReq(t, f, http.MethodPost, "/v1/spawn-profiles", map[string]any{"name": "portable-profile", "role_ref": role.Name}).Code)
	rec := profileReq(t, f, http.MethodGet, "/v1/config-bundle/export?only=roles/portable-role&only=profiles/portable-profile", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var b configbundle.Bundle
	testharness.DecodeJSON(t, rec, &b)
	require.Equal(t, configbundle.Format, b.Format)
	require.NotEmpty(t, b.CreatedAt)
	require.NotEmpty(t, b.TclaudeVersion)
	require.Len(t, b.Sections["profiles"], 1)
	_, err = db.DeleteSpawnProfile("portable-profile")
	require.NoError(t, err)
	_, err = db.DeleteRole(role.Name)
	require.NoError(t, err)
	rec = profileReq(t, f, http.MethodPost, "/v1/config-bundle/import", map[string]any{"bundle": b})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"security_changes":2`)
	missing, err := db.GetRole(role.Name)
	require.NoError(t, err)
	assert.Nil(t, missing, "preview must not write")
	rec = profileReq(t, f, http.MethodPost, "/v1/config-bundle/import", map[string]any{"bundle": b, "apply": true})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	got, err := db.GetSpawnProfile("portable-profile")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, role.Name, got.RoleRef)
	// Re-importing the same values is harmless without --replace.
	rec = profileReq(t, f, http.MethodPost, "/v1/config-bundle/import", map[string]any{"bundle": b, "apply": true})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	got.Model = "haiku"
	require.NoError(t, db.UpdateSpawnProfile(got))
	rec = profileReq(t, f, http.MethodPost, "/v1/config-bundle/import", map[string]any{"bundle": b, "apply": true})
	require.Equal(t, 409, rec.Code, rec.Body.String())
	got, err = db.GetSpawnProfile("portable-profile")
	require.NoError(t, err)
	assert.Equal(t, "haiku", got.Model)
	rec = profileReq(t, f, http.MethodPost, "/v1/config-bundle/import", map[string]any{"bundle": b, "apply": true, "replace": true})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	got, err = db.GetSpawnProfile("portable-profile")
	require.NoError(t, err)
	assert.Empty(t, got.Model)
}

func TestConfigBundleFlagsAndSelectiveImport(t *testing.T) {
	f := newFlow(t)
	_, err := db.CreateRole(&db.Role{Name: "credential-role", Brief: "Use api_key=supersecret123456789 to connect"})
	require.NoError(t, err)
	rec := profileReq(t, f, http.MethodGet, "/v1/config-bundle/export?only=roles/credential-role", nil)
	require.Equal(t, 422, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "supersecret")
	assert.Contains(t, rec.Body.String(), "credential-role")
	assert.Contains(t, rec.Body.String(), "brief")
	rec = profileReq(t, f, http.MethodGet, "/v1/config-bundle/export?only=roles/credential-role&allow_flagged=true", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "supersecret")
	var b configbundle.Bundle
	testharness.DecodeJSON(t, rec, &b)
	// Skip does not modify even an explicitly replaceable role.
	rec = profileReq(t, f, http.MethodPost, "/v1/config-bundle/import", map[string]any{"bundle": b, "skip": []string{"roles"}, "apply": true, "replace": true})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"applied":[]`)
	b.FormatVersion = 99
	rec = profileReq(t, f, http.MethodPost, "/v1/config-bundle/import", map[string]any{"bundle": b})
	require.Equal(t, 400, rec.Code)
	assert.Contains(t, rec.Body.String(), "format_version 99")
}

func TestConfigBundlePlaceholdersAndSandboxIncludes(t *testing.T) {
	f := newFlow(t)
	for _, v := range []map[string]any{
		{"name": "z-base", "filesystem": []map[string]string{{"path": "/opt/bundle-example", "access": "read"}}},
		{"name": "a-child", "includes": []string{"z-base"}},
	} {
		rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", v)
		require.Equal(t, 201, rec.Code, rec.Body.String())
	}
	rec := profileReq(t, f, http.MethodGet, "/v1/config-bundle/export?only=sandbox-profiles/z-base&only=sandbox-profiles/a-child", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "/opt/bundle-example")
	var b configbundle.Bundle
	testharness.DecodeJSON(t, rec, &b)
	require.NotEmpty(t, b.Placeholders)
	rec = profileReq(t, f, http.MethodPost, "/v1/config-bundle/import", map[string]any{"bundle": b})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "unresolved")
	rec = profileReq(t, f, http.MethodPost, "/v1/config-bundle/import", map[string]any{"bundle": b, "apply": true, "replace": true})
	require.Equal(t, 409, rec.Code, rec.Body.String())
	// Binding structured paths preserves valid JSON, including quote characters.
	values := map[string]string{}
	for _, p := range b.Placeholders {
		values[p.Name] = "/opt/rebound"
	}
	rec = profileReq(t, f, http.MethodPost, "/v1/config-bundle/import", map[string]any{"bundle": b, "values": values, "apply": true, "replace": true})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	got, err := db.GetSandboxProfile("z-base")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "/opt/rebound", got.Filesystem[0].Path)
	// External paths never appear in the exported bytes, even in nested arrays.
	raw, err := json.Marshal(b)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "/opt/bundle-example")
}

func TestConfigBundleTemplatesProcessesAndConfig(t *testing.T) {
	f, root := processAuthoringFlow(t)
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	_, err = fs.PutTemplate(t.Context(), processRESTTemplate("portable-process", "Portable process", 20))
	require.NoError(t, err)
	rec := profileReq(t, f, http.MethodPost, "/v1/templates", fullTemplateBody("portable-team"))
	require.Equal(t, 201, rec.Code, rec.Body.String())
	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.LogLevel = "debug"
	cfg.Federation = &config.FederationConfig{Invite: "secret-invite", HubCAFile: "/private/ca.pem"}
	require.NoError(t, config.Save(cfg))
	rec = profileReq(t, f, http.MethodGet, "/v1/config-bundle/export?only=templates/portable-team&only=process-templates/portable-process&only=config/log_level&only=default-permissions", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "secret-invite")
	assert.NotContains(t, rec.Body.String(), "/private/ca.pem")
	var b configbundle.Bundle
	testharness.DecodeJSON(t, rec, &b)
	_, err = db.DeleteGroupTemplate("portable-team")
	require.NoError(t, err)
	cfg.LogLevel = "info"
	require.NoError(t, config.Save(cfg))
	rec = profileReq(t, f, http.MethodPost, "/v1/config-bundle/import", map[string]any{"bundle": b, "apply": true, "replace": true})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	team, err := db.GetGroupTemplate("portable-team")
	require.NoError(t, err)
	require.NotNil(t, team)
	assert.Len(t, team.Agents, 2)
	cfg, err = config.Load()
	require.NoError(t, err)
	assert.Equal(t, "debug", cfg.LogLevel)
	assert.Equal(t, "secret-invite", cfg.Federation.Invite, "unselected private settings survive")
	// Importing process source under a new declared id exercises first creation.
	item := b.Sections["process-templates"][0]
	var source map[string]string
	require.NoError(t, json.Unmarshal(item.Value, &source))
	source["source"] = strings.ReplaceAll(source["source"], "portable-process", "portable-process-copy")
	item.Name = "portable-process-copy"
	item.Value, err = json.Marshal(source)
	require.NoError(t, err)
	b.Sections = map[string][]configbundle.Item{"process-templates": {item}}
	rec = profileReq(t, f, http.MethodPost, "/v1/config-bundle/import", map[string]any{"bundle": b, "apply": true})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	head, err := fs.GetTemplateHead(t.Context(), item.Name)
	require.NoError(t, err)
	require.NotEmpty(t, head.Ref)
}

func TestConfigBundleAgentGates(t *testing.T) {
	f := newFlow(t)
	const peer = "bundle-gate-aaaa-bbbb"
	f.HaveConvWithTitle(peer, "bundle-worker")
	for _, route := range []struct{ method, path, slug string }{
		{http.MethodGet, "/v1/config-bundle/export", agentd.PermConfigExport},
		{http.MethodPost, "/v1/config-bundle/import", agentd.PermConfigImport},
	} {
		rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, route.method, route.path, nil), peer))
		require.Equal(t, 403, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), route.slug)
	}
	require.NoError(t, db.GrantAgentPermission(peer, agentd.PermConfigExport, "test"))
	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/config-bundle/export?only=roles", nil), peer))
	require.Equal(t, 200, rec.Code, rec.Body.String())
}

func TestConfigBundlePreviewRejectsMissingTemplateDependency(t *testing.T) {
	f := newFlow(t)
	raw := json.RawMessage(`{"format":"tclaude-task-force","format_version":3,"template":{"name":"needs-profile","agents":[{"name":"worker","spawn_profile":"missing-profile"}]}}`)
	b := configbundle.Bundle{Format: configbundle.Format, FormatVersion: 1, Sections: map[string][]configbundle.Item{"templates": {{Name: "needs-profile", Value: raw}}}}
	rec := profileReq(t, f, http.MethodPost, "/v1/config-bundle/import", map[string]any{"bundle": b})
	require.Equal(t, 400, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "missing-profile")
	template, err := db.GetGroupTemplate("needs-profile")
	require.NoError(t, err)
	assert.Nil(t, template)
}

func TestConfigBundleNestedConfigReplacementMatchesPreview(t *testing.T) {
	f := newFlow(t)
	oldThreshold, oldTokens := 30, 12345
	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.ClaudeResume = &config.ClaudeResumeConfig{ThresholdMinutes: &oldThreshold, TokenThreshold: &oldTokens}
	require.NoError(t, config.Save(cfg))
	b := configbundle.Bundle{Format: configbundle.Format, FormatVersion: 1, Sections: map[string][]configbundle.Item{"config": {{Name: "claude_resume", Value: json.RawMessage(`{"threshold_minutes":60}`)}}}}
	rec := profileReq(t, f, http.MethodPost, "/v1/config-bundle/import", map[string]any{"bundle": b, "apply": true, "replace": true})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	cfg, err = config.Load()
	require.NoError(t, err)
	require.NotNil(t, cfg.ClaudeResume)
	require.NotNil(t, cfg.ClaudeResume.ThresholdMinutes)
	assert.Equal(t, 60, *cfg.ClaudeResume.ThresholdMinutes)
	assert.Nil(t, cfg.ClaudeResume.TokenThreshold)
}

func TestConfigBundleTemplateProfileAliasAndHarness(t *testing.T) {
	f := newFlow(t)
	require.Equal(t, 201, profileReq(t, f, http.MethodPost, "/v1/spawn-profiles", map[string]any{"name": "portable-codex", "aliases": []string{"codex-alias"}, "harness": "codex"}).Code)
	raw := json.RawMessage(`{"format":"tclaude-task-force","format_version":3,"template":{"name":"codex-team","agents":[{"name":"worker","spawn_profile":"codex-alias","sandbox":"workspace-write"}]}}`)
	b := configbundle.Bundle{Format: configbundle.Format, FormatVersion: 1, Sections: map[string][]configbundle.Item{"templates": {{Name: "codex-team", Value: raw}}}}
	rec := profileReq(t, f, http.MethodPost, "/v1/config-bundle/import", map[string]any{"bundle": b})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	rec = profileReq(t, f, http.MethodGet, "/v1/config-bundle/export?only=profiles/portable-codex", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var profiles configbundle.Bundle
	testharness.DecodeJSON(t, rec, &profiles)
	b.Sections["profiles"] = profiles.Sections["profiles"]
	_, err := db.DeleteSpawnProfile("portable-codex")
	require.NoError(t, err)
	rec = profileReq(t, f, http.MethodPost, "/v1/config-bundle/import", map[string]any{"bundle": b, "apply": true})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	team, err := db.GetGroupTemplate("codex-team")
	require.NoError(t, err)
	require.NotNil(t, team)
	require.Len(t, team.Agents, 1)
	assert.Equal(t, "portable-codex", team.Agents[0].SpawnProfile)
}
