package task

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// TasksConfig holds project-level configuration from tasks.json.
type TasksConfig struct {
	VerifyCmd           string `json:"verify,omitempty"`
	MaxVerifyIterations int    `json:"max_verify_iterations,omitempty"`
}

const defaultMaxVerifyIterations = 3

// TasksConfigPath returns the path to tasks.json in the given directory.
func TasksConfigPath(dir string) string {
	return filepath.Join(dir, "tasks.json")
}

// LoadTasksConfig reads tasks.json from the given directory.
// Returns defaults if the file does not exist.
func LoadTasksConfig(dir string) (TasksConfig, error) {
	cfg := TasksConfig{MaxVerifyIterations: defaultMaxVerifyIterations}
	data, err := os.ReadFile(TasksConfigPath(dir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("invalid tasks.json: %w", err)
	}
	if cfg.MaxVerifyIterations <= 0 {
		cfg.MaxVerifyIterations = defaultMaxVerifyIterations
	}
	return cfg, nil
}

// Task represents a task to be done
type Task struct {
	Title          string
	Prompt         string
	PlanMode       bool // run with --permission-mode plan instead of acceptEdits
	PlanAutoAccept bool // plan mode + auto-accept and implement
}

// TaskResult represents a completed task
type TaskResult struct {
	Title     string
	Prompt    string
	Status    string // "completed" or "failed"
	Error     string // error message if failed
	Commit    string // commit hash
	PlanFile  string // path to Claude's plan file, if any
	Report    string // Claude's output
	SessionID string // Claude's session_id from hook input
	Timestamp time.Time
}

type TaskParams struct {
	Dir string `short:"C" long:"dir" optional:"true" help:"Directory containing task files (defaults to current directory)"`
}

func Cmd() *cobra.Command {
	cmd := boa.CmdT[TaskParams]{
		Use:         "task",
		Short:       "Manage and run sequential tasks",
		Long:        "Define tasks in TODO.md and run them sequentially with Claude Code.\n\nWhen run without a subcommand, lists current tasks.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			AddCmd(),
			ListCmd(),
			RunCmd(),
		},
		RunFunc: func(params *TaskParams, cmd *cobra.Command, args []string) {
			if err := runList(&ListParams{Dir: params.Dir}); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
	cmd.Aliases = []string{"tasks"}
	return cmd
}

// resolveDir returns an absolute directory path from the given dir parameter.
// If dir is empty, the current working directory is used.
func resolveDir(dir string) (string, error) {
	if dir == "" {
		d, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("failed to get current directory: %w", err)
		}
		return d, nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("failed to resolve directory: %w", err)
	}
	return abs, nil
}

// TodoPath returns the path to TODO.md in the given directory
func TodoPath(dir string) string {
	return filepath.Join(dir, "TODO.md")
}

// DoingPath returns the path to DOING.md in the given directory
func DoingPath(dir string) string {
	return filepath.Join(dir, "DOING.md")
}

// DonePath returns the path to DONE.md in the given directory
func DonePath(dir string) string {
	return filepath.Join(dir, "DONE.md")
}

// ParseTodoMD reads and parses TODO.md into a list of tasks.
// Tasks are delimited by ## headers. The header text is the title,
// and everything until the next header (or EOF) is the prompt.
func ParseTodoMD(path string) ([]Task, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	return parseTasks(string(data)), nil
}

func parseTasks(content string) []Task {
	var tasks []Task
	var current *Task

	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "## ") {
			// Save previous task
			if current != nil {
				current.Prompt = strings.TrimSpace(current.Prompt)
				if current.Title != "" {
					tasks = append(tasks, *current)
				}
			}
			title := strings.TrimSpace(strings.TrimPrefix(line, "## "))
			planMode := false
			planAutoAccept := false
			if strings.HasPrefix(title, "[plan-auto] ") {
				planAutoAccept = true
				planMode = true
				title = strings.TrimPrefix(title, "[plan-auto] ")
			} else if strings.HasPrefix(title, "[plan] ") {
				planMode = true
				title = strings.TrimPrefix(title, "[plan] ")
			}
			current = &Task{
				Title:          title,
				PlanMode:       planMode,
				PlanAutoAccept: planAutoAccept,
			}
			continue
		}

		if current != nil {
			current.Prompt += line + "\n"
		}
	}

	// Save last task
	if current != nil {
		current.Prompt = strings.TrimSpace(current.Prompt)
		if current.Title != "" {
			tasks = append(tasks, *current)
		}
	}

	return tasks
}

// WriteTodoMD writes tasks to TODO.md
func WriteTodoMD(path string, tasks []Task) error {
	if len(tasks) == 0 {
		return os.WriteFile(path, []byte(""), 0644)
	}

	var sb strings.Builder
	for i, t := range tasks {
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString("## ")
		if t.PlanAutoAccept {
			sb.WriteString("[plan-auto] ")
		} else if t.PlanMode {
			sb.WriteString("[plan] ")
		}
		sb.WriteString(t.Title)
		sb.WriteString("\n\n")
		sb.WriteString(t.Prompt)
		sb.WriteString("\n")
	}

	return os.WriteFile(path, []byte(sb.String()), 0644)
}

// WriteDoingMD writes the current in-progress task to DOING.md
func WriteDoingMD(path string, task Task) error {
	var sb strings.Builder
	sb.WriteString("## ")
	if task.PlanAutoAccept {
		sb.WriteString("[plan-auto] ")
	} else if task.PlanMode {
		sb.WriteString("[plan] ")
	}
	sb.WriteString(task.Title)
	sb.WriteString("\n\n")
	sb.WriteString(task.Prompt)
	sb.WriteString("\n")
	return os.WriteFile(path, []byte(sb.String()), 0644)
}

// ClearDoingMD removes the DOING.md file
func ClearDoingMD(path string) error {
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// AppendDoneMD appends a completed task result to DONE.md
func AppendDoneMD(path string, result TaskResult) error {
	// Read existing content
	existing, _ := os.ReadFile(path)

	var sb strings.Builder
	if len(existing) > 0 {
		sb.Write(existing)
		if !strings.HasSuffix(string(existing), "\n") {
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}

	sb.WriteString("## ")
	sb.WriteString(result.Title)
	sb.WriteString("\n\n")
	sb.WriteString(fmt.Sprintf("- **Status:** %s\n", result.Status))
	sb.WriteString(fmt.Sprintf("- **Completed:** %s\n", result.Timestamp.Format("2006-01-02 15:04:05")))
	if result.SessionID != "" {
		sb.WriteString(fmt.Sprintf("- **Session ID:** %s\n", result.SessionID))
	}
	if result.Commit != "" {
		sb.WriteString(fmt.Sprintf("- **Commit:** %s\n", result.Commit))
	}
	if result.PlanFile != "" {
		sb.WriteString(fmt.Sprintf("- **Plan:** %s\n", result.PlanFile))
	}
	if result.Error != "" {
		sb.WriteString(fmt.Sprintf("- **Error:** %s\n", result.Error))
	}

	sb.WriteString("\n<details>\n<summary>Prompt</summary>\n\n")
	sb.WriteString(result.Prompt)
	sb.WriteString("\n\n</details>\n")

	if result.Report != "" {
		sb.WriteString("\n<details>\n<summary>Report</summary>\n\n")
		sb.WriteString(result.Report)
		sb.WriteString("\n\n</details>\n")
	}

	sb.WriteString("\n---\n")

	return os.WriteFile(path, []byte(sb.String()), 0644)
}
