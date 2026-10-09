// Package host exposes local cached resource metrics through agentd.
package host

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/hostmetrics"
)

func Cmd() *cobra.Command {
	return boa.CmdT[struct{}]{Use: "host", Short: "Read local host resource metrics", ParamEnrich: common.DefaultParamEnricher(), SubCmds: []*cobra.Command{statusCmd()}}.ToCobra()
}

type statusParams struct {
	NoCache bool `long:"no-cache" help:"Sample current host metrics (debugging escape hatch; rate limited)"`
	JSON    bool `long:"json" help:"Output cached resource observations as JSON"`
}

func statusCmd() *cobra.Command {
	return boa.CmdT[statusParams]{Use: "status", Short: "Show cached CPU, RAM, disk and live agent load", Long: "Read agentd's local host snapshot. Sampling runs only with --no-cache.\nAgents require host.read, which is explicitly granted because configured local paths are included.", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *statusParams, cmd *cobra.Command, _ []string) {
		if err := runStatus(p.JSON, p.NoCache, cmd.OutOrStdout()); err != nil {
			fmt.Fprintln(cmd.ErrOrStderr(), "Error:", err)
			os.Exit(1)
		}
	}}.ToCobra()
}

type statusReadout struct {
	hostmetrics.Snapshot
	Status     string                `json:"status"`
	AgeSeconds *int64                `json:"age_seconds"`
	Warnings   []hostmetrics.Warning `json:"warnings"`
}

func runStatus(asJSON, fresh bool, out io.Writer) error {
	var raw json.RawMessage
	if err := agent.DaemonGet(agent.FreshReadPath("/v1/host/status", fresh), &raw); err != nil {
		return err
	}
	if asJSON {
		var formatted bytes.Buffer
		if err := json.Indent(&formatted, raw, "", "  "); err != nil {
			return err
		}
		formatted.WriteByte('\n')
		_, err := formatted.WriteTo(out)
		return err
	}

	return renderStatus(raw, out)
}
func renderStatus(raw json.RawMessage, out io.Writer) error {
	var s statusReadout
	if err := json.Unmarshal(raw, &s); err != nil {
		return err
	}
	fmt.Fprintf(out, "Host: %s/%s — %s", s.OS, s.Arch, s.Status)
	if s.AgeSeconds != nil {
		fmt.Fprintf(out, " (sample %s ago)", time.Duration(*s.AgeSeconds)*time.Second)
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "CPU: %d logical cores", s.CPU.LogicalCores)
	if s.CPU.LoadAverage != nil {
		n := *s.CPU.LoadAverage
		fmt.Fprintf(out, "; load 1m/5m/15m %.2f / %.2f / %.2f", n[0], n[1], n[2])
	} else {
		fmt.Fprint(out, "; load unavailable")
	}
	fmt.Fprintln(out)
	if s.RAM != nil {
		fmt.Fprintf(out, "RAM: %s available / %s total", formatBytes(s.RAM.AvailableBytes), formatBytes(s.RAM.TotalBytes))
		if s.RAM.AvailableEstimated {
			fmt.Fprint(out, " (estimated available)")
		}
		fmt.Fprintln(out)
	} else {
		fmt.Fprintln(out, "RAM: unavailable")
	}
	for _, d := range s.Disks {
		fmt.Fprintf(out, "Disk %s %s: ", d.Kind, d.Path.Path)
		if d.Error != "" {
			fmt.Fprintf(out, "unavailable (%s)\n", d.Error)
			continue
		}
		fmt.Fprintf(out, "%s available / %s total", formatBytes(d.AvailableBytes), formatBytes(d.TotalBytes))
		if d.MeasuredPath != "" && d.MeasuredPath != d.Path.Path {
			fmt.Fprintf(out, " (via %s)", d.MeasuredPath)
		}
		fmt.Fprintln(out)
	}
	if s.Tclaude != nil {
		fmt.Fprintf(out, "tclaude: %d live agents; %d live sessions\n", s.Tclaude.LiveAgents, s.Tclaude.LiveSessions)
	} else {
		fmt.Fprintln(out, "tclaude: load unavailable")
	}
	for _, w := range s.Warnings {
		fmt.Fprintf(out, "Warning [%s] %s: %s\n", w.Code, w.Resource, w.Message)
	}
	for _, e := range s.Errors {
		fmt.Fprintln(out, "Unavailable:", e)
	}
	return nil
}
func formatBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	suffixes := []string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}
	for _, suffix := range suffixes {
		value /= unit
		if value < unit || suffix == "EiB" {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%d B", n)
}
