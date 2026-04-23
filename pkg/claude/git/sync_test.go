package git

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/conv"
	"github.com/tofutools/tclaude/pkg/claude/syncutil"
)

// setTestHome sets the home directory for testing, handling cross-platform differences.
// Returns a cleanup function that restores the original values.
func setTestHome(_ *testing.T, tmpDir string) func() {
	origHome := os.Getenv("HOME")
	origUserProfile := os.Getenv("USERPROFILE")

	os.Setenv("HOME", tmpDir)
	if runtime.GOOS == "windows" {
		os.Setenv("USERPROFILE", tmpDir)
	}

	return func() {
		os.Setenv("HOME", origHome)
		if runtime.GOOS == "windows" {
			os.Setenv("USERPROFILE", origUserProfile)
		}
	}
}

// Test path constants - same as in repair_test.go
// These are just string values for testing path transformations, not actual filesystem paths.
const (
	syncTestCanonicalHome = "/home/canonical"
	syncTestLocalHome     = "/home/local"
	syncTestCanonicalGit  = "/home/canonical/git"
	syncTestLocalGit      = "/home/local/projects"
)

func syncTestConfig() *SyncConfig {
	return &SyncConfig{
		Homes: []string{syncTestCanonicalHome, syncTestLocalHome},
		Dirs:  [][]string{{syncTestCanonicalGit, syncTestLocalGit}},
	}
}

func TestMergeSessionsIndex_CanonicalizesSourcePaths(t *testing.T) {
	// Setup temp directories
	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "src")
	dstDir := filepath.Join(tmpDir, "dst")
	os.MkdirAll(srcDir, 0755)
	os.MkdirAll(dstDir, 0755)

	// Create a config file with path mappings
	configDir := filepath.Join(tmpDir, ".claude")
	os.MkdirAll(configDir, 0755)

	// Temporarily override the config path for testing
	cleanup := setTestHome(t, tmpDir)
	defer cleanup()

	config := SyncConfig{
		Homes: []string{syncTestCanonicalHome, syncTestLocalHome},
		Dirs:  [][]string{{syncTestCanonicalGit, syncTestLocalGit}},
	}
	configData, _ := json.MarshalIndent(config, "", "  ")
	os.WriteFile(filepath.Join(configDir, "sync_config.json"), configData, 0644)

	// Create source index with non-canonical (local) paths
	srcIndex := conv.SessionsIndex{
		Version: 1,
		Entries: []conv.SessionEntry{
			{
				SessionID:   "session-1",
				ProjectPath: "/home/local/projects/myproject",
				FullPath:    "/home/local/.claude/projects/-home-local-projects-myproject/session-1.jsonl",
				Modified:    "2024-01-15T12:00:00Z",
			},
		},
	}
	srcData, _ := json.MarshalIndent(srcIndex, "", "  ")
	os.WriteFile(filepath.Join(srcDir, "sessions-index.json"), srcData, 0644)

	// Create empty dst index
	dstIndex := conv.SessionsIndex{Version: 1, Entries: []conv.SessionEntry{}}
	dstData, _ := json.MarshalIndent(dstIndex, "", "  ")
	os.WriteFile(filepath.Join(dstDir, "sessions-index.json"), dstData, 0644)

	// Run merge
	err := mergeSessionsIndex(srcDir, dstDir)
	if err != nil {
		t.Fatalf("mergeSessionsIndex failed: %v", err)
	}

	// Read result
	resultData, _ := os.ReadFile(filepath.Join(dstDir, "sessions-index.json"))
	var result conv.SessionsIndex
	json.Unmarshal(resultData, &result)

	// Check that paths are canonicalized
	if len(result.Entries) != 1 {
		t.Fatalf("Expected 1 entry, got %d", len(result.Entries))
	}

	entry := result.Entries[0]
	if entry.ProjectPath != "/home/canonical/git/myproject" {
		t.Errorf("ProjectPath not canonicalized: got %q, want %q", entry.ProjectPath, "/home/canonical/git/myproject")
	}
	// FullPath home prefix should be canonicalized
	expectedPrefix := "/home/canonical/"
	if len(entry.FullPath) < len(expectedPrefix) || entry.FullPath[:len(expectedPrefix)] != expectedPrefix {
		t.Errorf("FullPath home prefix not canonicalized: got %q", entry.FullPath)
	}
}

func TestMergeSessionsIndex_PreservesExistingCanonicalPaths(t *testing.T) {
	// Setup temp directories
	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "src")
	dstDir := filepath.Join(tmpDir, "dst")
	os.MkdirAll(srcDir, 0755)
	os.MkdirAll(dstDir, 0755)

	// Create config
	configDir := filepath.Join(tmpDir, ".claude")
	os.MkdirAll(configDir, 0755)
	cleanup := setTestHome(t, tmpDir)
	defer cleanup()

	config := SyncConfig{
		Homes: []string{syncTestCanonicalHome, syncTestLocalHome},
	}
	configData, _ := json.MarshalIndent(config, "", "  ")
	os.WriteFile(filepath.Join(configDir, "sync_config.json"), configData, 0644)

	// Create source index with old non-canonical path and older timestamp
	srcIndex := conv.SessionsIndex{
		Version: 1,
		Entries: []conv.SessionEntry{
			{
				SessionID:   "session-1",
				ProjectPath: "/home/local/git/myproject",
				Modified:    "2024-01-10T12:00:00Z", // Older
			},
		},
	}
	srcData, _ := json.MarshalIndent(srcIndex, "", "  ")
	os.WriteFile(filepath.Join(srcDir, "sessions-index.json"), srcData, 0644)

	// Create dst index with canonical path and newer timestamp
	dstIndex := conv.SessionsIndex{
		Version: 1,
		Entries: []conv.SessionEntry{
			{
				SessionID:   "session-1",
				ProjectPath: "/home/canonical/git/myproject",
				Modified:    "2024-01-15T12:00:00Z", // Newer
			},
		},
	}
	dstData, _ := json.MarshalIndent(dstIndex, "", "  ")
	os.WriteFile(filepath.Join(dstDir, "sessions-index.json"), dstData, 0644)

	// Run merge
	err := mergeSessionsIndex(srcDir, dstDir)
	if err != nil {
		t.Fatalf("mergeSessionsIndex failed: %v", err)
	}

	// Read result
	resultData, _ := os.ReadFile(filepath.Join(dstDir, "sessions-index.json"))
	var result conv.SessionsIndex
	json.Unmarshal(resultData, &result)

	// Check that the newer (canonical) path is preserved
	if len(result.Entries) != 1 {
		t.Fatalf("Expected 1 entry, got %d", len(result.Entries))
	}

	entry := result.Entries[0]
	if entry.ProjectPath != "/home/canonical/git/myproject" {
		t.Errorf("Should keep newer canonical path: got %q, want %q", entry.ProjectPath, "/home/canonical/git/myproject")
	}
}

func TestMergeSessionsIndex_NewerLocalWins(t *testing.T) {
	// Setup temp directories
	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "src")
	dstDir := filepath.Join(tmpDir, "dst")
	os.MkdirAll(srcDir, 0755)
	os.MkdirAll(dstDir, 0755)

	// Create config
	configDir := filepath.Join(tmpDir, ".claude")
	os.MkdirAll(configDir, 0755)
	cleanup := setTestHome(t, tmpDir)
	defer cleanup()

	config := SyncConfig{
		Homes: []string{syncTestCanonicalHome, syncTestLocalHome},
	}
	configData, _ := json.MarshalIndent(config, "", "  ")
	os.WriteFile(filepath.Join(configDir, "sync_config.json"), configData, 0644)

	// Create source index with newer timestamp
	srcIndex := conv.SessionsIndex{
		Version: 1,
		Entries: []conv.SessionEntry{
			{
				SessionID:    "session-1",
				ProjectPath:  "/home/local/git/myproject",
				MessageCount: 100,                    // More messages
				Modified:     "2024-01-20T12:00:00Z", // Newer
			},
		},
	}
	srcData, _ := json.MarshalIndent(srcIndex, "", "  ")
	os.WriteFile(filepath.Join(srcDir, "sessions-index.json"), srcData, 0644)

	// Create dst index with older timestamp
	dstIndex := conv.SessionsIndex{
		Version: 1,
		Entries: []conv.SessionEntry{
			{
				SessionID:    "session-1",
				ProjectPath:  "/home/canonical/git/myproject",
				MessageCount: 50,                     // Fewer messages
				Modified:     "2024-01-15T12:00:00Z", // Older
			},
		},
	}
	dstData, _ := json.MarshalIndent(dstIndex, "", "  ")
	os.WriteFile(filepath.Join(dstDir, "sessions-index.json"), dstData, 0644)

	// Run merge
	err := mergeSessionsIndex(srcDir, dstDir)
	if err != nil {
		t.Fatalf("mergeSessionsIndex failed: %v", err)
	}

	// Read result
	resultData, _ := os.ReadFile(filepath.Join(dstDir, "sessions-index.json"))
	var result conv.SessionsIndex
	json.Unmarshal(resultData, &result)

	// Check that the newer entry wins (but with canonicalized path)
	if len(result.Entries) != 1 {
		t.Fatalf("Expected 1 entry, got %d", len(result.Entries))
	}

	entry := result.Entries[0]
	if entry.MessageCount != 100 {
		t.Errorf("Should use newer entry: got MessageCount %d, want 100", entry.MessageCount)
	}
	if entry.ProjectPath != "/home/canonical/git/myproject" {
		t.Errorf("Path should be canonicalized: got %q, want %q", entry.ProjectPath, "/home/canonical/git/myproject")
	}
}

func TestFindLocalEquivalent_FindsExistingDir(t *testing.T) {
	localDirs := map[string]bool{
		"-home-local-projects-myproject": true,
	}

	// Looking for canonical name, should find the local equivalent
	result := findLocalEquivalent("-home-canonical-git-myproject", localDirs, syncTestConfig())
	if result != "-home-local-projects-myproject" {
		t.Errorf("Should find local equivalent: got %q, want %q", result, "-home-local-projects-myproject")
	}
}

func TestFindLocalEquivalent_ReturnsCanonicalIfNoLocal(t *testing.T) {
	config := &SyncConfig{
		Homes: []string{syncTestCanonicalHome, syncTestLocalHome},
	}

	localDirs := map[string]bool{} // No local dirs

	result := findLocalEquivalent("-home-canonical-git-myproject", localDirs, config)
	if result != "-home-canonical-git-myproject" {
		t.Errorf("Should return canonical when no local: got %q, want %q", result, "-home-canonical-git-myproject")
	}
}

func TestFindLocalEquivalent_PrefersCanonicalIfExists(t *testing.T) {
	config := &SyncConfig{
		Homes: []string{syncTestCanonicalHome, syncTestLocalHome},
	}

	localDirs := map[string]bool{
		"-home-canonical-git-myproject": true,
		"-home-local-git-myproject":     true,
	}

	// Both exist, should prefer canonical
	result := findLocalEquivalent("-home-canonical-git-myproject", localDirs, config)
	if result != "-home-canonical-git-myproject" {
		t.Errorf("Should prefer canonical: got %q, want %q", result, "-home-canonical-git-myproject")
	}
}

func TestCopyAndLocalizeIndex(t *testing.T) {
	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "src-project")
	dstDir := filepath.Join(tmpDir, "dst-project")
	os.MkdirAll(srcDir, 0755)
	os.MkdirAll(dstDir, 0755)

	srcPath := filepath.Join(srcDir, "sessions-index.json")
	dstPath := filepath.Join(dstDir, "sessions-index.json")

	// Create a dummy .jsonl file so LoadSessionsIndex finds it
	sessionID := "01234567-89ab-cdef-0123-456789abcdef"
	jsonlPath := filepath.Join(srcDir, sessionID+".jsonl")
	jsonlContent := `{"type":"user","sessionId":"` + sessionID + `","timestamp":"2026-01-31T10:00:00Z","cwd":"/home/canonical/git/myproject","message":{"role":"user","content":"test"}}`
	os.WriteFile(jsonlPath, []byte(jsonlContent), 0644)

	// Create source index with canonical paths
	srcIndex := conv.SessionsIndex{
		Version: 1,
		Entries: []conv.SessionEntry{
			{
				SessionID:   sessionID,
				ProjectPath: "/home/canonical/git/myproject",
				FullPath:    "/home/canonical/.claude/projects/-home-canonical-git-myproject/" + sessionID + ".jsonl",
			},
		},
	}
	srcData, _ := json.MarshalIndent(srcIndex, "", "  ")
	os.WriteFile(srcPath, srcData, 0644)

	// Copy with localization (simulating local machine)
	err := copyAndLocalizeIndex(srcPath, dstPath, syncTestConfig(), syncTestLocalHome)
	if err != nil {
		t.Fatalf("copyAndLocalizeIndex failed: %v", err)
	}

	// Read result
	resultData, _ := os.ReadFile(dstPath)
	var result conv.SessionsIndex
	json.Unmarshal(resultData, &result)

	if len(result.Entries) != 1 {
		t.Fatalf("Expected 1 entry, got %d", len(result.Entries))
	}

	entry := result.Entries[0]
	expectedProjectPath := "/home/local/projects/myproject"
	if entry.ProjectPath != expectedProjectPath {
		t.Errorf("ProjectPath not localized: got %q, want %q", entry.ProjectPath, expectedProjectPath)
	}

	// FullPath should be fully localized (both home prefix AND embedded project dir)
	expectedFullPath := "/home/local/.claude/projects/-home-local-projects-myproject/" + sessionID + ".jsonl"
	if entry.FullPath != expectedFullPath {
		t.Errorf("FullPath not localized: got %q, want %q", entry.FullPath, expectedFullPath)
	}
}

func TestCopyAndLocalizeIndex_AlreadyLocal(t *testing.T) {
	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "src-project")
	dstDir := filepath.Join(tmpDir, "dst-project")
	os.MkdirAll(srcDir, 0755)
	os.MkdirAll(dstDir, 0755)

	srcPath := filepath.Join(srcDir, "sessions-index.json")
	dstPath := filepath.Join(dstDir, "sessions-index.json")

	config := &SyncConfig{
		Homes: []string{syncTestCanonicalHome, syncTestLocalHome},
	}

	// Create a dummy .jsonl file so LoadSessionsIndex finds it
	sessionID := "01234567-89ab-cdef-0123-456789abcdef"
	jsonlPath := filepath.Join(srcDir, sessionID+".jsonl")
	jsonlContent := `{"type":"user","sessionId":"` + sessionID + `","timestamp":"2026-01-31T10:00:00Z","cwd":"/home/canonical/git/myproject","message":{"role":"user","content":"test"}}`
	os.WriteFile(jsonlPath, []byte(jsonlContent), 0644)

	// Source already has canonical paths
	srcIndex := conv.SessionsIndex{
		Version: 1,
		Entries: []conv.SessionEntry{
			{
				SessionID:   sessionID,
				ProjectPath: "/home/canonical/git/myproject",
			},
		},
	}
	srcData, _ := json.MarshalIndent(srcIndex, "", "  ")
	os.WriteFile(srcPath, srcData, 0644)

	// Copy with localization on canonical machine (canonical = local)
	err := copyAndLocalizeIndex(srcPath, dstPath, config, syncTestCanonicalHome)
	if err != nil {
		t.Fatalf("copyAndLocalizeIndex failed: %v", err)
	}

	// Read result
	resultData, _ := os.ReadFile(dstPath)
	var result conv.SessionsIndex
	json.Unmarshal(resultData, &result)

	if len(result.Entries) != 1 {
		t.Fatalf("Expected 1 entry, got %d", len(result.Entries))
	}

	entry := result.Entries[0]
	// Should stay as canonical since we're on the canonical machine
	if entry.ProjectPath != "/home/canonical/git/myproject" {
		t.Errorf("Path should stay canonical: got %q", entry.ProjectPath)
	}
}

func TestRoundTrip_LocalToSyncToLocal(t *testing.T) {
	// This tests the full round trip:
	// 1. Local paths → sync (canonicalized)
	// 2. sync → Local (localized back)

	tmpDir := t.TempDir()

	// Setup config
	configDir := filepath.Join(tmpDir, ".claude")
	os.MkdirAll(configDir, 0755)
	cleanup := setTestHome(t, tmpDir)
	defer cleanup()

	config := SyncConfig{
		Homes: []string{syncTestCanonicalHome, syncTestLocalHome},
		Dirs:  [][]string{{syncTestCanonicalGit, syncTestLocalGit}},
	}
	configData, _ := json.MarshalIndent(config, "", "  ")
	os.WriteFile(filepath.Join(configDir, "sync_config.json"), configData, 0644)

	// Create source (local machine) index
	srcDir := filepath.Join(tmpDir, "local")
	os.MkdirAll(srcDir, 0755)
	srcIndex := conv.SessionsIndex{
		Version: 1,
		Entries: []conv.SessionEntry{
			{
				SessionID:   "session-1",
				ProjectPath: "/home/local/projects/myproject",
				FullPath:    "/home/local/.claude/projects/-home-local-projects-myproject/session-1.jsonl",
				Modified:    "2024-01-15T12:00:00Z",
			},
		},
	}
	srcData, _ := json.MarshalIndent(srcIndex, "", "  ")
	os.WriteFile(filepath.Join(srcDir, "sessions-index.json"), srcData, 0644)

	// Create sync dir (starts empty)
	syncDir := filepath.Join(tmpDir, "sync")
	os.MkdirAll(syncDir, 0755)
	syncIndex := conv.SessionsIndex{Version: 1, Entries: []conv.SessionEntry{}}
	syncData, _ := json.MarshalIndent(syncIndex, "", "  ")
	os.WriteFile(filepath.Join(syncDir, "sessions-index.json"), syncData, 0644)

	// Step 1: Merge local → sync (should canonicalize)
	err := mergeSessionsIndex(srcDir, syncDir)
	if err != nil {
		t.Fatalf("mergeSessionsIndex failed: %v", err)
	}

	// Verify sync has canonical paths
	syncResultData, _ := os.ReadFile(filepath.Join(syncDir, "sessions-index.json"))
	var syncResult conv.SessionsIndex
	json.Unmarshal(syncResultData, &syncResult)

	if syncResult.Entries[0].ProjectPath != "/home/canonical/git/myproject" {
		t.Errorf("Sync should have canonical path: got %q", syncResult.Entries[0].ProjectPath)
	}

	// Step 2: Copy sync → local (should localize back to local paths)
	dstDir := filepath.Join(tmpDir, "local-copy")
	os.MkdirAll(dstDir, 0755)

	err = copyAndLocalizeIndex(
		filepath.Join(syncDir, "sessions-index.json"),
		filepath.Join(dstDir, "sessions-index.json"),
		&config,
		syncTestLocalHome,
	)
	if err != nil {
		t.Fatalf("copyAndLocalizeIndex failed: %v", err)
	}

	// Verify local copy has local paths
	localResultData, _ := os.ReadFile(filepath.Join(dstDir, "sessions-index.json"))
	var localResult conv.SessionsIndex
	json.Unmarshal(localResultData, &localResult)

	expectedPath := "/home/local/projects/myproject"
	if localResult.Entries[0].ProjectPath != expectedPath {
		t.Errorf("Local should have local path: got %q, want %q", localResult.Entries[0].ProjectPath, expectedPath)
	}
}

func TestHasPrefix(t *testing.T) {
	tests := []struct {
		name     string
		data     []byte
		prefix   []byte
		expected bool
	}{
		{"exact match", []byte("hello"), []byte("hello"), true},
		{"prefix match", []byte("hello world"), []byte("hello"), true},
		{"no match", []byte("hello"), []byte("world"), false},
		{"prefix longer than data", []byte("hi"), []byte("hello"), false},
		{"empty prefix", []byte("hello"), []byte(""), true},
		{"empty data empty prefix", []byte(""), []byte(""), true},
		{"empty data non-empty prefix", []byte(""), []byte("x"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := hasPrefix(tt.data, tt.prefix)
			if result != tt.expected {
				t.Errorf("hasPrefix(%q, %q) = %v, want %v", tt.data, tt.prefix, result, tt.expected)
			}
		})
	}
}

func TestCountLinesBytes(t *testing.T) {
	tests := []struct {
		name     string
		data     []byte
		expected int
	}{
		{"empty", []byte(""), 0},
		{"no newlines", []byte("hello"), 0},
		{"one line", []byte("hello\n"), 1},
		{"two lines", []byte("hello\nworld\n"), 2},
		{"trailing no newline", []byte("hello\nworld"), 1},
		{"only newlines", []byte("\n\n\n"), 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := countLinesBytes(tt.data)
			if result != tt.expected {
				t.Errorf("countLinesBytes(%q) = %d, want %d", tt.data, result, tt.expected)
			}
		})
	}
}

func TestHandleConversationConflict_LocalExtends(t *testing.T) {
	tmpDir := t.TempDir()
	localPath := filepath.Join(tmpDir, "local.jsonl")
	remotePath := filepath.Join(tmpDir, "remote.jsonl")

	// Remote has 2 messages
	remoteContent := `{"msg": "first"}
{"msg": "second"}
`
	// Local has same 2 messages plus a third
	localContent := `{"msg": "first"}
{"msg": "second"}
{"msg": "third"}
`
	os.WriteFile(remotePath, []byte(remoteContent), 0644)
	os.WriteFile(localPath, []byte(localContent), 0644)

	params := &SyncParams{}
	err := handleConversationConflict(localPath, remotePath, "test.jsonl", params)
	if err != nil {
		t.Fatalf("handleConversationConflict failed: %v", err)
	}

	// Remote should now have local content (local extends remote)
	result, _ := os.ReadFile(remotePath)
	if string(result) != localContent {
		t.Errorf("Expected remote to be updated with local content")
	}
}

func TestHandleConversationConflict_RemoteExtends(t *testing.T) {
	tmpDir := t.TempDir()
	localPath := filepath.Join(tmpDir, "local.jsonl")
	remotePath := filepath.Join(tmpDir, "remote.jsonl")

	// Local has 2 messages
	localContent := `{"msg": "first"}
{"msg": "second"}
`
	// Remote has same 2 messages plus a third
	remoteContent := `{"msg": "first"}
{"msg": "second"}
{"msg": "third"}
`
	os.WriteFile(remotePath, []byte(remoteContent), 0644)
	os.WriteFile(localPath, []byte(localContent), 0644)

	params := &SyncParams{}
	err := handleConversationConflict(localPath, remotePath, "test.jsonl", params)
	if err != nil {
		t.Fatalf("handleConversationConflict failed: %v", err)
	}

	// Remote should stay as is (remote extends local)
	result, _ := os.ReadFile(remotePath)
	if string(result) != remoteContent {
		t.Errorf("Expected remote to stay unchanged")
	}
}

func TestHandleConversationConflict_KeepLocalFlag(t *testing.T) {
	tmpDir := t.TempDir()
	localPath := filepath.Join(tmpDir, "local.jsonl")
	remotePath := filepath.Join(tmpDir, "remote.jsonl")

	// Different content that doesn't extend
	os.WriteFile(remotePath, []byte("remote\n"), 0644)
	os.WriteFile(localPath, []byte("local\n"), 0644)

	params := &SyncParams{KeepLocal: true}
	err := handleConversationConflict(localPath, remotePath, "test.jsonl", params)
	if err != nil {
		t.Fatalf("handleConversationConflict failed: %v", err)
	}

	result, _ := os.ReadFile(remotePath)
	if string(result) != "local\n" {
		t.Errorf("Expected remote to have local content with --keep-local")
	}
}

func TestHandleConversationConflict_KeepRemoteFlag(t *testing.T) {
	tmpDir := t.TempDir()
	localPath := filepath.Join(tmpDir, "local.jsonl")
	remotePath := filepath.Join(tmpDir, "remote.jsonl")

	// Different content that doesn't extend
	os.WriteFile(remotePath, []byte("remote\n"), 0644)
	os.WriteFile(localPath, []byte("local\n"), 0644)

	params := &SyncParams{KeepRemote: true}
	err := handleConversationConflict(localPath, remotePath, "test.jsonl", params)
	if err != nil {
		t.Fatalf("handleConversationConflict failed: %v", err)
	}

	result, _ := os.ReadFile(remotePath)
	if string(result) != "remote\n" {
		t.Errorf("Expected remote to stay unchanged with --keep-remote")
	}
}

func TestUpdateUnprocessedSyncDirs(t *testing.T) {
	tmpDir := t.TempDir()
	cleanup := setTestHome(t, tmpDir)
	defer cleanup()

	// Setup config
	configDir := filepath.Join(tmpDir, ".claude")
	os.MkdirAll(configDir, 0755)
	config := SyncConfig{
		Homes: []string{"/home/canonical", "/home/local"},
	}
	configData, _ := json.MarshalIndent(config, "", "  ")
	os.WriteFile(filepath.Join(configDir, "sync_config.json"), configData, 0644)

	// Create projects dir (local) with one project
	projectsDir := filepath.Join(tmpDir, ".claude", "projects")
	localProject := filepath.Join(projectsDir, "-home-local-git-myproject")
	os.MkdirAll(localProject, 0755)

	// Create sync dir with two projects:
	// 1. One that has a local counterpart (will be processed by mergeLocalToSync)
	// 2. One that has NO local counterpart (should be processed by updateUnprocessedSyncDirs)
	syncDir := filepath.Join(tmpDir, ".claude", "projects_sync")

	// Sync project with local counterpart
	syncProject1 := filepath.Join(syncDir, "-home-canonical-git-myproject")
	os.MkdirAll(syncProject1, 0755)

	// Sync project WITHOUT local counterpart - has an unindexed .jsonl file
	syncProject2 := filepath.Join(syncDir, "-home-canonical-git-otherproject")
	os.MkdirAll(syncProject2, 0755)

	// Create an unindexed .jsonl file in the sync-only project
	sessionID := "01234567-89ab-cdef-0123-456789abcdef"
	jsonlPath := filepath.Join(syncProject2, sessionID+".jsonl")
	jsonlContent := `{"type":"user","sessionId":"` + sessionID + `","timestamp":"2026-01-31T10:00:00Z","cwd":"/home/canonical/git/otherproject","message":{"role":"user","content":"test"}}`
	os.WriteFile(jsonlPath, []byte(jsonlContent), 0644)

	// Create an empty sessions-index.json (the .jsonl is unindexed)
	emptyIndex := conv.SessionsIndex{Version: 1, Entries: []conv.SessionEntry{}}
	emptyData, _ := json.MarshalIndent(emptyIndex, "", "  ")
	os.WriteFile(filepath.Join(syncProject2, "sessions-index.json"), emptyData, 0644)

	// Run updateUnprocessedSyncDirs
	updateUnprocessedSyncDirs(projectsDir, syncDir)

	// The sync-only project's index should now include the unindexed session
	indexPath := filepath.Join(syncProject2, "sessions-index.json")
	indexData, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("Failed to read updated index: %v", err)
	}

	var resultIndex conv.SessionsIndex
	json.Unmarshal(indexData, &resultIndex)

	if len(resultIndex.Entries) != 1 {
		t.Fatalf("Expected 1 entry after updateUnprocessedSyncDirs, got %d", len(resultIndex.Entries))
	}

	if resultIndex.Entries[0].SessionID != sessionID {
		t.Errorf("Expected session %s, got %s", sessionID, resultIndex.Entries[0].SessionID)
	}
}

func TestMergeProject_DeletesTombstonedFiles(t *testing.T) {
	tmpDir := t.TempDir()
	cleanup := setTestHome(t, tmpDir)
	defer cleanup()

	// Setup config
	configDir := filepath.Join(tmpDir, ".claude")
	os.MkdirAll(configDir, 0755)
	config := SyncConfig{
		Homes: []string{"/home/canonical", "/home/local"},
	}
	configData, _ := json.MarshalIndent(config, "", "  ")
	os.WriteFile(filepath.Join(configDir, "sync_config.json"), configData, 0644)

	// Create local project
	localProject := filepath.Join(tmpDir, "local-project")
	os.MkdirAll(localProject, 0755)
	localIndex := conv.SessionsIndex{Version: 1, Entries: []conv.SessionEntry{}}
	localData, _ := json.MarshalIndent(localIndex, "", "  ")
	os.WriteFile(filepath.Join(localProject, "sessions-index.json"), localData, 0644)

	// Create sync project with a file that should be deleted
	syncProject := filepath.Join(tmpDir, "sync-project")
	os.MkdirAll(syncProject, 0755)

	sessionID := "01234567-89ab-cdef-0123-456789abcdef"
	jsonlPath := filepath.Join(syncProject, sessionID+".jsonl")
	os.WriteFile(jsonlPath, []byte(`{"test":"data"}`), 0644)

	// Create deletions.json marking the session as tombstoned
	deletions := syncutil.Deletions{
		Version: 1,
		Entries: []syncutil.Tombstone{
			{SessionID: sessionID, DeletedAt: "2026-01-31T10:00:00Z", DeletedBy: "testhost"},
		},
	}
	deletionsData, _ := json.MarshalIndent(deletions, "", "  ")
	os.WriteFile(filepath.Join(syncProject, "deletions.json"), deletionsData, 0644)

	// Create sessions-index.json (empty since file is tombstoned)
	syncIndex := conv.SessionsIndex{Version: 1, Entries: []conv.SessionEntry{}}
	syncData, _ := json.MarshalIndent(syncIndex, "", "  ")
	os.WriteFile(filepath.Join(syncProject, "sessions-index.json"), syncData, 0644)

	// Run mergeProject
	params := &SyncParams{}
	err := mergeProject(localProject, syncProject, params)
	if err != nil {
		t.Fatalf("mergeProject failed: %v", err)
	}

	// The tombstoned file should be deleted
	if _, err := os.Stat(jsonlPath); !os.IsNotExist(err) {
		t.Errorf("Expected tombstoned file to be deleted, but it still exists")
	}
}
