package task

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// TestMain allows the test binary to act as a fake "claude" subprocess when
// FAKE_CLAUDE=1 is set. Tests that need a fake claude copy the test binary into
// a temp dir, name it "claude", prepend that dir to PATH, and set the env vars
// below before calling runTaskLoop.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_CLAUDE") == "1" {
		fakeClaude()
		os.Exit(0)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

// fakeClaude implements the behaviour selected by FAKE_CLAUDE_BEHAVIOR.
func fakeClaude() {
	cwd, _ := os.Getwd()
	switch os.Getenv("FAKE_CLAUDE_BEHAVIOR") {
	case "create_file":
		_ = os.WriteFile(filepath.Join(cwd, "result.txt"), []byte("task done\n"), 0644)
	case "create_unique_file":
		// Append a new result file on each invocation using a counter file.
		counterPath := filepath.Join(cwd, ".fake_counter")
		data, _ := os.ReadFile(counterPath)
		n := 0
		_, _ = fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &n)
		n++
		_ = os.WriteFile(counterPath, []byte(fmt.Sprintf("%d", n)), 0644)
		_ = os.WriteFile(filepath.Join(cwd, fmt.Sprintf("result_%d.txt", n)), []byte("done\n"), 0644)
	case "fail":
		os.Exit(1)
	case "print_stdin":
		_, _ = io.Copy(os.Stdout, os.Stdin)
	case "sleep":
		time.Sleep(10 * time.Second)
	case "count_invocations":
		// Increment a counter file and print "has feedback" so the review loop gets output.
		counterPath := filepath.Join(cwd, ".review_counter")
		data, _ := os.ReadFile(counterPath)
		n := 0
		_, _ = fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &n)
		n++
		_ = os.WriteFile(counterPath, []byte(fmt.Sprintf("%d", n)), 0644)
		fmt.Print("has feedback")
		// "no_change" and default: exit 0, touch nothing.
	}
}

// initGitRepo sets up a git repo with an initial empty commit.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@example.com"},
		{"git", "config", "user.name", "Test"},
		{"git", "commit", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git setup %v: %v\n%s", args, err, out)
		}
	}
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// setupFakeClaude copies the current test binary as "claude" into a temp bin
// dir, prepends it to PATH, and sets FAKE_CLAUDE / FAKE_CLAUDE_BEHAVIOR.
func setupFakeClaude(t *testing.T, behavior string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	binDir := t.TempDir()
	claudeName := "claude"
	if runtime.GOOS == "windows" {
		claudeName = "claude.exe"
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatalf("read test binary: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, claudeName), data, 0755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CLAUDE", "1")
	t.Setenv("FAKE_CLAUDE_BEHAVIOR", behavior)
}

// setupTclaudeEnv redirects all ~/.tclaude access to a fresh temp directory so
// tests don't read or write the real user config and database.
func setupTclaudeEnv(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// Reset the db singleton so it re-opens against the temp HOME.
	db.ResetForTest()
	t.Cleanup(func() { db.ResetForTest() })

	// Write a minimal config: notifications off
	tclaudeDir := filepath.Join(home, ".tclaude")
	if err := os.MkdirAll(tclaudeDir, 0755); err != nil {
		t.Fatalf("create .tclaude dir: %v", err)
	}
	cfg := map[string]any{
		"notifications": map[string]any{"enabled": false},
	}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(tclaudeDir, "config.json"), data, 0644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}
}

// ── hasTrackedChanges ────────────────────────────────────────────────────────

func TestHasTrackedChanges_Clean(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	got, err := hasTrackedChanges(dir, false, "")
	require.NoError(t, err)
	assert.False(t, got, "expected false for clean repo")
}

func TestHasTrackedChanges_ModifiedTrackedFile(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	path := filepath.Join(dir, "file.txt")
	os.WriteFile(path, []byte("original"), 0644)
	gitRun(t, dir, "add", "file.txt")
	gitRun(t, dir, "commit", "-m", "add file")
	os.WriteFile(path, []byte("modified"), 0644)
	got, err := hasTrackedChanges(dir, false, "")
	require.NoError(t, err)
	assert.True(t, got, "expected true for modified tracked file")
}

func TestHasTrackedChanges_UntrackedFile(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new"), 0644)
	got, err := hasTrackedChanges(dir, false, "")
	require.NoError(t, err)
	assert.True(t, got, "expected true for untracked file")
}

func TestHasTrackedChanges_OnlyTaskFilesExcluded(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	os.WriteFile(filepath.Join(dir, "TODO.md"), []byte("## t\n\np\n"), 0644)
	os.WriteFile(filepath.Join(dir, "DOING.md"), []byte("## t\n\np\n"), 0644)
	got, err := hasTrackedChanges(dir, true, "")
	require.NoError(t, err)
	assert.False(t, got, "expected false when only task files present and excludeTaskFiles=true")
}

func TestHasTrackedChanges_TaskAndOtherFilesExcluded(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	os.WriteFile(filepath.Join(dir, "TODO.md"), []byte("## t\n\np\n"), 0644)
	os.WriteFile(filepath.Join(dir, "result.txt"), []byte("work"), 0644)
	got, err := hasTrackedChanges(dir, true, "")
	require.NoError(t, err)
	assert.True(t, got, "expected true when non-task file is also present")
}

func TestHasTrackedChanges_AgentCommit(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	baseCommit, err := getCurrentCommit(dir)
	require.NoError(t, err)
	os.WriteFile(filepath.Join(dir, "result.txt"), []byte("done"), 0644)
	gitRun(t, dir, "add", "result.txt")
	gitRun(t, dir, "commit", "-m", "agent work")
	got, err := hasTrackedChanges(dir, false, baseCommit)
	require.NoError(t, err)
	assert.True(t, got, "expected true when agent committed a file")
}

func TestHasTrackedChanges_AgentCommitOnlyTaskFiles(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	baseCommit, err := getCurrentCommit(dir)
	require.NoError(t, err)
	os.WriteFile(filepath.Join(dir, "TODO.md"), []byte(""), 0644)
	gitRun(t, dir, "add", "TODO.md")
	gitRun(t, dir, "commit", "-m", "update tasks")
	got, err := hasTrackedChanges(dir, true, baseCommit)
	require.NoError(t, err)
	assert.False(t, got, "expected false when agent only committed task files and excludeTaskFiles=true")
}

func TestHasTrackedChanges_AgentCommitNonTaskFile(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	baseCommit, err := getCurrentCommit(dir)
	require.NoError(t, err)
	os.WriteFile(filepath.Join(dir, "TODO.md"), []byte(""), 0644)
	os.WriteFile(filepath.Join(dir, "result.txt"), []byte("done"), 0644)
	gitRun(t, dir, "add", "TODO.md", "result.txt")
	gitRun(t, dir, "commit", "-m", "agent work")
	got, err := hasTrackedChanges(dir, true, baseCommit)
	require.NoError(t, err)
	assert.True(t, got, "expected true when agent committed result.txt alongside task files")
}

// ── getCurrentCommit ──────────────────────────────────────────────────────────

func TestGetCurrentCommit_ValidRepo(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	commit, err := getCurrentCommit(dir)
	require.NoError(t, err, "expected no error for valid repo")
	assert.NotEmpty(t, commit, "expected non-empty commit hash for valid repo")
	assert.GreaterOrEqual(t, len(commit), 40, "expected hex SHA of at least 40 characters")
}

func TestGetCurrentCommit_NotARepo(t *testing.T) {
	dir := t.TempDir()
	_, err := getCurrentCommit(dir)
	require.Error(t, err, "expected error for non-repo directory")
}

// ── uncommittedDiffHash ───────────────────────────────────────────────────────

func TestUncommittedDiffHash_CleanRepo(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	h, err := uncommittedDiffHash(dir)
	require.NoError(t, err)
	assert.NotEmpty(t, h, "expected non-empty hash for clean repo")
}

func TestUncommittedDiffHash_Deterministic(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	h1, err := uncommittedDiffHash(dir)
	require.NoError(t, err, "first call")
	h2, err := uncommittedDiffHash(dir)
	require.NoError(t, err, "second call")
	assert.Equal(t, h1, h2, "same state produced different hashes")
}

func TestUncommittedDiffHash_ChangesAfterModify(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	path := filepath.Join(dir, "file.txt")
	os.WriteFile(path, []byte("original"), 0644)
	gitRun(t, dir, "add", "file.txt")
	gitRun(t, dir, "commit", "-m", "add file")

	before, err := uncommittedDiffHash(dir)
	require.NoError(t, err, "before modify")

	os.WriteFile(path, []byte("modified"), 0644)
	after, err := uncommittedDiffHash(dir)
	require.NoError(t, err, "after modify")

	assert.NotEqual(t, before, after, "hash should change after modifying a tracked file")
}

func TestUncommittedDiffHash_NotARepo(t *testing.T) {
	dir := t.TempDir()
	_, err := uncommittedDiffHash(dir)
	require.Error(t, err, "expected error for non-repo directory")
}

func TestUncommittedDiffHash_ChangesWhenUntrackedFileAdded(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	before, err := uncommittedDiffHash(dir)
	require.NoError(t, err, "before add")

	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("untracked"), 0644)
	after, err := uncommittedDiffHash(dir)
	require.NoError(t, err, "after add")

	assert.NotEqual(t, before, after, "hash should change when an untracked file is added")
}

func TestUncommittedDiffHash_ChangesWhenUntrackedFileModified(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	path := filepath.Join(dir, "new.txt")
	os.WriteFile(path, []byte("v1"), 0644)

	before, err := uncommittedDiffHash(dir)
	require.NoError(t, err, "before modify")

	os.WriteFile(path, []byte("v2"), 0644)
	after, err := uncommittedDiffHash(dir)
	require.NoError(t, err, "after modify")

	assert.NotEqual(t, before, after, "hash should change when an untracked file's content changes")
}

// ── runVerifyCmd ─────────────────────────────────────────────────────────────

func TestRunVerifyCmd_Pass(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	out, err := runVerifyCmd(context.Background(), "echo hello", t.TempDir(), 5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, "hello", out)
}

func TestRunVerifyCmd_Fail(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	out, err := runVerifyCmd(context.Background(), "echo 'build failed'; exit 1", t.TempDir(), 5*time.Second)
	require.Error(t, err, "expected error for failing command")
	assert.Contains(t, out, "build failed")
}

func TestRunVerifyCmd_Timeout(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	_, err := runVerifyCmd(context.Background(), "sleep 10", t.TempDir(), 100*time.Millisecond)
	require.Error(t, err, "expected error for timed-out command")
}

func TestRunVerifyCmd_CancelledContext(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := runVerifyCmd(ctx, "echo hi", t.TempDir(), 5*time.Second)
	require.Error(t, err, "expected error for cancelled context")
}

// ── gitCommitAll ─────────────────────────────────────────────────────────────

func TestGitCommitAll_NothingToCommit(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	assert.Empty(t, gitCommitAll(dir, "test", false), "expected empty hash for clean repo")
}

func TestGitCommitAll_NewFile(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	os.WriteFile(filepath.Join(dir, "result.txt"), []byte("done"), 0644)
	hash := gitCommitAll(dir, "add result", false)
	assert.NotEmpty(t, hash, "expected non-empty commit hash")
	cmd := exec.Command("git", "log", "--oneline", "-1")
	cmd.Dir = dir
	out, _ := cmd.Output()
	assert.Contains(t, string(out), "add result", "commit not in log")
}

func TestGitCommitAll_ExcludeTaskFiles_OnlyTaskFiles(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	os.WriteFile(filepath.Join(dir, "TODO.md"), []byte("## t\n\np\n"), 0644)
	os.WriteFile(filepath.Join(dir, "DOING.md"), []byte("## t\n\np\n"), 0644)
	assert.Empty(t, gitCommitAll(dir, "test", true), "expected empty hash when only task files changed")
}

func TestGitCommitAll_ExcludeTaskFiles_MixedFiles(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	os.WriteFile(filepath.Join(dir, "TODO.md"), []byte("## t\n\np\n"), 0644)
	os.WriteFile(filepath.Join(dir, "result.txt"), []byte("done"), 0644)
	hash := gitCommitAll(dir, "test", true)
	assert.NotEmpty(t, hash, "expected commit hash for non-task file")
	cmd := exec.Command("git", "show", "--name-only", "--format=", "HEAD")
	cmd.Dir = dir
	out, _ := cmd.Output()
	committed := string(out)
	assert.NotContains(t, committed, "TODO.md", "TODO.md should not have been committed")
	assert.Contains(t, committed, "result.txt", "result.txt should be in commit")
}

// ── runTaskLoop integration ───────────────────────────────────────────────────

func TestRunTaskLoop_SingleTaskCompletes(t *testing.T) {
	setupTclaudeEnv(t)
	dir := t.TempDir()
	initGitRepo(t, dir)
	setupFakeClaude(t, "create_file")

	if err := WriteTodoMD(TodoPath(dir), []Task{{Title: "Test task", Prompt: "Do something"}}); err != nil {
		t.Fatalf("WriteTodoMD: %v", err)
	}

	require.NoError(t, runTaskLoop(io.Discard, dir, dir, nil, false, false), "runTaskLoop")

	remaining, _ := ParseTodoMD(TodoPath(dir))
	assert.Empty(t, remaining, "expected empty TODO.md")
	_, err := os.Stat(DoingPath(dir))
	assert.True(t, os.IsNotExist(err), "DOING.md should not exist after completion")

	doneData, err := os.ReadFile(DonePath(dir))
	require.NoError(t, err, "read DONE.md")
	content := string(doneData)
	assert.Contains(t, content, "## Test task", "DONE.md missing task title")
	assert.Contains(t, content, "**Status:** completed", "DONE.md missing completed status")
}

func TestRunTaskLoop_ClaudeFails(t *testing.T) {
	setupTclaudeEnv(t)
	dir := t.TempDir()
	initGitRepo(t, dir)
	setupFakeClaude(t, "fail")

	if err := WriteTodoMD(TodoPath(dir), []Task{{Title: "Failing task", Prompt: "Will fail"}}); err != nil {
		t.Fatalf("WriteTodoMD: %v", err)
	}

	err := runTaskLoop(io.Discard, dir, dir, nil, false, false)
	require.Error(t, err, "expected error from failing claude")
	assert.Contains(t, err.Error(), "Failing task", "error should mention task title")
	doneData, _ := os.ReadFile(DonePath(dir))
	assert.Contains(t, string(doneData), "**Status:** failed", "DONE.md missing failed status")
}

func TestRunTaskLoop_NoFileChanges(t *testing.T) {
	setupTclaudeEnv(t)
	dir := t.TempDir()
	initGitRepo(t, dir)
	setupFakeClaude(t, "no_change")

	if err := WriteTodoMD(TodoPath(dir), []Task{{Title: "Empty task", Prompt: "Do nothing"}}); err != nil {
		t.Fatalf("WriteTodoMD: %v", err)
	}

	// excludeTaskFiles=true so that writing empty TODO.md doesn't count as work.
	err := runTaskLoop(io.Discard, dir, dir, nil, false, true)
	require.Error(t, err, "expected error when no files changed")
	assert.Contains(t, err.Error(), "no files were changed")
}

func TestRunTaskLoop_MultipleTasksSequential(t *testing.T) {
	setupTclaudeEnv(t)
	dir := t.TempDir()
	initGitRepo(t, dir)
	setupFakeClaude(t, "create_unique_file")

	tasks := []Task{
		{Title: "Task one", Prompt: "First"},
		{Title: "Task two", Prompt: "Second"},
	}
	if err := WriteTodoMD(TodoPath(dir), tasks); err != nil {
		t.Fatalf("WriteTodoMD: %v", err)
	}

	require.NoError(t, runTaskLoop(io.Discard, dir, dir, nil, false, false), "runTaskLoop")

	doneData, _ := os.ReadFile(DonePath(dir))
	content := string(doneData)
	assert.Contains(t, content, "## Task one", "DONE.md missing 'Task one'")
	assert.Contains(t, content, "## Task two", "DONE.md missing 'Task two'")
	for _, f := range []string{"result_1.txt", "result_2.txt"} {
		_, err := os.Stat(filepath.Join(dir, f))
		assert.NoError(t, err, "expected %s to exist", f)
	}
}

func TestRunTaskLoop_ExcludeTaskFiles(t *testing.T) {
	setupTclaudeEnv(t)
	dir := t.TempDir()
	initGitRepo(t, dir)
	setupFakeClaude(t, "create_file")

	if err := WriteTodoMD(TodoPath(dir), []Task{{Title: "Test task", Prompt: "Do something"}}); err != nil {
		t.Fatalf("WriteTodoMD: %v", err)
	}

	require.NoError(t, runTaskLoop(io.Discard, dir, dir, nil, false, true), "runTaskLoop")

	cmd := exec.Command("git", "show", "--name-only", "--format=", "HEAD")
	cmd.Dir = dir
	out, _ := cmd.Output()
	committed := string(out)
	for _, f := range []string{"TODO.md", "DOING.md", "DONE.md"} {
		assert.NotContains(t, committed, f, "task file %s should not be in commit", f)
	}
	assert.Contains(t, committed, "result.txt", "result.txt should be committed")
}

func TestRunTaskLoop_SeparateTaskDir(t *testing.T) {
	// Verifies that taskDir controls where task files are read from while cwd
	// controls where Claude runs (and where git commits land).
	setupTclaudeEnv(t)
	cwd := t.TempDir()
	taskDir := filepath.Join(cwd, "tasks")
	if err := os.Mkdir(taskDir, 0755); err != nil {
		t.Fatalf("mkdir taskDir: %v", err)
	}
	initGitRepo(t, cwd)
	setupFakeClaude(t, "create_file") // writes result.txt into os.Getwd() (cwd)

	if err := WriteTodoMD(TodoPath(taskDir), []Task{{Title: "Separate dir task", Prompt: "Do something"}}); err != nil {
		t.Fatalf("WriteTodoMD: %v", err)
	}

	require.NoError(t, runTaskLoop(io.Discard, cwd, taskDir, nil, false, false), "runTaskLoop")

	// Task management files should be in taskDir.
	remaining, _ := ParseTodoMD(TodoPath(taskDir))
	assert.Empty(t, remaining, "expected empty TODO.md in taskDir")
	doneData, err := os.ReadFile(DonePath(taskDir))
	require.NoError(t, err, "DONE.md should exist in taskDir")
	assert.Contains(t, string(doneData), "## Separate dir task", "DONE.md in taskDir missing task title")
	// result.txt was created in cwd (where fake claude ran), and committed there.
	_, err = os.Stat(filepath.Join(cwd, "result.txt"))
	assert.NoError(t, err, "result.txt should exist in cwd")
	// The commit should be on cwd's repo, not inside taskDir.
	cmd := exec.Command("git", "show", "--name-only", "--format=", "HEAD")
	cmd.Dir = cwd
	out, err := cmd.Output()
	require.NoError(t, err, "git show")
	assert.Contains(t, string(out), "result.txt", "result.txt should be committed in cwd repo")
}

func TestRunTaskLoop_ExternalTaskDir(t *testing.T) {
	// taskDir is a completely separate temp directory outside the git repo.
	// Verifies that git operations target cwd exclusively and that task files
	// are written to and read from the external taskDir.
	setupTclaudeEnv(t)
	cwd := t.TempDir()
	taskDir := t.TempDir() // independent — not under cwd
	initGitRepo(t, cwd)
	setupFakeClaude(t, "create_file") // writes result.txt into cwd

	if err := WriteTodoMD(TodoPath(taskDir), []Task{{Title: "External dir task", Prompt: "Do something"}}); err != nil {
		t.Fatalf("WriteTodoMD: %v", err)
	}

	require.NoError(t, runTaskLoop(io.Discard, cwd, taskDir, nil, false, false), "runTaskLoop")

	// Task management files must be in taskDir, not in the git repo.
	remaining, _ := ParseTodoMD(TodoPath(taskDir))
	assert.Empty(t, remaining, "expected empty TODO.md in taskDir")
	doneData, err := os.ReadFile(DonePath(taskDir))
	require.NoError(t, err, "DONE.md should exist in taskDir")
	assert.Contains(t, string(doneData), "## External dir task", "DONE.md in taskDir missing task title")
	// No task files should have leaked into cwd.
	for _, name := range []string{"TODO.md", "DOING.md", "DONE.md"} {
		_, err := os.Stat(filepath.Join(cwd, name))
		assert.True(t, os.IsNotExist(err), "%s should not exist in cwd", name)
	}
	// result.txt was committed in cwd's repo, not in taskDir.
	_, err = os.Stat(filepath.Join(cwd, "result.txt"))
	assert.NoError(t, err, "result.txt should exist in cwd")
	cmd := exec.Command("git", "show", "--name-only", "--format=", "HEAD")
	cmd.Dir = cwd
	out, err := cmd.Output()
	require.NoError(t, err, "git show")
	assert.Contains(t, string(out), "result.txt", "result.txt should be committed in cwd repo")
	// taskDir should not be a git repo (sanity check that we never ran git there).
	_, err = os.Stat(filepath.Join(taskDir, ".git"))
	assert.True(t, os.IsNotExist(err), "taskDir should not have a .git directory")
}

// ── getGitDiff ────────────────────────────────────────────────────────────────

func TestGetGitDiff_CleanRepo(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	diff, err := getGitDiff(dir, "")
	require.NoError(t, err)
	assert.Empty(t, diff, "expected empty diff for clean repo")
}

func TestGetGitDiff_WithChanges(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	path := filepath.Join(dir, "file.txt")
	os.WriteFile(path, []byte("original"), 0644)
	gitRun(t, dir, "add", "file.txt")
	gitRun(t, dir, "commit", "-m", "add file")
	os.WriteFile(path, []byte("modified"), 0644)
	diff, err := getGitDiff(dir, "")
	require.NoError(t, err)
	assert.NotEmpty(t, diff, "expected non-empty diff for modified file")
	assert.Contains(t, diff, "modified", "diff should mention modified content")
}

func TestGetGitDiff_UntrackedFileIgnored(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	// Untracked files don't appear in git diff HEAD
	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new"), 0644)
	diff, err := getGitDiff(dir, "")
	require.NoError(t, err)
	assert.Empty(t, diff, "expected empty diff for untracked-only changes")
}

func TestGetGitDiff_WithBaseCommit(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	baseCommit, err := getCurrentCommit(dir)
	require.NoError(t, err)
	os.WriteFile(filepath.Join(dir, "result.txt"), []byte("agent output"), 0644)
	gitRun(t, dir, "add", "result.txt")
	gitRun(t, dir, "commit", "-m", "agent work")
	diff, err := getGitDiff(dir, baseCommit)
	require.NoError(t, err)
	assert.NotEmpty(t, diff, "expected non-empty diff when baseCommit predates a commit")
	assert.Contains(t, diff, "agent output", "diff should contain committed content")
}

func TestGetGitDiff_WithBaseCommitAndUncommitted(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	baseCommit, err := getCurrentCommit(dir)
	require.NoError(t, err)

	// Agent commits one file.
	os.WriteFile(filepath.Join(dir, "committed.txt"), []byte("committed content"), 0644)
	gitRun(t, dir, "add", "committed.txt")
	gitRun(t, dir, "commit", "-m", "agent work")

	// Then leaves another file modified but uncommitted.
	os.WriteFile(filepath.Join(dir, "uncommitted.txt"), []byte("uncommitted content"), 0644)
	gitRun(t, dir, "add", "uncommitted.txt")

	diff, err := getGitDiff(dir, baseCommit)
	require.NoError(t, err)
	assert.Contains(t, diff, "committed content", "diff should contain committed content")
	assert.Contains(t, diff, "uncommitted content", "diff should contain uncommitted content")
}

// ── runReviewAgent ────────────────────────────────────────────────────────────

func TestRunReviewAgent_DiffPrependedToPrompt(t *testing.T) {
	setupFakeClaude(t, "print_stdin")
	out, err := runReviewAgent(context.Background(), "review this", "some diff content", t.TempDir(), 5*time.Second)
	require.NoError(t, err)
	assert.Contains(t, out, "some diff content", "output should contain diff content")
	assert.NotContains(t, out, "review this", "output should not contain review prompt")
}

func TestRunReviewAgent_EmptyDiffOmitsDiffBlock(t *testing.T) {
	setupFakeClaude(t, "print_stdin")
	out, err := runReviewAgent(context.Background(), "review this", "", t.TempDir(), 5*time.Second)
	require.NoError(t, err)
	assert.NotContains(t, out, "review this", "output should not contain review prompt")
	assert.NotContains(t, out, "```diff", "output should not contain diff block for empty diff")
}

func TestRunReviewAgent_Timeout(t *testing.T) {
	setupFakeClaude(t, "sleep")
	_, err := runReviewAgent(context.Background(), "review prompt", "some diff", t.TempDir(), 100*time.Millisecond)
	require.Error(t, err, "expected error for timed-out review agent")
}

// ── reviewDiff flag ───────────────────────────────────────────────────────────

// simulateReviewDiffDecision exercises the real resolveReviewDiff decision.
// Returns true if the review agent was invoked and produced output, false if the
// review was skipped. Calls t.Fatalf if the review agent runs but returns an error.
func simulateReviewDiffDecision(t *testing.T, cwd, baseCommit string, opts taskRunOpts) bool {
	t.Helper()
	diff, skip := resolveReviewDiff(cwd, baseCommit, opts.reviewDiff)
	if skip {
		return false
	}
	out, err := runReviewAgent(context.Background(), opts.reviewSkill, diff, cwd, opts.reviewTimeout)
	if err != nil {
		t.Fatalf("review agent failed: %v", err)
	}
	return out != ""
}

func TestReviewDiff_TrueSkipsReviewWhenDiffEmpty(t *testing.T) {
	setupFakeClaude(t, "count_invocations")
	dir := t.TempDir()
	initGitRepo(t, dir)
	// Clean repo → diff is empty; reviewDiff=true → review must be skipped.
	called := simulateReviewDiffDecision(t, dir, "", taskRunOpts{
		reviewSkill:   "check the work",
		reviewPrefix:  defaultReviewPrefix,
		reviewTimeout: 5 * time.Second,
		reviewDiff:    true,
	})
	assert.False(t, called, "review should be skipped when reviewDiff=true and diff is empty")
}

func TestReviewDiff_FalseRunsReviewWithoutDiff(t *testing.T) {
	setupFakeClaude(t, "count_invocations")
	dir := t.TempDir()
	initGitRepo(t, dir)
	// Clean repo → diff is empty; reviewDiff=false → review must still run.
	called := simulateReviewDiffDecision(t, dir, "", taskRunOpts{
		reviewSkill:   "check the work",
		reviewPrefix:  defaultReviewPrefix,
		reviewTimeout: 5 * time.Second,
		reviewDiff:    false,
	})
	assert.True(t, called, "review should run when reviewDiff=false regardless of empty diff")
}

// ── review iteration loop ─────────────────────────────────────────────────────

// simulateReviewLoop mirrors the reviewAttempts logic in watchForTaskCompletion.
// It returns how many feedback rounds were sent before the loop exited.
func simulateReviewLoop(t *testing.T, cwd string, maxIter int) int {
	t.Helper()
	reviewAttempts := 0
	feedbackSent := 0
	for {
		out, err := runReviewAgent(context.Background(), "review prompt", "some diff", cwd, 5*time.Second)
		if err != nil || out == "" {
			break
		}
		if reviewAttempts < maxIter {
			reviewAttempts++
			feedbackSent++
			continue // would send feedback to Claude and wait for next Stop
		}
		break // max iterations reached — exit anyway
	}
	return feedbackSent
}

func TestReviewLoop_StopsAtMaxIterations(t *testing.T) {
	setupFakeClaude(t, "count_invocations")
	dir := t.TempDir()

	const maxIter = 3
	feedbackSent := simulateReviewLoop(t, dir, maxIter)
	assert.Equal(t, maxIter, feedbackSent)

	// Verify the fake claude was called maxIter+1 times:
	// maxIter sends that triggered a loop, plus 1 final call that hit the cap.
	data, _ := os.ReadFile(filepath.Join(dir, ".review_counter"))
	n := 0
	_, _ = fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &n)
	assert.Equal(t, maxIter+1, n, "review agent called wrong number of times")
}

func TestReviewLoop_ExitsImmediatelyWhenNoFeedback(t *testing.T) {
	setupFakeClaude(t, "no_change") // exits 0, prints nothing
	dir := t.TempDir()

	feedbackSent := simulateReviewLoop(t, dir, 3)
	assert.Zero(t, feedbackSent, "expected 0 feedback rounds for agent with no output")
}

// ── isAgentStuck ─────────────────────────────────────────────────────────────

func saveTestSession(t *testing.T, id string, status string, created, lastHook time.Time) {
	t.Helper()
	s := &session.SessionState{
		ID:       id,
		Status:   status,
		Created:  created,
		LastHook: lastHook,
	}
	if err := session.SaveSessionState(s); err != nil {
		t.Fatalf("SaveSessionState: %v", err)
	}
}

func TestIsAgentStuck(t *testing.T) {
	now := time.Now()
	const timeout = 5 * time.Minute

	tests := []struct {
		name      string
		sessionID string
		timeout   time.Duration
		setup     func(t *testing.T)
		want      bool
	}{
		{
			name:      "empty session ID",
			sessionID: "",
			timeout:   timeout,
			want:      false,
		},
		{
			name:      "zero timeout",
			sessionID: "s1",
			timeout:   0,
			want:      false,
		},
		{
			name:      "negative timeout",
			sessionID: "s1",
			timeout:   -time.Second,
			want:      false,
		},
		{
			name:      "session not in DB",
			sessionID: "nonexistent",
			timeout:   timeout,
			setup:     func(t *testing.T) { setupTclaudeEnv(t) },
			want:      false,
		},
		{
			name:      "session not working",
			sessionID: "s-idle",
			timeout:   timeout,
			setup: func(t *testing.T) {
				setupTclaudeEnv(t)
				saveTestSession(t, "s-idle", session.StatusIdle,
					now.Add(-10*time.Minute), now.Add(-10*time.Minute))
			},
			want: false,
		},
		{
			name:      "no hooks have fired yet (LastHook is zero)",
			sessionID: "s-new",
			timeout:   timeout,
			setup: func(t *testing.T) {
				setupTclaudeEnv(t)
				saveTestSession(t, "s-new", session.StatusWorking,
					now, time.Time{})
			},
			want: false,
		},
		{
			name:      "working but hook fired recently",
			sessionID: "s-active",
			timeout:   timeout,
			setup: func(t *testing.T) {
				setupTclaudeEnv(t)
				saveTestSession(t, "s-active", session.StatusWorking,
					now.Add(-10*time.Minute), now.Add(-1*time.Minute))
			},
			want: false,
		},
		{
			name:      "working and hook is stale",
			sessionID: "s-stuck",
			timeout:   timeout,
			setup: func(t *testing.T) {
				setupTclaudeEnv(t)
				saveTestSession(t, "s-stuck", session.StatusWorking,
					now.Add(-20*time.Minute), now.Add(-6*time.Minute))
			},
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setup != nil {
				tc.setup(t)
			}
			got := isAgentStuck(tc.sessionID, tc.timeout)
			assert.Equal(t, tc.want, got, "isAgentStuck(%q, %v)", tc.sessionID, tc.timeout)
		})
	}
}
