package task

import (
	"fmt"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

type ListParams struct{}

func ListCmd() *cobra.Command {
	return boa.CmdT[ListParams]{
		Use:         "list",
		Short:       "List tasks from TODO.md",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *ListParams, cmd *cobra.Command, args []string) {
			if err := runList(params); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
}

func runList(_ *ListParams) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current directory: %w", err)
	}

	tasks, err := ParseTodoMD(TodoPath(cwd))
	if err != nil {
		return fmt.Errorf("failed to read TODO.md: %w", err)
	}

	if len(tasks) == 0 {
		fmt.Println("No tasks in TODO.md")
		return nil
	}

	fmt.Printf("%d task(s) in TODO.md:\n\n", len(tasks))
	for i, t := range tasks {
		prompt := t.Prompt
		if len(prompt) > 80 {
			prompt = prompt[:77] + "..."
		}
		fmt.Printf("  %d. %s\n     %s\n\n", i+1, t.Title, prompt)
	}

	return nil
}
