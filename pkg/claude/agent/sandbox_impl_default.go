package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// sandboxImplDefaultCmd is `tclaude agent sandbox-impl default` — the
// install-wide sandbox implementation new agents get when neither the spawn nor
// any spawn profile chooses one. Existing agents are never affected: they
// relaunch under the implementation recorded when they were spawned.
func sandboxImplDefaultCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "default",
		Short: "Show, set or clear the default sandbox implementation for new agents",
		Long: "The lowest tier of sandbox-implementation resolution: explicit flag > spawn profile > " +
			"group default profile > global default profile > THIS > harness default.\n\n" +
			"`tclaude-layer` confines new agents in tclaude's own sandbox (the default on new " +
			"installs); `harness` leaves each harness's own sandbox in charge. `clear` returns the " +
			"install to undecided, which behaves like `harness` and is raised as a decision on the " +
			"dashboard's Doctor tab. `set` and `clear` require the `sandbox-profiles.manage` permission.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds:     []*cobra.Command{sandboxImplDefaultShowCmd(), sandboxImplDefaultSetCmd(), sandboxImplDefaultClearCmd()},
	}.ToCobra()
}

type sandboxImplDefaultResp struct {
	Value   string `json:"value"`
	Decided bool   `json:"decided"`
}

type sandboxImplDefaultShowParams struct {
	JSON bool `long:"json" help:"Emit stable JSON"`
}

func sandboxImplDefaultShowCmd() *cobra.Command {
	return boa.CmdT[sandboxImplDefaultShowParams]{Use: "show", Short: "Show the default sandbox implementation for new agents", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *sandboxImplDefaultShowParams, _ *cobra.Command, _ []string) {
		os.Exit(runSandboxImplDefault(http.MethodGet, "", p.JSON, os.Stdout, os.Stderr))
	}}.ToCobra()
}

type sandboxImplDefaultSetParams struct {
	Value string `pos:"true" help:"tclaude-layer | harness"`
	JSON  bool   `long:"json" help:"Emit stable JSON"`
}

func sandboxImplDefaultSetCmd() *cobra.Command {
	return boa.CmdT[sandboxImplDefaultSetParams]{
		Use: "set", Short: "Set the default sandbox implementation for new agents", ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *sandboxImplDefaultSetParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Value).SetAlternatives([]string{"tclaude-layer", "harness"})
			return nil
		},
		RunFunc: func(p *sandboxImplDefaultSetParams, _ *cobra.Command, _ []string) {
			os.Exit(runSandboxImplDefault(http.MethodPut, p.Value, p.JSON, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func sandboxImplDefaultClearCmd() *cobra.Command {
	return boa.CmdT[sandboxImplDefaultShowParams]{Use: "clear", Short: "Return the default sandbox implementation to undecided", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *sandboxImplDefaultShowParams, _ *cobra.Command, _ []string) {
		os.Exit(runSandboxImplDefault(http.MethodDelete, "", p.JSON, os.Stdout, os.Stderr))
	}}.ToCobra()
}

func runSandboxImplDefault(method, value string, asJSON bool, stdout, stderr io.Writer) int {
	var body any
	if method == http.MethodPut {
		value = strings.TrimSpace(value)
		if value == "" {
			fmt.Fprintln(stderr, "Error: a value is required (tclaude-layer | harness).")
			return rcInvalidArg
		}
		body = map[string]any{"value": value}
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp sandboxImplDefaultResp
	if err := DaemonRequest(method, "/v1/sandbox-impl-default", body, &resp, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(resp); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return rcIOFailure
		}
		return rcOK
	}
	switch {
	case !resp.Decided:
		fmt.Fprintln(stdout, "undecided — new agents use each harness's own sandbox (choose on the dashboard's Doctor tab)")
	case resp.Value == "tclaude-layer":
		fmt.Fprintln(stdout, "tclaude-layer — new agents use tclaude's own sandbox")
	default:
		fmt.Fprintln(stdout, "harness — new agents use each harness's own sandbox")
	}
	return rcOK
}
