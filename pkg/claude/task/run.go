package task

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
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
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/notify"
	"github.com/tofutools/tclaude/pkg/claude/common/usageapi"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/common/executil"
)

type RunParams struct {
	Dir      string `short:"C" long:"dir" optional:"true" help:"Directory containing task files (defaults to current directory)"`
	Detached bool   `short:"d" long:"detached" help:"Start detached (don't attach to session)"`
	Watch    bool   `short:"w" long:"watch" help:"Watch for new tasks instead of exiting when TODO.md is empty"`
	Compact  int    `long:"compact" optional:"true" help:"Auto-compact at this context usage percentage (overrides config)"`
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
				slog.Warn("Error in task runner", "err", err, "module", "task")
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
	cmd.Args = cobra.ArbitraryArgs
	return cmd
}

func runRun(params *RunParams) error {
	// cwd is where Claude runs and git operates — always the actual working directory.
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current directory: %w", err)
	}

	// taskDir is where TODO.md/DOING.md/DONE.md live; defaults to cwd when --dir is not given.
	taskDir, err := resolveDir(params.Dir)
	if err != nil {
		return err
	}

	tasks, err := ParseTodoMD(TodoPath(taskDir))
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

	if os.Getenv("TCLAUDE_TASK_TMUX") != "" {
		return runTaskLoop(os.Stdout, cwd, taskDir, clcommon.ExtractClaudeExtraArgs(), params.Watch, excludeTaskFiles)
	}

	// Run in tmux session
	return runInTmux(cwd, taskDir, params.Detached, params.Watch, excludeTaskFiles, params.Compact)
}

// runInTmux starts the task runner inside a tmux session.
// cwd is the working directory where Claude runs; taskDir is where task files live.
func runInTmux(cwd, taskDir string, detached, watch, excludeTaskFiles bool, compact int) error {
	if err := session.CheckTmuxInstalled(); err != nil {
		return err
	}
	session.EnsureHooksInstalled(false, os.Stdout, os.Stderr)

	sessionID := "tasks-" + session.GenerateSessionID()
	tmuxSession := sessionID

	// Build command to run the task loop inside tmux with all environment variables forwarded
	watchFlag := ""
	if watch {
		watchFlag = " --watch"
	}

	additionalEnv := map[string]string{
		"TCLAUDE_SESSION_ID": sessionID,
		"TCLAUDE_TASK_TMUX":  tmuxSession,
	}
	if !excludeTaskFiles {
		additionalEnv["TCLAUDE_TASK_EXPLICIT_DIR"] = "1"
	}
	if compact > 0 {
		additionalEnv["TCLAUDE_AUTO_COMPACT"] = fmt.Sprintf("%d", compact)
	}

	envExports := clcommon.BuildEnvExports(additionalEnv)
	// -C passes the task file directory; the pane's working dir (-c cwd) provides Claude's cwd.
	runnerCmd := envExports + clcommon.DetectCmd() + " task run" + watchFlag + " -C " + clcommon.ShellQuoteArg(taskDir)

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

	tmuxCmd := clcommon.TmuxCommand(tmuxArgs...)
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
	if taskDir != cwd {
		fmt.Printf("  Task files: %s\n", taskDir)
	}

	if detached {
		fmt.Printf("\nAttach with: tclaude session attach %s\n", sessionID)
		return nil
	}

	fmt.Println("\nAttaching... (Ctrl+B D to detach)")
	return session.AttachToSession(sessionID, tmuxSession, false)
}

// runTaskLoop is the internal loop that runs tasks sequentially.
// cwd is the working directory for Claude and git; taskDir is where task files live.
// When watch is true, it waits for new tasks instead of exiting when TODO.md is empty.
func runTaskLoop(out io.Writer, cwd, taskDir string, extraClaudeArgs []string, watch, excludeTaskFiles bool) error {
	todoPath := TodoPath(taskDir)
	doingPath := DoingPath(taskDir)
	donePath := DonePath(taskDir)

	taskCfg, err := LoadTasksConfig(taskDir)
	if err != nil {
		return fmt.Errorf("failed to read .claude/tclaude/tasks.json: %w", err)
	}

	hasPermissionMode := slices.Contains(extraClaudeArgs, "--permission-mode")

	// Set up signal handling for clean shutdown in watch mode
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	cfg, _ := config.Load()

	for {
		usage, err := usageapi.GetCached()
		if usage == nil {
			if err != nil {
				slog.Warn("task run: unable to check rate limit", "error", err, "module", "task")
			}
			// Continue without rate limit check - usage data unavailable
		} else {
			if err != nil {
				slog.Warn("task run: using stale usage cache", "error", err, "module", "task")
			}
			if usage.FiveHour != nil {
				if usage.FiveHour.Pct > cfg.Tasks.FiveHourRateLimitPercentMaxUsed { // rate limited
					resetsAt := usage.FiveHour.ResetsAt
					slog.Debug("Waiting for 5 hour rate limit to reset", "time", resetsAt, "module", "task")
					fmt.Fprintf(out, "Waiting for 5 hour rate limit to reset at %v...\n", resetsAt.Local().Format("15:04"))
					time.Sleep(time.Until(resetsAt.Add(10 * time.Second)))
					fmt.Fprintf(out, "Rate limit reset, running tasks\n")
				}
			}
		}

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
			if err := waitForTasks(out, todoPath, sigCh); err != nil {
				return err
			}
			continue
		}

		task := tasks[0]
		remaining := tasks[1:]
		totalOriginal := len(tasks)

		fmt.Fprintf(out, "\n%s\n", strings.Repeat("=", 60))
		fmt.Fprintf(out, "Task: %s (%d remaining)\n", task.Title, totalOriginal)
		fmt.Fprintf(out, "%s\n\n", strings.Repeat("=", 60))

		// Move task from TODO.md to DOING.md
		if err := WriteDoingMD(doingPath, task); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to write DOING.md: %v\n", err)
		}
		if err := WriteTodoMD(todoPath, remaining); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to update TODO.md: %v\n", err)
		}

		// Snapshot plan files before running Claude
		plansBefore := snapshotPlanFiles()

		// Build per-task args (permission mode depends on task)
		taskArgs := extraClaudeArgs
		if !hasPermissionMode {
			mode := "acceptEdits"
			if task.PlanMode {
				mode = "plan"
			}
			taskArgs = append(slices.Clone(extraClaudeArgs), "--permission-mode", mode)
		}

		// Run Claude Code interactively with the task prompt
		opts := taskRunOpts{
			planMode:            task.PlanMode,
			planAutoAccept:      task.PlanAutoAccept,
			verifyCmd:           taskCfg.VerifyCmd,
			verifyMaxRetries:    taskCfg.MaxVerifyIterations,
			verifyTimeout:       taskCfg.VerifyTimeout,
			reviewPrompt:        taskCfg.ReviewPrompt,
			maxReviewIterations: taskCfg.MaxReviewIterations,
			reviewTimeout:       taskCfg.ReviewTimeout,
		}
		report, sessionID, runErr := runClaude(cwd, task.Prompt, taskArgs, excludeTaskFiles, opts)

		result := TaskResult{
			Title:     task.Title,
			Prompt:    task.Prompt,
			PlanFile:  findNewPlanFile(plansBefore),
			Report:    report,
			SessionID: sessionID,
			Timestamp: time.Now(),
		}

		if runErr != nil {
			result.Status = "failed"
			result.Error = runErr.Error()
			slog.Warn("task failed", "err", runErr, "module", "task")
			fmt.Fprintf(out, "\nTask failed: %s\nError: %v\n", task.Title, runErr)
		} else {
			result.Status = "completed"
			fmt.Fprintf(out, "\nTask completed: %s\n", task.Title)
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
			fmt.Fprintf(out, "\nTask not completed (no files to commit): %s\n", task.Title)
		}

		if err := AppendDoneMD(donePath, result); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to update DONE.md: %v\n", err)
		}

		if result.Status == "failed" {
			sendNotification(sessionID, cwd, "failed", fmt.Sprintf("Task failed: %s", task.Title))
			return fmt.Errorf("task %q failed: %s", task.Title, result.Error)
		}
	}

	fmt.Fprintf(out, "\n%s\n", strings.Repeat("=", 60))
	fmt.Fprintln(out, "All tasks completed!")
	fmt.Fprintf(out, "%s\n", strings.Repeat("=", 60))

	sendNotification("tasks", cwd, "completed", "All tasks completed!")

	return nil
}

// waitForTasks watches TODO.md using fsnotify until tasks appear or a signal is received.
func waitForTasks(out io.Writer, todoPath string, sigCh <-chan os.Signal) error {
	fmt.Fprintln(out, "\nWatching for new tasks in TODO.md... (Ctrl-C to stop)")

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
		fmt.Fprintf(out, "Found %d new task(s) in TODO.md\n", len(tasks))
		return nil
	}

	for {
		select {
		case <-sigCh:
			fmt.Fprintln(out, "\nReceived signal, stopping task watcher.")
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
				fmt.Fprintf(out, "Found %d new task(s) in TODO.md\n", len(tasks))
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

type taskRunOpts struct {
	planMode            bool
	planAutoAccept      bool
	verifyCmd           string
	verifyMaxRetries    int
	verifyTimeout       time.Duration
	reviewPrompt        string
	maxReviewIterations int
	reviewTimeout       time.Duration
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
func runClaude(cwd, prompt string, extraArgs []string, excludeTaskFiles bool, opts taskRunOpts) (report string, sessionID string, err error) {
	signalPath := session.TaskSignalPath(cwd)
	os.Remove(signalPath) // clean up stale signal from previous run

	// Snapshot HEAD so we can detect commits made by the agent during the task.
	baseCommit, err := getCurrentCommit(cwd)
	if err != nil {
		return "", "", err
	}

	var args []string
	args = append(args, extraArgs...)
	args = append(args, "--")
	args = append(args, strings.ReplaceAll(prompt, "\n", " "))

	cmd := exec.Command("claude", args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "TCLAUDE_TASK_SIGNAL="+signalPath)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Start watcher for auto-continue in tmux mode
	tmuxSession := os.Getenv("TCLAUDE_TASK_TMUX")
	if tmuxSession != "" {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go watchForTaskCompletion(ctx, signalPath, tmuxSession, cwd, excludeTaskFiles, opts, baseCommit)
	}

	err = cmd.Run()

	// Read report and session ID from signal file (written by Stop hook as JSON)
	if data, readErr := os.ReadFile(signalPath); readErr == nil {
		var taskSignal session.TaskSignal
		if json.Unmarshal(data, &taskSignal) == nil {
			report = taskSignal.Report
			sessionID = taskSignal.SessionID
		}
	}
	os.Remove(signalPath)

	return report, sessionID, err
}

// watchForTaskCompletion watches for the signal file using fsnotify and sends
// /exit to the tmux session after a grace period. The grace period allows the
// user to start typing (which triggers UserPromptSubmit, removing the signal
// file) before auto-exit kicks in.
func watchForTaskCompletion(ctx context.Context, signalPath, tmuxSession, cwd string, excludeTaskFiles bool, opts taskRunOpts, baseCommit string) {
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

	planAccepted := false
	attempts := 0
	reviewAttempts := 0

	for {
		if signalExists {
			// Read the signal to determine what triggered it
			var taskSignal session.TaskSignal
			if data, readErr := os.ReadFile(signalPath); readErr == nil {
				json.Unmarshal(data, &taskSignal)
			}

			slog.Debug("signal received", "signal", taskSignal, "module", "task")

			// Signal detected — enter grace period, watching for removal
			if gracePeriod(ctx, watcher, signalPath, base) {
				// Signal removed during grace (user interacted) — reset
				signalExists = false
				continue
			}

			if opts.planMode {
				if taskSignal.Event == "PermissionRequest" && taskSignal.ToolName == "ExitPlanMode" {
					slog.Debug("plan ready", "module", "task")

					// Plan auto-accept before plan is accepted: wait specifically for
					// the ExitPlanMode permission request, ignore everything else
					// (including Stop events that fire when Claude finishes planning)
					if opts.planAutoAccept && !planAccepted {
						slog.Debug("accepting plan", "module", "task")
						planAccepted = true
						sendTmuxEnter(tmuxSession)
					} else {
						sendNotification(taskSignal.SessionID, cwd, "plan ready", "Please review and accept plan")
					}
				} else {
					slog.Debug("ignoring signal while waiting for plan", "event", taskSignal.Event, "module", "task")
				}
				signalExists = false
				continue
			}

			// Signal survived grace period — check if any files were actually changed
			if taskSignal.Event != "Stop" {
				// Non-Stop signals that weren't handled above — reset and wait
				slog.Debug("ignoring signal", "event", taskSignal.Event, "module", "task")
				signalExists = false
				continue
			}
			hasChanges, err := hasTrackedChanges(cwd, excludeTaskFiles, baseCommit)
			if err != nil {
				slog.Warn("failed to check for changes", "err", err, "event", taskSignal.Event, "module", "task")
				sendNotification(taskSignal.SessionID, cwd, "waiting", fmt.Sprintf("Failed to check for changes: %v", err))
				return
			}
			if !hasChanges {
				slog.Debug("task produced no file changes", "event", taskSignal.Event, "module", "task")
				sendNotification(taskSignal.SessionID, cwd, "waiting", "Task produced no file changes")
				return
			}
			if opts.verifyCmd != "" {
				if ctx.Err() != nil {
					return
				}
				output, verifyErr := runVerifyCmd(ctx, opts.verifyCmd, cwd, opts.verifyTimeout)
				attempts++
				slog.Debug("verify attempt", "attempt", attempts, "max", opts.verifyMaxRetries, "err", verifyErr, "module", "task")
				if verifyErr != nil {
					if ctx.Err() != nil {
						return
					}
					if attempts <= opts.verifyMaxRetries {
						os.Remove(signalPath)
						signalExists = false
						msg := fmt.Sprintf("Verification failed (attempt %d/%d), please fix the issue and try again:\n```\n%s\n```\n", attempts, opts.verifyMaxRetries, output)
						sendTmuxMessage(tmuxSession, msg)
						sendTmuxEnter(tmuxSession)
						continue
					}
					// Retries exhausted
					msg := fmt.Sprintf("verification failed after %d attempt(s): %s", attempts, output)
					slog.Debug(msg, "event", taskSignal.Event, "module", "task")
					sendNotification(taskSignal.SessionID, cwd, "waiting", msg)
					return
				}
				slog.Debug("verify passed", "attempt", attempts)
			}
			if opts.reviewPrompt != "" {
				if reviewAttempts < opts.maxReviewIterations {
					diff, diffErr := getGitDiff(cwd, baseCommit)
					if diffErr != nil {
						slog.Warn("failed to get git diff, skipping review", "err", diffErr, "module", "task")
					} else if diff == "" {
						slog.Debug("skipping review: empty diff", "module", "task")
					} else {
						if ctx.Err() != nil {
							return
						}
						slog.Debug("reviewing", "attempt", reviewAttempts+1, "max", opts.maxReviewIterations, "module", "task")
						reviewOutput, reviewErr := runReviewAgent(ctx, opts.reviewPrompt, diff, cwd, opts.reviewTimeout)
						outputPreview := reviewOutput
						if r := []rune(reviewOutput); len(r) > 50 {
							outputPreview = string(r[:50])
						}
						slog.Debug("review complete", "err", reviewErr, "output_len", len(reviewOutput), "output", outputPreview, "module", "task")
						if reviewErr == nil && reviewOutput != "" {
							reviewAttempts++
							os.Remove(signalPath)
							signalExists = false
							msg := fmt.Sprintf("Please address the following review feedback:\n%s", reviewOutput)
							sendTmuxMessage(tmuxSession, msg)
							sendTmuxEnter(tmuxSession)
							attempts = 0 // reset verify attempts for post-review changes
							continue
						}
					}
				} else {
					slog.Info("review max iterations reached, proceeding anyway", "attempts", reviewAttempts, "module", "task")
				}
			}
			slog.Debug("exiting", "event", taskSignal.Event, "module", "task")
			sendTmuxMessage(tmuxSession, "/exit")
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

// runVerifyCmd runs a shell verification command in cwd with the given timeout.
// Returns combined output and the error (nil if the command succeeds).
func runVerifyCmd(ctx context.Context, verifyCmd, cwd string, timeout time.Duration) (string, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Using `bash` rather than `sh` to be consistent with how Claude Code executes commands
	cmd := executil.CommandContext(timeoutCtx, "bash", "-c", verifyCmd)
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// getCurrentCommit returns the full SHA of HEAD.
func getCurrentCommit(cwd string) (string, error) {
	timeoutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := executil.CommandContext(timeoutCtx, "git", "rev-parse", "HEAD")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// hasTrackedChanges returns true if any git-tracked files changed since baseCommit,
// whether committed by the agent or left as uncommitted edits.
// When excludeTaskFiles is set, TODO.md/DOING.md/DONE.md are ignored.
func hasTrackedChanges(cwd string, excludeTaskFiles bool, baseCommit string) (bool, error) {
	isTaskFile := func(path string) bool {
		b := filepath.Base(path)
		return b == "TODO.md" || b == "DOING.md" || b == "DONE.md"
	}

	// Check for new commits since the task started (agent committed its own changes).
	// getCurrentCommit runs git rev-parse on every poll tick; if HEAD is stable this
	// is a no-op fast-path, but it could be memoised if profiling ever surfaces it.
	if baseCommit != "" {
		currentCommit, err := getCurrentCommit(cwd)
		if err != nil {
			return false, err
		}
		if currentCommit != baseCommit {
			if !excludeTaskFiles {
				return true, nil
			}
			// Verify commits touched at least one non-task file.
			cmd := exec.Command("git", "diff", "--name-only", baseCommit+"..HEAD")
			cmd.Dir = cwd
			out, err := cmd.Output()
			if err != nil {
				return true, fmt.Errorf("git diff --name-only: %w", err)
			}
			for _, f := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if f != "" && !isTaskFile(f) {
					return true, nil
				}
			}
		}
	}

	// Check for uncommitted changes.
	statusCmd := exec.Command("git", "status", "--porcelain")
	statusCmd.Dir = cwd
	out, err := statusCmd.Output()
	if err != nil {
		return false, fmt.Errorf("git status: %w", err)
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
		if excludeTaskFiles && isTaskFile(file) {
			continue
		}
		return true, nil
	}
	return false, nil
}

// getGitDiff returns changes since baseCommit: all commits between baseCommit and HEAD
// concatenated with any uncommitted working-tree edits (staged and unstaged vs HEAD).
// Falls back to git diff HEAD when baseCommit is empty.
// A file modified both by an agent commit and by a subsequent working-tree edit will
// appear in both sections; this is intentional — the review agent sees the full picture.
func getGitDiff(cwd, baseCommit string) (string, error) {
	var parts []string

	if baseCommit != "" {
		// Committed changes since the task started.
		cmd := exec.Command("git", "diff", baseCommit+"..HEAD")
		cmd.Dir = cwd
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("git diff committed changes: %w", err)
		}
		if s := strings.TrimSpace(string(out)); s != "" {
			parts = append(parts, s)
		}
	}

	// Uncommitted working-tree changes vs HEAD.
	cmd := exec.Command("git", "diff", "HEAD")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git diff HEAD: %w", err)
	}
	if s := strings.TrimSpace(string(out)); s != "" {
		parts = append(parts, s)
	}

	return strings.Join(parts, "\n\n"), nil
}

// runReviewAgent runs a non-interactive Claude review with the given prompt in cwd.
// diff covers all changes since the task's base commit (committed and uncommitted),
// as produced by getGitDiff; it is appended after reviewPrompt.
// Returns the review output and any error.
func runReviewAgent(ctx context.Context, reviewPrompt, diff, cwd string, timeout time.Duration) (string, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	prompt := reviewPrompt + "\n```diff\n" + diff + "\n```\n"
	cmd := executil.CommandContext(timeoutCtx, "claude", "--print", "--permission-mode", "default")
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "TCLAUDE_IGNORE_HOOKS=true")
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// sendTmuxMessage sends arbitrary text + Enter to the tmux session.
func sendTmuxMessage(tmuxSession, message string) {
	cmd := clcommon.TmuxCommand("send-keys", "-t", tmuxSession, message, "Enter")
	cmd.Run()
}

// sendTmuxEnter sends just an Enter keypress to the tmux session.
func sendTmuxEnter(tmuxSession string) {
	cmd := clcommon.TmuxCommand("send-keys", "-t", tmuxSession, "Enter")
	cmd.Run()
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
