package task

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/fsnotify/fsnotify"
	"github.com/spf13/cobra"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/notify"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/common"
)

type RunParams struct {
	Dir      string `short:"C" long:"dir" optional:"true" help:"Directory to run tasks in (defaults to current directory)"`
	Detached bool   `short:"d" long:"detached" help:"Start detached (don't attach to session)"`
	NoTmux   bool   `long:"no-tmux" help:"Run without tmux session management"`
	Watch    bool   `short:"w" long:"watch" help:"Watch for new tasks instead of exiting when TODO.md is empty"`
}

func RunCmd() *cobra.Command {
	cmd := boa.CmdT[RunParams]{
		Use:   "run",
		Short: "Run tasks from TODO.md sequentially",
		Long: `Run all tasks from TODO.md sequentially using Claude Code.
Each task is run in a fresh Claude context. After completion,
changes are committed and the task is moved to DONE.md.

Claude runs interactively in tmux — you can attach to approve
permissions or answer questions. When Claude finishes and you
type /exit, the next task starts automatically.

Pass extra Claude flags after -- (e.g., -- --dangerously-skip-permissions).`,
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *RunParams, cmd *cobra.Command, args []string) {
			if err := runRun(params); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
	cmd.Args = cobra.ArbitraryArgs
	return cmd
}

func runRun(params *RunParams) error {
	cwd, err := resolveDir(params.Dir)
	if err != nil {
		return err
	}

	tasks, err := ParseTodoMD(TodoPath(cwd))
	if err != nil {
		return fmt.Errorf("failed to read TODO.md: %w", err)
	}

	// Check we have tasks (skip check in watch mode)
	if !params.Watch {
		if len(tasks) == 0 {
			return fmt.Errorf("no tasks found in TODO.md")
		}
		fmt.Printf("Found %d task(s) in TODO.md\n", len(tasks))
	}

	// When task files live in the project directory (no --dir), exclude them from commits
	excludeTaskFiles := params.Dir == "" && os.Getenv("TCLAUDE_TASK_EXPLICIT_DIR") == ""

	if params.NoTmux {
		return runTaskLoop(cwd, clcommon.ExtractClaudeExtraArgs(), params.Watch, excludeTaskFiles)
	}

	// Run in tmux session
	return runInTmux(cwd, params.Detached, params.Watch, excludeTaskFiles)
}

// runInTmux starts the task runner inside a tmux session
func runInTmux(cwd string, detached, watch, excludeTaskFiles bool) error {
	if err := session.CheckTmuxInstalled(); err != nil {
		return err
	}
	session.EnsureHooksInstalled(false, os.Stdout, os.Stderr)

	sessionID := "tasks-" + session.GenerateSessionID()
	tmuxSession := "tclaude-" + sessionID

	// Build command to run the task loop inside tmux
	watchFlag := ""
	if watch {
		watchFlag = " --watch"
	}
	explicitDirEnv := ""
	if !excludeTaskFiles {
		explicitDirEnv = " TCLAUDE_TASK_EXPLICIT_DIR=1"
	}
	runnerCmd := fmt.Sprintf("TCLAUDE_SESSION_ID=%s TCLAUDE_TASK_TMUX=%s%s %s task run --no-tmux%s -C %s",
		sessionID, tmuxSession, explicitDirEnv, clcommon.DetectCmd(), watchFlag, clcommon.ShellQuoteArg(cwd))

	// Forward extra claude args through
	if extraArgs := clcommon.ExtractClaudeExtraArgs(); len(extraArgs) > 0 {
		runnerCmd += " --"
		for _, a := range extraArgs {
			runnerCmd += " " + clcommon.ShellQuoteArg(a)
		}
	}

	tmuxArgs := []string{
		"new-session",
		"-d",
		"-s", tmuxSession,
		"-c", cwd,
		"sh", "-c", runnerCmd,
	}

	tmuxCmd := exec.Command("tmux", clcommon.TmuxArgs(tmuxArgs...)...)
	tmuxCmd.Stdout = os.Stdout
	tmuxCmd.Stderr = os.Stderr

	if err := tmuxCmd.Run(); err != nil {
		return fmt.Errorf("failed to create tmux session: %w", err)
	}

	// Save session state
	pid := session.ParsePIDFromTmux(tmuxSession)
	state := &session.SessionState{
		ID:          sessionID,
		TmuxSession: tmuxSession,
		PID:         pid,
		Cwd:         cwd,
		Status:      session.StatusWorking,
		Created:     time.Now(),
		Updated:     time.Now(),
	}

	if err := session.SaveSessionState(state); err != nil {
		return fmt.Errorf("failed to save session state: %w", err)
	}

	fmt.Printf("Task runner started in session %s\n", sessionID)
	fmt.Printf("  Directory: %s\n", cwd)

	if detached {
		fmt.Printf("\nAttach with: tclaude session attach %s\n", sessionID)
		return nil
	}

	fmt.Println("\nAttaching... (Ctrl+B D to detach)")
	return session.AttachToSession(sessionID, tmuxSession, false)
}

// runTaskLoop is the internal loop that runs tasks sequentially.
// It is called directly (--no-tmux) or inside a tmux session.
// When watch is true, it waits for new tasks instead of exiting when TODO.md is empty.
func runTaskLoop(cwd string, extraClaudeArgs []string, watch, excludeTaskFiles bool) error {
	todoPath := TodoPath(cwd)
	doingPath := DoingPath(cwd)
	donePath := DonePath(cwd)

	hasPermissionMode := slices.Contains(extraClaudeArgs, "--permission-mode")
	if !hasPermissionMode {
		extraClaudeArgs = append(extraClaudeArgs, "--permission-mode", "acceptEdits")
	}

	// Set up signal handling for clean shutdown in watch mode
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	for {
		// Re-read TODO.md each iteration (in case it was modified externally)
		tasks, err := ParseTodoMD(todoPath)
		if err != nil {
			return fmt.Errorf("failed to read TODO.md: %w", err)
		}
		if len(tasks) == 0 {
			if !watch {
				break
			}
			// Watch mode: wait for tasks to appear
			if err := waitForTasks(todoPath, sigCh); err != nil {
				return err
			}
			continue
		}

		task := tasks[0]
		remaining := tasks[1:]
		totalOriginal := len(tasks)

		fmt.Printf("\n%s\n", strings.Repeat("=", 60))
		fmt.Printf("Task: %s (%d remaining)\n", task.Title, totalOriginal)
		fmt.Printf("%s\n\n", strings.Repeat("=", 60))

		// Move task from TODO.md to DOING.md
		if err := WriteDoingMD(doingPath, task); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to write DOING.md: %v\n", err)
		}
		if err := WriteTodoMD(todoPath, remaining); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to update TODO.md: %v\n", err)
		}

		// Snapshot plan files before running Claude
		plansBefore := snapshotPlanFiles()

		// Run Claude Code interactively with the task prompt
		report, sessionID, err := runClaude(cwd, task.Prompt, extraClaudeArgs)

		result := TaskResult{
			Title:     task.Title,
			Prompt:    task.Prompt,
			PlanFile:  findNewPlanFile(plansBefore),
			Report:    report,
			SessionID: sessionID,
			Timestamp: time.Now(),
		}

		if err != nil {
			result.Status = "failed"
			result.Error = err.Error()
			fmt.Printf("\nTask failed: %s\nError: %v\n", task.Title, err)
		} else {
			result.Status = "completed"
			fmt.Printf("\nTask completed: %s\n", task.Title)
		}

		// Move the task from DOING.md to DONE.md
		if err := ClearDoingMD(doingPath); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to clear DOING.md: %v\n", err)
		}

		// Git commit all changes with task title
		result.Commit = gitCommitAll(cwd, task.Title, excludeTaskFiles)

		// If there are no files to commit, the task is not truly completed
		if result.Status == "completed" && result.Commit == "" {
			result.Status = "failed"
			result.Error = "no files were changed"
			fmt.Printf("\nTask not completed (no files to commit): %s\n", task.Title)
		}

		if err := AppendDoneMD(donePath, result); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to update DONE.md: %v\n", err)
		}

		if result.Status == "failed" {
			sendNotification(sessionID, cwd, "failed", fmt.Sprintf("Task failed: %s", task.Title))
			return fmt.Errorf("task %q failed: %s", task.Title, result.Error)
		}
	}

	fmt.Printf("\n%s\n", strings.Repeat("=", 60))
	fmt.Println("All tasks completed!")
	fmt.Printf("%s\n", strings.Repeat("=", 60))

	sendNotification("tasks", cwd, "completed", "All tasks completed!")

	return nil
}

// waitForTasks watches TODO.md using fsnotify until tasks appear or a signal is received.
func waitForTasks(todoPath string, sigCh <-chan os.Signal) error {
	fmt.Println("\nWatching for new tasks in TODO.md... (Ctrl-C to stop)")

	// Watch the directory containing TODO.md (file may not exist yet)
	dir := filepath.Dir(todoPath)
	base := filepath.Base(todoPath)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to create file watcher: %w", err)
	}
	defer watcher.Close()

	if err := watcher.Add(dir); err != nil {
		return fmt.Errorf("failed to watch directory %s: %w", dir, err)
	}

	// Check once immediately in case tasks were added before we started watching
	if tasks, err := ParseTodoMD(todoPath); err == nil && len(tasks) > 0 {
		fmt.Printf("Found %d new task(s) in TODO.md\n", len(tasks))
		return nil
	}

	for {
		select {
		case <-sigCh:
			fmt.Println("\nReceived signal, stopping task watcher.")
			return fmt.Errorf("interrupted")
		case event, ok := <-watcher.Events:
			if !ok {
				return fmt.Errorf("file watcher closed")
			}
			if filepath.Base(event.Name) != base {
				continue
			}
			if event.Op&(fsnotify.Write|fsnotify.Create) == 0 {
				continue
			}
			tasks, err := ParseTodoMD(todoPath)
			if err != nil {
				return fmt.Errorf("failed to read TODO.md: %w", err)
			}
			if len(tasks) > 0 {
				fmt.Printf("Found %d new task(s) in TODO.md\n", len(tasks))
				return nil
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return fmt.Errorf("file watcher closed")
			}
			return fmt.Errorf("file watcher error: %w", err)
		}
	}
}

// runClaude runs Claude Code interactively with the given prompt.
// Claude gets full terminal I/O — the user can approve permissions,
// answer questions, etc. When the user types /exit or Claude exits,
// control returns to the task runner.
//
// In tmux mode (TCLAUDE_TASK_TMUX set), a watcher goroutine uses fsnotify
// to detect a signal file written by the Stop hook and auto-sends /exit
// after a grace period, enabling hands-free task sequencing.
// claudeResult holds the output from a Claude run.
//
// report string - Claude's last assistant message
// sessionID string - Claude's session_id from hook
func runClaude(cwd, prompt string, extraArgs []string) (report string, sessionID string, err error) {
	signalPath := taskSignalPath()
	os.Remove(signalPath) // clean up stale signal from previous run

	args := []string{prompt}
	args = append(args, extraArgs...)

	cmd := exec.Command("claude", args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "TCLAUDE_TASK_SIGNAL="+signalPath)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Start watcher for auto-continue in tmux mode
	tmuxSession := os.Getenv("TCLAUDE_TASK_TMUX")
	if tmuxSession != "" {
		excludeTaskFiles := os.Getenv("TCLAUDE_TASK_EXPLICIT_DIR") == ""
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go watchForTaskCompletion(ctx, signalPath, tmuxSession, cwd, excludeTaskFiles)
	}

	err = cmd.Run()

	// Read report from signal file (written by Stop hook with last_assistant_message)
	if data, readErr := os.ReadFile(signalPath); readErr == nil {
		report = string(data)
	}
	os.Remove(signalPath)

	// Read session_id from companion file (written by Stop hook)
	sessionIDPath := signalPath + ".session-id"
	if data, readErr := os.ReadFile(sessionIDPath); readErr == nil {
		sessionID = string(data)
	}
	os.Remove(sessionIDPath)

	return report, sessionID, err
}

// taskSignalPath returns the path to the task signal file.
func taskSignalPath() string {
	return filepath.Join(common.CacheDir(), "task-signal")
}

// watchForTaskCompletion watches for the signal file using fsnotify and sends
// /exit to the tmux session after a grace period. The grace period allows the
// user to start typing (which triggers UserPromptSubmit, removing the signal
// file) before auto-exit kicks in.
func watchForTaskCompletion(ctx context.Context, signalPath, tmuxSession, cwd string, excludeTaskFiles bool) {
	dir := filepath.Dir(signalPath)
	base := filepath.Base(signalPath)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return // silently fall back to no auto-exit
	}
	defer watcher.Close()

	if err := watcher.Add(dir); err != nil {
		return
	}

	// Check if signal file already exists (race with hook)
	signalExists := false
	if _, err := os.Stat(signalPath); err == nil {
		signalExists = true
	}

	for {
		if signalExists {
			// Signal detected — enter grace period, watching for removal
			if gracePeriod(ctx, watcher, signalPath, base) {
				// Signal removed during grace (user interacted) — reset
				signalExists = false
				continue
			}
			// Signal survived grace period — check if any files were actually changed
			sessionIDPath := signalPath + ".session-id"
			var sessionID string
			if data, readErr := os.ReadFile(sessionIDPath); readErr == nil {
				sessionID = string(data)
			}
			if !hasTrackedChanges(cwd, excludeTaskFiles) {
				sendNotification(sessionID, cwd, "waiting", "Task produced no file changes")
				return
			}
			sendTmuxExit(tmuxSession)
			return
		}

		// Wait for signal file to appear
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if filepath.Base(event.Name) != base {
				continue
			}
			if event.Op&(fsnotify.Write|fsnotify.Create) != 0 {
				signalExists = true
			}
		case _, ok := <-watcher.Errors:
			if !ok {
				return
			}
		}
	}
}

// gracePeriod waits 5 seconds, watching for the signal file to be removed.
// Returns true if the signal was removed (cancelled), false if it survived.
func gracePeriod(ctx context.Context, watcher *fsnotify.Watcher, signalPath, base string) bool {
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return true // treat context cancellation as cancelled
		case <-timer.C:
			// Grace period expired — check signal one final time
			if _, err := os.Stat(signalPath); err != nil {
				return true // removed just before timer fired
			}
			return false
		case event, ok := <-watcher.Events:
			if !ok {
				return false
			}
			if filepath.Base(event.Name) != base {
				continue
			}
			if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				return true // signal removed (user interacted)
			}
		case _, ok := <-watcher.Errors:
			if !ok {
				return false
			}
		}
	}
}

// sendTmuxExit sends /exit + Enter to the tmux session.
func sendTmuxExit(tmuxSession string) {
	cmd := clcommon.TmuxCommand("send-keys", "-t", tmuxSession, "/exit", "Enter")
	cmd.Run()
}

// hasTrackedChanges returns true if there are uncommitted changes to git-tracked
// files, excluding task management files (TODO.md/DOING.md/DONE.md) when excludeTaskFiles is set.
func hasTrackedChanges(cwd string, excludeTaskFiles bool) bool {
	statusCmd := exec.Command("git", "status", "--porcelain")
	statusCmd.Dir = cwd
	out, err := statusCmd.Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		// Porcelain format: XY filename (or XY old -> new for renames)
		file := strings.TrimSpace(line[2:])
		if idx := strings.Index(file, " -> "); idx >= 0 {
			file = file[idx+4:]
		}
		if excludeTaskFiles {
			base := filepath.Base(file)
			if base == "TODO.md" || base == "DOING.md" || base == "DONE.md" {
				continue
			}
		}
		return true
	}
	return false
}

// gitCommitAll stages all changes and commits with the given message.
// When excludeTaskFiles is true, TODO.md/DOING.md/DONE.md are unstaged before committing.
// Returns the commit hash, or empty string on failure.
func gitCommitAll(cwd, message string, excludeTaskFiles bool) string {
	// Check if there are changes to commit
	statusCmd := exec.Command("git", "status", "--porcelain")
	statusCmd.Dir = cwd
	statusOut, err := statusCmd.Output()
	if err != nil || len(strings.TrimSpace(string(statusOut))) == 0 {
		return "" // nothing to commit
	}

	// Stage all changes
	addCmd := exec.Command("git", "add", "-A")
	addCmd.Dir = cwd
	if err := addCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: git add failed: %v\n", err)
		return ""
	}

	// Unstage task management files so they aren't committed
	if excludeTaskFiles {
		resetCmd := exec.Command("git", "reset", "HEAD", "--", "TODO.md", "DOING.md", "DONE.md")
		resetCmd.Dir = cwd
		_ = resetCmd.Run()
	}

	// Commit
	commitCmd := exec.Command("git", "commit", "-m", message)
	commitCmd.Dir = cwd
	if err := commitCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: git commit failed: %v\n", err)
		return ""
	}

	// Get commit hash
	hashCmd := exec.Command("git", "rev-parse", "--short", "HEAD")
	hashCmd.Dir = cwd
	hashOut, err := hashCmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(hashOut))
}

// plansDir returns the path to Claude's plans directory.
func plansDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "plans")
}

// snapshotPlanFiles returns a set of current .md file names in ~/.claude/plans/.
func snapshotPlanFiles() map[string]bool {
	dir := plansDir()
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	result := make(map[string]bool, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			result[e.Name()] = true
		}
	}
	return result
}

// findNewPlanFile returns the path of the first new .md file in ~/.claude/plans/
// that wasn't present in the before snapshot.
func findNewPlanFile(before map[string]bool) string {
	dir := plansDir()
	if dir == "" {
		return ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") && !before[e.Name()] {
			return filepath.Join(dir, e.Name())
		}
	}
	return ""
}

// sendNotification sends a desktop notification about task completion.
func sendNotification(sessionId, cwd, status, message string) {
	if !notify.IsEnabled() {
		return
	}
	notify.Send(sessionId, status, cwd, message)
}
