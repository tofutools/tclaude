package agentd_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// protectedTestDirs materializes the three protected roots inside the flow
// harness's per-test HOME. Every protected-path assertion below therefore
// operates on temporary state; production ~/.tclaude, ~/.claude and ~/.codex
// are never read or written by these tests.
func protectedTestDirs(t *testing.T) (tclaudeData, claudeSessions, codexHome string) {
	t.Helper()
	home := os.Getenv("HOME")
	require.NotEmpty(t, home, "the flow harness must provide an isolated HOME")
	require.NotEqual(t, "/", home)
	tclaudeData = filepath.Join(home, ".tclaude", "data")
	claudeSessions = filepath.Join(home, ".claude", "sessions")
	codexHome = filepath.Join(home, ".codex")
	for _, dir := range []string{tclaudeData, claudeSessions, codexHome} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}
	canonicalHome, err := filepath.EvalSymlinks(home)
	require.NoError(t, err)
	return filepath.Join(canonicalHome, ".tclaude", "data"),
		filepath.Join(canonicalHome, ".claude", "sessions"),
		filepath.Join(canonicalHome, ".codex")
}

// The compatibility floor: a profile that sets neither new field must behave
// and serialize exactly as before, with no acknowledgement demanded anywhere.
func TestSandboxProfileWithoutNewFieldsNeedsNoAcknowledgement(t *testing.T) {
	f := newFlow(t)
	home := os.Getenv("HOME")
	workspace := filepath.Join(home, "workspace")
	require.NoError(t, os.MkdirAll(workspace, 0o755))
	_, err := db.CreateAgentGroup("crew", "")
	require.NoError(t, err)

	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name":       "ordinary",
		"filesystem": []map[string]any{{"path": workspace, "access": "write"}},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	rec = profileReq(t, f, http.MethodGet, "/v1/sandbox-profiles/ordinary", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "read_baseline")
	assert.NotContains(t, rec.Body.String(), "break_glass_filesystem")

	// Both assignment surfaces accept it with no acknowledgement.
	rec = profileReq(t, f, http.MethodPut, "/v1/sandbox-profile-default", map[string]any{"name": "ordinary"})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	rec = profileReq(t, f, http.MethodPut, "/v1/groups/crew/sandbox-profile", map[string]any{"name": "ordinary"})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
}

// Ordinary filesystem rules must keep rejecting protected paths, so break-glass
// really is the only representation that can reach them.
func TestOrdinaryFilesystemRuleStillRejectsProtectedPath(t *testing.T) {
	f := newFlow(t)
	tclaudeData, claudeSessions, codexHome := protectedTestDirs(t)

	for _, path := range []string{tclaudeData, claudeSessions, codexHome} {
		rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
			"name":       "sneaky",
			"filesystem": []map[string]any{{"path": path, "access": "read"}},
		})
		require.Equalf(t, http.StatusBadRequest, rec.Code, "path %s body=%s", path, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "intersects protected directory")
	}
}

func TestBreakGlassProfileRequiresAcknowledgementAtEverySurface(t *testing.T) {
	f := newFlow(t)
	tclaudeData, _, _ := protectedTestDirs(t)
	_, err := db.CreateAgentGroup("crew", "")
	require.NoError(t, err)

	payload := map[string]any{
		"name":                   "debug-tclaude",
		"filesystem":             []map[string]any{},
		"break_glass_filesystem": []map[string]any{{"path": tclaudeData, "access": "read"}},
	}

	// CREATE without acknowledgement is refused with the typed code, and the
	// message names the exact path and access so the operator can judge it.
	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", payload)
	require.Equalf(t, http.StatusUnprocessableEntity, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "break_glass_acknowledgement_required")
	assert.Contains(t, rec.Body.String(), tclaudeData)
	assert.Contains(t, rec.Body.String(), "read")

	// A dry-run preview stays ack-free so the editor can render the
	// canonicalized rules in its confirmation dialog.
	rec = profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles?dry_run=1", payload)
	require.Equalf(t, http.StatusOK, rec.Code, "preview must not require the ack; body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "break_glass_filesystem")

	// Nothing was persisted by either call.
	rec = profileReq(t, f, http.MethodGet, "/v1/sandbox-profiles/debug-tclaude", nil)
	require.Equal(t, http.StatusNotFound, rec.Code)

	// With the acknowledgement the create succeeds.
	acked := map[string]any{}
	for k, v := range payload {
		acked[k] = v
	}
	acked["break_glass_acknowledged"] = true
	rec = profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", acked)
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	// The acknowledgement is TRANSIENT: it is never stored or echoed back, so
	// the durable danger marker is the break-glass field alone.
	rec = profileReq(t, f, http.MethodGet, "/v1/sandbox-profiles/debug-tclaude", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "break_glass_filesystem")
	assert.NotContains(t, rec.Body.String(), "break_glass_acknowledged")

	// EDIT re-demands it.
	rec = profileReq(t, f, http.MethodPatch, "/v1/sandbox-profiles/debug-tclaude", payload)
	require.Equalf(t, http.StatusUnprocessableEntity, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "break_glass_acknowledgement_required")
	rec = profileReq(t, f, http.MethodPatch, "/v1/sandbox-profiles/debug-tclaude", acked)
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	// GLOBAL assignment re-demands it, with the persistent-risk wording.
	rec = profileReq(t, f, http.MethodPut, "/v1/sandbox-profile-default", map[string]any{"name": "debug-tclaude"})
	require.Equalf(t, http.StatusUnprocessableEntity, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "break_glass_acknowledgement_required")
	assert.Contains(t, rec.Body.String(), "EVERY agent")
	rec = profileReq(t, f, http.MethodPut, "/v1/sandbox-profile-default",
		map[string]any{"name": "debug-tclaude", "break_glass_acknowledged": true})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	// GROUP assignment re-demands it too.
	rec = profileReq(t, f, http.MethodPut, "/v1/groups/crew/sandbox-profile", map[string]any{"name": "debug-tclaude"})
	require.Equalf(t, http.StatusUnprocessableEntity, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "EVERY agent")
	rec = profileReq(t, f, http.MethodPut, "/v1/groups/crew/sandbox-profile",
		map[string]any{"name": "debug-tclaude", "break_glass_acknowledged": true})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
}

// Composition must not let an include hide the danger: assigning a profile
// whose break-glass comes from an INCLUDED profile still demands the ack.
func TestIncludedBreakGlassStillRequiresAssignmentAcknowledgement(t *testing.T) {
	f := newFlow(t)
	tclaudeData, _, _ := protectedTestDirs(t)

	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name":                     "danger-base",
		"filesystem":               []map[string]any{},
		"break_glass_filesystem":   []map[string]any{{"path": tclaudeData, "access": "write"}},
		"break_glass_acknowledged": true,
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	// The wrapper itself declares no break-glass at all.
	rec = profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name": "innocent-looking", "filesystem": []map[string]any{}, "includes": []string{"danger-base"},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	rec = profileReq(t, f, http.MethodPut, "/v1/sandbox-profile-default", map[string]any{"name": "innocent-looking"})
	require.Equalf(t, http.StatusUnprocessableEntity, rec.Code,
		"an include must not be able to hide protected access from the assignment gate; body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), tclaudeData)
	assert.Contains(t, rec.Body.String(), "write")
}

// Export preserves the danger marker but never the acknowledgement, so an
// import on another machine must acknowledge again after canonicalization.
func TestBreakGlassExportImportRequiresFreshAcknowledgement(t *testing.T) {
	f := newFlow(t)
	tclaudeData, _, _ := protectedTestDirs(t)

	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name":                     "debug-tclaude",
		"filesystem":               []map[string]any{},
		"break_glass_filesystem":   []map[string]any{{"path": tclaudeData, "access": "read"}},
		"break_glass_acknowledged": true,
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	rec = profileReq(t, f, http.MethodGet, "/v1/sandbox-profiles/export", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var envelope map[string]any
	testharness.DecodeJSON(t, rec, &envelope)
	assert.EqualValues(t, 3, envelope["format_version"], "the new fields bump the portable format")
	assert.Contains(t, rec.Body.String(), "break_glass_filesystem")
	assert.NotContains(t, rec.Body.String(), "break_glass_acknowledged",
		"an export must never carry an acknowledgement to the next machine")

	// Simulate the receiving machine.
	_, err := db.DeleteSandboxProfile("debug-tclaude")
	require.NoError(t, err)

	// The ack-free preview still reports which profiles will demand one.
	rec = profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles/import/inspect", envelope)
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "break_glass_profiles")
	assert.Contains(t, rec.Body.String(), "debug-tclaude")

	rec = profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles/import", envelope)
	require.Equalf(t, http.StatusUnprocessableEntity, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "break_glass_acknowledgement_required")

	envelope["break_glass_acknowledged"] = true
	rec = profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles/import", envelope)
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	rec = profileReq(t, f, http.MethodGet, "/v1/sandbox-profiles/debug-tclaude", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), tclaudeData)
}

// An ordinary bundle must import exactly as before — no new acknowledgement,
// and older format versions stay readable.
func TestOrdinaryImportUnaffectedAndOlderFormatsStillAccepted(t *testing.T) {
	f := newFlow(t)
	home := os.Getenv("HOME")
	workspace := filepath.Join(home, "workspace")
	require.NoError(t, os.MkdirAll(workspace, 0o755))

	for _, version := range []int{1, 2, 3} {
		rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles/import", map[string]any{
			"format":         "tclaude-sandbox-profiles",
			"format_version": version,
			"on_conflict":    "overwrite",
			"profiles": []map[string]any{{
				"name":       "ordinary",
				"filesystem": []map[string]any{{"path": workspace, "access": "read"}},
			}},
		})
		require.Equalf(t, http.StatusOK, rec.Code, "format_version %d body=%s", version, rec.Body.String())
	}
}

// A minimal read baseline is ordinary (non-dangerous) configuration: it
// restricts rather than grants, so it must NOT demand an acknowledgement.
func TestMinimalReadBaselineNeedsNoAcknowledgement(t *testing.T) {
	f := newFlow(t)
	_, err := db.CreateAgentGroup("crew", "")
	require.NoError(t, err)

	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name": "strict", "filesystem": []map[string]any{}, "read_baseline": "minimal",
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	rec = profileReq(t, f, http.MethodGet, "/v1/sandbox-profiles/strict", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"read_baseline":"minimal"`)

	rec = profileReq(t, f, http.MethodPut, "/v1/sandbox-profile-default", map[string]any{"name": "strict"})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	// The explicit "default" spelling is accepted and normalized away.
	rec = profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name": "explicitly-default", "filesystem": []map[string]any{}, "read_baseline": "default",
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	rec = profileReq(t, f, http.MethodGet, "/v1/sandbox-profiles/explicitly-default", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "read_baseline")

	// An unknown value is rejected rather than silently ignored.
	rec = profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name": "bogus", "filesystem": []map[string]any{}, "read_baseline": "strict",
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// The launch surface: selecting a launch whose RESOLVED policy carries
// protected access is itself an acknowledgement point, and the gate keys off
// the composed snapshot (a group assignment introduces it without the spawner
// naming any profile).
func TestSpawnUnderBreakGlassGroupAssignmentRequiresAcknowledgement(t *testing.T) {
	f := newFlow(t)
	tclaudeData, _, _ := protectedTestDirs(t)
	f.HaveGroup("crew")

	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name":                     "debug-tclaude",
		"filesystem":               []map[string]any{},
		"break_glass_filesystem":   []map[string]any{{"path": tclaudeData, "access": "read"}},
		"break_glass_acknowledged": true,
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	rec = profileReq(t, f, http.MethodPut, "/v1/groups/crew/sandbox-profile",
		map[string]any{"name": "debug-tclaude", "break_glass_acknowledged": true})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	// No profile is named on the spawn at all — the ack is still required,
	// because the gate reads the RESOLVED policy.
	spawn := f.AsHuman().SpawnWith("crew", map[string]any{"name": "worker", "approval": "bypassPermissions"})
	require.Equalf(t, http.StatusUnprocessableEntity, spawn.Code, "body=%s", spawn.Raw)
	assert.Contains(t, string(spawn.Raw), "break_glass_acknowledgement_required")
	assert.Contains(t, string(spawn.Raw), tclaudeData)

	// Acknowledged, the launch proceeds to the harness capability gate. Claude
	// can only re-open its protected denies under sandbox `on`, so an inherit
	// launch is refused with the typed capability error rather than launching
	// without the access the operator just acknowledged.
	spawn = f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "worker", "approval": "bypassPermissions", "break_glass_acknowledged": true,
	})
	require.Equalf(t, http.StatusUnprocessableEntity, spawn.Code, "body=%s", spawn.Raw)
	assert.Contains(t, string(spawn.Raw), "unsupported_sandbox_profile_break_glass")

	// Under sandbox `on` the same acknowledged launch is accepted, and the
	// launch echo keeps the dangerous access and its exact source visible.
	spawn = f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "worker", "approval": "bypassPermissions",
		"break_glass_acknowledged": true, "sandbox": harness.ClaudeSandboxOn,
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "body=%s", spawn.Raw)
	var wire struct {
		Resolved *agent.ResolvedLaunch `json:"resolved"`
	}
	require.NoError(t, json.Unmarshal(spawn.Raw, &wire))
	require.NotNil(t, wire.Resolved)
	require.NotNil(t, wire.Resolved.SandboxPolicy)
	require.Len(t, wire.Resolved.SandboxPolicy.BreakGlass, 1)
	assert.Equal(t, tclaudeData, wire.Resolved.SandboxPolicy.BreakGlass[0].Path)
	assert.Equal(t, sandboxpolicy.AccessRead, wire.Resolved.SandboxPolicy.BreakGlass[0].Access)
	sources := wire.Resolved.SandboxPolicy.BreakGlassSources[tclaudeData]
	require.Len(t, sources, 1, "the resolved echo must name the exact source")
	assert.Equal(t, sandboxpolicy.ScopeGroup, sources[0].Scope)
	assert.Equal(t, "debug-tclaude", sources[0].Profile)
}

// Lineage: an agent may never introduce protected access its parent lacks,
// even when a human has assigned a break-glass profile to the group in the
// meantime. The child inherits the parent's recorded authority, not ambient
// policy.
func TestAgentSpawnCannotIntroduceBreakGlass(t *testing.T) {
	f := newFlow(t)
	tclaudeData, _, _ := protectedTestDirs(t)
	f.HaveGroup("crew")

	// A parent launched with no protected access at all.
	parent := f.AsHuman().SpawnWith("crew", map[string]any{"name": "parent", "approval": "bypassPermissions"})
	require.Equalf(t, http.StatusOK, parent.Code, "body=%s", parent.Raw)
	require.NoError(t, db.GrantAgentPermission(parent.ConvID, agentd.PermGroupsSpawn, "test"))

	// The human now assigns a break-glass profile to the whole group.
	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name":                     "debug-tclaude",
		"filesystem":               []map[string]any{},
		"break_glass_filesystem":   []map[string]any{{"path": tclaudeData, "access": "write"}},
		"break_glass_acknowledged": true,
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	rec = profileReq(t, f, http.MethodPut, "/v1/groups/crew/sandbox-profile",
		map[string]any{"name": "debug-tclaude", "break_glass_acknowledged": true})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	// The agent cannot pick that access up by spawning a child, and cannot
	// self-acknowledge its way past the lineage rule either.
	child := f.AsAgent(parent.ConvID).SpawnWith("crew", map[string]any{
		"name": "child", "approval": "default",
		"break_glass_acknowledged": true, "sandbox": harness.ClaudeSandboxOn,
	})
	require.Equalf(t, http.StatusForbidden, child.Code, "body=%s", child.Raw)
	assert.Contains(t, string(child.Raw), "sandbox_profile_restricted")
	assert.Contains(t, string(child.Raw), "not contained by the parent snapshot")
}

// A minimal read baseline is refused on Claude with a typed capability error
// rather than silently launching with today's broad read baseline.
func TestSpawnRejectsMinimalReadBaselineOnClaudeWithTypedError(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name": "strict", "filesystem": []map[string]any{}, "read_baseline": "minimal",
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "worker", "approval": "bypassPermissions", "sandbox_profile": "strict",
		"sandbox": harness.ClaudeSandboxOn,
	})
	require.Equalf(t, http.StatusUnprocessableEntity, spawn.Code, "body=%s", spawn.Raw)
	assert.Contains(t, string(spawn.Raw), "unsupported_sandbox_profile_read_baseline")
	assert.Contains(t, string(spawn.Raw), "denylist-shaped")
}
