package task

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

type AddParams struct {
	Dir            string
	PlanMode       bool
	PlanAutoAccept bool
}

func AddCmd() *cobra.Command {
	params := &AddParams{}
	cmd := &cobra.Command{
		Use:   "add <prompt> | add <title> <prompt>",
		Short: "Add a task to TODO.md",
		Long:  "Add a new task to the TODO.md file in the current project.\nWith one arg, the title is auto-generated via Claude Code.\nWith two args, the first is the title and the second is the prompt.",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAdd(params, args)
		},
	}
	cmd.Flags().StringVarP(&params.Dir, "dir", "C", "", "Directory containing TODO.md (defaults to current directory)")
	cmd.Flags().BoolVar(&params.PlanMode, "plan", false, "Mark task as requiring planning (runs with --permission-mode plan)")
	cmd.Flags().BoolVar(&params.PlanAutoAccept, "plan-auto", false, "Plan first, then auto-accept and implement")
	return cmd
}

func runAdd(params *AddParams, args []string) error {
	var title, prompt string
	switch len(args) {
	case 1:
		prompt = args[0]
	case 2:
		title = args[0]
		prompt = args[1]
	default:
		return fmt.Errorf("expected 1 or 2 arguments: <prompt> or <title> <prompt>")
	}

	if title == "" {
		var err error
		title, err = generateTitle(prompt)
		if err != nil {
			return fmt.Errorf("failed to generate title: %w", err)
		}
		fmt.Printf("Generated title: %s\n", title)
	}

	dir, err := resolveDir(params.Dir)
	if err != nil {
		return err
	}

	todoPath := TodoPath(dir)

	// Parse existing tasks
	tasks, err := ParseTodoMD(todoPath)
	if err != nil {
		return fmt.Errorf("failed to read TODO.md: %w", err)
	}

	// Add new task (--plan-auto is a superset of --plan)
	planAutoAccept := params.PlanAutoAccept
	planMode := params.PlanMode || planAutoAccept
	tasks = append(tasks, Task{
		Title:          title,
		Prompt:         prompt,
		PlanMode:       planMode,
		PlanAutoAccept: planAutoAccept,
	})

	// Write back
	if err := WriteTodoMD(todoPath, tasks); err != nil {
		return fmt.Errorf("failed to write TODO.md: %w", err)
	}

	fmt.Printf("Added task: %s (%d total)\n", title, len(tasks))
	return nil
}

// generateTitle uses Claude Code in print mode to generate a short task title from a prompt.
func generateTitle(prompt string) (string, error) {
	cmd := exec.Command("claude", "-p",
		"Generate a short task title (max 8 words, no quotes, no markdown) for this task prompt: "+prompt)
	cmd.Env = append(os.Environ(), "TCLAUDE_IGNORE_HOOKS=true")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	title := strings.TrimSpace(string(out))
	if title == "" {
		return "", fmt.Errorf("claude returned empty title")
	}
	return title, nil
}
