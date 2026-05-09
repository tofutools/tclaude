package agent

import (
	"bytes"
	"errors"
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
	if err := db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      convID,
		ProjectDir:  "/tmp/proj",
		FullPath:    "/tmp/proj/" + convID + ".jsonl",
		FileMtime:   time.Now().Unix(),
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
	if r.convID != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("convID = %q", r.convID)
	}
}

func TestResolveSelector_ByPrefix(t *testing.T) {
	setupTestDB(t)
	upsertConvIndex(t, "abcd1234-2222-3333-4444-555555555555", "planner", "", "")

	r, _, err := resolveSelector("abcd1234")
	if err != nil {
		t.Fatalf("resolveSelector: %v", err)
	}
	if !strings.HasPrefix(r.convID, "abcd1234") {
		t.Fatalf("convID = %q", r.convID)
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
	if !strings.HasPrefix(r.convID, "11111111") {
		t.Fatalf("convID = %q", r.convID)
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

func TestRunLookup(t *testing.T) {
	setupTestDB(t)
	upsertConvIndex(t, "abcd1234-2222-3333-4444-555555555555", "planner", "", "")

	var stdout, stderr bytes.Buffer
	rc := runLookup(&lookupParams{Selector: "planner"}, &stdout, &stderr)
	if rc != rcOK {
		t.Fatalf("runLookup rc = %d, stderr = %s", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "abcd1234") {
		t.Fatalf("expected stdout to contain conv id, got %q", stdout.String())
	}
}

func TestRunWhoami_HumanFallback(t *testing.T) {
	setupTestDB(t)
	// No TCLAUDE_SESSION_ID, no CC ancestor (go test is run from a plain
	// shell). Expect the <human> fallback rather than an error.
	var stdout, stderr bytes.Buffer
	rc := runWhoami(&stdout, &stderr)
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
	rc := runWhoami(&stdout, &stderr)
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
	rc := runLookup(&lookupParams{Selector: "dup"}, &stdout, &stderr)
	if rc != rcAmbiguous {
		t.Fatalf("runLookup rc = %d", rc)
	}
	if !strings.Contains(stderr.String(), "matches 2 conversations") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
