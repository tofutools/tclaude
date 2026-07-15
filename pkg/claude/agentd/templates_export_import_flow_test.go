package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// JOH-341: export / import task forces as a portable JSON file. A
// template's inner wire JSON already round-trips; these flow tests cover
// the portable *envelope* around it — export produces a versioned file,
// import consumes it with collision / --update / --as semantics, degrades
// gracefully on machine-local references that don't exist here, and
// rejects an envelope from a newer tclaude.

// exportEnvelope mirrors the daemon's templateExportEnvelope. The inner
// template is decoded loosely so a test can assert on the fields that
// must ride along (agents incl. JOH-239 launch fields + work_pattern).
type exportEnvelope struct {
	Format        string          `json:"format"`
	FormatVersion int             `json:"format_version"`
	ExportedAt    string          `json:"exported_at"`
	Template      wireTemplateFor `json:"template"`
}

type wireTemplateFor struct {
	Name           string `json:"name"`
	Descr          string `json:"descr"`
	DefaultContext string `json:"default_context"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
	Agents         []struct {
		Name         string   `json:"name"`
		Role         string   `json:"role"`
		Descr        string   `json:"descr"`
		IsOwner      bool     `json:"is_owner"`
		Permissions  []string `json:"permissions"`
		SpawnProfile string   `json:"spawn_profile"`
		Harness      string   `json:"harness"`
		Model        string   `json:"model"`
		Effort       string   `json:"effort"`
	} `json:"agents"`
	WorkPattern []struct {
		SendTo string `json:"send_to"`
		Value  string `json:"value"`
	} `json:"work_pattern"`
}

type tmplImportResult struct {
	Imported string   `json:"imported"`
	Updated  bool     `json:"updated"`
	Warnings []string `json:"warnings"`
}

// fullTemplateBody is a rich template covering the JOH-239 per-role
// launch fields and a JOH-336 work pattern, so a round-trip has
// something meaty to preserve.
func fullTemplateBody(name string) map[string]any {
	return map[string]any{
		"name":            name,
		"descr":           "a lead and a tester",
		"default_context": "TEAM RULES: worktrees + PRs",
		"agents": []map[string]any{
			{"name": "lead", "role": "lead", "descr": "coordinates", "is_owner": true,
				"initial_message": "Lead the team.", "permissions": []string{"groups.spawn"},
				"model": "opus", "effort": "high"},
			{"name": "tester", "role": "qa", "model": "haiku"},
		},
		"work_pattern": []map[string]any{
			{"send_to": "lead", "value": "Kick off: {{task}}"},
			{"send_to": "all", "value": "House rules apply."},
		},
	}
}

// TestTemplateExportImport_RoundTrip: create a full template, export it,
// import it under a new name via ?as=, then fetch the imported template
// and assert every carried field survives the trip.
func TestTemplateExportImport_RoundTrip(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", fullTemplateBody("origin")).Code, "create template")

	// Export.
	rec := humanReq(t, f, http.MethodGet, "/v1/templates/origin/export", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "export: %s", rec.Body.String())
	var env exportEnvelope
	testharness.DecodeJSON(t, rec, &env)
	assert.Equal(t, "tclaude-task-force", env.Format, "envelope format tag")
	assert.Equal(t, 3, env.FormatVersion, "envelope format version")
	assert.NotEmpty(t, env.ExportedAt, "exported_at provenance stamp")
	// Machine-local identity is stripped from the blueprint.
	assert.Empty(t, env.Template.CreatedAt, "created_at stripped from export")
	assert.Empty(t, env.Template.UpdatedAt, "updated_at stripped from export")
	require.Len(t, env.Template.Agents, 2, "both agents exported")

	// Import under a fresh name.
	rec = humanReq(t, f, http.MethodPost, "/v1/templates/import?as=cloned",
		map[string]any{"format": env.Format, "format_version": env.FormatVersion, "template": env.Template})
	require.Equalf(t, http.StatusCreated, rec.Code, "import: %s", rec.Body.String())
	var ir tmplImportResult
	testharness.DecodeJSON(t, rec, &ir)
	assert.Equal(t, "cloned", ir.Imported, "imported under the --as name")
	assert.False(t, ir.Updated, "a fresh import is not an update")
	assert.Empty(t, ir.Warnings, "no degradation warnings for a clean import")

	// Fetch the imported template and assert the full shape survived.
	rec = humanReq(t, f, http.MethodGet, "/v1/templates/cloned", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "fetch imported: %s", rec.Body.String())
	var got wireTemplateFor
	testharness.DecodeJSON(t, rec, &got)
	assert.Equal(t, "cloned", got.Name)
	assert.Equal(t, "a lead and a tester", got.Descr)
	assert.Equal(t, "TEAM RULES: worktrees + PRs", got.DefaultContext)
	require.Len(t, got.Agents, 2, "both agents stored")
	lead := got.Agents[0]
	assert.Equal(t, "lead", lead.Name)
	assert.True(t, lead.IsOwner, "owner flag preserved")
	assert.Equal(t, []string{"groups.spawn"}, lead.Permissions, "permission slug preserved")
	assert.Equal(t, "opus", lead.Model, "inline model preserved")
	assert.Equal(t, "high", lead.Effort, "inline effort preserved")
	assert.Equal(t, "haiku", got.Agents[1].Model, "tester model preserved")
	require.Len(t, got.WorkPattern, 2, "work pattern preserved")
	assert.Equal(t, "lead", got.WorkPattern[0].SendTo)
	assert.Equal(t, "Kick off: {{task}}", got.WorkPattern[0].Value, "{{task}} placeholder survives export/import")
	assert.Equal(t, "all", got.WorkPattern[1].SendTo)
}

// TestTemplateImport_CollisionRequiresUpdate: importing over an existing
// name is a 409 unless --update is set; with it, the template is
// overwritten in place.
func TestTemplateImport_CollisionRequiresUpdate(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", fullTemplateBody("dup")).Code, "create original")

	envBody := func(descr string) map[string]any {
		tmpl := fullTemplateBody("dup")
		tmpl["descr"] = descr
		return map[string]any{"format": "tclaude-task-force", "format_version": 1, "template": tmpl}
	}

	// Bare import → 409.
	rec := humanReq(t, f, http.MethodPost, "/v1/templates/import", envBody("v2 descr"))
	assert.Equalf(t, http.StatusConflict, rec.Code, "collision must 409: %s", rec.Body.String())

	// With ?update=true → overwrite in place.
	rec = humanReq(t, f, http.MethodPost, "/v1/templates/import?update=true", envBody("v2 descr"))
	require.Equalf(t, http.StatusOK, rec.Code, "update import: %s", rec.Body.String())
	var ir tmplImportResult
	testharness.DecodeJSON(t, rec, &ir)
	assert.Equal(t, "dup", ir.Imported)
	assert.True(t, ir.Updated, "update flag reported")

	// The overwrite took effect.
	rec = humanReq(t, f, http.MethodGet, "/v1/templates/dup", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var got wireTemplateFor
	testharness.DecodeJSON(t, rec, &got)
	assert.Equal(t, "v2 descr", got.Descr, "descr overwritten by the update import")
}

// TestTemplateImport_AsAvoidsCollision: --as imports under a different
// name, sidestepping a collision — both templates then coexist.
func TestTemplateImport_AsAvoidsCollision(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", fullTemplateBody("base")).Code, "create base")

	env := map[string]any{"format": "tclaude-task-force", "format_version": 1, "template": fullTemplateBody("base")}
	rec := humanReq(t, f, http.MethodPost, "/v1/templates/import?as=base-copy", env)
	require.Equalf(t, http.StatusCreated, rec.Code, "import as: %s", rec.Body.String())

	// Both exist.
	for _, n := range []string{"base", "base-copy"} {
		got := humanReq(t, f, http.MethodGet, "/v1/templates/"+n, nil)
		assert.Equalf(t, http.StatusOK, got.Code, "template %q exists", n)
	}
}

// TestTemplateImport_MissingProfileRefDegrades: importing a template that
// references a spawn profile absent on this machine strips the ref and
// reports a warning, but still stores an instantiable template (inline
// overrides intact).
func TestTemplateImport_MissingProfileRefDegrades(t *testing.T) {
	f := newFlow(t)

	tmpl := map[string]any{
		"name": "portable",
		"agents": []map[string]any{
			{"name": "worker", "role": "dev", "spawn_profile": "ghost-profile", "model": "haiku"},
		},
	}
	env := map[string]any{"format": "tclaude-task-force", "format_version": 1, "template": tmpl}

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/import", env)
	require.Equalf(t, http.StatusCreated, rec.Code, "import should succeed by degrading: %s", rec.Body.String())
	var ir tmplImportResult
	testharness.DecodeJSON(t, rec, &ir)
	require.Len(t, ir.Warnings, 1, "one degradation warning")
	assert.Contains(t, ir.Warnings[0], "ghost-profile", "warning names the missing profile")

	// Stored without the dangling ref, inline override intact.
	rec = humanReq(t, f, http.MethodGet, "/v1/templates/portable", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var got wireTemplateFor
	testharness.DecodeJSON(t, rec, &got)
	require.Len(t, got.Agents, 1)
	assert.Empty(t, got.Agents[0].SpawnProfile, "dangling profile ref stripped")
	assert.Equal(t, "haiku", got.Agents[0].Model, "inline override survives")
}

// TestTemplateImport_UnknownPermSlugDegrades: an unknown permission slug
// is dropped with a warning rather than failing the whole import.
func TestTemplateImport_UnknownPermSlugDegrades(t *testing.T) {
	f := newFlow(t)

	tmpl := map[string]any{
		"name": "perms",
		"agents": []map[string]any{
			{"name": "worker", "permissions": []string{"groups.spawn", "made.up.slug"}},
		},
	}
	env := map[string]any{"format": "tclaude-task-force", "format_version": 1, "template": tmpl}

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/import", env)
	require.Equalf(t, http.StatusCreated, rec.Code, "import degrades on unknown slug: %s", rec.Body.String())
	var ir tmplImportResult
	testharness.DecodeJSON(t, rec, &ir)
	require.Len(t, ir.Warnings, 1, "one warning for the bad slug")
	assert.Contains(t, ir.Warnings[0], "made.up.slug")

	rec = humanReq(t, f, http.MethodGet, "/v1/templates/perms", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var got wireTemplateFor
	testharness.DecodeJSON(t, rec, &got)
	require.Len(t, got.Agents, 1)
	assert.Equal(t, []string{"groups.spawn"}, got.Agents[0].Permissions, "known slug kept, unknown dropped")
}

// TestTemplateImport_UnknownSlugInInlineProfileDegrades: an unknown slug
// inside a template-local profile's permission_overrides degrades exactly like
// the legacy list — dropped with a warning, never a whole-import failure (a
// template exported from a newer tclaude with a new slug must still import).
func TestTemplateImport_UnknownSlugInInlineProfileDegrades(t *testing.T) {
	f := newFlow(t)

	tmpl := map[string]any{
		"name": "perms2",
		"agents": []map[string]any{
			{"name": "worker", "profile_inline": map[string]any{
				"model": "haiku",
				"permission_overrides": map[string]any{
					"groups.spawn":  "grant",
					"made.up.slug2": "grant",
				},
			}},
		},
	}
	env := map[string]any{"format": "tclaude-task-force", "format_version": 1, "template": tmpl}

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/import", env)
	require.Equalf(t, http.StatusCreated, rec.Code, "import degrades on unknown inline-profile slug: %s", rec.Body.String())
	var ir tmplImportResult
	testharness.DecodeJSON(t, rec, &ir)
	require.Len(t, ir.Warnings, 1, "one warning for the bad slug")
	assert.Contains(t, ir.Warnings[0], "made.up.slug2")

	rec = humanReq(t, f, http.MethodGet, "/v1/templates/perms2", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var got struct {
		Agents []struct {
			ProfileInline *struct {
				Model               string            `json:"model"`
				PermissionOverrides map[string]string `json:"permission_overrides"`
			} `json:"profile_inline"`
		} `json:"agents"`
	}
	testharness.DecodeJSON(t, rec, &got)
	require.Len(t, got.Agents, 1)
	require.NotNil(t, got.Agents[0].ProfileInline, "inline profile survives the import")
	assert.Equal(t, "haiku", got.Agents[0].ProfileInline.Model)
	assert.Equal(t, map[string]string{"groups.spawn": "grant"},
		got.Agents[0].ProfileInline.PermissionOverrides, "known slug kept, unknown dropped")
}

// TestTemplateImport_VersionTooNewRejected: an envelope whose
// format_version exceeds this build's is rejected with an upgrade
// message.
func TestTemplateImport_VersionTooNewRejected(t *testing.T) {
	f := newFlow(t)

	env := map[string]any{
		"format":         "tclaude-task-force",
		"format_version": 999,
		"template":       fullTemplateBody("future"),
	}
	rec := humanReq(t, f, http.MethodPost, "/v1/templates/import", env)
	require.Equalf(t, http.StatusBadRequest, rec.Code, "version-too-new must be rejected: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "upgrade tclaude", "error tells the user to upgrade")
}

// TestTemplateImport_MissingVersionRejected: an envelope with no (or a
// zero/negative) format_version is rejected — a plain JSON blob that
// isn't a real export must not slip through the version gate.
func TestTemplateImport_MissingVersionRejected(t *testing.T) {
	f := newFlow(t)

	// No format_version field at all → decodes to 0 → rejected.
	env := map[string]any{"format": "tclaude-task-force", "template": fullTemplateBody("nover")}
	rec := humanReq(t, f, http.MethodPost, "/v1/templates/import", env)
	require.Equalf(t, http.StatusBadRequest, rec.Code, "missing format_version must be rejected: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "format_version", "error names the missing field")
}

// TestTemplateExportImport_PermissionGating: export is open (a plain
// agent peer with no slug can read it), but import is gated on
// templates.manage (the same peer is refused).
func TestTemplateExportImport_PermissionGating(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", fullTemplateBody("gated")).Code, "create template")

	const peer = "gate-aaaa-bbbb-cccc-dddd"
	f.HaveConvWithTitle(peer, "peer")

	// Export is open — a bare agent peer can read it.
	rec := testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/templates/gated/export", nil), peer))
	assert.Equalf(t, http.StatusOK, rec.Code, "export is open to a plain peer: %s", rec.Body.String())

	// Import is gated on templates.manage — the same peer is refused.
	env := map[string]any{"format": "tclaude-task-force", "format_version": 1, "template": fullTemplateBody("gated2")}
	rec = testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/templates/import", env), peer))
	assert.Equalf(t, http.StatusForbidden, rec.Code, "import requires templates.manage: %s", rec.Body.String())
}

// TestTemplateImport_WrongFormatRejected: a JSON file that isn't a
// task-force export is rejected with a clear format error, not a
// confusing field-validation failure.
func TestTemplateImport_WrongFormatRejected(t *testing.T) {
	f := newFlow(t)

	env := map[string]any{"format": "something-else", "format_version": 1, "template": fullTemplateBody("x")}
	rec := humanReq(t, f, http.MethodPost, "/v1/templates/import", env)
	require.Equalf(t, http.StatusBadRequest, rec.Code, "wrong format must be rejected: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "tclaude task-force", "error identifies the expected format")
}
