package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// This file replaces sandbox_break_glass_flow_test.go. TCL-791 removed
// break-glass, so what has to be proven through the real mux changed shape:
// not "the acknowledgement is demanded everywhere" but "the field is refused
// everywhere, loudly, and nothing else regressed while it was torn out".
//
// The three things under test here:
//
//  1. Every INPUT surface refuses a payload still carrying break_glass_*.
//     The endpoints decode without DisallowUnknownFields, so silence would
//     mean a caller is told their profile saved when it is not the profile
//     they sent — the exact failure this ticket exists to prevent.
//  2. The protected-root invariant is absolute at the wire: an ordinary rule
//     naming a protected root is still refused, and there is no longer any
//     second representation that reaches one.
//  3. Everything that merely lived next to break-glass still works —
//     reopen-under-deny, current-policy relaunches, older import formats.

// protectedTestDirs materializes the protected roots and an ordinary ~/.codex
// tree inside the flow harness's per-test HOME. Every assertion therefore
// operates on temporary state; production harness state is never touched.
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

// The compatibility floor: a profile that sets none of the removed fields
// behaves and serializes exactly as before.
func TestOrdinarySandboxProfileIsUnaffectedByTheRemoval(t *testing.T) {
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
	assert.NotContains(t, rec.Body.String(), "break_glass")

	rec = profileReq(t, f, http.MethodPut, "/v1/sandbox-profile-default", map[string]any{"name": "ordinary"})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	rec = profileReq(t, f, http.MethodPut, "/v1/groups/crew/sandbox-profile", map[string]any{"name": "ordinary"})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
}

// The invariant anchor at the wire. This used to be half the story — the other
// half being that break-glass was the sanctioned way through. There is no
// other half now.
func TestOrdinaryFilesystemRuleStillRejectsProtectedPath(t *testing.T) {
	f := newFlow(t)
	tclaudeData, claudeSessions, codexHome := protectedTestDirs(t)

	for _, path := range []string{tclaudeData, claudeSessions} {
		rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
			"name":       "sneaky",
			"filesystem": []map[string]any{{"path": path, "access": "read"}},
		})
		require.Equalf(t, http.StatusBadRequest, rec.Code, "path %s body=%s", path, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "intersects protected directory")
	}
	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name":       "codex-runtime",
		"filesystem": []map[string]any{{"path": codexHome, "access": "read"}},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "~/.codex should be an ordinary readable root: %s", rec.Body.String())
}

// Every input surface that used to demand an acknowledgement now refuses the
// field outright, with a code that does not send the caller looking for an
// acknowledgement that no longer exists.
func TestBreakGlassPayloadIsRefusedAtEverySurface(t *testing.T) {
	f := newFlow(t)
	tclaudeData, _, _ := protectedTestDirs(t)
	_, err := db.CreateAgentGroup("crew", "")
	require.NoError(t, err)

	carrier := map[string]any{
		"name":                   "debug-tclaude",
		"filesystem":             []map[string]any{},
		"break_glass_filesystem": []map[string]any{{"path": tclaudeData, "access": "read"}},
	}
	assertRefused := func(t *testing.T, rec *httptest.ResponseRecorder, what string) {
		t.Helper()
		require.Equalf(t, http.StatusUnprocessableEntity, rec.Code, "%s body=%s", what, rec.Body.String())
		assert.Containsf(t, rec.Body.String(), "break_glass_removed", "%s", what)
		assert.NotContainsf(t, rec.Body.String(), "break_glass_acknowledgement_required",
			"%s must not offer the retired acknowledgement path", what)
	}

	// CREATE, and the dry-run preview alongside it: a preview that quietly
	// dropped the field would render a profile the real create cannot produce.
	assertRefused(t, profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", carrier), "create")
	assertRefused(t, profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles?dry_run=1", carrier), "preview")

	// Nothing was persisted by either call.
	rec := profileReq(t, f, http.MethodGet, "/v1/sandbox-profiles/debug-tclaude", nil)
	require.Equal(t, http.StatusNotFound, rec.Code)

	// EDIT of an existing ordinary profile.
	rec = profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name": "debug-tclaude", "filesystem": []map[string]any{},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	assertRefused(t, profileReq(t, f, http.MethodPatch, "/v1/sandbox-profiles/debug-tclaude", carrier), "edit")
	stored, err := db.GetSandboxProfile("debug-tclaude")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Empty(t, stored.Filesystem, "the refused edit must not have been applied")

	// The scribe draft handoff. It never writes the registry, but the human is
	// shown the draft and asked to save it: handing them a silently stripped
	// profile would put the agent's name on a document it did not write.
	assertRefused(t, profileReq(t, f, http.MethodPost,
		"/v1/sandbox-profile-drafts/abcdefghijklmnop",
		map[string]any{"profile": carrier}), "draft submit")

	// Both assignment surfaces, whose only break-glass-shaped input was the
	// acknowledgement itself.
	assertRefused(t, profileReq(t, f, http.MethodPut, "/v1/sandbox-profile-default",
		map[string]any{"name": "debug-tclaude", "break_glass_acknowledged": true}), "global assignment")
	assertRefused(t, profileReq(t, f, http.MethodPut, "/v1/groups/crew/sandbox-profile",
		map[string]any{"name": "debug-tclaude", "break_glass_acknowledged": true}), "group assignment")

	// The acknowledgement alone, with no rules at all, is still refused: there
	// is nothing left to acknowledge, and accepting it would imply otherwise.
	assertRefused(t, profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name": "ack-only", "filesystem": []map[string]any{}, "break_glass_acknowledged": false,
	}), "acknowledgement-only create")
}

// An import bundle is hand-editable operator input, so the refusal names every
// offending profile at once rather than making the operator re-run the import
// to discover the next one.
func TestBreakGlassImportBundleIsRefusedAndNamesEveryCarrier(t *testing.T) {
	f := newFlow(t)
	tclaudeData, _, _ := protectedTestDirs(t)

	bundle := map[string]any{
		"format": "tclaude-sandbox-profiles", "format_version": 5, "on_conflict": "overwrite",
		"profiles": []map[string]any{
			{"name": "zeta", "filesystem": []map[string]any{},
				"break_glass_filesystem": []map[string]any{{"path": tclaudeData, "access": "read"}}},
			{"name": "harmless", "filesystem": []map[string]any{}},
			{"name": "alpha", "filesystem": []map[string]any{},
				"break_glass_filesystem": []map[string]any{{"path": tclaudeData, "access": "write"}}},
		},
	}
	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles/import", bundle)
	require.Equalf(t, http.StatusUnprocessableEntity, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "break_glass_removed")
	assert.Contains(t, rec.Body.String(), "alpha, zeta", "every carrier must be named, in a stable order")
	assert.NotContains(t, rec.Body.String(), "harmless")

	// The refusal is total.
	for _, name := range []string{"zeta", "harmless", "alpha"} {
		stored, err := db.GetSandboxProfile(name)
		require.NoError(t, err)
		assert.Nilf(t, stored, "a refused import must not write %s", name)
	}

	// The envelope-level acknowledgement is refused on its own too.
	rec = profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles/import", map[string]any{
		"format": "tclaude-sandbox-profiles", "format_version": 5, "on_conflict": "overwrite",
		"profiles":                 []map[string]any{{"name": "harmless", "filesystem": []map[string]any{}}},
		"break_glass_acknowledged": true,
	})
	require.Equalf(t, http.StatusUnprocessableEntity, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "break_glass_removed")
}

// An EMPTY break_glass_filesystem is absence, not a request for protected
// access. A client round-tripping an old export through a serializer that
// emits empty slices is asking for exactly the profile this ticket wants, and
// refusing it would be a removal that breaks working configurations.
func TestEmptyBreakGlassListIsTreatedAsAbsent(t *testing.T) {
	f := newFlow(t)
	protectedTestDirs(t)

	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name": "round-tripped", "filesystem": []map[string]any{},
		"break_glass_filesystem": []map[string]any{},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	rec = profileReq(t, f, http.MethodGet, "/v1/sandbox-profiles/round-tripped", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "break_glass")
}

// The two removals are handled DIFFERENTLY on purpose, and this is the test
// that pins the difference. read_baseline was a RESTRICTION: dropping it can
// only narrow, so a payload still carrying it is accepted with it ignored.
// break_glass was a GRANT: dropping it silently would hand back a profile that
// is not the one the operator wrote.
func TestRemovedRestrictionIsDroppedWhileRemovedGrantIsRefused(t *testing.T) {
	f := newFlow(t)
	tclaudeData, _, _ := protectedTestDirs(t)
	denied := t.TempDir()

	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name": "legacy-shape", "filesystem": []map[string]any{{"path": denied, "access": "deny"}},
		"read_baseline": "minimal", "read_baseline_exclusions": []string{"home.directory"},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "a removed restriction is dropped: %s", rec.Body.String())
	rec = profileReq(t, f, http.MethodGet, "/v1/sandbox-profiles/legacy-shape", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"access":"deny"`)
	assert.NotContains(t, rec.Body.String(), "read_baseline")
	assert.NotContains(t, rec.Body.String(), "home.directory")

	rec = profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name": "legacy-grant", "filesystem": []map[string]any{},
		"break_glass_filesystem": []map[string]any{{"path": tclaudeData, "access": "read"}},
	})
	require.Equalf(t, http.StatusUnprocessableEntity, rec.Code,
		"a removed grant is refused: %s", rec.Body.String())
}

// Export drops to a new format version, and every bundle version that never
// carried break-glass keeps importing byte-for-byte as before. The removal
// must not cost an operator their existing bundles.
func TestExportBumpsFormatAndEveryOlderBundleStillImports(t *testing.T) {
	f := newFlow(t)
	home := os.Getenv("HOME")
	workspace := filepath.Join(home, "workspace")
	require.NoError(t, os.MkdirAll(workspace, 0o755))

	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name": "ordinary", "filesystem": []map[string]any{{"path": workspace, "access": "read"}},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	rec = profileReq(t, f, http.MethodGet, "/v1/sandbox-profiles/export", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var envelope map[string]any
	testharness.DecodeJSON(t, rec, &envelope)
	assert.EqualValues(t, 9, envelope["format_version"],
		"compositional network pack references are a format change")
	assert.NotContains(t, rec.Body.String(), "break_glass")

	// The exported bundle must import back into itself.
	envelope["on_conflict"] = "overwrite"
	rec = profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles/import", envelope)
	require.Equalf(t, http.StatusOK, rec.Code, "self round-trip body=%s", rec.Body.String())

	for _, version := range []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10} {
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

// Reopen-under-deny (TCL-623) is a separate feature that happens to touch the
// same machinery, and it must survive untouched: an ordinary deny with a
// narrower reopen among unprotected paths is still refused on a Claude sandbox
// mode that cannot enforce it, and still accepted on one that can.
func TestSpawnRejectsReopenUnderDenyOnClaudeInheritWithTypedError(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	denied, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	workspace := filepath.Join(denied, "workspace")
	require.NoError(t, os.MkdirAll(workspace, 0o755))

	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name": "strict", "filesystem": []map[string]any{
			{"path": denied, "access": "deny"},
			{"path": workspace, "access": "read"},
		},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "worker", "approval": "bypassPermissions", "sandbox_profile": "strict",
		"sandbox": harness.ClaudeSandboxInherit,
	})
	require.Equalf(t, http.StatusUnprocessableEntity, spawn.Code, "body=%s", spawn.Raw)
	assert.Contains(t, string(spawn.Raw), "unsupported_sandbox_profile_reopen_under_deny")

	// Sandbox "on" is the mode that CAN enforce it.
	ok := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "worker-on", "approval": "bypassPermissions", "sandbox_profile": "strict",
		"sandbox": harness.ClaudeSandboxOn,
	})
	require.Equalf(t, http.StatusOK, ok.Code, "body=%s", ok.Raw)
}

// A spawn payload is an input surface too, and it was the one place an agent
// could try to acknowledge on its own behalf.
func TestSpawnCarryingBreakGlassAcknowledgementIsRefused(t *testing.T) {
	f := newFlow(t)
	protectedTestDirs(t)
	f.HaveGroup("crew")

	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "worker", "approval": "bypassPermissions",
		"break_glass_acknowledged": true, "sandbox": harness.ClaudeSandboxOn,
	})
	require.Equalf(t, http.StatusUnprocessableEntity, spawn.Code, "body=%s", spawn.Raw)
	assert.Contains(t, string(spawn.Raw), "break_glass_removed")
}

// A profile edit followed by stop/resume is an operator-authored policy
// change, including when the edit removes a deny.
func TestResumeAppliesDroppedDenyRow(t *testing.T) {
	f := newFlow(t)
	protectedTestDirs(t)
	f.HaveGroup("crew")
	denied, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)

	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name": "group-policy", "filesystem": []map[string]any{{"path": denied, "access": "deny"}},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	rec = profileReq(t, f, http.MethodPut, "/v1/groups/crew/sandbox-profile", map[string]any{"name": "group-policy"})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "worker", "approval": "never",
		"harness": harness.CodexName, "sandbox": harness.SandboxManagedProfile,
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "body=%s", spawn.Raw)
	launched, err := db.AgentEffectiveSandboxConfigForConv(spawn.ConvID)
	require.NoError(t, err)
	require.NotNil(t, launched)
	require.Contains(t, launched.Effective.Filesystem,
		sandboxpolicy.FilesystemGrant{Path: denied, Access: sandboxpolicy.AccessDeny})

	// Widen the profile by removing the deny entirely.
	rec = profileReq(t, f, http.MethodPatch, "/v1/sandbox-profiles/group-policy", map[string]any{
		"name": "group-policy", "filesystem": []map[string]any{},
	})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	f.MarkOffline(spawn.TmuxSession)
	resumed := f.AsHuman().Resume(spawn.ConvID)
	require.Equalf(t, http.StatusOK, resumed.Code, "resume body=%s", resumed.Raw)

	after, err := db.AgentEffectiveSandboxConfigForConv(spawn.ConvID)
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.NotContains(t, after.Effective.Filesystem,
		sandboxpolicy.FilesystemGrant{Path: denied, Access: sandboxpolicy.AccessDeny})
}

// The same boundary on reincarnation, which is a relaunch through a different
// endpoint and therefore a separate wiring.
func TestSelfReincarnateAppliesDroppedDenyRow(t *testing.T) {
	f := newFlow(t)
	protectedTestDirs(t)
	f.HaveGroup("crew")
	denied, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)

	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name": "grp", "filesystem": []map[string]any{{"path": denied, "access": "deny"}},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	rec = profileReq(t, f, http.MethodPut, "/v1/groups/crew/sandbox-profile", map[string]any{"name": "grp"})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "worker", "approval": "never",
		"harness": harness.CodexName, "sandbox": harness.SandboxManagedProfile,
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "body=%s", spawn.Raw)

	rec = profileReq(t, f, http.MethodPatch, "/v1/sandbox-profiles/grp", map[string]any{
		"name": "grp", "filesystem": []map[string]any{},
	})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	newConv := selfReincarnate(t, f, spawn.ConvID)
	after, err := db.AgentEffectiveSandboxConfigForConv(newConv)
	require.NoError(t, err)
	require.NotNil(t, after, "the real self endpoint must persist an exact successor snapshot")
	assert.NotContains(t, after.Effective.Filesystem,
		sandboxpolicy.FilesystemGrant{Path: denied, Access: sandboxpolicy.AccessDeny})
}

// An import whose graph is invalid must still report the graph error rather
// than anything break-glass-shaped, and must write nothing.
func TestImportDanglingIncludeStillReportsTheGraphError(t *testing.T) {
	f := newFlow(t)
	protectedTestDirs(t)

	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles/import", map[string]any{
		"format": "tclaude-sandbox-profiles", "format_version": 3,
		"profiles": []map[string]any{{"name": "orphan", "includes": []string{"missing"}}},
	})
	require.Equalf(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "invalid sandbox profile import")
	assert.Contains(t, rec.Body.String(), `included sandbox profile \"missing\" was not found`)
	stored, err := db.GetSandboxProfile("orphan")
	require.NoError(t, err)
	assert.Nil(t, stored)
}

// selfReincarnate drives the real self-reincarnation endpoint and returns the
// successor conversation id. It lives here because this file owns the
// relaunch-lineage flow coverage.
func selfReincarnate(t *testing.T, f *testharness.Flow, convID string) string {
	t.Helper()
	rec := agentReq(t, f, convID, http.MethodPost, "/v1/whoami/reincarnate",
		map[string]any{"follow_up": "carry on"})
	require.Equalf(t, http.StatusOK, rec.Code, "self-reincarnate body=%s", rec.Body.String())
	var response struct {
		NewConv string `json:"new_conv"`
	}
	testharness.DecodeJSON(t, rec, &response)
	require.NotEmpty(t, response.NewConv)
	return response.NewConv
}

// An assignment the transaction skips applies no authority, and the skip is
// still reported rather than swallowed.
func TestImportSkipsNonexistentGroup(t *testing.T) {
	f := newFlow(t)
	protectedTestDirs(t)
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "ordinary"})
	require.NoError(t, err)

	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles/import", map[string]any{
		"format": "tclaude-sandbox-profiles", "format_version": 3,
		"apply_assignments": true,
		"profiles":          []map[string]any{},
		"assignments": map[string]any{
			"groups": map[string]string{"does-not-exist": "ordinary"},
		},
	})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), `group assignment skipped: no group \"does-not-exist\"`)
}
