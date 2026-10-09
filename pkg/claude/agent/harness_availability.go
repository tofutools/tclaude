package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/nodeinfo"
)

type harnessLsParams struct {
	Node    string `long:"node" help:"Read a trusted peer's harness availability by local label or instance ID (operator only)"`
	Refresh bool   `long:"refresh" help:"Refresh the cached probe (10-second minimum between probes)"`
	JSON    bool   `long:"json" help:"Output full JSON, including peer omissions"`
}

func HarnessCmd() *cobra.Command {
	ls := boa.CmdT[harnessLsParams]{Use: "ls", Short: "List harness binaries available on the daemon's PATH (operator only)", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *harnessLsParams, _ *cobra.Command, _ []string) { os.Exit(runHarnessLs(p, os.Stdout, os.Stderr)) }}.ToCobra()
	return boa.CmdT[struct{}]{Use: "harness", Short: "Inspect coding harness availability", ParamEnrich: common.DefaultParamEnricher(), SubCmds: []*cobra.Command{ls}}.ToCobra()
}
func runHarnessLs(p *harnessLsParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	path := "/v1/harnesses/availability"
	if p.Node != "" {
		path = "/v1/federation/peer/" + url.PathEscape(p.Node) + "/harnesses/availability"
	}
	if p.Refresh {
		path += "?refresh=1"
	}
	var raw json.RawMessage
	if err := DaemonRequest(http.MethodGet, path, nil, &raw, DaemonOpts{Timeout: 20 * time.Second, NoRetry: true}); err != nil {
		var de *DaemonError
		if errors.As(err, &de) && json.Valid(de.Raw) {
			fmt.Fprintln(stderr, string(de.Raw))
		} else {
			fmt.Fprintf(stderr, "Error: %v\n", err)
		}
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		if err := json.NewEncoder(stdout).Encode(raw); err != nil {
			return rcIOFailure
		}
		return 0
	}
	var data nodeinfo.Availability
	if err := json.Unmarshal(raw, &data); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcIOFailure
	}
	cell := func(s string) string { return strings.Join(strings.Fields(proto.StripControls(s)), " ") }
	flag := func(v *bool) string {
		if v == nil {
			return "unknown"
		}
		if *v {
			return "yes"
		}
		return "no"
	}
	fmt.Fprintf(stdout, "Probe: %s (refresh after %s)\n", data.ObservedAt.Format(time.RFC3339), data.RefreshAfter.Format(time.RFC3339))
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "HARNESS\tINSTALLED\tPATH\tVERSION\tCREDENTIAL PRESENT\tUSABLE")
	for _, h := range data.Harnesses {
		version := h.Version
		if version == "" {
			version = h.VersionStatus
		}
		fmt.Fprintf(tw, "%s\t%t\t%s\t%s\t%s\t%s\n", cell(h.Name), h.Installed, cell(h.Path), cell(version), flag(h.CredentialPresent), flag(h.Usable))
	}
	if err := tw.Flush(); err != nil {
		return rcIOFailure
	}
	return 0
}
