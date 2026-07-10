package setup

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// installWorktreeContainerGitPlaceholder plants an empty ".git" placeholder
// FILE in the sandbox-writable worktree container so an OS sandbox does not
// materialize a bogus ".git" DIRECTORY there.
//
// Background: Claude Code's Bash sandbox denies writes to git internals
// (".git/config", ".git/hooks", …) inside every writable root by bind-mounting
// /dev/null over those paths. To hang the mount it must create the ".git"
// directory — even in a worktree CONTAINER (e.g. ~/git) that is not itself a
// repository. That phantom ".git" makes Go's buildvcs walk up, treat the
// container as the repo root, run `git status`, and fail every `go build` under
// it with "error obtaining VCS status: exit status 128". An empty ".git" FILE
// occupies the path so the sandbox leaves it alone (it only creates the
// directory when the path is absent), while Go — which treats only a ".git"
// DIRECTORY as a VCS root — ignores the file and skips stamping cleanly. Real
// git is unaffected: every worktree has its own ".git" and git stops there,
// never walking up to the container.
//
// The container is derived from the current working directory, so this is a
// no-op when `tclaude setup` is not run inside a git worktree whose parent is a
// grantable sandbox root (see harness.SandboxWorktreeContainer).
func installWorktreeContainerGitPlaceholder(assumeYes bool) {
	cwd, err := os.Getwd()
	if err != nil {
		return
	}
	commonDir, err := harness.GitCommonDir(cwd)
	if err != nil || commonDir == "" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	container := harness.SandboxWorktreeContainer(commonDir, home)
	if container == "" {
		// Not inside a worktree with a grantable container — nothing to
		// reconcile, and no section header so a plain setup stays quiet.
		return
	}
	fmt.Println("\n=== Sandbox .git Placeholder ===")
	applyGitPlaceholder(filepath.Join(container, ".git"), assumeYes)
}

// applyGitPlaceholder reconciles a single container ".git" path to the empty
// placeholder file. Split from the resolution above so it is testable against a
// temp directory without a real git checkout. Best-effort: every failure prints
// a note and returns rather than aborting setup.
//
//   - absent                → create the empty placeholder file;
//   - already a regular file → no-op (idempotent);
//   - a DIRECTORY            → warn; if it looks like the sandbox phantom, offer
//     to replace it (respecting --yes); if it looks like a real repository,
//     refuse and tell the user to inspect it — tclaude never deletes a
//     directory that might hold a repo;
//   - anything else (symlink…) → surface it and leave it untouched.
func applyGitPlaceholder(placeholder string, assumeYes bool) {
	info, err := os.Lstat(placeholder)
	if os.IsNotExist(err) {
		if werr := os.WriteFile(placeholder, nil, 0o644); werr != nil {
			fmt.Printf("  Could not create sandbox .git placeholder at %s: %v\n", placeholder, werr)
			return
		}
		fmt.Printf("✓ Created sandbox .git placeholder at %s\n", placeholder)
		return
	}
	if err != nil {
		fmt.Printf("  Could not inspect %s: %v\n", placeholder, err)
		return
	}
	if info.Mode().IsRegular() {
		fmt.Printf("✓ Sandbox .git placeholder already present at %s\n", placeholder)
		return
	}
	if !info.IsDir() {
		fmt.Printf("  %s exists but is neither a file nor a directory (%s) — leaving as-is\n",
			placeholder, info.Mode())
		return
	}

	// It is a directory. Warn, then act on its shape.
	fmt.Printf("⚠ %s is a DIRECTORY.\n", placeholder)
	if !looksLikeSandboxGitPhantom(placeholder) {
		fmt.Printf("  It has real git content, so it may be an actual repository — tclaude\n")
		fmt.Printf("  will not delete it. If %s is not meant to be a git repository,\n",
			filepath.Dir(placeholder))
		fmt.Printf("  remove %s yourself and re-run setup.\n", placeholder)
		return
	}
	fmt.Println("  This is the OS-sandbox phantom .git — it breaks `go build` under this")
	fmt.Println("  directory with 'VCS status: exit 128'. Replacing it with an empty .git")
	fmt.Println("  file fixes builds and preserves sibling-worktree support.")
	if !askYesNo("Replace it with an empty .git placeholder file?", true, assumeYes) {
		fmt.Println("  Left as-is. Builds under this directory may fail with 'exit 128';")
		fmt.Println("  set GOFLAGS=-buildvcs=false, or re-run setup to fix it later.")
		return
	}
	// Re-verify immediately before the destructive step. The confirmation
	// prompt above is a wide window (human think-time) in which real repo
	// content could appear; never RemoveAll something that has stopped looking
	// like the phantom since we checked.
	if !looksLikeSandboxGitPhantom(placeholder) {
		fmt.Printf("  %s changed and no longer looks like the sandbox phantom — leaving it untouched.\n",
			placeholder)
		return
	}
	if rerr := os.RemoveAll(placeholder); rerr != nil {
		fmt.Printf("  Could not remove %s: %v\n", placeholder, rerr)
		return
	}
	if werr := os.WriteFile(placeholder, nil, 0o644); werr != nil {
		fmt.Printf("  Removed the phantom directory but could not create the placeholder file at %s: %v\n",
			placeholder, werr)
		return
	}
	fmt.Printf("✓ Replaced the phantom .git directory with an empty placeholder at %s\n", placeholder)
}

// looksLikeSandboxGitPhantom reports whether dir is the OS sandbox's phantom
// ".git": it holds only the mount-stub entries the sandbox creates
// (config, config.lock, config.worktree, hooks) — or nothing at all — and none
// of the markers of a real repository (HEAD, objects, refs, …). Conservative:
// any unexpected entry makes it return false so a real repository is never
// mistaken for the phantom and deleted.
func looksLikeSandboxGitPhantom(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	stub := map[string]bool{
		"config":          true,
		"config.lock":     true,
		"config.worktree": true,
		"hooks":           true,
	}
	for _, e := range entries {
		if !stub[e.Name()] {
			return false
		}
	}
	return true
}
