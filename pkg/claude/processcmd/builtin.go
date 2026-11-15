package processcmd

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/process/builtin"
	"github.com/tofutools/tclaude/pkg/common"
)

// The built-in ways of working (tk_d02b0f4c): the same list and copy the
// dashboard's Start from strip offers.

func builtinCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "builtin",
		Short:       "The built-in ways of working: list them, and copy one into a group",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds:     []*cobra.Command{builtinLsCmd(), builtinUseCmd()},
		RunFunc: func(_ *struct{}, cmd *cobra.Command, _ []string) {
			_ = cmd.Help()
		},
	}.ToCobra()
}

type builtinLsParams struct {
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (for example 30s). Capped at 300s; timeout means deny."`
}

func builtinLsCmd() *cobra.Command {
	return boa.CmdT[builtinLsParams]{
		Use:         "ls",
		Short:       "List the built-in ways of working",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *builtinLsParams, _ *cobra.Command, _ []string) {
			exitProcessRuntime(runBuiltinLs(p, os.Stdout, os.Stderr), os.Stderr)
		},
	}.ToCobra()
}

type builtinSummaryJSON struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Steps       []string `json:"steps"`
}

func runBuiltinLs(p *builtinLsParams, stdout, stderr io.Writer) error {
	ask, err := agent.ParseAskHuman(p.AskHuman)
	if err != nil {
		return err
	}
	if err := requireProcessRuntime(stderr); err != nil {
		return err
	}
	var response struct {
		Builtins []builtinSummaryJSON `json:"builtins"`
	}
	if err := processRequest("", http.MethodGet, "/v1/process/builtins", nil, &response,
		agent.DaemonOpts{AskHuman: ask, RetryOutput: stderr}); err != nil {
		return err
	}
	for _, b := range response.Builtins {
		fmt.Fprintf(stdout, "%s\t%s\n  %s\n  steps: %s\n", b.ID, b.Name, b.Description, strings.Join(b.Steps, " → "))
	}
	fmt.Fprintln(stdout, "\nCopy one into a group: tclaude process templates builtin use <id> --group <group>")
	return nil
}

type builtinUseParams struct {
	ID       string `pos:"true" help:"The built-in's id (see builtin ls)"`
	Group    string `long:"group" help:"The group to copy it into: it becomes one of the group's saved processes"`
	Name     string `long:"name" optional:"true" help:"Name the group's copy (default: the built-in's name)"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (for example 30s). Capped at 300s; timeout means deny."`
}

func builtinUseCmd() *cobra.Command {
	return boa.CmdT[builtinUseParams]{
		Use:         "use",
		Short:       "Copy a built-in way of working into a group, as its own process to run and edit",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *builtinUseParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.ID).SetAlternatives(builtin.IDs())
			return nil
		},
		RunFunc: func(p *builtinUseParams, _ *cobra.Command, _ []string) {
			exitProcessRuntime(runBuiltinUse(p, os.Stdout, os.Stderr), os.Stderr)
		},
	}.ToCobra()
}

func runBuiltinUse(p *builtinUseParams, stdout, stderr io.Writer) error {
	ask, err := agent.ParseAskHuman(p.AskHuman)
	if err != nil {
		return err
	}
	if strings.TrimSpace(p.Group) == "" {
		return fmt.Errorf("--group is required")
	}
	if err := requireProcessRuntime(stderr); err != nil {
		return err
	}
	var response struct {
		ID  string `json:"id"`
		Ref string `json:"ref"`
	}
	body := map[string]string{"group": p.Group, "name": p.Name}
	if err := processRequest("", http.MethodPost, "/v1/process/builtins/"+url.PathEscape(p.ID)+"/use", body, &response,
		agent.DaemonOpts{AskHuman: ask, RetryOutput: stderr}); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Copied %s into %s as template %s.\nRun it: tclaude process run %s --group %s\n", p.ID, p.Group, response.ID, response.ID, p.Group)
	return nil
}
