package workflows

import (
	"fmt"
	"io"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/ccworkflows"
	"github.com/tofutools/tclaude/pkg/common"
)

// CatParams configures `workflows cat`.
type CatParams struct {
	Name string `pos:"true" help:"The saved workflow name (filename without .js)."`
}

// CatCmd returns the `workflows cat` subcommand.
func CatCmd() *cobra.Command {
	return boa.CmdT[CatParams]{
		Use:   "cat <name>",
		Short: "Print a saved workflow script",
		Long: "Print the source of a saved workflow template by name (its filename without\n" +
			".js), searched in ~/.claude/workflows/saved and the project-local mirror.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *CatParams, _ *cobra.Command, _ []string) {
			if code := RunCat(params, os.Stdout, os.Stderr); code != 0 {
				os.Exit(code)
			}
		},
	}.ToCobra()
}

// RunCat is the testable core of `workflows cat`.
func RunCat(params *CatParams, stdout, stderr io.Writer) int {
	if params.Name == "" {
		fmt.Fprintln(stderr, "Error: a saved workflow name is required (see `workflows ls --saved`).")
		return 2
	}
	projectDir, _ := os.Getwd() // "" is fine: just skips the project mirror
	_, content, err := ccworkflows.FindSavedScript(params.Name, projectDir)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	fmt.Fprint(stdout, content)
	if !endsWithNewline(content) {
		fmt.Fprintln(stdout)
	}
	return 0
}
