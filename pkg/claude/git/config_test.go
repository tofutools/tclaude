package git

import (
	"testing"
)

// Test paths - these are just string values for testing path transformations,
// not actual filesystem paths. Using generic names for clarity.
const (
	canonicalHome = "/home/canonical"
	localHome     = "/home/local"
	canonicalGit  = "/home/canonical/git"
	localGit      = "/home/local/projects"
)

func TestProjectDirToPath(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"-home-canonical-git-myproject", "/home/canonical/git/myproject"},
		{"-home-local-projects-myproject", "/home/local/projects/myproject"},
		{"home-canonical-git-myproject", "/home/canonical/git/myproject"}, // without leading dash
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := ProjectDirToPath(tt.input)
			if result != tt.expected {
				t.Errorf("ProjectDirToPath(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestPathToProjectDir(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"/home/canonical/git/myproject", "-home-canonical-git-myproject"},
		{"/home/local/projects/myproject", "-home-local-projects-myproject"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := PathToProjectDir(tt.input)
			if result != tt.expected {
				t.Errorf("PathToProjectDir(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestCanonicalizeProjectDir_HomesOnly(t *testing.T) {
	config := &SyncConfig{
		Homes: []string{canonicalHome, localHome},
	}

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "already canonical",
			input:    "-home-canonical-git-myproject",
			expected: "-home-canonical-git-myproject",
		},
		{
			name:     "local to canonical",
			input:    "-home-local-git-myproject",
			expected: "-home-canonical-git-myproject",
		},
		{
			name:     "unrelated path unchanged",
			input:    "-var-log-something",
			expected: "-var-log-something",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := config.CanonicalizeProjectDir(tt.input)
			if result != tt.expected {
				t.Errorf("CanonicalizeProjectDir(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestCanonicalizeProjectDir_DirsOnly(t *testing.T) {
	config := &SyncConfig{
		Dirs: [][]string{
			{canonicalGit, localGit},
		},
	}

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "already canonical",
			input:    "-home-canonical-git-myproject",
			expected: "-home-canonical-git-myproject",
		},
		{
			name:     "local projects to canonical git",
			input:    "-home-local-projects-myproject",
			expected: "-home-canonical-git-myproject",
		},
		{
			name:     "unrelated local path unchanged",
			input:    "-home-local-Documents-notes",
			expected: "-home-local-Documents-notes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := config.CanonicalizeProjectDir(tt.input)
			if result != tt.expected {
				t.Errorf("CanonicalizeProjectDir(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestCanonicalizeProjectDir_DirsAndHomes(t *testing.T) {
	// Real-world scenario: dirs mapping first, then homes
	config := &SyncConfig{
		Homes: []string{canonicalHome, localHome},
		Dirs: [][]string{
			{canonicalGit, localGit},
		},
	}

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "already canonical",
			input:    "-home-canonical-git-myproject",
			expected: "-home-canonical-git-myproject",
		},
		{
			name:     "local projects to canonical git",
			input:    "-home-local-projects-myproject",
			expected: "-home-canonical-git-myproject",
		},
		{
			name:     "local home other dir uses homes mapping",
			input:    "-home-local-Documents-notes",
			expected: "-home-canonical-Documents-notes",
		},
		{
			name:     "local non-projects git uses homes mapping only",
			input:    "-home-local-git-work-project",
			expected: "-home-canonical-git-work-project",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := config.CanonicalizeProjectDir(tt.input)
			if result != tt.expected {
				t.Errorf("CanonicalizeProjectDir(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestFindEquivalentProjectDirs(t *testing.T) {
	config := &SyncConfig{
		Homes: []string{canonicalHome, localHome},
		Dirs: [][]string{
			{canonicalGit, localGit},
		},
	}

	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{
			name:  "canonical git project",
			input: "-home-canonical-git-myproject",
			expected: []string{
				"-home-canonical-git-myproject",
				"-home-local-git-myproject",      // from homes mapping
				"-home-local-projects-myproject", // from dirs mapping
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := config.FindEquivalentProjectDirs(tt.input)

			// Check that all expected are present
			for _, exp := range tt.expected {
				found := false
				for _, r := range result {
					if r == exp {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("FindEquivalentProjectDirs(%q) missing %q, got %v", tt.input, exp, result)
				}
			}
		})
	}
}

func TestNilConfig(t *testing.T) {
	var config *SyncConfig
	result := config.CanonicalizeProjectDir("-home-canonical-git-myproject")
	if result != "-home-canonical-git-myproject" {
		t.Errorf("nil config should return input unchanged, got %q", result)
	}
}

func TestEmptyConfig(t *testing.T) {
	config := &SyncConfig{}
	result := config.CanonicalizeProjectDir("-home-canonical-git-myproject")
	if result != "-home-canonical-git-myproject" {
		t.Errorf("empty config should return input unchanged, got %q", result)
	}
}

func TestLocalizePath_ToLocal(t *testing.T) {
	config := &SyncConfig{
		Homes: []string{canonicalHome, localHome},
		Dirs:  [][]string{{canonicalGit, localGit}},
	}

	tests := []struct {
		name      string
		path      string
		localHome string
		expected  string
	}{
		{
			name:      "canonical to local with dirs mapping",
			path:      "/home/canonical/git/myproject",
			localHome: localHome,
			expected:  "/home/local/projects/myproject",
		},
		{
			name:      "canonical to local with homes only",
			path:      "/home/canonical/Documents/notes",
			localHome: localHome,
			expected:  "/home/local/Documents/notes",
		},
		{
			name:      "already local path unchanged",
			path:      "/home/local/projects/myproject",
			localHome: localHome,
			expected:  "/home/local/projects/myproject",
		},
		{
			name:      "unrelated path unchanged",
			path:      "/var/log/something",
			localHome: localHome,
			expected:  "/var/log/something",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := config.LocalizePath(tt.path, tt.localHome)
			if result != tt.expected {
				t.Errorf("LocalizePath(%q, %q) = %q, want %q", tt.path, tt.localHome, result, tt.expected)
			}
		})
	}
}

func TestLocalizePath_ToCanonical(t *testing.T) {
	// When localHome matches canonical, non-local paths should be converted to canonical (local)
	config := &SyncConfig{
		Homes: []string{canonicalHome, localHome},
		Dirs:  [][]string{{canonicalGit, localGit}},
	}

	tests := []struct {
		name      string
		path      string
		localHome string
		expected  string
	}{
		{
			name:      "canonical stays canonical when on canonical machine",
			path:      "/home/canonical/git/myproject",
			localHome: canonicalHome,
			expected:  "/home/canonical/git/myproject",
		},
		{
			name:      "other machine path gets converted to local (canonical) on canonical machine",
			path:      "/home/local/projects/myproject",
			localHome: canonicalHome,
			expected:  "/home/canonical/git/myproject", // dirs mapping converts local→canonical
		},
		{
			name:      "other machine home path gets converted to local (canonical)",
			path:      "/home/local/Documents/notes",
			localHome: canonicalHome,
			expected:  "/home/canonical/Documents/notes", // homes mapping converts local→canonical
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := config.LocalizePath(tt.path, tt.localHome)
			if result != tt.expected {
				t.Errorf("LocalizePath(%q, %q) = %q, want %q", tt.path, tt.localHome, result, tt.expected)
			}
		})
	}
}

func TestLocalizePath_NilConfig(t *testing.T) {
	var config *SyncConfig
	result := config.LocalizePath("/home/canonical/git/myproject", localHome)
	if result != "/home/canonical/git/myproject" {
		t.Errorf("nil config should return path unchanged, got %q", result)
	}
}

func TestLocalizePath_EmptyConfig(t *testing.T) {
	config := &SyncConfig{}
	result := config.LocalizePath("/home/canonical/git/myproject", localHome)
	if result != "/home/canonical/git/myproject" {
		t.Errorf("empty config should return path unchanged, got %q", result)
	}
}

func TestLocalizePath_FullPathWithProjectDir(t *testing.T) {
	config := &SyncConfig{
		Homes: []string{canonicalHome, localHome},
		Dirs:  [][]string{{canonicalGit, localGit}},
	}

	// FullPath contains the .claude path which should also be localized,
	// including the embedded project directory name
	path := "/home/canonical/.claude/projects/-home-canonical-git-myproject/session.jsonl"

	result := config.LocalizePath(path, localHome)
	// Both home prefix AND embedded project dir should be localized
	expected := "/home/local/.claude/projects/-home-local-projects-myproject/session.jsonl"

	if result != expected {
		t.Errorf("LocalizePath(%q, %q) = %q, want %q", path, localHome, result, expected)
	}
}

func TestLocalizeProjectDir(t *testing.T) {
	config := &SyncConfig{
		Homes: []string{canonicalHome, localHome},
		Dirs:  [][]string{{canonicalGit, localGit}},
	}

	tests := []struct {
		name       string
		projectDir string
		localHome  string
		expected   string
	}{
		{
			name:       "canonical to local with dirs mapping",
			projectDir: "-home-canonical-git-myproject",
			localHome:  localHome,
			expected:   "-home-local-projects-myproject",
		},
		{
			name:       "canonical to local with homes only (no dirs match)",
			projectDir: "-home-canonical-Documents-notes",
			localHome:  localHome,
			expected:   "-home-local-Documents-notes",
		},
		{
			name:       "already local stays unchanged",
			projectDir: "-home-local-projects-myproject",
			localHome:  localHome,
			expected:   "-home-local-projects-myproject",
		},
		{
			name:       "canonical stays canonical on canonical machine",
			projectDir: "-home-canonical-git-myproject",
			localHome:  canonicalHome,
			expected:   "-home-canonical-git-myproject",
		},
		{
			name:       "unrelated path unchanged",
			projectDir: "-var-log-something",
			localHome:  localHome,
			expected:   "-var-log-something",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := config.LocalizeProjectDir(tt.projectDir, tt.localHome)
			if result != tt.expected {
				t.Errorf("LocalizeProjectDir(%q, %q) = %q, want %q", tt.projectDir, tt.localHome, result, tt.expected)
			}
		})
	}
}

func TestLocalizeProjectDir_NilConfig(t *testing.T) {
	var config *SyncConfig
	result := config.LocalizeProjectDir("-home-canonical-git-myproject", localHome)
	if result != "-home-canonical-git-myproject" {
		t.Errorf("nil config should return input unchanged, got %q", result)
	}
}

func TestLocalizeProjectDir_EmptyConfig(t *testing.T) {
	config := &SyncConfig{}
	result := config.LocalizeProjectDir("-home-canonical-git-myproject", localHome)
	if result != "-home-canonical-git-myproject" {
		t.Errorf("empty config should return input unchanged, got %q", result)
	}
}
