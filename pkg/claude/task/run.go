package task

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
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
	cwd := params.Dir
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get current directory: %w", err)
		}
	}

	// Make path absolute
	if cwd[0] != '/' {
		wd, _ := os.Getwd()
		cwd = wd + "/" + cwd
	}

	// Check we have tasks
	tasks, err := ParseTodoMD(TodoPath(cwd))
	if err != nil {
		return fmt.Errorf("failed to read TODO.md: %w", err)
	}
	if len(tasks) == 0 {
		return fmt.Errorf("no tasks found in TODO.md")
	}

	fmt.Printf("Found %d task(s) in TODO.md\n", len(tasks))

	if params.NoTmux {
		return runTaskLoop(cwd, clcommon.ExtractClaudeExtraArgs())
	}

	// Run in tmux session
	return runInTmux(cwd, params.Detached)
}

// runInTmux starts the task runner inside a tmux session
func runInTmux(cwd string, detached bool) error {
	if err := session.CheckTmuxInstalled(); err != nil {
		return err
	}
	session.EnsureHooksInstalled(false, os.Stdout, os.Stderr)

	sessionID := "tasks-" + session.GenerateSessionID()
	tmuxSession := "tclaude-" + sessionID

	// Build command to run the task loop inside tmux
	runnerCmd := fmt.Sprintf("%s task run --no-tmux -C %s",
		clcommon.DetectCmd(), clcommon.ShellQuoteArg(cwd))

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

	tmuxCmd := exec.Command("tmux", tmuxArgs...)
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
func runTaskLoop(cwd string, extraClaudeArgs []string) error {
	todoPath := TodoPath(cwd)
	donePath := DonePath(cwd)

	for {
		// Re-read TODO.md each iteration (in case it was modified externally)
		tasks, err := ParseTodoMD(todoPath)
		if err != nil {
			return fmt.Errorf("failed to read TODO.md: %w", err)
		}
		if len(tasks) == 0 {
			break
		}

		task := tasks[0]
		remaining := tasks[1:]
		totalOriginal := len(tasks)

		fmt.Printf("\n%s\n", strings.Repeat("=", 60))
		fmt.Printf("Task: %s (%d remaining)\n", task.Title, totalOriginal)
		fmt.Printf("%s\n\n", strings.Repeat("=", 60))

		// Run Claude Code interactively with the task prompt
		report, err := runClaude(cwd, task.Prompt, extraClaudeArgs)

		result := TaskResult{
			Title:     task.Title,
			Prompt:    task.Prompt,
			Report:    report,
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

		// Git commit all changes with task title as commit message
		commitHash := gitCommitAll(cwd, task.Title)
		result.Commit = commitHash

		// Update TODO.md (remove completed task)
		if err := WriteTodoMD(todoPath, remaining); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to update TODO.md: %v\n", err)
		}

		// Append to DONE.md
		if err := AppendDoneMD(donePath, result); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to update DONE.md: %v\n", err)
		}

		// Commit the tracking file updates
		gitCommitFiles(cwd, fmt.Sprintf("task: update tracking for %q", task.Title),
			[]string{"TODO.md", "DONE.md"})

		if result.Status == "failed" {
			sendNotification(cwd, fmt.Sprintf("Task failed: %s", task.Title))
			return fmt.Errorf("task %q failed: %s", task.Title, result.Error)
		}
	}

	fmt.Printf("\n%s\n", strings.Repeat("=", 60))
	fmt.Println("All tasks completed!")
	fmt.Printf("%s\n", strings.Repeat("=", 60))

	sendNotification(cwd, "All tasks completed!")

	return nil
}

// runClaude runs Claude Code interactively with the given prompt.
// Claude gets full terminal I/O — the user can approve permissions,
// answer questions, etc. When the user types /exit or Claude exits,
// control returns to the task runner.
func runClaude(cwd, prompt string, extraArgs []string) (string, error) {
	// Use --output-file to capture Claude's response
	outputFile, err := os.CreateTemp("", "tclaude-task-report-*.md")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}
	outputPath := outputFile.Name()
	outputFile.Close()
	defer os.Remove(outputPath)

	args := []string{prompt, "--output-file", outputPath}
	args = append(args, extraArgs...)

	cmd := exec.Command("claude", args...)
	cmd.Dir = cwd
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err = cmd.Run()

	// Read the captured output
	report, _ := os.ReadFile(outputPath)

	return string(report), err
}

// gitCommitAll stages all changes and commits with the given message.
// Returns the commit hash, or empty string on failure.
func gitCommitAll(cwd, message string) string {
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

// gitCommitFiles stages specific files and commits.
func gitCommitFiles(cwd, message string, files []string) {
	// Stage specified files
	args := append([]string{"add", "--"}, files...)
	addCmd := exec.Command("git", args...)
	addCmd.Dir = cwd
	if err := addCmd.Run(); err != nil {
		return // files might not exist or have no changes
	}

	// Check if there are staged changes
	diffCmd := exec.Command("git", "diff", "--cached", "--quiet")
	diffCmd.Dir = cwd
	if diffCmd.Run() == nil {
		return // nothing staged
	}

	commitCmd := exec.Command("git", "commit", "-m", message)
	commitCmd.Dir = cwd
	commitCmd.Run()
}

// sendNotification sends a desktop notification about task completion.
func sendNotification(cwd, message string) {
	if !notify.IsEnabled() {
		return
	}
	notify.OnStateTransition("tasks", "", "idle", cwd, message)
}
