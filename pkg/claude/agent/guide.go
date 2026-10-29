package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
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
	Tab    string `long:"tab" optional:"true" help:"Dashboard tab to switch to: groups, processes, terminals, messages, jobs, usage, config, access, diagnostics, plugins, map, fleet-admin, boards"`
	Path   string `long:"path" optional:"true" help:"Dashboard path to open instead of a tab, e.g. /processes/templates/<id> or /access/sudo"`
	Target string `long:"target" optional:"true" help:"Visible text of the control or heading to highlight on that view, e.g. 'Save draft'"`
	Note   string `long:"note" optional:"true" help:"Short note shown next to the highlight (or on its own)"`

	Circle    []string `long:"circle" optional:"true" help:"Draw a circle around the control or text with this visible text (repeatable)"`
	Underline []string `long:"underline" optional:"true" help:"Underline the control or text with this visible text (repeatable)"`
	Arrow     []string `long:"arrow" optional:"true" help:"Draw an arrow between two controls by visible text, as 'From text -> To text' (repeatable)"`
}

// guideMarks turns the mark flags into the cue's marks.
func guideMarks(p *guideShowParams) ([]map[string]string, error) {
	var marks []map[string]string
	for _, t := range p.Circle {
		marks = append(marks, map[string]string{"kind": "circle", "target": strings.TrimSpace(t)})
	}
	for _, t := range p.Underline {
		marks = append(marks, map[string]string{"kind": "underline", "target": strings.TrimSpace(t)})
	}
	for _, a := range p.Arrow {
		from, to, ok := strings.Cut(a, "->")
		if !ok || strings.TrimSpace(from) == "" || strings.TrimSpace(to) == "" {
			return nil, fmt.Errorf("--arrow takes 'From text -> To text', got %q", a)
		}
		marks = append(marks, map[string]string{"kind": "arrow", "from": strings.TrimSpace(from), "target": strings.TrimSpace(to)})
	}
	return marks, nil
}

func guideShowCmd() *cobra.Command {
	return boa.CmdT[guideShowParams]{
		Use:   "show",
		Short: "Move the operator's dashboard to a view and highlight a control with a note",
		Long: "Posts a short-lived cue the operator's dashboard plays once: it switches to --tab or --path, then " +
			"highlights the control or heading whose visible text matches --target, with --note beside it. " +
			"--circle, --underline and --arrow draw hand-drawn marks on that view, by visible text like --target, to " +
			"point at several things at once. Give at least one of the flags. The cue cannot press anything; the operator acts.\n\n" +
			"Example: tclaude agent guide show --tab processes --target 'Save draft' --note 'Save once the diff looks right'\n" +
			"         tclaude agent guide show --circle 'Review' --arrow 'Rework -> Review' --note 'Rework never loops back'",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *guideShowParams, _ *cobra.Command, _ []string) {
			os.Exit(runGuideShow(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGuideShow(p *guideShowParams, stdout, stderr io.Writer) int {
	marks, err := guideMarks(p)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if p.Tab == "" && p.Path == "" && p.Target == "" && p.Note == "" && len(marks) == 0 {
		fmt.Fprintln(stderr, "Error: give at least one of --tab, --path, --target, --note, --circle, --underline or --arrow")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp struct {
		ID int64 `json:"id"`
	}
	body := map[string]any{"tab": p.Tab, "path": p.Path, "target": p.Target, "note": p.Note, "marks": marks}
	if err := DaemonPost("/v1/dashboard/cue", body, &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Cue #%d sent to the operator's dashboard.\n", resp.ID)
	return rcOK
}
