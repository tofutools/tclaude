package conv

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// renameTestSetup creates a "real" project dir, plants a .jsonl inside the
// HOME-based Claude projects dir, indexes it, and chdirs to the real path so
// cwd-based selectors work. Returns the project dir under HOME, the .jsonl
// path, and the real project path.
func renameTestSetup(t *testing.T, sessionID, jsonlContent string) (projectDir, jsonlPath, realPath string) {
	t.Helper()
	setupTestDB(t)

	tmp := t.TempDir()
	realPath = filepath.Join(tmp, "myrepo")
	if err := os.MkdirAll(realPath, 0755); err != nil {
		t.Fatal(err)
	}
	// Canonicalize symlinks (e.g. /var -> /private/var on macOS) so the slug
	// computed from realPath matches the one computed from os.Getwd() after chdir.
	if resolved, err := filepath.EvalSymlinks(realPath); err == nil {
		realPath = resolved
	}

	projectDir = GetClaudeProjectPath(realPath)
	// Belt-and-braces: setupTestDB sets HOME to a tmp dir, so this should
	// resolve under tmp. If it doesn't, fail loudly rather than risk writing
	// into the user's real ~/.claude/projects.
	homeDir := os.Getenv("HOME")
	if homeDir == "" || !strings.HasPrefix(projectDir, homeDir) {
		t.Fatalf("project dir %q is not under tmp HOME %q (refusing to touch real Claude dir)", projectDir, homeDir)
	}
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}

	jsonlPath = filepath.Join(projectDir, sessionID+".jsonl")
	if jsonlContent == "" {
		jsonlContent = `{"type":"user","sessionId":"` + sessionID + `","cwd":"` + realPath + `","message":{"role":"user","content":"hi"},"timestamp":"2026-03-01T10:00:00Z"}` + "\n"
	}
	if err := os.WriteFile(jsonlPath, []byte(jsonlContent), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadSessionsIndex(projectDir); err != nil {
		t.Fatalf("LoadSessionsIndex failed: %v", err)
	}

	prevCwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(prevCwd) })
	if err := os.Chdir(realPath); err != nil {
		t.Fatal(err)
	}

	return projectDir, jsonlPath, realPath
}

func TestRunRename_AppendsCustomTitleLine(t *testing.T) {
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	_, jsonlPath, _ := renameTestSetup(t, sessionID, "")

	rc := RunRename(&RenameParams{
		Selector: sessionID,
		Name:     "[PR:my-org/my-repo/pull/417] fix river ctx",
		Yes:      true,
	}, os.Stdout, os.Stderr, os.Stdin)
	if rc != rcOK {
		t.Fatalf("RunRename returned %d", rc)
	}

	data, err := os.ReadFile(jsonlPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"customTitle":"[PR:my-org/my-repo/pull/417] fix river ctx"`) {
		t.Fatalf("custom-title line not found:\n%s", data)
	}

	// And the DB cache should reflect it.
	row, err := db.GetConvIndex(sessionID)
	if err != nil || row == nil {
		t.Fatalf("DB row missing: %v", err)
	}
	if row.CustomTitle != "[PR:my-org/my-repo/pull/417] fix river ctx" {
		t.Fatalf("DB CustomTitle = %q", row.CustomTitle)
	}
}

func TestRunRename_WritesBothCustomTitleAndAgentName(t *testing.T) {
	sessionID := "ffffffff-1111-2222-3333-444444444444"
	_, jsonlPath, _ := renameTestSetup(t, sessionID, "")

	rc := RunRename(&RenameParams{Selector: sessionID, Name: "paired-name", Yes: true},
		os.Stdout, os.Stderr, os.Stdin)
	if rc != rcOK {
		t.Fatalf("rename returned %d", rc)
	}

	data, err := os.ReadFile(jsonlPath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `"type":"custom-title","customTitle":"paired-name"`) {
		t.Errorf("custom-title line missing:\n%s", s)
	}
	if !strings.Contains(s, `"type":"agent-name","agentName":"paired-name"`) {
		t.Errorf("agent-name line missing:\n%s", s)
	}
}

func TestParseJSONLSession_CustomTitleWinsOverStaleAgentName(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "11111111-2222-3333-4444-555555555555"
	filePath := filepath.Join(tmpDir, sessionID+".jsonl")

	// Simulates a live CC instance that re-emits a stale cached `agent-name`
	// after we updated `custom-title`. The latest `custom-title` should win.
	content := `{"type":"user","cwd":"/p","message":{"role":"user","content":"hi"},"timestamp":"2026-03-01T10:00:00Z"}
{"type":"custom-title","customTitle":"new","sessionId":"` + sessionID + `"}
{"type":"agent-name","agentName":"new","sessionId":"` + sessionID + `"}
{"type":"agent-name","agentName":"stale","sessionId":"` + sessionID + `"}
`
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	entry := parseJSONLSession(filePath, sessionID)
	if entry == nil {
		t.Fatal("parseJSONLSession returned nil")
	}
	if entry.CustomTitle != "new" {
		t.Errorf("expected CustomTitle 'new' (custom-title beats stale agent-name), got %q", entry.CustomTitle)
	}
}

func TestParseJSONLSession_AgentNameUsedAsFallback(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "22222222-3333-4444-5555-666666666666"
	filePath := filepath.Join(tmpDir, sessionID+".jsonl")

	// No custom-title; an agent-name should still be picked up.
	content := `{"type":"user","cwd":"/p","message":{"role":"user","content":"hi"},"timestamp":"2026-03-01T10:00:00Z"}
{"type":"agent-name","agentName":"only-agent-name","sessionId":"` + sessionID + `"}
`
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	entry := parseJSONLSession(filePath, sessionID)
	if entry == nil || entry.CustomTitle != "only-agent-name" {
		t.Fatalf("expected agent-name fallback, got %+v", entry)
	}
}

func TestRunRename_LastWriteWins(t *testing.T) {
	sessionID := "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"
	_, jsonlPath, realPath := renameTestSetup(t, sessionID, "")
	original := `{"type":"user","sessionId":"` + sessionID + `","cwd":"` + realPath + `","message":{"role":"user","content":"hi"},"timestamp":"2026-03-01T10:00:00Z"}` + "\n"
	if err := os.WriteFile(jsonlPath, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSessionsIndex(filepath.Dir(jsonlPath)); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"first", "second", "third"} {
		rc := RunRename(&RenameParams{Selector: sessionID, Name: name, Yes: true}, os.Stdout, os.Stderr, os.Stdin)
		if rc != rcOK {
			t.Fatalf("rename %q returned %d", name, rc)
		}
	}

	row, err := db.GetConvIndex(sessionID)
	if err != nil || row == nil {
		t.Fatal("DB row missing")
	}
	if row.CustomTitle != "third" {
		t.Fatalf("expected last name 'third', got %q", row.CustomTitle)
	}
}

func TestRunRename_StripClearsTitle(t *testing.T) {
	sessionID := "cccccccc-dddd-eeee-ffff-000000000000"
	_, jsonlPath, realPath := renameTestSetup(t, sessionID, "")
	original := `{"type":"user","sessionId":"` + sessionID + `","cwd":"` + realPath + `","message":{"role":"user","content":"hi"},"timestamp":"2026-03-01T10:00:00Z"}
{"type":"custom-title","customTitle":"will-be-stripped","sessionId":"` + sessionID + `"}
`
	if err := os.WriteFile(jsonlPath, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSessionsIndex(filepath.Dir(jsonlPath)); err != nil {
		t.Fatal(err)
	}

	rc := RunRename(&RenameParams{Selector: sessionID, Strip: true, Yes: true}, os.Stdout, os.Stderr, os.Stdin)
	if rc != rcOK {
		t.Fatalf("strip returned %d", rc)
	}

	row, err := db.GetConvIndex(sessionID)
	if err != nil || row == nil {
		t.Fatal("DB row missing")
	}
	if row.CustomTitle != "" {
		t.Fatalf("expected cleared CustomTitle, got %q", row.CustomTitle)
	}
}

func TestRunRename_PrefixSelector(t *testing.T) {
	sessionID := "dddddddd-eeee-ffff-0000-111111111111"
	_, jsonlPath, realPath := renameTestSetup(t, sessionID, "")
	original := `{"type":"user","sessionId":"` + sessionID + `","cwd":"` + realPath + `","message":{"role":"user","content":"hi"},"timestamp":"2026-03-01T10:00:00Z"}` + "\n"
	if err := os.WriteFile(jsonlPath, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSessionsIndex(filepath.Dir(jsonlPath)); err != nil {
		t.Fatal(err)
	}

	rc := RunRename(&RenameParams{Selector: sessionID[:8], Name: "via-prefix", Yes: true}, os.Stdout, os.Stderr, os.Stdin)
	if rc != rcOK {
		t.Fatalf("prefix rename returned %d", rc)
	}
	row, _ := db.GetConvIndex(sessionID)
	if row == nil || row.CustomTitle != "via-prefix" {
		t.Fatalf("prefix rename did not update title (row=%+v)", row)
	}
}

func TestRunRename_CurrentEnvVar(t *testing.T) {
	sessionID := "eeeeeeee-ffff-0000-1111-222222222222"
	_, jsonlPath, realPath := renameTestSetup(t, sessionID, "")
	original := `{"type":"user","sessionId":"` + sessionID + `","cwd":"` + realPath + `","message":{"role":"user","content":"hi"},"timestamp":"2026-03-01T10:00:00Z"}` + "\n"
	if err := os.WriteFile(jsonlPath, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSessionsIndex(filepath.Dir(jsonlPath)); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TCLAUDE_SESSION_ID", sessionID[:8])
	rc := RunRename(&RenameParams{Selector: ".", Name: "via-env", Yes: true}, os.Stdout, os.Stderr, os.Stdin)
	if rc != rcOK {
		t.Fatalf(". selector returned %d", rc)
	}
	row, _ := db.GetConvIndex(sessionID)
	if row == nil || row.CustomTitle != "via-env" {
		t.Fatalf("env-var rename did not update title (row=%+v)", row)
	}
}

func TestRunRename_NotFound(t *testing.T) {
	setupTestDB(t)
	rc := RunRename(&RenameParams{Selector: "no-such-id", Name: "x", Yes: true}, os.Stdout, os.Stderr, os.Stdin)
	if rc != rcNotFound {
		t.Fatalf("expected rcNotFound, got %d", rc)
	}
}

func TestRunRename_InvalidName(t *testing.T) {
	setupTestDB(t)
	rc := RunRename(&RenameParams{Selector: "abc", Name: "has\nnewline", Yes: true}, os.Stdout, os.Stderr, os.Stdin)
	if rc != rcInvalidName {
		t.Fatalf("expected rcInvalidName, got %d", rc)
	}
}

func TestRunRename_MissingArg(t *testing.T) {
	setupTestDB(t)
	rc := RunRename(&RenameParams{Selector: "abc", Name: ""}, os.Stdout, os.Stderr, os.Stdin)
	if rc != rcMissingArg {
		t.Fatalf("expected rcMissingArg, got %d", rc)
	}
}

func TestUpdateCCSessionName_PreservesOtherFields(t *testing.T) {
	// Verify our writer round-trips arbitrary fields. We can't simulate a
	// process tree containing CC, so we exercise the JSON read/write logic
	// directly.
	setupTestDB(t)

	pid := os.Getpid()
	path := ccSessionFile(pid)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })

	convID := "abcdef12-3456-7890-abcd-ef1234567890"
	original := `{"pid":` + strings.TrimSpace(intStr(pid)) + `,"sessionId":"` + convID + `","cwd":"/x","status":"idle","name":"old"}`
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := readCCSessionFile(pid)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if m["status"] != "idle" {
		t.Errorf("expected status=idle, got %v", m["status"])
	}

	// Forge the writer path: we can't use updateCCSessionName since
	// FindClaudePID won't return our test PID, so we mimic what it does.
	m["name"] = "new"
	data, _ := json.Marshal(m)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	m2, err := readCCSessionFile(pid)
	if err != nil {
		t.Fatalf("re-read failed: %v", err)
	}
	if m2["name"] != "new" {
		t.Errorf("name not updated: %v", m2["name"])
	}
	if m2["status"] != "idle" {
		t.Errorf("status was lost: %v", m2["status"])
	}
	if m2["sessionId"] != convID {
		t.Errorf("sessionId was lost: %v", m2["sessionId"])
	}
}

// intStr is a small helper to avoid importing strconv just for one Itoa.
func intStr(i int) string { return fmt.Sprintf("%d", i) }
