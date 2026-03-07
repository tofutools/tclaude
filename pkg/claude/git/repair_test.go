package git

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/conv"
)

// Test path constants - these are just string values for testing path transformations,
// not actual filesystem paths. Using generic names for clarity.
// Must match the constants in config_test.go
const (
	testCanonicalHome = "/home/canonical"
	testLocalHome     = "/home/local"
	testCanonicalGit  = "/home/canonical/git"
	testLocalGit      = "/home/local/projects"
)

func testConfig() *SyncConfig {
	return &SyncConfig{
		Homes: []string{testCanonicalHome, testLocalHome},
		Dirs:  [][]string{{testCanonicalGit, testLocalGit}},
	}
}

func TestUpdateIndexFile_Canonicalize(t *testing.T) {
	tempDir := t.TempDir()
	projectDir := filepath.Join(tempDir, "-home-local-projects-myproject")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a sessions-index.json with local paths
	index := conv.SessionsIndex{
		Entries: []conv.SessionEntry{
			{
				SessionID:   "abc123",
				ProjectPath: "/home/local/projects/myproject",
				FullPath:    "/home/local/.claude/projects/-home-local-projects-myproject/abc123.jsonl",
			},
		},
	}
	indexData, _ := json.MarshalIndent(index, "", "  ")
	indexPath := filepath.Join(projectDir, "sessions-index.json")
	if err := os.WriteFile(indexPath, indexData, 0644); err != nil {
		t.Fatal(err)
	}

	// Create a dummy session file so LoadSessionsIndex works
	sessionFile := filepath.Join(projectDir, "abc123.jsonl")
	if err := os.WriteFile(sessionFile, []byte(`{"type":"test"}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Canonicalize (empty localHome)
	wasFixed, err := updateIndexFile(indexPath, testConfig(), "")
	if err != nil {
		t.Fatalf("updateIndexFile failed: %v", err)
	}
	if !wasFixed {
		t.Error("expected file to be modified")
	}

	// Read and verify
	data, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}

	var result conv.SessionsIndex
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}

	if len(result.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(result.Entries))
	}

	// Should be canonicalized (including embedded project dir in fullPath)
	if result.Entries[0].ProjectPath != "/home/canonical/git/myproject" {
		t.Errorf("ProjectPath not canonicalized: got %q, want %q",
			result.Entries[0].ProjectPath, "/home/canonical/git/myproject")
	}

	// FullPath should have both the home prefix AND embedded project dir canonicalized
	expectedFullPath := "/home/canonical/.claude/projects/-home-canonical-git-myproject/abc123.jsonl"
	if result.Entries[0].FullPath != expectedFullPath {
		t.Errorf("FullPath not canonicalized: got %q, want %q",
			result.Entries[0].FullPath, expectedFullPath)
	}
}

func TestUpdateIndexFile_Localize(t *testing.T) {
	tempDir := t.TempDir()
	projectDir := filepath.Join(tempDir, "-home-canonical-git-myproject")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a sessions-index.json with canonical paths
	index := conv.SessionsIndex{
		Entries: []conv.SessionEntry{
			{
				SessionID:   "abc123",
				ProjectPath: "/home/canonical/git/myproject",
				FullPath:    "/home/canonical/.claude/projects/-home-canonical-git-myproject/abc123.jsonl",
			},
		},
	}
	indexData, _ := json.MarshalIndent(index, "", "  ")
	indexPath := filepath.Join(projectDir, "sessions-index.json")
	if err := os.WriteFile(indexPath, indexData, 0644); err != nil {
		t.Fatal(err)
	}

	// Create a dummy session file so LoadSessionsIndex works
	sessionFile := filepath.Join(projectDir, "abc123.jsonl")
	if err := os.WriteFile(sessionFile, []byte(`{"type":"test"}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Localize to local
	wasFixed, err := updateIndexFile(indexPath, testConfig(), testLocalHome)
	if err != nil {
		t.Fatalf("updateIndexFile failed: %v", err)
	}
	if !wasFixed {
		t.Error("expected file to be modified")
	}

	// Read and verify
	data, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}

	var result conv.SessionsIndex
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}

	if len(result.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(result.Entries))
	}

	// Should be localized
	if result.Entries[0].ProjectPath != "/home/local/projects/myproject" {
		t.Errorf("ProjectPath not localized: got %q, want %q",
			result.Entries[0].ProjectPath, "/home/local/projects/myproject")
	}

	// FullPath should be fully localized (both home prefix AND embedded project dir)
	if result.Entries[0].FullPath != "/home/local/.claude/projects/-home-local-projects-myproject/abc123.jsonl" {
		t.Errorf("FullPath not localized: got %q, want %q",
			result.Entries[0].FullPath, "/home/local/.claude/projects/-home-local-projects-myproject/abc123.jsonl")
	}
}

func TestUpdateIndexFile_NoChange(t *testing.T) {
	tempDir := t.TempDir()
	projectDir := filepath.Join(tempDir, "-home-canonical-git-myproject")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a sessions-index.json that's already canonical
	index := conv.SessionsIndex{
		Entries: []conv.SessionEntry{
			{
				SessionID:   "abc123",
				ProjectPath: "/home/canonical/git/myproject",
				FullPath:    "/home/canonical/.claude/projects/-home-canonical-git-myproject/abc123.jsonl",
			},
		},
	}
	indexData, _ := json.MarshalIndent(index, "", "  ")
	indexPath := filepath.Join(projectDir, "sessions-index.json")
	if err := os.WriteFile(indexPath, indexData, 0644); err != nil {
		t.Fatal(err)
	}

	// Create a dummy session file
	sessionFile := filepath.Join(projectDir, "abc123.jsonl")
	if err := os.WriteFile(sessionFile, []byte(`{"type":"test"}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Canonicalize - should report no change since it's already canonical
	wasFixed, err := updateIndexFile(indexPath, testConfig(), "")
	if err != nil {
		t.Fatalf("updateIndexFile failed: %v", err)
	}
	if wasFixed {
		t.Error("expected no changes to already canonical file")
	}
}

func TestUpdateSessionPaths_MultipleProjects(t *testing.T) {
	tempDir := t.TempDir()

	// Create two project directories with local paths
	projects := []string{"-home-local-projects-myproject", "-home-local-projects-other"}
	for _, proj := range projects {
		projectDir := filepath.Join(tempDir, proj)
		if err := os.MkdirAll(projectDir, 0755); err != nil {
			t.Fatal(err)
		}

		index := conv.SessionsIndex{
			Entries: []conv.SessionEntry{
				{
					SessionID:   "session1",
					ProjectPath: "/home/local/projects/something",
				},
			},
		}
		indexData, _ := json.MarshalIndent(index, "", "  ")
		if err := os.WriteFile(filepath.Join(projectDir, "sessions-index.json"), indexData, 0644); err != nil {
			t.Fatal(err)
		}

		// Create dummy session file
		if err := os.WriteFile(filepath.Join(projectDir, "session1.jsonl"), []byte(`{}`), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Canonicalize both
	fixed, err := updateSessionPaths(tempDir, testConfig(), "")
	if err != nil {
		t.Fatalf("updateSessionPaths failed: %v", err)
	}
	if fixed != 2 {
		t.Errorf("expected 2 files fixed, got %d", fixed)
	}

	// Verify both are canonicalized
	for _, proj := range projects {
		indexPath := filepath.Join(tempDir, proj, "sessions-index.json")
		data, _ := os.ReadFile(indexPath)
		var result conv.SessionsIndex
		json.Unmarshal(data, &result)

		if result.Entries[0].ProjectPath != "/home/canonical/git/something" {
			t.Errorf("project %s not canonicalized: %q", proj, result.Entries[0].ProjectPath)
		}
	}
}

func TestRepairDirectory_CanonicalizesSyncDir(t *testing.T) {
	tempDir := t.TempDir()

	// Create a project dir with local path naming
	localProjectDir := filepath.Join(tempDir, "-home-local-projects-myproject")
	if err := os.MkdirAll(localProjectDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create index with local paths
	index := conv.SessionsIndex{
		Entries: []conv.SessionEntry{
			{SessionID: "test1", ProjectPath: "/home/local/projects/myproject"},
		},
	}
	indexData, _ := json.MarshalIndent(index, "", "  ")
	if err := os.WriteFile(filepath.Join(localProjectDir, "sessions-index.json"), indexData, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localProjectDir, "test1.jsonl"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Repair as sync dir (canonicalize - empty localHome)
	count, err := repairDirectory(tempDir, testConfig(), false, "")
	if err != nil {
		t.Fatalf("repairDirectory failed: %v", err)
	}

	// Should have merged and created canonical directory
	if count != 1 {
		t.Errorf("expected 1 merge, got %d", count)
	}

	// Canonical dir should exist
	canonicalDir := filepath.Join(tempDir, "-home-canonical-git-myproject")
	if _, err := os.Stat(canonicalDir); os.IsNotExist(err) {
		t.Error("canonical directory was not created")
	}

	// Local dir should be removed
	if _, err := os.Stat(localProjectDir); !os.IsNotExist(err) {
		t.Error("local directory was not removed after merge")
	}

	// Check paths in index are canonicalized
	indexPath := filepath.Join(canonicalDir, "sessions-index.json")
	data, _ := os.ReadFile(indexPath)
	var result conv.SessionsIndex
	json.Unmarshal(data, &result)

	if result.Entries[0].ProjectPath != "/home/canonical/git/myproject" {
		t.Errorf("path not canonicalized: %q", result.Entries[0].ProjectPath)
	}
}

func TestRepairDirectory_LocalizesLocalDir(t *testing.T) {
	tempDir := t.TempDir()

	// Create a project dir with canonical path naming
	canonicalProjectDir := filepath.Join(tempDir, "-home-canonical-git-myproject")
	if err := os.MkdirAll(canonicalProjectDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create index with canonical paths
	index := conv.SessionsIndex{
		Entries: []conv.SessionEntry{
			{SessionID: "test1", ProjectPath: "/home/canonical/git/myproject"},
		},
	}
	indexData, _ := json.MarshalIndent(index, "", "  ")
	if err := os.WriteFile(filepath.Join(canonicalProjectDir, "sessions-index.json"), indexData, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonicalProjectDir, "test1.jsonl"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Repair as local dir (localize - with localHome set)
	count, err := repairDirectory(tempDir, testConfig(), false, testLocalHome)
	if err != nil {
		t.Fatalf("repairDirectory failed: %v", err)
	}

	// Should have renamed the directory
	if count != 1 {
		t.Errorf("expected 1 directory to be renamed, got %d", count)
	}

	// Local dir should exist with content
	localProjectDir := filepath.Join(tempDir, "-home-local-projects-myproject")
	if _, err := os.Stat(localProjectDir); os.IsNotExist(err) {
		t.Error("local directory should have been created")
	}

	// Canonical dir should be removed
	if _, err := os.Stat(canonicalProjectDir); !os.IsNotExist(err) {
		t.Error("canonical directory should be removed after localizing")
	}

	// Check paths in index are localized
	indexPath := filepath.Join(localProjectDir, "sessions-index.json")
	data, _ := os.ReadFile(indexPath)
	var result conv.SessionsIndex
	json.Unmarshal(data, &result)

	if result.Entries[0].ProjectPath != "/home/local/projects/myproject" {
		t.Errorf("path not localized: got %q, want %q",
			result.Entries[0].ProjectPath, "/home/local/projects/myproject")
	}
}

func TestRepairDirectory_LocalizesLocalDir_AlreadyLocalStaysUnchanged(t *testing.T) {
	tempDir := t.TempDir()

	// Create a project dir already in local form
	localProjectDir := filepath.Join(tempDir, "-home-local-projects-myproject")
	if err := os.MkdirAll(localProjectDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create index with already localized paths
	index := conv.SessionsIndex{
		Entries: []conv.SessionEntry{
			{SessionID: "test1", ProjectPath: "/home/local/projects/myproject"},
		},
	}
	indexData, _ := json.MarshalIndent(index, "", "  ")
	if err := os.WriteFile(filepath.Join(localProjectDir, "sessions-index.json"), indexData, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localProjectDir, "test1.jsonl"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Repair as local dir - should be a no-op since already localized
	count, err := repairDirectory(tempDir, testConfig(), false, testLocalHome)
	if err != nil {
		t.Fatalf("repairDirectory failed: %v", err)
	}

	// Should not need any repairs
	if count != 0 {
		t.Errorf("expected 0 directories to be renamed (already local), got %d", count)
	}

	// Local dir should still exist
	if _, err := os.Stat(localProjectDir); os.IsNotExist(err) {
		t.Error("local directory should still exist")
	}
}

func TestRepairDirectory_DryRun(t *testing.T) {
	tempDir := t.TempDir()

	// Create a project dir with local path naming
	localProjectDir := filepath.Join(tempDir, "-home-local-projects-myproject")
	if err := os.MkdirAll(localProjectDir, 0755); err != nil {
		t.Fatal(err)
	}

	index := conv.SessionsIndex{
		Entries: []conv.SessionEntry{
			{SessionID: "test1", ProjectPath: "/home/local/projects/myproject"},
		},
	}
	indexData, _ := json.MarshalIndent(index, "", "  ")
	if err := os.WriteFile(filepath.Join(localProjectDir, "sessions-index.json"), indexData, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localProjectDir, "test1.jsonl"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Dry run
	count, err := repairDirectory(tempDir, testConfig(), true, "")
	if err != nil {
		t.Fatalf("repairDirectory dry-run failed: %v", err)
	}

	// Should report the merge would happen
	if count != 1 {
		t.Errorf("expected 1 merge to be reported, got %d", count)
	}

	// But local dir should still exist (dry run doesn't modify)
	if _, err := os.Stat(localProjectDir); os.IsNotExist(err) {
		t.Error("local directory should still exist in dry-run mode")
	}

	// And canonical dir should NOT exist
	canonicalDir := filepath.Join(tempDir, "-home-canonical-git-myproject")
	if _, err := os.Stat(canonicalDir); !os.IsNotExist(err) {
		t.Error("canonical directory should not be created in dry-run mode")
	}
}

func TestCanonicalizePath(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		expected string
	}{
		{
			name:     "local projects to canonical",
			path:     "/home/local/projects/myproject",
			expected: "/home/canonical/git/myproject",
		},
		{
			name:     "local home other to canonical",
			path:     "/home/local/Documents/notes",
			expected: "/home/canonical/Documents/notes",
		},
		{
			name:     "already canonical",
			path:     "/home/canonical/git/myproject",
			expected: "/home/canonical/git/myproject",
		},
		{
			name:     "unrelated path unchanged",
			path:     "/var/log/something",
			expected: "/var/log/something",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := canonicalizePath(tt.path, testConfig())
			if result != tt.expected {
				t.Errorf("canonicalizePath(%q) = %q, want %q", tt.path, result, tt.expected)
			}
		})
	}
}

func TestCanonicalizeEmbeddedProjectDir(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		expected string
	}{
		{
			name:     "fullPath with embedded local project dir",
			path:     "/home/canonical/.claude/projects/-home-local-projects-myproject/session.jsonl",
			expected: "/home/canonical/.claude/projects/-home-canonical-git-myproject/session.jsonl",
		},
		{
			name:     "fullPath with already canonical project dir",
			path:     "/home/canonical/.claude/projects/-home-canonical-git-myproject/session.jsonl",
			expected: "/home/canonical/.claude/projects/-home-canonical-git-myproject/session.jsonl",
		},
		{
			name:     "fullPath without session file (project dir only)",
			path:     "/home/canonical/.claude/projects/-home-local-projects-myproject",
			expected: "/home/canonical/.claude/projects/-home-canonical-git-myproject",
		},
		{
			name:     "path without .claude/projects marker unchanged",
			path:     "/home/canonical/some/other/path/session.jsonl",
			expected: "/home/canonical/some/other/path/session.jsonl",
		},
		{
			name:     "complete local fullPath gets fully canonicalized",
			path:     "/home/local/.claude/projects/-home-local-projects-myproject/abc.jsonl",
			expected: "/home/canonical/.claude/projects/-home-canonical-git-myproject/abc.jsonl",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := canonicalizePath(tt.path, testConfig())
			if result != tt.expected {
				t.Errorf("canonicalizePath(%q) = %q, want %q", tt.path, result, tt.expected)
			}
		})
	}
}

func TestRepairRoundTrip_SyncThenLocal(t *testing.T) {
	// Simulate the full workflow: local paths -> sync (canonicalize) -> local machine (localize)
	syncDir := t.TempDir()
	localDir := t.TempDir()

	// Step 1: Create project with local paths (as if on local machine)
	localProjectDir := filepath.Join(syncDir, "-home-local-projects-myproject")
	if err := os.MkdirAll(localProjectDir, 0755); err != nil {
		t.Fatal(err)
	}

	index := conv.SessionsIndex{
		Entries: []conv.SessionEntry{
			{
				SessionID:   "test1",
				ProjectPath: "/home/local/projects/myproject",
				FullPath:    "/home/local/.claude/projects/-home-local-projects-myproject/test1.jsonl",
			},
		},
	}
	indexData, _ := json.MarshalIndent(index, "", "  ")
	if err := os.WriteFile(filepath.Join(localProjectDir, "sessions-index.json"), indexData, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localProjectDir, "test1.jsonl"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Step 2: Repair sync dir (canonicalize)
	_, err := repairDirectory(syncDir, testConfig(), false, "")
	if err != nil {
		t.Fatalf("sync repair failed: %v", err)
	}

	// Verify canonical dir was created in sync
	canonicalDir := filepath.Join(syncDir, "-home-canonical-git-myproject")
	if _, err := os.Stat(canonicalDir); os.IsNotExist(err) {
		t.Fatal("canonical directory not created")
	}

	// Verify local dir was removed in sync
	if _, err := os.Stat(localProjectDir); !os.IsNotExist(err) {
		t.Fatal("local directory should be removed from sync after canonicalize")
	}

	// Check sync paths are canonical
	syncIndexPath := filepath.Join(canonicalDir, "sessions-index.json")
	data, _ := os.ReadFile(syncIndexPath)
	var syncResult conv.SessionsIndex
	json.Unmarshal(data, &syncResult)

	if syncResult.Entries[0].ProjectPath != "/home/canonical/git/myproject" {
		t.Errorf("sync not canonicalized: %q", syncResult.Entries[0].ProjectPath)
	}

	// Step 3: Copy to local dir (simulating sync -> local copy)
	localCanonicalDir := filepath.Join(localDir, "-home-canonical-git-myproject")
	if err := os.MkdirAll(localCanonicalDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Copy files
	if err := os.WriteFile(filepath.Join(localCanonicalDir, "sessions-index.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localCanonicalDir, "test1.jsonl"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Step 4: Repair local dir with localHome (localize)
	count, err := repairDirectory(localDir, testConfig(), false, testLocalHome)
	if err != nil {
		t.Fatalf("local repair failed: %v", err)
	}

	// Should have renamed 1 directory
	if count != 1 {
		t.Errorf("expected 1 directory rename, got %d", count)
	}

	// Verify canonical dir was renamed to local form
	localMacDir := filepath.Join(localDir, "-home-local-projects-myproject")
	if _, err := os.Stat(localMacDir); os.IsNotExist(err) {
		t.Fatal("local directory should be created after localize")
	}

	// Verify canonical dir was removed in local
	if _, err := os.Stat(localCanonicalDir); !os.IsNotExist(err) {
		t.Fatal("canonical directory should be removed from local after localize")
	}

	// Check local paths are localized
	localIndexPath := filepath.Join(localMacDir, "sessions-index.json")
	localData, _ := os.ReadFile(localIndexPath)
	var localResult conv.SessionsIndex
	json.Unmarshal(localData, &localResult)

	if localResult.Entries[0].ProjectPath != "/home/local/projects/myproject" {
		t.Errorf("local not localized: got %q, want %q",
			localResult.Entries[0].ProjectPath, "/home/local/projects/myproject")
	}

	expectedFullPath := "/home/local/.claude/projects/-home-local-projects-myproject/test1.jsonl"
	if localResult.Entries[0].FullPath != expectedFullPath {
		t.Errorf("local fullPath not localized: got %q, want %q",
			localResult.Entries[0].FullPath, expectedFullPath)
	}
}

func TestRepairDirectory_MergeEquivalentDirs(t *testing.T) {
	tempDir := t.TempDir()

	// Create TWO equivalent directories
	canonicalDir := filepath.Join(tempDir, "-home-canonical-git-myproject")
	localDir := filepath.Join(tempDir, "-home-local-projects-myproject")

	for _, dir := range []string{canonicalDir, localDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}

	// Canonical dir has session1
	canonicalIndex := conv.SessionsIndex{
		Entries: []conv.SessionEntry{{SessionID: "session1", ProjectPath: "/home/canonical/git/myproject"}},
	}
	canonicalData, _ := json.MarshalIndent(canonicalIndex, "", "  ")
	os.WriteFile(filepath.Join(canonicalDir, "sessions-index.json"), canonicalData, 0644)
	os.WriteFile(filepath.Join(canonicalDir, "session1.jsonl"), []byte(`{"canonical":true}`), 0644)

	// Local dir has session2
	localIndex := conv.SessionsIndex{
		Entries: []conv.SessionEntry{{SessionID: "session2", ProjectPath: "/home/local/projects/myproject"}},
	}
	localData, _ := json.MarshalIndent(localIndex, "", "  ")
	os.WriteFile(filepath.Join(localDir, "sessions-index.json"), localData, 0644)
	os.WriteFile(filepath.Join(localDir, "session2.jsonl"), []byte(`{"local":true}`), 0644)

	// Repair (canonicalize)
	count, err := repairDirectory(tempDir, testConfig(), false, "")
	if err != nil {
		t.Fatalf("repairDirectory failed: %v", err)
	}

	if count != 1 {
		t.Errorf("expected 1 merge group, got %d", count)
	}

	// Only canonical dir should exist
	if _, err := os.Stat(canonicalDir); os.IsNotExist(err) {
		t.Error("canonical directory should still exist")
	}
	if _, err := os.Stat(localDir); !os.IsNotExist(err) {
		t.Error("local directory should be removed after merge")
	}

	// Canonical dir should have both session files
	if _, err := os.Stat(filepath.Join(canonicalDir, "session1.jsonl")); os.IsNotExist(err) {
		t.Error("session1.jsonl should exist in merged dir")
	}
	if _, err := os.Stat(filepath.Join(canonicalDir, "session2.jsonl")); os.IsNotExist(err) {
		t.Error("session2.jsonl should exist in merged dir")
	}
}

func TestFixProjectPathInconsistencies_WrongPath(t *testing.T) {
	tempDir := t.TempDir()

	// Create a project dir "-home-canonical-git-forge" but with a session that has
	// projectPath "/home/canonical/git" (missing /forge suffix) - this is the bug we're fixing
	projectDir := filepath.Join(tempDir, "-home-canonical-git-forge")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Session with WRONG projectPath (doesn't include /forge)
	index := conv.SessionsIndex{
		Entries: []conv.SessionEntry{
			{
				SessionID:   "abc123",
				ProjectPath: "/home/canonical/git", // Wrong! Should be /home/canonical/git/forge
				FullPath:    "/home/canonical/.claude/projects/-home-canonical-git-forge/abc123.jsonl",
			},
		},
	}
	indexData, _ := json.MarshalIndent(index, "", "  ")
	indexPath := filepath.Join(projectDir, "sessions-index.json")
	if err := os.WriteFile(indexPath, indexData, 0644); err != nil {
		t.Fatal(err)
	}

	// Create dummy session file
	if err := os.WriteFile(filepath.Join(projectDir, "abc123.jsonl"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Fix inconsistencies (canonicalize mode - empty localHome)
	fixed, err := fixProjectPathInconsistencies(tempDir, testConfig(), false, "")
	if err != nil {
		t.Fatalf("fixProjectPathInconsistencies failed: %v", err)
	}

	if fixed != 1 {
		t.Errorf("expected 1 entry fixed, got %d", fixed)
	}

	// Read and verify
	data, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}

	var result conv.SessionsIndex
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}

	// Should be corrected to match the directory
	if result.Entries[0].ProjectPath != "/home/canonical/git/forge" {
		t.Errorf("ProjectPath not corrected: got %q, want %q",
			result.Entries[0].ProjectPath, "/home/canonical/git/forge")
	}
}

func TestFixProjectPathInconsistencies_CorrectPath(t *testing.T) {
	tempDir := t.TempDir()

	// Create a project dir with correct projectPath - should not be modified
	projectDir := filepath.Join(tempDir, "-home-canonical-git-myproject")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Session with CORRECT projectPath
	index := conv.SessionsIndex{
		Entries: []conv.SessionEntry{
			{
				SessionID:   "abc123",
				ProjectPath: "/home/canonical/git/myproject", // Correct!
				FullPath:    "/home/canonical/.claude/projects/-home-canonical-git-myproject/abc123.jsonl",
			},
		},
	}
	indexData, _ := json.MarshalIndent(index, "", "  ")
	indexPath := filepath.Join(projectDir, "sessions-index.json")
	if err := os.WriteFile(indexPath, indexData, 0644); err != nil {
		t.Fatal(err)
	}

	// Create dummy session file
	if err := os.WriteFile(filepath.Join(projectDir, "abc123.jsonl"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Fix inconsistencies
	fixed, err := fixProjectPathInconsistencies(tempDir, testConfig(), false, "")
	if err != nil {
		t.Fatalf("fixProjectPathInconsistencies failed: %v", err)
	}

	if fixed != 0 {
		t.Errorf("expected 0 entries fixed (already correct), got %d", fixed)
	}
}

func TestFixProjectPathInconsistencies_LocalizesAfterFix(t *testing.T) {
	tempDir := t.TempDir()

	// Create a project dir with local naming but wrong projectPath
	projectDir := filepath.Join(tempDir, "-home-local-projects-forge")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Session with wrong projectPath (missing /forge)
	index := conv.SessionsIndex{
		Entries: []conv.SessionEntry{
			{
				SessionID:   "abc123",
				ProjectPath: "/home/local/projects", // Wrong! Should be /home/local/projects/forge
			},
		},
	}
	indexData, _ := json.MarshalIndent(index, "", "  ")
	indexPath := filepath.Join(projectDir, "sessions-index.json")
	if err := os.WriteFile(indexPath, indexData, 0644); err != nil {
		t.Fatal(err)
	}

	// Create dummy session file
	if err := os.WriteFile(filepath.Join(projectDir, "abc123.jsonl"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Fix with localHome (localize mode)
	fixed, err := fixProjectPathInconsistencies(tempDir, testConfig(), false, testLocalHome)
	if err != nil {
		t.Fatalf("fixProjectPathInconsistencies failed: %v", err)
	}

	if fixed != 1 {
		t.Errorf("expected 1 entry fixed, got %d", fixed)
	}

	// Read and verify
	data, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}

	var result conv.SessionsIndex
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}

	// Should be corrected to match the directory AND localized
	if result.Entries[0].ProjectPath != "/home/local/projects/forge" {
		t.Errorf("ProjectPath not corrected/localized: got %q, want %q",
			result.Entries[0].ProjectPath, "/home/local/projects/forge")
	}
}

func TestFixProjectPathInconsistencies_DryRun(t *testing.T) {
	tempDir := t.TempDir()

	// Create a project dir with wrong projectPath
	projectDir := filepath.Join(tempDir, "-home-canonical-git-forge")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}

	index := conv.SessionsIndex{
		Entries: []conv.SessionEntry{
			{
				SessionID:   "abc123",
				ProjectPath: "/home/canonical/git", // Wrong!
			},
		},
	}
	indexData, _ := json.MarshalIndent(index, "", "  ")
	indexPath := filepath.Join(projectDir, "sessions-index.json")
	if err := os.WriteFile(indexPath, indexData, 0644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(projectDir, "abc123.jsonl"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Dry run
	fixed, err := fixProjectPathInconsistencies(tempDir, testConfig(), true, "")
	if err != nil {
		t.Fatalf("fixProjectPathInconsistencies dry-run failed: %v", err)
	}

	if fixed != 1 {
		t.Errorf("expected 1 entry reported, got %d", fixed)
	}

	// Read and verify - should NOT be modified
	data, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}

	var result conv.SessionsIndex
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}

	// Should still have the wrong path (dry run didn't change it)
	if result.Entries[0].ProjectPath != "/home/canonical/git" {
		t.Errorf("ProjectPath was modified in dry-run: got %q, want %q",
			result.Entries[0].ProjectPath, "/home/canonical/git")
	}
}

func TestFixProjectPathInconsistencies_MultipleEntries(t *testing.T) {
	tempDir := t.TempDir()

	projectDir := filepath.Join(tempDir, "-home-canonical-git-forge")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}

	// One wrong, one correct
	index := conv.SessionsIndex{
		Entries: []conv.SessionEntry{
			{
				SessionID:   "wrong123",
				ProjectPath: "/home/canonical/git", // Wrong!
			},
			{
				SessionID:   "correct456",
				ProjectPath: "/home/canonical/git/forge", // Correct
			},
		},
	}
	indexData, _ := json.MarshalIndent(index, "", "  ")
	indexPath := filepath.Join(projectDir, "sessions-index.json")
	if err := os.WriteFile(indexPath, indexData, 0644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(projectDir, "wrong123.jsonl"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "correct456.jsonl"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	fixed, err := fixProjectPathInconsistencies(tempDir, testConfig(), false, "")
	if err != nil {
		t.Fatalf("fixProjectPathInconsistencies failed: %v", err)
	}

	if fixed != 1 {
		t.Errorf("expected 1 entry fixed (only wrong one), got %d", fixed)
	}

	// Verify both are now correct
	data, _ := os.ReadFile(indexPath)
	var result conv.SessionsIndex
	json.Unmarshal(data, &result)

	for _, entry := range result.Entries {
		if entry.ProjectPath != "/home/canonical/git/forge" {
			t.Errorf("entry %s has wrong path: %q", entry.SessionID, entry.ProjectPath)
		}
	}
}

func TestUpdateIndexFile_IncludesUnindexedSessions(t *testing.T) {
	tempDir := t.TempDir()
	projectDir := filepath.Join(tempDir, "-home-canonical-git-myproject")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create sessions-index.json with ONE session
	index := conv.SessionsIndex{
		Version: 1,
		Entries: []conv.SessionEntry{
			{
				SessionID:   "indexed-session",
				ProjectPath: "/home/canonical/git/myproject",
			},
		},
	}
	indexData, _ := json.MarshalIndent(index, "", "  ")
	indexPath := filepath.Join(projectDir, "sessions-index.json")
	if err := os.WriteFile(indexPath, indexData, 0644); err != nil {
		t.Fatal(err)
	}

	// Create the indexed session file
	if err := os.WriteFile(filepath.Join(projectDir, "indexed-session.jsonl"), []byte(`{"type":"user","message":{"role":"user","content":"indexed prompt"}}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Create an UNINDEXED session file (valid UUID, not in index)
	unindexedID := "12345678-1234-1234-1234-123456789abc"
	unindexedContent := `{"type":"user","timestamp":"2026-01-01T10:00:00Z","message":{"role":"user","content":"unindexed prompt"}}`
	if err := os.WriteFile(filepath.Join(projectDir, unindexedID+".jsonl"), []byte(unindexedContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Run updateIndexFile - should pick up the unindexed session
	config := testConfig()
	modified, err := updateIndexFile(indexPath, config, "")
	if err != nil {
		t.Fatalf("updateIndexFile failed: %v", err)
	}

	if !modified {
		t.Error("expected index to be modified (unindexed session added)")
	}

	// Load result and verify unindexed session was added
	data, _ := os.ReadFile(indexPath)
	var result conv.SessionsIndex
	json.Unmarshal(data, &result)

	if len(result.Entries) != 2 {
		t.Errorf("expected 2 entries after including unindexed, got %d", len(result.Entries))
	}

	// Verify both sessions are present
	foundIndexed := false
	foundUnindexed := false
	for _, entry := range result.Entries {
		if entry.SessionID == "indexed-session" {
			foundIndexed = true
		}
		if entry.SessionID == unindexedID {
			foundUnindexed = true
			// Verify the unindexed session has display data
			if entry.FirstPrompt == "" {
				t.Error("unindexed session should have FirstPrompt populated")
			}
		}
	}

	if !foundIndexed {
		t.Error("indexed session should still be present")
	}
	if !foundUnindexed {
		t.Error("unindexed session should be added to index")
	}
}

func TestRepairDirectory_IncludesUnindexedSessions(t *testing.T) {
	tempDir := t.TempDir()

	// Create a project directory
	projectDir := filepath.Join(tempDir, "-home-canonical-git-myproject")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create sessions-index.json with ONE session
	index := conv.SessionsIndex{
		Version: 1,
		Entries: []conv.SessionEntry{
			{
				SessionID:   "indexed-session",
				ProjectPath: "/home/canonical/git/myproject",
			},
		},
	}
	indexData, _ := json.MarshalIndent(index, "", "  ")
	if err := os.WriteFile(filepath.Join(projectDir, "sessions-index.json"), indexData, 0644); err != nil {
		t.Fatal(err)
	}

	// Create the indexed session file
	if err := os.WriteFile(filepath.Join(projectDir, "indexed-session.jsonl"), []byte(`{"type":"user","message":{"role":"user","content":"indexed"}}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Create an UNINDEXED session file
	unindexedID := "12345678-1234-1234-1234-123456789abc"
	unindexedContent := `{"type":"user","timestamp":"2026-01-01T10:00:00Z","message":{"role":"user","content":"unindexed prompt"}}`
	if err := os.WriteFile(filepath.Join(projectDir, unindexedID+".jsonl"), []byte(unindexedContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Run repairDirectory - should include unindexed sessions
	config := testConfig()
	_, err := repairDirectory(tempDir, config, false, "")
	if err != nil {
		t.Fatalf("repairDirectory failed: %v", err)
	}

	// Load result and verify both sessions are present
	indexPath := filepath.Join(projectDir, "sessions-index.json")
	data, _ := os.ReadFile(indexPath)
	var result conv.SessionsIndex
	json.Unmarshal(data, &result)

	if len(result.Entries) != 2 {
		t.Errorf("expected 2 entries (indexed + unindexed), got %d", len(result.Entries))
	}

	// Verify the unindexed session was included
	foundUnindexed := false
	for _, entry := range result.Entries {
		if entry.SessionID == unindexedID {
			foundUnindexed = true
			break
		}
	}

	if !foundUnindexed {
		t.Error("unindexed session should be included after repair")
	}
}

func TestUpdateIndexFile_FiltersTombstonedSessions(t *testing.T) {
	tempDir := t.TempDir()
	cleanup := setTestHome(t, tempDir)
	defer cleanup()

	projectDir := filepath.Join(tempDir, "-home-canonical-git-myproject")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create sessions-index.json with two sessions
	index := conv.SessionsIndex{
		Version: 1,
		Entries: []conv.SessionEntry{
			{
				SessionID:   "session-alive",
				ProjectPath: "/home/canonical/git/myproject",
			},
			{
				SessionID:   "session-deleted",
				ProjectPath: "/home/canonical/git/myproject",
			},
		},
	}
	indexData, _ := json.MarshalIndent(index, "", "  ")
	indexPath := filepath.Join(projectDir, "sessions-index.json")
	if err := os.WriteFile(indexPath, indexData, 0644); err != nil {
		t.Fatal(err)
	}

	// Create deletions.json marking one session as tombstoned
	deletions := `{
		"version": 1,
		"entries": [
			{"sessionId": "session-deleted", "deletedAt": "2026-01-31T10:00:00Z", "deletedBy": "testhost"}
		]
	}`
	if err := os.WriteFile(filepath.Join(projectDir, "deletions.json"), []byte(deletions), 0644); err != nil {
		t.Fatal(err)
	}

	// Run updateIndexFile
	config := testConfig()
	modified, err := updateIndexFile(indexPath, config, "")
	if err != nil {
		t.Fatalf("updateIndexFile failed: %v", err)
	}

	if !modified {
		t.Error("expected index to be modified (tombstoned session filtered)")
	}

	// Load result and verify tombstoned session was removed
	data, _ := os.ReadFile(indexPath)
	var result conv.SessionsIndex
	json.Unmarshal(data, &result)

	if len(result.Entries) != 1 {
		t.Errorf("expected 1 entry after filtering, got %d", len(result.Entries))
	}

	if result.Entries[0].SessionID != "session-alive" {
		t.Errorf("expected session-alive to remain, got %s", result.Entries[0].SessionID)
	}
}
