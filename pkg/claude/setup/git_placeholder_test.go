package setup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestApplyGitPlaceholderCreatesFileWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	placeholder := filepath.Join(dir, ".git")

	applyGitPlaceholder(placeholder, true)

	info, err := os.Lstat(placeholder)
	if err != nil {
		t.Fatalf("placeholder not created: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("placeholder is not a regular file: mode %s", info.Mode())
	}
	if info.Size() != 0 {
		t.Fatalf("placeholder is not empty: size %d", info.Size())
	}
}

func TestApplyGitPlaceholderLeavesExistingFile(t *testing.T) {
	dir := t.TempDir()
	placeholder := filepath.Join(dir, ".git")
	if err := os.WriteFile(placeholder, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	applyGitPlaceholder(placeholder, true)

	got, err := os.ReadFile(placeholder)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "keep" {
		t.Fatalf("existing file was modified: got %q, want %q", got, "keep")
	}
}

func TestApplyGitPlaceholderReplacesPhantomDir(t *testing.T) {
	dir := t.TempDir()
	placeholder := filepath.Join(dir, ".git")
	if err := os.Mkdir(placeholder, 0o755); err != nil {
		t.Fatal(err)
	}
	// The phantom holds only the sandbox mount-stub entries.
	for _, name := range []string{"config", "config.lock", "config.worktree", "hooks"} {
		if err := os.WriteFile(filepath.Join(placeholder, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	applyGitPlaceholder(placeholder, true) // assumeYes → replace

	info, err := os.Lstat(placeholder)
	if err != nil {
		t.Fatalf("placeholder missing after replace: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("phantom dir was not replaced with a file: mode %s", info.Mode())
	}
}

func TestApplyGitPlaceholderRefusesRealRepoDir(t *testing.T) {
	dir := t.TempDir()
	placeholder := filepath.Join(dir, ".git")
	if err := os.Mkdir(placeholder, 0o755); err != nil {
		t.Fatal(err)
	}
	// Real-repo markers must never be treated as the phantom.
	if err := os.WriteFile(filepath.Join(placeholder, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(placeholder, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}

	applyGitPlaceholder(placeholder, true) // assumeYes must NOT delete a real repo

	info, err := os.Lstat(placeholder)
	if err != nil {
		t.Fatalf("real repo dir was removed: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("real repo dir was replaced: mode %s", info.Mode())
	}
	if _, err := os.Stat(filepath.Join(placeholder, "HEAD")); err != nil {
		t.Fatalf("real repo content was disturbed: %v", err)
	}
}

func TestLooksLikeSandboxGitPhantom(t *testing.T) {
	t.Run("empty dir is phantom", func(t *testing.T) {
		dir := t.TempDir()
		if !looksLikeSandboxGitPhantom(dir) {
			t.Fatal("empty dir should be treated as phantom (nothing real to lose)")
		}
	})
	t.Run("only stub entries is phantom", func(t *testing.T) {
		dir := t.TempDir()
		for _, name := range []string{"config", "hooks", "config.lock", "config.worktree"} {
			if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if !looksLikeSandboxGitPhantom(dir) {
			t.Fatal("stub-only dir should be phantom")
		}
	})
	t.Run("HEAD present is not phantom", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "HEAD"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if looksLikeSandboxGitPhantom(dir) {
			t.Fatal("a dir containing HEAD must not be treated as the phantom")
		}
	})
}
