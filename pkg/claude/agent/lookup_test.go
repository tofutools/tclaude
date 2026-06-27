package agent

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func setupTestDB(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	// Hide any inherited env that would resolve `.` to a real conv-id.
	t.Setenv("TCLAUDE_SESSION_ID", "")
	db.ResetForTest()
}

func upsertConvIndex(t *testing.T, convID, customTitle, summary, firstPrompt string) {
	t.Helper()
	// Materialise a placeholder .jsonl file at FullPath so that
	// conv.RefreshConvIndexEntry's "file-missing → drop cached row"
	// branch doesn't evict our test fixtures. The file's mtime is set
	// to the same value we record on the row so the freshness check
	// sees no rescan as needed.
	dir := filepath.Join(t.TempDir(), "proj")
	require.NoError(t, os.MkdirAll(dir, 0o755), "mkdir")
	fullPath := filepath.Join(dir, convID+".jsonl")
	require.NoError(t, os.WriteFile(fullPath, []byte(""), 0o600), "write fixture")
	mtime := time.Now().Unix()
	require.NoError(t, os.Chtimes(fullPath, time.Unix(mtime, 0), time.Unix(mtime, 0)), "chtimes")
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      convID,
		ProjectDir:  dir,
		FullPath:    fullPath,
		FileMtime:   mtime,
		CustomTitle: customTitle,
		Summary:     summary,
		FirstPrompt: firstPrompt,
		IndexedAt:   time.Now(),
	}), "UpsertConvIndex")
}

func TestResolveSelector_ByID(t *testing.T) {
	setupTestDB(t)
	upsertConvIndex(t, "11111111-2222-3333-4444-555555555555", "planner", "", "")

	r, _, err := resolveSelector("11111111-2222-3333-4444-555555555555")
	require.NoError(t, err, "resolveSelector")
	assert.Equal(t, "11111111-2222-3333-4444-555555555555", r.ConvID)
}

func TestResolveSelector_ByPrefix(t *testing.T) {
	setupTestDB(t)
	upsertConvIndex(t, "abcd1234-2222-3333-4444-555555555555", "planner", "", "")

	r, _, err := resolveSelector("abcd1234")
	require.NoError(t, err, "resolveSelector")
	assert.True(t, strings.HasPrefix(r.ConvID, "abcd1234"), "convID = %q", r.ConvID)
}

func TestResolveSelector_ByTitle(t *testing.T) {
	setupTestDB(t)
	upsertConvIndex(t, "11111111-2222-3333-4444-555555555555", "planner", "", "")
	upsertConvIndex(t, "22222222-2222-3333-4444-555555555555", "reviewer", "", "")

	r, _, err := resolveSelector("planner")
	require.NoError(t, err, "resolveSelector")
	assert.True(t, strings.HasPrefix(r.ConvID, "11111111"), "convID = %q", r.ConvID)
}

func TestResolveSelector_AmbiguousByTitle(t *testing.T) {
	setupTestDB(t)
	// Two convs whose first-prompt happens to match exactly.
	upsertConvIndex(t, "11111111-2222-3333-4444-555555555555", "", "", "shared")
	upsertConvIndex(t, "22222222-2222-3333-4444-555555555555", "", "", "shared")

	_, matches, err := resolveSelector("shared")
	require.True(t, errors.Is(err, errAmbiguous), "expected errAmbiguous, got %v", err)
	require.Len(t, matches, 2)
}

func TestResolveSelector_NotFound(t *testing.T) {
	setupTestDB(t)
	_, _, err := resolveSelector("nope-no-such-conv")
	require.Error(t, err, "expected error for missing selector")
}

// TestResolveSelector_ByGroupConvID covers the membership fallback: a
// conv that only lives in agent_group_members (no conv_index row, e.g.
// fresh from `agent spawn` before its /rename is scanned) is still
// findable by its full conv-id.
func TestResolveSelector_ByGroupConvID(t *testing.T) {
	setupTestDB(t)
	g, _ := db.CreateAgentGroup("alpha", "")
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: g, ConvID: "f6c6e261-deaf-bead-cafe-feedfacefeed",
		Role: "builder",
	}), "AddAgentGroupMember")

	r, _, err := resolveSelector("f6c6e261-deaf-bead-cafe-feedfacefeed")
	require.NoError(t, err, "resolveSelector")
	assert.Equal(t, "f6c6e261-deaf-bead-cafe-feedfacefeed", r.ConvID, "conv_id, want freshly-spawned uuid")
	assert.Nil(t, r.Row, "Row should be nil for non-indexed conv")
}

// TestResolveSelector_ByGroupConvPrefix covers the same fallback for
// the conv-id prefix path — `agent message f6c6e261 ...` should work
// even before the conv shows up in conv_index.
func TestResolveSelector_ByGroupConvPrefix(t *testing.T) {
	setupTestDB(t)
	g, _ := db.CreateAgentGroup("alpha", "")
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: g, ConvID: "f6c6e261-deaf-bead-cafe-feedfacefeed",
	}), "AddAgentGroupMember")

	r, _, err := resolveSelector("f6c6e261")
	require.NoError(t, err, "resolveSelector")
	assert.Equal(t, "f6c6e261-deaf-bead-cafe-feedfacefeed", r.ConvID, "conv_id, want freshly-spawned uuid")
}

// TestResolveSelector_PrefersConvIndexOverMembers: when both have
// the conv, conv_index hit short-circuits the chain so we still get
// the .Row populated.
func TestResolveSelector_PrefersConvIndexOverMembers(t *testing.T) {
	setupTestDB(t)
	convID := "abcd1234-2222-3333-4444-555555555555"
	upsertConvIndex(t, convID, "indexed-title", "", "")

	g, _ := db.CreateAgentGroup("alpha", "")
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: g, ConvID: convID,
	}))

	// A conv-index hit on the prefix returns the Row; we don't fall
	// through to the agent_group_members step.
	r, _, err := resolveSelector("abcd1234")
	require.NoError(t, err, "resolveSelector")
	assert.NotNil(t, r.Row, "expected conv_index row on prefix hit, got nil")
}

// TestFreshConvRowAt_ScansJSONLOnMissingRow covers the reincarnate
// fast-path: a freshly-spawned conv has no conv_index row yet, but the
// .jsonl on disk already has the custom-title written by `/rename`.
// FreshConvRow alone returns nil; FreshConvRowAt with the cwd derives
// the project path, scans the .jsonl, and produces the row.
//
// Regression: without this path, back-to-back reincarnations produced
// names like `reincarnate-1` instead of `<parent>-reincarnate-N`
// because prevTitle silently resolved to "".
func TestFreshConvRowAt_ScansJSONLOnMissingRow(t *testing.T) {
	setupTestDB(t)
	home, err := os.UserHomeDir()
	require.NoError(t, err, "UserHomeDir")
	cwd := "/home/u/myproj"
	convID := "12345678-1234-1234-1234-123456789012"

	projectDir := filepath.Join(home, ".claude", "projects", "-home-u-myproj")
	require.NoError(t, os.MkdirAll(projectDir, 0o755), "mkdir project")
	fixture := strings.Join([]string{
		`{"type":"user","sessionId":"` + convID + `","timestamp":"2026-05-10T01:00:00Z","cwd":"/home/u/myproj","message":{"role":"user","content":"hi"}}`,
		`{"type":"custom-title","customTitle":"my-agent","sessionId":"` + convID + `"}`,
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, convID+".jsonl"), []byte(fixture), 0o600), "write jsonl")

	row := FreshConvRowAt(convID, cwd)
	require.NotNil(t, row, "FreshConvRowAt returned nil; expected row from .jsonl scan")
	assert.Equal(t, "my-agent", row.CustomTitle)
}

// TestFreshConvRowAt_CacheHitWinsOverDiskWalk: once the row is in the
// cache, the cwd-derived disk walk is short-circuited entirely. We can
// verify by passing a bogus cwd — if FreshConvRowAt walked the disk it
// would scan nothing and overwrite our cached row.
func TestFreshConvRowAt_CacheHitWinsOverDiskWalk(t *testing.T) {
	setupTestDB(t)
	convID := "abcd1234-2222-3333-4444-555555555555"
	upsertConvIndex(t, convID, "cached", "", "")

	row := FreshConvRowAt(convID, "/no/such/dir")
	require.NotNil(t, row, "FreshConvRowAt returned nil for cached row")
	assert.Equal(t, "cached", row.CustomTitle, "disk walk should not have fired")
}

// TestFreshConvRowAt_EmptyCwdReturnsNil: with neither a cached row nor
// a cwd we have no way to find the .jsonl, so we return nil — the
// reincarnate caller will fall back to the prefix-less title.
func TestFreshConvRowAt_EmptyCwdReturnsNil(t *testing.T) {
	setupTestDB(t)
	row := FreshConvRowAt("11111111-1111-1111-1111-111111111111", "")
	assert.Nil(t, row, "expected nil with empty cwd + no cache")
}

// TestFreshConvRowAt_NoFileNoRow: known cwd but the .jsonl doesn't
// exist there → ScanAndUpsertFile is a no-op and we return nil.
func TestFreshConvRowAt_NoFileNoRow(t *testing.T) {
	setupTestDB(t)
	row := FreshConvRowAt("99999999-9999-9999-9999-999999999999", "/home/u/empty")
	assert.Nil(t, row, "expected nil for missing conv + missing file")
}

// TestFreshConvRowResolved_UsesSessionRowCwd covers the dashboard
// "(unknown)" bug: a freshly-spawned conv has no conv_index row yet,
// the caller doesn't know cwd directly, but a session row exists with
// the right cwd. FreshConvRowResolved looks up the session row and
// uses its cwd to find the .jsonl.
//
// Regression: without this resolver, the dashboard showed
// `(unknown)` for the active session right after a reincarnation —
// same root cause as the prevTitle="" reincarnate-prefix bug, just
// surfaced in the dashboard rather than the rename flow.
func TestFreshConvRowResolved_UsesSessionRowCwd(t *testing.T) {
	setupTestDB(t)
	home, err := os.UserHomeDir()
	require.NoError(t, err, "UserHomeDir")
	cwd := "/home/u/myproj"
	convID := "22222222-3333-4444-5555-666666666666"

	projectDir := filepath.Join(home, ".claude", "projects", "-home-u-myproj")
	require.NoError(t, os.MkdirAll(projectDir, 0o755), "mkdir project")
	fixture := strings.Join([]string{
		`{"type":"user","sessionId":"` + convID + `","timestamp":"2026-05-10T01:00:00Z","cwd":"/home/u/myproj","message":{"role":"user","content":"hi"}}`,
		`{"type":"custom-title","customTitle":"my-resolved-agent","sessionId":"` + convID + `"}`,
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, convID+".jsonl"), []byte(fixture), 0o600), "write jsonl")

	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:        "spwn-deadbe",
		ConvID:    convID,
		Cwd:       cwd,
		Status:    "idle",
		CreatedAt: time.Now(),
	}), "SaveSession")

	row := FreshConvRowResolved(convID)
	require.NotNil(t, row, "FreshConvRowResolved returned nil; expected row via session-row cwd fallback")
	assert.Equal(t, "my-resolved-agent", row.CustomTitle)
}

// TestFreshConvRowResolved_NoSessionRow: conv has no session row → no
// way to know cwd → returns nil. Same shape as the empty-cwd FreshConvRowAt
// case, just routed through the resolver.
func TestFreshConvRowResolved_NoSessionRow(t *testing.T) {
	setupTestDB(t)
	row := FreshConvRowResolved("77777777-8888-9999-aaaa-bbbbbbbbbbbb")
	assert.Nil(t, row, "expected nil with no session row + no cache")
}

// TestCachedTitle_CustomTitleFromCache: CachedTitle reads the custom
// title straight from the conv_index cache — FreshTitle's cache-only
// twin, no .jsonl rescan.
func TestCachedTitle_CustomTitleFromCache(t *testing.T) {
	setupTestDB(t)
	const convID = "11111111-2222-3333-4444-555555555555"
	upsertConvIndex(t, convID, "renamed-agent", "", "")
	assert.Equal(t, "renamed-agent", CachedTitle(convID))
}

// TestCachedTitle_SummaryFallback: with no custom title and no pending
// name, CachedTitle falls through to the cached summary — same priority
// chain as FreshTitle (custom title > pending name > summary > first
// prompt).
func TestCachedTitle_SummaryFallback(t *testing.T) {
	setupTestDB(t)
	const convID = "11111111-2222-3333-4444-555555555555"
	upsertConvIndex(t, convID, "", "a summary line", "")
	assert.Equal(t, "a summary line", CachedTitle(convID))
}

// TestCachedTitle_PendingNameWhenNotIndexed covers the freshly-spawned
// agent: registered with its intended --name, but its .jsonl is not yet
// in conv_index. CachedTitle has no row to read, so it must fall back
// to the actor's pending name rather than returning "(unknown)".
// This is the path that keeps the dashboard from blanking a just-
// spawned agent in the gap before its first index event lands.
func TestCachedTitle_PendingNameWhenNotIndexed(t *testing.T) {
	setupTestDB(t)
	const convID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	require.NoError(t, db.SetAgentPendingName(agentID, "spawned-worker"))

	assert.Equal(t, "spawned-worker", CachedTitle(convID),
		"with no conv_index row, CachedTitle must fall back to the actor's pending name")
}

// TestCachedTitle_UnknownWhenUnresolvable: no cached row and no
// actor → the UnknownTitle placeholder, exactly as FreshTitle.
func TestCachedTitle_UnknownWhenUnresolvable(t *testing.T) {
	setupTestDB(t)
	assert.Equal(t, UnknownTitle, CachedTitle("00000000-0000-0000-0000-000000000000"))
}

// TestResolveSelector_FollowsSuccession: typing the original conv-id
// after a reincarnate redirects to the new conv-id. Without this, CLI
// commands like `tclaude agent message <old-id>` would silently target
// a dead pane.
func TestResolveSelector_FollowsSuccession(t *testing.T) {
	setupTestDB(t)
	const oldID = "11111111-aaaa-bbbb-cccc-111111111111"
	const newID = "22222222-aaaa-bbbb-cccc-222222222222"
	upsertConvIndex(t, oldID, "old-name", "", "")
	upsertConvIndex(t, newID, "new-name", "", "")

	// Record the succession edge old → new.
	require.NoError(t, db.RecordConvSuccession(oldID, newID, "reincarnate"), "RecordConvSuccession")

	// Resolving the old ID should redirect to the new one + carry the
	// new conv-index row (so display titles reflect the live conv).
	r, _, err := resolveSelector(oldID)
	require.NoError(t, err, "resolveSelector")
	require.NotNil(t, r, "expected a resolved match, got nil")
	assert.Equal(t, newID, r.ConvID, "succession redirect missed")
	if assert.NotNil(t, r.Row, "Row.CustomTitle should reflect the new conv") {
		assert.Equal(t, "new-name", r.Row.CustomTitle, "Row.CustomTitle should reflect the new conv")
	}
}

// TestResolveSelector_NoSuccession_LeavesAsIs: with no succession row
// the resolver returns the conv unchanged. Sanity check that
// redirectResolvedToLatest doesn't molest "current" convs.
func TestResolveSelector_NoSuccession_LeavesAsIs(t *testing.T) {
	setupTestDB(t)
	const id = "33333333-aaaa-bbbb-cccc-333333333333"
	upsertConvIndex(t, id, "self", "", "")
	r, _, err := resolveSelector(id)
	require.NoError(t, err, "resolveSelector")
	require.NotNil(t, r)
	assert.Equal(t, id, r.ConvID)
}

// TestResolveSelector_ByAgentID: the stable agent_id resolves to the
// actor's current conversation generation — the canonical, rotation-immune
// handle this work makes a first-class selector.
func TestResolveSelector_ByAgentID(t *testing.T) {
	setupTestDB(t)
	const conv = "aaaa1111-2222-3333-4444-555555555555"
	upsertConvIndex(t, conv, "worker", "", "")
	agentID, _, err := db.EnsureAgentForConv(conv, "spawn")
	require.NoError(t, err, "EnsureAgentForConv")

	r, _, err := resolveSelector(agentID)
	require.NoError(t, err, "resolveSelector by agent_id")
	require.NotNil(t, r)
	assert.Equal(t, conv, r.ConvID, "agent_id should resolve to current_conv_id")
}

// TestResolveSelector_ByAgentIDPrefix: a shortened agent_id (the typing
// convenience) resolves the same way when it's unique.
func TestResolveSelector_ByAgentIDPrefix(t *testing.T) {
	setupTestDB(t)
	const conv = "aaaa1111-2222-3333-4444-555555555555"
	upsertConvIndex(t, conv, "worker", "", "")
	agentID, _, err := db.EnsureAgentForConv(conv, "spawn")
	require.NoError(t, err, "EnsureAgentForConv")

	// agent_id is "agt_" + 32 hex; a short unique prefix must still resolve.
	r, _, err := resolveSelector(agentID[:12])
	require.NoError(t, err, "resolveSelector by agent_id prefix")
	require.NotNil(t, r)
	assert.Equal(t, conv, r.ConvID)
}

// TestResolveSelector_AgentIDAmbiguous: the bare "agt_" tag prefixes every
// actor, so with two agents it surfaces as an ambiguity (candidates, no
// silent pick).
func TestResolveSelector_AgentIDAmbiguous(t *testing.T) {
	setupTestDB(t)
	upsertConvIndex(t, "aaaa1111-2222-3333-4444-555555555555", "a", "", "")
	upsertConvIndex(t, "bbbb2222-2222-3333-4444-555555555555", "b", "", "")
	_, _, errA := db.EnsureAgentForConv("aaaa1111-2222-3333-4444-555555555555", "spawn")
	require.NoError(t, errA)
	_, _, errB := db.EnsureAgentForConv("bbbb2222-2222-3333-4444-555555555555", "spawn")
	require.NoError(t, errB)

	_, matches, err := resolveSelector(db.AgentIDPrefix)
	require.True(t, errors.Is(err, errAmbiguous), "expected errAmbiguous, got %v", err)
	require.Len(t, matches, 2)
}

// TestResolveSelector_AgentIDNotFound: an `agt_`-tagged selector with no
// matching actor is reported against the agent layer (terminal
// errNoAgentMatch), not silently passed through to the conv/title steps —
// and it must NOT pay for the ~/.claude/projects rescan, since a rescan
// can never make a missing agent appear.
func TestResolveSelector_AgentIDNotFound(t *testing.T) {
	setupTestDB(t)
	prev := refreshAllProjects
	refreshed := 0
	refreshAllProjects = func() { refreshed++ }
	t.Cleanup(func() { refreshAllProjects = prev })

	_, _, err := resolveSelector("agt_0000000000000000000000000000beef")
	require.Error(t, err, "expected error for missing agent_id")
	assert.True(t, errors.Is(err, errNoAgentMatch), "want errNoAgentMatch, got %v", err)
	assert.Equal(t, 0, refreshed, "an agent miss must skip the project rescan")
}

// TestResolveSelector_ConvMissRefreshes is the contrast to the agent-miss
// case: a plain conv/title miss is a genuine cache miss, so the resolver
// still rescans-and-retries before giving up.
func TestResolveSelector_ConvMissRefreshes(t *testing.T) {
	setupTestDB(t)
	prev := refreshAllProjects
	refreshed := 0
	refreshAllProjects = func() { refreshed++ }
	t.Cleanup(func() { refreshAllProjects = prev })

	_, _, err := resolveSelector("nope-no-such-conv")
	require.Error(t, err, "expected error for missing conv")
	assert.Equal(t, 1, refreshed, "a conv/title miss must trigger the project rescan")
}

// TestResolveSelector_AgentIDSurvivesRotation is the headline guarantee: a
// reincarnation rotates the conv-id, but the SAME agent_id keeps resolving
// to the live generation. This is the exact case where a conv-id selector
// would land on a dead pane.
func TestResolveSelector_AgentIDSurvivesRotation(t *testing.T) {
	setupTestDB(t)
	const convOld = "11111111-aaaa-bbbb-cccc-111111111111"
	const convNew = "22222222-aaaa-bbbb-cccc-222222222222"
	upsertConvIndex(t, convOld, "worker", "", "")
	upsertConvIndex(t, convNew, "worker-r-1", "", "")

	agentID, _, err := db.EnsureAgentForConv(convOld, "spawn")
	require.NoError(t, err, "EnsureAgentForConv")

	// Reincarnate: link the fresh generation, then advance the live pointer.
	require.NoError(t, db.LinkConvToAgent(convNew, agentID, db.ConvRoleHead, "reincarnate"), "LinkConvToAgent")
	moved, err := db.SetAgentCurrentConv(agentID, convOld, convNew)
	require.NoError(t, err, "SetAgentCurrentConv")
	require.True(t, moved, "live pointer should have advanced")

	r, _, err := resolveSelector(agentID)
	require.NoError(t, err, "resolveSelector by agent_id after rotation")
	require.NotNil(t, r)
	assert.Equal(t, convNew, r.ConvID, "agent_id must follow the rotation to the live head")
}

// TestResolveSelector_ByAgentID_Retired pins the intended behaviour that a
// retired actor is still addressable by its stable agent_id (resolving to its
// last generation) — a stable id keeps referencing an agent even after it is
// retired; display surfaces flag the retired state separately.
func TestResolveSelector_ByAgentID_Retired(t *testing.T) {
	setupTestDB(t)
	const conv = "aaaa1111-2222-3333-4444-555555555555"
	upsertConvIndex(t, conv, "worker", "", "")
	agentID, _, err := db.EnsureAgentForConv(conv, "spawn")
	require.NoError(t, err, "EnsureAgentForConv")

	retired, err := db.RetireAgentByID(agentID, "operator", "done")
	require.NoError(t, err, "RetireAgentByID")
	require.True(t, retired, "agent should have been retired")

	r, _, err := resolveSelector(agentID)
	require.NoError(t, err, "a retired agent must still resolve by its stable id")
	require.NotNil(t, r)
	assert.Equal(t, conv, r.ConvID)
}

func TestRunLookup(t *testing.T) {
	setupTestDB(t)
	upsertConvIndex(t, "abcd1234-2222-3333-4444-555555555555", "planner", "", "")

	var stdout, stderr bytes.Buffer
	rc := runLookupDirect(&lookupParams{Selector: "planner"}, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "runLookup rc = %d, stderr = %s", rc, stderr.String())
	assert.Contains(t, stdout.String(), "abcd1234", "expected stdout to contain conv id")
}

func TestRunWhoami_HumanFallback(t *testing.T) {
	setupTestDB(t)
	// Force findClaudePID to report no CC ancestor — the actual process tree
	// may include one (e.g. when `go test` is run from inside Claude Code),
	// but the test premise is "human shell, no ancestor".
	prev := findClaudePID
	findClaudePID = func() int { return 0 }
	t.Cleanup(func() { findClaudePID = prev })
	var stdout, stderr bytes.Buffer
	rc := runWhoamiDirect(&stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr = %q", stderr.String())
	assert.Contains(t, stdout.String(), HumanIdentity)
}

func TestRunWhoami_KnownConv(t *testing.T) {
	setupTestDB(t)
	upsertConvIndex(t, "abcd1234-2222-3333-4444-555555555555", "planner", "", "")
	t.Setenv("TCLAUDE_SESSION_ID", "abcd1234-2222-3333-4444-555555555555")

	var stdout, stderr bytes.Buffer
	rc := runWhoamiDirect(&stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr = %q", stderr.String())
	out := stdout.String()
	assert.Contains(t, out, "abcd1234")
	assert.Contains(t, out, "planner")
}

func TestRunLookup_Ambiguous(t *testing.T) {
	setupTestDB(t)
	upsertConvIndex(t, "11111111-2222-3333-4444-555555555555", "", "", "dup")
	upsertConvIndex(t, "22222222-2222-3333-4444-555555555555", "", "", "dup")

	var stdout, stderr bytes.Buffer
	rc := runLookupDirect(&lookupParams{Selector: "dup"}, &stdout, &stderr)
	require.Equal(t, rcAmbiguous, rc, "runLookup rc")
	assert.Contains(t, stderr.String(), "matches 2 conversations")
}
