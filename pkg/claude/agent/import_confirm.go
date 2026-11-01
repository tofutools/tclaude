package agent

import (
	"fmt"
	"io"
	"net/url"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// import_confirm.go is the CLI face of the import confirm list: the flags that
// keep a file's elevated items, and the list a preview prints. The daemon decides
// what is kept; see agentd/import_confirm.go.

// ImportKeepParams are the keep flags both imports share.
type ImportKeepParams struct {
	Keep         []string `long:"keep" optional:"true" help:"Human only: keep this elevated item, by the id the preview lists (repeatable). Needs --keep-digest."`
	KeepDefaults bool     `long:"keep-defaults" help:"Human only: keep what the dashboard's list starts with ticked: all but environment and scheduled jobs. Needs --keep-digest."`
	KeepAll      bool     `long:"keep-all" help:"Human only: keep every elevated item the preview lists. Needs --keep-digest."`
	KeepNone     bool     `long:"keep-none" help:"Keep no elevated item, not even the owners that land by default."`
	KeepDigest   string   `long:"keep-digest" optional:"true" help:"The file digest the preview prints, so a keep cannot apply to a file that was swapped."`
}

func (p *ImportKeepParams) addTo(q url.Values) {
	for _, id := range p.Keep {
		q.Add("keep", id)
	}
	if p.KeepDefaults {
		q.Set("keep_defaults", "true")
	}
	if p.KeepAll {
		q.Set("keep_all", "true")
	}
	if p.KeepNone {
		q.Set("keep_none", "true")
	}
	if p.KeepDigest != "" {
		q.Set("keep_digest", p.KeepDigest)
	}
}

// importConfirm is a preview's confirm list, as the daemon reports it.
type importConfirm struct {
	Digest  string `json:"digest"`
	CanKeep bool   `json:"can_keep"`
	Items   []struct {
		ID      string `json:"id"`
		Kind    string `json:"kind"`
		Label   string `json:"label"`
		Gives   string `json:"gives"`
		Default bool   `json:"default"`
	} `json:"items"`
	Warnings []string `json:"warnings"`
}

func (c *importConfirm) print(out io.Writer) {
	if c == nil || len(c.Items) == 0 {
		return
	}
	fmt.Fprintf(out, "\nElevated items in this file (* would start ticked in the dashboard):\n")
	for _, item := range c.Items {
		tick := " "
		if item.Default {
			tick = "*"
		}
		fmt.Fprintf(out, "  %s %s  (%s)\n      %s\n", tick, shown(item.ID), shown(item.Label), shown(item.Gives))
	}
	if !c.CanKeep {
		fmt.Fprintln(out, "Only a human on this node can keep these; this caller keeps none.")
		return
	}
	fmt.Fprintf(out, "Keep them with --keep-defaults, --keep <id> or --keep-all, plus --keep-digest %s\n", c.Digest)
}

// importOutcome is what became of a file's elevated items.
type importOutcome struct {
	Kept    []string       `json:"kept,omitempty"`
	Dropped []string       `json:"dropped,omitempty"`
	Confirm *importConfirm `json:"confirm,omitempty"`
}

func (o importOutcome) print(out io.Writer) {
	for _, item := range o.Kept {
		fmt.Fprintf(out, "  kept: %s\n", item)
	}
	for _, item := range o.Dropped {
		fmt.Fprintf(out, "  dropped: %s\n", item)
	}
}

type pafRevealParams struct {
	File string `pos:"true" help:"The file to read: a .paf or .zip archive"`
	Name string `pos:"true" help:"The environment variable, as the import preview names it"`
}

// PAFRevealCmd is `tclaude paf reveal`: the terminal's Reveal button.
func PAFRevealCmd() *cobra.Command {
	return boa.CmdT[pafRevealParams]{Use: "reveal", Short: "Print one environment variable's value from an import file",
		Long: "An import preview names a file's environment variables and never shows their values. " +
			"This prints one, so you can see what you would be keeping. Human only.",
		ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *pafRevealParams, _ *cobra.Command, _ []string) {
			os.Exit(runPAFReveal(p, os.Stdout, os.Stderr))
		}}.ToCobra()
}

func runPAFReveal(p *pafRevealParams, stdout, stderr io.Writer) int {
	archive, err := os.ReadFile(p.File)
	if err != nil {
		fmt.Fprintf(stderr, "Error: reading %q: %v\n", p.File, err)
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var revealed struct {
		Value string `json:"value"`
	}
	path := "/v1/import/reveal?" + url.Values{"item": {"env:" + p.Name}}.Encode()
	if err := DaemonPostRaw(path, "application/zip", archive, &revealed); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintln(stdout, revealed.Value)
	return rcOK
}
