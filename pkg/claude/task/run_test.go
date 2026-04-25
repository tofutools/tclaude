package task

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
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
	os.Exit(m.Run())
}

// fakeClaude implements the behaviour selected by FAKE_CLAUDE_BEHAVIOR.
func fakeClaude() {
	cwd, _ := os.Getwd()
	switch os.Getenv("FAKE_CLAUDE_BEHAVIOR") {
	case "create_file":
		os.WriteFile(filepath.Join(cwd, "result.txt"), []byte("task done\n"), 0644)
	case "create_unique_file":
		// Append a new result file on each invocation using a counter file.
		counterPath := filepath.Join(cwd, ".fake_counter")
		data, _ := os.ReadFile(counterPath)
		n := 0
		fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &n)
		n++
		os.WriteFile(counterPath, []byte(fmt.Sprintf("%d", n)), 0644)
		os.WriteFile(filepath.Join(cwd, fmt.Sprintf("result_%d.txt", n)), []byte("done\n"), 0644)
	case "fail":
		os.Exit(1)
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

	// Reset the db singleton so it re-opens against the temp HOME.
	db.ResetForTest()
	t.Cleanup(func() { db.ResetForTest() })

	// Write a minimal config: notifications off, sensible rate-limit default.
	tclaudeDir := filepath.Join(home, ".tclaude")
	if err := os.MkdirAll(tclaudeDir, 0755); err != nil {
		t.Fatalf("create .tclaude dir: %v", err)
	}
	cfg := map[string]any{
		"notifications": map[string]any{"enabled": false},
		"tasks":         map[string]any{"five_hour_rate_limit_percent_max_used": 99.0},
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
	if hasTrackedChanges(dir, false) {
		t.Error("expected false for clean repo")
	}
}

func TestHasTrackedChanges_ModifiedTrackedFile(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	path := filepath.Join(dir, "file.txt")
	os.WriteFile(path, []byte("original"), 0644)
	exec.Command("git", "-C", dir, "add", "file.txt").Run()
	exec.Command("git", "-C", dir, "commit", "-m", "add file").Run()
	os.WriteFile(path, []byte("modified"), 0644)
	if !hasTrackedChanges(dir, false) {
		t.Error("expected true for modified tracked file")
	}
}

func TestHasTrackedChanges_UntrackedFile(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new"), 0644)
	if !hasTrackedChanges(dir, false) {
		t.Error("expected true for untracked file")
	}
}

func TestHasTrackedChanges_OnlyTaskFilesExcluded(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	os.WriteFile(filepath.Join(dir, "TODO.md"), []byte("## t\n\np\n"), 0644)
	os.WriteFile(filepath.Join(dir, "DOING.md"), []byte("## t\n\np\n"), 0644)
	if hasTrackedChanges(dir, true) {
		t.Error("expected false when only task files present and excludeTaskFiles=true")
	}
}

func TestHasTrackedChanges_TaskAndOtherFilesExcluded(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	os.WriteFile(filepath.Join(dir, "TODO.md"), []byte("## t\n\np\n"), 0644)
	os.WriteFile(filepath.Join(dir, "result.txt"), []byte("work"), 0644)
	if !hasTrackedChanges(dir, true) {
		t.Error("expected true when non-task file is also present")
	}
}

// ── runVerifyCmd ─────────────────────────────────────────────────────────────

func TestRunVerifyCmd_Pass(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	out, err := runVerifyCmd(context.Background(), "echo hello", t.TempDir(), 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "hello" {
		t.Errorf("output = %q, want %q", out, "hello")
	}
}

func TestRunVerifyCmd_Fail(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	out, err := runVerifyCmd(context.Background(), "echo 'build failed'; exit 1", t.TempDir(), 5*time.Second)
	if err == nil {
		t.Fatal("expected error for failing command")
	}
	if !strings.Contains(out, "build failed") {
		t.Errorf("output %q should contain 'build failed'", out)
	}
}

func TestRunVerifyCmd_Timeout(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	_, err := runVerifyCmd(context.Background(), "sleep 10", t.TempDir(), 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected error for timed-out command")
	}
}

func TestRunVerifyCmd_CancelledContext(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := runVerifyCmd(ctx, "echo hi", t.TempDir(), 5*time.Second)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

// ── gitCommitAll ─────────────────────────────────────────────────────────────

func TestGitCommitAll_NothingToCommit(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	if hash := gitCommitAll(dir, "test", false); hash != "" {
		t.Errorf("expected empty hash for clean repo, got %q", hash)
	}
}

func TestGitCommitAll_NewFile(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	os.WriteFile(filepath.Join(dir, "result.txt"), []byte("done"), 0644)
	hash := gitCommitAll(dir, "add result", false)
	if hash == "" {
		t.Fatal("expected non-empty commit hash")
	}
	cmd := exec.Command("git", "log", "--oneline", "-1")
	cmd.Dir = dir
	out, _ := cmd.Output()
	if !strings.Contains(string(out), "add result") {
		t.Errorf("commit not in log: %s", out)
	}
}

func TestGitCommitAll_ExcludeTaskFiles_OnlyTaskFiles(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	os.WriteFile(filepath.Join(dir, "TODO.md"), []byte("## t\n\np\n"), 0644)
	os.WriteFile(filepath.Join(dir, "DOING.md"), []byte("## t\n\np\n"), 0644)
	if hash := gitCommitAll(dir, "test", true); hash != "" {
		t.Errorf("expected empty hash when only task files changed, got %q", hash)
	}
}

func TestGitCommitAll_ExcludeTaskFiles_MixedFiles(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	os.WriteFile(filepath.Join(dir, "TODO.md"), []byte("## t\n\np\n"), 0644)
	os.WriteFile(filepath.Join(dir, "result.txt"), []byte("done"), 0644)
	hash := gitCommitAll(dir, "test", true)
	if hash == "" {
		t.Fatal("expected commit hash for non-task file")
	}
	cmd := exec.Command("git", "show", "--name-only", "--format=", "HEAD")
	cmd.Dir = dir
	out, _ := cmd.Output()
	committed := string(out)
	if strings.Contains(committed, "TODO.md") {
		t.Error("TODO.md should not have been committed")
	}
	if !strings.Contains(committed, "result.txt") {
		t.Errorf("result.txt should be in commit; committed: %s", committed)
	}
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

	if err := runTaskLoop(dir, nil, false, false); err != nil {
		t.Fatalf("runTaskLoop: %v", err)
	}

	remaining, _ := ParseTodoMD(TodoPath(dir))
	if len(remaining) != 0 {
		t.Errorf("expected empty TODO.md, got %d tasks", len(remaining))
	}
	if _, err := os.Stat(DoingPath(dir)); !os.IsNotExist(err) {
		t.Error("DOING.md should not exist after completion")
	}
	doneData, err := os.ReadFile(DonePath(dir))
	if err != nil {
		t.Fatalf("read DONE.md: %v", err)
	}
	content := string(doneData)
	if !strings.Contains(content, "## Test task") {
		t.Error("DONE.md missing task title")
	}
	if !strings.Contains(content, "**Status:** completed") {
		t.Error("DONE.md missing completed status")
	}
}

func TestRunTaskLoop_ClaudeFails(t *testing.T) {
	setupTclaudeEnv(t)
	dir := t.TempDir()
	initGitRepo(t, dir)
	setupFakeClaude(t, "fail")

	if err := WriteTodoMD(TodoPath(dir), []Task{{Title: "Failing task", Prompt: "Will fail"}}); err != nil {
		t.Fatalf("WriteTodoMD: %v", err)
	}

	err := runTaskLoop(dir, nil, false, false)
	if err == nil {
		t.Fatal("expected error from failing claude")
	}
	if !strings.Contains(err.Error(), "Failing task") {
		t.Errorf("error should mention task title, got: %v", err)
	}
	doneData, _ := os.ReadFile(DonePath(dir))
	if !strings.Contains(string(doneData), "**Status:** failed") {
		t.Error("DONE.md missing failed status")
	}
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
	err := runTaskLoop(dir, nil, false, true)
	if err == nil {
		t.Fatal("expected error when no files changed")
	}
	if !strings.Contains(err.Error(), "no files were changed") {
		t.Errorf("expected 'no files were changed' in error, got: %v", err)
	}
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

	if err := runTaskLoop(dir, nil, false, false); err != nil {
		t.Fatalf("runTaskLoop: %v", err)
	}

	doneData, _ := os.ReadFile(DonePath(dir))
	content := string(doneData)
	if !strings.Contains(content, "## Task one") {
		t.Error("DONE.md missing 'Task one'")
	}
	if !strings.Contains(content, "## Task two") {
		t.Error("DONE.md missing 'Task two'")
	}
	for _, f := range []string{"result_1.txt", "result_2.txt"} {
		if _, err := os.Stat(filepath.Join(dir, f)); os.IsNotExist(err) {
			t.Errorf("expected %s to exist", f)
		}
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

	if err := runTaskLoop(dir, nil, false, true); err != nil {
		t.Fatalf("runTaskLoop: %v", err)
	}

	cmd := exec.Command("git", "show", "--name-only", "--format=", "HEAD")
	cmd.Dir = dir
	out, _ := cmd.Output()
	committed := string(out)
	for _, f := range []string{"TODO.md", "DOING.md", "DONE.md"} {
		if strings.Contains(committed, f) {
			t.Errorf("task file %s should not be in commit", f)
		}
	}
	if !strings.Contains(committed, "result.txt") {
		t.Errorf("result.txt should be committed; committed files:\n%s", committed)
	}
}
