package federationcmd

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common"
)

type hubStaleParams struct {
	Days    int  `long:"days" default:"30" help:"Remove admissions not seen for this many days (admins and this node excluded)"`
	Preview bool `long:"preview" help:"Only print the candidate list; do not ask to revoke"`
}
type hubStalePreview struct {
	Days      int       `json:"days"`
	Cutoff    time.Time `json:"cutoff"`
	Instances []struct {
		Instance string    `json:"instance"`
		Name     string    `json:"name"`
		LastSeen time.Time `json:"last_seen"`
	} `json:"instances"`
	More bool `json:"more"`
}

func hubStaleCmd() *cobra.Command {
	return boa.CmdT[hubStaleParams]{Use: "cleanup-stale", Short: "Preview and confirm removal of stale hub admissions", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *hubStaleParams, _ *cobra.Command, _ []string) {
		os.Exit(runHubStale(p, os.Stdin, os.Stdout, os.Stderr))
	}}.ToCobra()
}
func runHubStale(p *hubStaleParams, stdin io.Reader, stdout, stderr io.Writer) int {
	if p.Days < 1 || p.Days > 36500 {
		return fail(stderr, fmt.Errorf("--days must be between 1 and 36500"))
	}
	if rc := agent.RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	opts := agent.DaemonOpts{NoRetry: true, Timeout: 35 * time.Second}
	path := "/v1/federation/hub/admissions/cleanup"
	var preview hubStalePreview
	if err := agent.DaemonRequest("POST", path, map[string]any{"days": p.Days}, &preview, opts); err != nil {
		return fail(stderr, err)
	}
	if rc := printJSON(stdout, preview); rc != 0 {
		return rc
	}
	if p.Preview || len(preview.Instances) == 0 {
		return 0
	}
	fmt.Fprintf(stderr, "Revoke these %d admissions? Type yes to confirm: ", len(preview.Instances))
	var answer string
	if _, err := fmt.Fscan(io.LimitReader(stdin, 16), &answer); err != nil || answer != "yes" {
		fmt.Fprintln(stderr, "Cancelled; no admissions changed.")
		return 0
	}
	ids := make([]string, 0, len(preview.Instances))
	for _, row := range preview.Instances {
		ids = append(ids, row.Instance)
	}
	var result any
	if err := agent.DaemonRequest("POST", path, map[string]any{"days": preview.Days, "cutoff": preview.Cutoff, "instances": ids, "apply": true}, &result, opts); err != nil {
		return fail(stderr, err)
	}
	return printJSON(stdout, result)
}
