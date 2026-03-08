package task

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

type AddParams struct {
	Args []string `pos:"true" help:"<prompt> or <title> <prompt>"`
}

func AddCmd() *cobra.Command {
	return boa.CmdT[AddParams]{
		Use:         "add",
		Short:       "Add a task to TODO.md",
		Long:        "Add a new task to the TODO.md file in the current project.\nWith one arg, the title is auto-generated via Claude Code.\nWith two args, the first is the title and the second is the prompt.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *AddParams, cmd *cobra.Command, args []string) {
			if err := runAdd(params); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
}

func runAdd(params *AddParams) error {
	var title, prompt string
	switch len(params.Args) {
	case 1:
		prompt = params.Args[0]
	case 2:
		title = params.Args[0]
		prompt = params.Args[1]
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

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current directory: %w", err)
	}

	todoPath := TodoPath(cwd)

	// Parse existing tasks
	tasks, err := ParseTodoMD(todoPath)
	if err != nil {
		return fmt.Errorf("failed to read TODO.md: %w", err)
	}

	// Add new task
	tasks = append(tasks, Task{
		Title:  title,
		Prompt: prompt,
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
