package task

import (
	"fmt"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

type AddParams struct {
	Title  string `pos:"true" help:"Task title (used as commit message)"`
	Prompt string `pos:"true" help:"Prompt to send to Claude Code"`
}

func AddCmd() *cobra.Command {
	return boa.CmdT[AddParams]{
		Use:         "add",
		Short:       "Add a task to TODO.md",
		Long:        "Add a new task to the TODO.md file in the current project.",
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
	if params.Title == "" {
		return fmt.Errorf("task title is required")
	}
	if params.Prompt == "" {
		return fmt.Errorf("task prompt is required")
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
		Title:  params.Title,
		Prompt: params.Prompt,
	})

	// Write back
	if err := WriteTodoMD(todoPath, tasks); err != nil {
		return fmt.Errorf("failed to write TODO.md: %w", err)
	}

	fmt.Printf("Added task: %s (%d total)\n", params.Title, len(tasks))
	return nil
}
