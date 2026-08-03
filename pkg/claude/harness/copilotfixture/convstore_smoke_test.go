package copilotfixture_test

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/copilotfixture"
)

// The real-binary evidence behind tclaude's Copilot conversation store.
//
// Everything the store reads is undocumented runtime state: that Copilot keeps
// one directory per session under <COPILOT_HOME>/session-state, that
// workspace.yaml carries the identity, cwd, git context, title and timestamps,
// that `user_named` distinguishes an operator title from a generated summary,
// and that a resume APPENDS to the existing events.jsonl instead of starting a
// new log. Unit tests in the harness package cover the degraded shapes a live
// CLI cannot be asked to produce; this test covers the one thing they cannot —
// that the layout is real.
//
// It deliberately drives the REGISTERED production store through
// COPILOT_HOME rather than an internal constructor, so the path resolution
// tclaude actually performs is under test too. If a future CLI moves or
// renames any of this, tclaude's conversation list silently empties in the
// field; here it fails loudly instead.

// convStore is the registered Copilot store, pointed at a fixture home.
func convStore(t *testing.T, home string) harness.ConvStore {
	t.Helper()
	t.Setenv(harness.CopilotHomeEnvVar, home)
	h := harness.MustGet(harness.CopilotName)
	require.True(t, h.SupportsConvs(), "copilot descriptor must expose a ConvStore")
	return h.Convs
}

// TestCopilotConvStoreReadsRealSessionState runs a fresh turn and a resume,
// then asserts the production store recovers the conversation from what the
// CLI actually wrote — including that the resume stayed ONE conversation.
func TestCopilotConvStoreReadsRealSessionState(t *testing.T) {
	requireSmoke(t)

	const sessionID = "11111111-2222-4333-8444-555555555555"

	mock := copilotfixture.NewMockProvider(t, []copilotfixture.Turn{
		{Text: "MOCK FIRST TURN"},
		{Text: "MOCK RESUMED TURN"},
	})
	dirs := copilotfixture.NewSandboxDirs(t)

	fresh := copilotfixture.Run(t, copilotfixture.RunOptions{
		Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, WorkDir: dirs.WorkDir,
		BaseURL: mock.BaseURL(), Model: copilotfixture.MockModel,
		SessionID: sessionID, Prompt: "first prompt about widgets",
	})
	require.Equal(t, 0, fresh.ExitCode, "stderr: %s", fresh.Stderr)

	store := convStore(t, dirs.Home)

	entries, err := store.ListConvs("")
	require.NoError(t, err)
	require.Len(t, entries, 1, "one run must produce exactly one conversation")
	entry := entries[0]

	assert.Equal(t, sessionID, entry.SessionID,
		"the session-state directory is named by the caller-chosen --session-id")
	assert.Equal(t, filepath.Clean(dirs.WorkDir), entry.ProjectPath)
	assert.Equal(t, harness.CopilotName, entry.Harness)
	assert.Equal(t, "first prompt about widgets", entry.FirstPrompt)
	assert.Equal(t, 1, entry.MessageCount)
	assert.Equal(t, copilotfixture.MockModel, entry.Model)
	assert.NotEmpty(t, entry.Created)
	assert.NotEmpty(t, entry.Modified)
	assert.False(t, entry.FileMtime.IsZero())
	assert.Positive(t, entry.FileSize)

	// With no `--name`, Copilot's own name is a generated summary, never an
	// operator override. At 1.0.77 it is seeded from the first prompt.
	assert.NotEmpty(t, entry.Summary)
	assert.Empty(t, entry.CustomTitle)

	// A cwd filter must match the directory the CLI ran in.
	scoped, err := store.ListConvs(dirs.WorkDir)
	require.NoError(t, err)
	assert.Len(t, scoped, 1)
	elsewhere, err := store.ListConvs(t.TempDir())
	require.NoError(t, err)
	assert.Empty(t, elsewhere)

	exists, err := store.Exists(sessionID, dirs.WorkDir)
	require.NoError(t, err)
	assert.True(t, exists)

	// Exact and prefix resolution both land on the resumable id.
	ref, err := store.Resolve(sessionID, "", true)
	require.NoError(t, err)
	require.NotNil(t, ref)
	assert.Equal(t, sessionID, ref.ConvID)
	assert.Equal(t, filepath.Clean(dirs.WorkDir), ref.ProjectPath)

	ref, err = store.Resolve(sessionID[:8], dirs.WorkDir, false)
	require.NoError(t, err)
	require.NotNil(t, ref)
	assert.Equal(t, sessionID, ref.ConvID)

	title, err := store.Title(sessionID)
	require.NoError(t, err)
	assert.NotEmpty(t, title)

	// Resume: the store must still see ONE conversation, with the second turn
	// counted. This is what pins "a resume appends to the same events.jsonl".
	resumed := copilotfixture.Run(t, copilotfixture.RunOptions{
		Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, WorkDir: dirs.WorkDir,
		BaseURL: mock.BaseURL(), Model: copilotfixture.MockModel,
		ResumeID: sessionID, Prompt: "second prompt about gadgets",
	})
	require.Equal(t, 0, resumed.ExitCode, "stderr: %s", resumed.Stderr)

	entries, err = store.ListConvs("")
	require.NoError(t, err)
	require.Len(t, entries, 1, "a resume must not fork a second conversation")
	assert.Equal(t, sessionID, entries[0].SessionID)
	assert.Equal(t, 2, entries[0].MessageCount,
		"the resumed turn must be appended to the same event log")
	assert.Equal(t, "first prompt about widgets", entries[0].FirstPrompt,
		"the FIRST prompt stays first across a resume")
}

// TestCopilotConvStoreReadsUserNamedSession pins the title split. `--name` is
// how tclaude carries a launch-time title, so a session started that way must
// surface as an operator override rather than a generated summary.
func TestCopilotConvStoreReadsUserNamedSession(t *testing.T) {
	requireSmoke(t)

	mock := copilotfixture.NewMockProvider(t, []copilotfixture.Turn{{Text: "MOCK ANSWER"}})
	dirs := copilotfixture.NewSandboxDirs(t)

	// --name has no RunOptions field; the shell runner exercises it under the
	// same credential-free environment.
	const name = "My Named Session"
	result := copilotfixture.RunShell(t, copilotfixture.RunOptions{
		Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, WorkDir: dirs.WorkDir,
		BaseURL: mock.BaseURL(),
	}, "copilot -C "+dirs.WorkDir+" --allow-all-tools --output-format json"+
		" --no-color --log-level none --name '"+name+"' -p 'hello there'")
	require.Equal(t, 0, result.ExitCode, "stderr: %s", result.Stderr)

	entries, err := convStore(t, dirs.Home).ListConvs("")
	require.NoError(t, err)
	require.Len(t, entries, 1)

	assert.Equal(t, name, entries[0].CustomTitle,
		"`user_named: true` is an operator title, not a Copilot summary")
	assert.Empty(t, entries[0].Summary)
	assert.Equal(t, name, entries[0].DisplayTitle())
}

// TestCopilotConvStoreReadsGitContext pins the one field the store takes from
// workspace.yaml's git block. It is the reason no SQLite read is needed: the
// branch is in the per-session file, not only in session-store.db.
func TestCopilotConvStoreReadsGitContext(t *testing.T) {
	requireSmoke(t)

	mock := copilotfixture.NewMockProvider(t, []copilotfixture.Turn{{Text: "MOCK ANSWER"}})
	dirs := copilotfixture.NewSandboxDirs(t)
	initFixtureGitRepo(t, dirs.WorkDir)

	result := copilotfixture.Run(t, copilotfixture.RunOptions{
		Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, WorkDir: dirs.WorkDir,
		BaseURL: mock.BaseURL(), Model: copilotfixture.MockModel, Prompt: "hello git",
	})
	require.Equal(t, 0, result.ExitCode, "stderr: %s", result.Stderr)

	entries, err := convStore(t, dirs.Home).ListConvs("")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, fixtureGitBranch, entries[0].GitBranch)
}

// TestCopilotConvStoreEmptyOnUntouchedHome pins the cold-start answer: a
// COPILOT_HOME the CLI has never run under lists nothing and errors on
// nothing, so a fresh install is indistinguishable from "no conversations".
func TestCopilotConvStoreEmptyOnUntouchedHome(t *testing.T) {
	requireSmoke(t)

	dirs := copilotfixture.NewSandboxDirs(t)
	store := convStore(t, dirs.Home)

	entries, err := store.ListConvs("")
	require.NoError(t, err)
	assert.Empty(t, entries)

	exists, err := store.Exists("11111111-2222-4333-8444-555555555555", dirs.WorkDir)
	require.NoError(t, err)
	assert.False(t, exists)
}

// fixtureGitBranch is the branch initFixtureGitRepo checks out. It is not a
// default branch name, so an assertion on it cannot pass by coincidence.
const fixtureGitBranch = "copilot-fixture-branch"

// initFixtureGitRepo makes dir a git repository with a deterministic branch
// and remote, which is what causes Copilot to record a git block in
// workspace.yaml. The repo is local and never contacted.
func initFixtureGitRepo(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-b", fixtureGitBranch},
		{"config", "user.email", "fixture@example.invalid"},
		{"config", "user.name", "Copilot Fixture"},
		{"remote", "add", "origin", "https://github.com/octo/example.git"},
		{"commit", "--allow-empty", "-m", "fixture"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
}
