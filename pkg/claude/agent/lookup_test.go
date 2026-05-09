package agent

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	fullPath := filepath.Join(dir, convID+".jsonl")
	if err := os.WriteFile(fullPath, []byte(""), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	mtime := time.Now().Unix()
	if err := os.Chtimes(fullPath, time.Unix(mtime, 0), time.Unix(mtime, 0)); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if err := db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      convID,
		ProjectDir:  dir,
		FullPath:    fullPath,
		FileMtime:   mtime,
		CustomTitle: customTitle,
		Summary:     summary,
		FirstPrompt: firstPrompt,
		IndexedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("UpsertConvIndex: %v", err)
	}
}

func TestResolveSelector_ByID(t *testing.T) {
	setupTestDB(t)
	upsertConvIndex(t, "11111111-2222-3333-4444-555555555555", "planner", "", "")

	r, _, err := resolveSelector("11111111-2222-3333-4444-555555555555")
	if err != nil {
		t.Fatalf("resolveSelector: %v", err)
	}
	if r.ConvID != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("convID = %q", r.ConvID)
	}
}

func TestResolveSelector_ByPrefix(t *testing.T) {
	setupTestDB(t)
	upsertConvIndex(t, "abcd1234-2222-3333-4444-555555555555", "planner", "", "")

	r, _, err := resolveSelector("abcd1234")
	if err != nil {
		t.Fatalf("resolveSelector: %v", err)
	}
	if !strings.HasPrefix(r.ConvID, "abcd1234") {
		t.Fatalf("convID = %q", r.ConvID)
	}
}

func TestResolveSelector_ByTitle(t *testing.T) {
	setupTestDB(t)
	upsertConvIndex(t, "11111111-2222-3333-4444-555555555555", "planner", "", "")
	upsertConvIndex(t, "22222222-2222-3333-4444-555555555555", "reviewer", "", "")

	r, _, err := resolveSelector("planner")
	if err != nil {
		t.Fatalf("resolveSelector: %v", err)
	}
	if !strings.HasPrefix(r.ConvID, "11111111") {
		t.Fatalf("convID = %q", r.ConvID)
	}
}

func TestResolveSelector_AmbiguousByTitle(t *testing.T) {
	setupTestDB(t)
	// Two convs whose first-prompt happens to match exactly.
	upsertConvIndex(t, "11111111-2222-3333-4444-555555555555", "", "", "shared")
	upsertConvIndex(t, "22222222-2222-3333-4444-555555555555", "", "", "shared")

	_, matches, err := resolveSelector("shared")
	if !errors.Is(err, errAmbiguous) {
		t.Fatalf("expected errAmbiguous, got %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(matches))
	}
}

func TestResolveSelector_NotFound(t *testing.T) {
	setupTestDB(t)
	_, _, err := resolveSelector("nope-no-such-conv")
	if err == nil {
		t.Fatal("expected error for missing selector")
	}
}

// TestResolveSelector_ByGroupAlias covers the v2 fallback: a conv that
// only lives in agent_group_members (no conv_index row, e.g. fresh
// from `agent spawn`) is findable by its per-group alias.
func TestResolveSelector_ByGroupAlias(t *testing.T) {
	setupTestDB(t)
	g, _ := db.CreateAgentGroup("alpha", "")
	if err := db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: g, ConvID: "f6c6e261-deaf-bead-cafe-feedfacefeed",
		Alias: "second-banana", Role: "builder",
	}); err != nil {
		t.Fatalf("AddAgentGroupMember: %v", err)
	}

	r, _, err := resolveSelector("second-banana")
	if err != nil {
		t.Fatalf("resolveSelector: %v", err)
	}
	if r.ConvID != "f6c6e261-deaf-bead-cafe-feedfacefeed" {
		t.Errorf("conv_id = %q, want freshly-spawned uuid", r.ConvID)
	}
	if r.Row != nil {
		t.Errorf("Row should be nil for non-indexed conv, got %+v", r.Row)
	}
}

// TestResolveSelector_ByGroupConvPrefix covers the same fallback for
// the conv-id prefix path — `agent message f6c6e261 ...` should work
// even before the conv shows up in conv_index.
func TestResolveSelector_ByGroupConvPrefix(t *testing.T) {
	setupTestDB(t)
	g, _ := db.CreateAgentGroup("alpha", "")
	if err := db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: g, ConvID: "f6c6e261-deaf-bead-cafe-feedfacefeed",
		Alias: "x",
	}); err != nil {
		t.Fatalf("AddAgentGroupMember: %v", err)
	}

	r, _, err := resolveSelector("f6c6e261")
	if err != nil {
		t.Fatalf("resolveSelector: %v", err)
	}
	if r.ConvID != "f6c6e261-deaf-bead-cafe-feedfacefeed" {
		t.Errorf("conv_id = %q, want freshly-spawned uuid", r.ConvID)
	}
}

// TestResolveSelector_GroupAliasAmbiguous: same alias in two groups
// for two different convs is genuinely ambiguous.
func TestResolveSelector_GroupAliasAmbiguous(t *testing.T) {
	setupTestDB(t)
	g1, _ := db.CreateAgentGroup("alpha", "")
	g2, _ := db.CreateAgentGroup("beta", "")
	if err := db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: g1, ConvID: "11111111-1111-1111-1111-111111111111", Alias: "dup",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: g2, ConvID: "22222222-2222-2222-2222-222222222222", Alias: "dup",
	}); err != nil {
		t.Fatal(err)
	}

	_, matches, err := resolveSelector("dup")
	if !errors.Is(err, errAmbiguous) {
		t.Fatalf("expected errAmbiguous, got %v matches=%d", err, len(matches))
	}
	if len(matches) != 2 {
		t.Errorf("expected 2 distinct matches, got %d", len(matches))
	}
}

// TestResolveSelector_PrefersConvIndexOverMembers: when both have
// the conv, conv_index hit short-circuits the chain so we still get
// the .Row populated.
func TestResolveSelector_PrefersConvIndexOverMembers(t *testing.T) {
	setupTestDB(t)
	convID := "abcd1234-2222-3333-4444-555555555555"
	upsertConvIndex(t, convID, "indexed-title", "", "")

	g, _ := db.CreateAgentGroup("alpha", "")
	if err := db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: g, ConvID: convID, Alias: "alias-only",
	}); err != nil {
		t.Fatal(err)
	}

	// A conv-index hit on the prefix returns the Row; we don't fall
	// through to the agent_group_members step.
	r, _, err := resolveSelector("abcd1234")
	if err != nil {
		t.Fatalf("resolveSelector: %v", err)
	}
	if r.Row == nil {
		t.Error("expected conv_index row on prefix hit, got nil")
	}
}

func TestRunLookup(t *testing.T) {
	setupTestDB(t)
	upsertConvIndex(t, "abcd1234-2222-3333-4444-555555555555", "planner", "", "")

	var stdout, stderr bytes.Buffer
	rc := runLookupDirect(&lookupParams{Selector: "planner"}, &stdout, &stderr)
	if rc != rcOK {
		t.Fatalf("runLookup rc = %d, stderr = %s", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "abcd1234") {
		t.Fatalf("expected stdout to contain conv id, got %q", stdout.String())
	}
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
	if rc != rcOK {
		t.Fatalf("rc = %d, stderr = %q", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), HumanIdentity) {
		t.Fatalf("stdout = %q, want %q", stdout.String(), HumanIdentity)
	}
}

func TestRunWhoami_KnownConv(t *testing.T) {
	setupTestDB(t)
	upsertConvIndex(t, "abcd1234-2222-3333-4444-555555555555", "planner", "", "")
	t.Setenv("TCLAUDE_SESSION_ID", "abcd1234-2222-3333-4444-555555555555")

	var stdout, stderr bytes.Buffer
	rc := runWhoamiDirect(&stdout, &stderr)
	if rc != rcOK {
		t.Fatalf("rc = %d, stderr = %q", rc, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "abcd1234") || !strings.Contains(out, "planner") {
		t.Fatalf("stdout = %q", out)
	}
}

func TestRunLookup_Ambiguous(t *testing.T) {
	setupTestDB(t)
	upsertConvIndex(t, "11111111-2222-3333-4444-555555555555", "", "", "dup")
	upsertConvIndex(t, "22222222-2222-3333-4444-555555555555", "", "", "dup")

	var stdout, stderr bytes.Buffer
	rc := runLookupDirect(&lookupParams{Selector: "dup"}, &stdout, &stderr)
	if rc != rcAmbiguous {
		t.Fatalf("runLookup rc = %d", rc)
	}
	if !strings.Contains(stderr.String(), "matches 2 conversations") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
