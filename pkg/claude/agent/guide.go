package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// `tclaude agent guide` — dashboard guidance for companion agents (the
// Steward): see which dashboard view the operator is on, and point at it.

func guideCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "guide",
		Short: "See and point at the operator's dashboard view (companion agents)",
		Long: "Dashboard guidance for companion agents such as the Steward.\n\n" +
			"`guide view` reports which dashboard view the operator is looking at (needs dashboard.view.read). " +
			"`guide show` moves the operator's dashboard to a view and/or highlights a control there with a short note " +
			"(needs dashboard.guide). A cue never presses anything or changes state: the operator acts.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds:     []*cobra.Command{guideViewCmd(), guideShowCmd()},
	}.ToCobra()
}

type guideViewParams struct {
	JSON bool `long:"json" help:"Output JSON"`
}

type guideViewResp struct {
	Known      bool      `json:"known"`
	Tab        string    `json:"tab"`
	Path       string    `json:"path"`
	Title      string    `json:"title"`
	At         time.Time `json:"at"`
	AgeSeconds int       `json:"age_seconds"`
}

func guideViewCmd() *cobra.Command {
	return boa.CmdT[guideViewParams]{
		Use:         "view",
		Short:       "Show which dashboard view the operator is on",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *guideViewParams, _ *cobra.Command, _ []string) {
			os.Exit(runGuideView(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGuideView(p *guideViewParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp guideViewResp
	if err := DaemonGet("/v1/dashboard/view", &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(resp)
		return rcOK
	}
	if !resp.Known {
		fmt.Fprintln(stdout, "No dashboard view reported yet (the operator has not opened the dashboard since the daemon started).")
		return rcOK
	}
	fmt.Fprintf(stdout, "tab:   %s\npath:  %s\n", resp.Tab, resp.Path)
	if resp.Title != "" {
		fmt.Fprintf(stdout, "title: %s\n", resp.Title)
	}
	fmt.Fprintf(stdout, "seen:  %ds ago\n", resp.AgeSeconds)
	return rcOK
}

type guideShowParams struct {
	Tab    string `long:"tab" optional:"true" help:"Dashboard tab to switch to: groups, processes, terminals, messages, jobs, usage, config, access, diagnostics, plugins, map, fleet-admin"`
	Path   string `long:"path" optional:"true" help:"Dashboard path to open instead of a tab, e.g. /processes/templates/<id> or /access/sudo"`
	Target string `long:"target" optional:"true" help:"Visible text of the control or heading to highlight on that view, e.g. 'Save draft'"`
	Note   string `long:"note" optional:"true" help:"Short note shown next to the highlight (or on its own)"`
}

func guideShowCmd() *cobra.Command {
	return boa.CmdT[guideShowParams]{
		Use:   "show",
		Short: "Move the operator's dashboard to a view and highlight a control with a note",
		Long: "Posts a short-lived cue the operator's dashboard plays once: it switches to --tab or --path, then " +
			"highlights the control or heading whose visible text matches --target, with --note beside it. " +
			"Give at least one of the flags. The cue cannot press anything; the operator acts.\n\n" +
			"Example: tclaude agent guide show --tab processes --target 'Save draft' --note 'Save once the diff looks right'",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *guideShowParams, _ *cobra.Command, _ []string) {
			os.Exit(runGuideShow(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGuideShow(p *guideShowParams, stdout, stderr io.Writer) int {
	if p.Tab == "" && p.Path == "" && p.Target == "" && p.Note == "" {
		fmt.Fprintln(stderr, "Error: give at least one of --tab, --path, --target or --note")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp struct {
		ID int64 `json:"id"`
	}
	body := map[string]string{"tab": p.Tab, "path": p.Path, "target": p.Target, "note": p.Note}
	if err := DaemonPost("/v1/dashboard/cue", body, &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Cue #%d sent to the operator's dashboard.\n", resp.ID)
	return rcOK
}
