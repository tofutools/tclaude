package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/common/termtext"
	"github.com/tofutools/tclaude/pkg/launcher"
)

type serviceStatusParams struct {
	Refresh bool `long:"refresh" help:"Detect the launcher again instead of reading the daemon's cached detection"`
	JSON    bool `long:"json" help:"Output JSON"`
}

type serviceRestartParams struct {
	JSON bool `long:"json" help:"Output JSON"`
}

// serviceStatus mirrors agentd.NodeService; agent cannot import agentd.
type serviceStatus struct {
	Launcher       launcher.Info `json:"launcher"`
	RestartPending string        `json:"restart_pending,omitempty"`
}

type serviceRestartResult struct {
	Method   string `json:"method"`
	Launcher string `json:"launcher"`
}

// ServiceCmd is `tclaude service`: how agentd is launched, and restarting it
// through that launcher.
func ServiceCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "service",
		Short:       "Show how agentd is launched (systemd, launchd, tmux, shell) and restart it through that launcher (operator only)",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			boa.CmdT[serviceStatusParams]{
				Use:         "status",
				Short:       "Show the running agentd's launcher: kind, unit or label, observed arguments and environment (secrets redacted)",
				ParamEnrich: common.DefaultParamEnricher(),
				RunFunc: func(p *serviceStatusParams, _ *cobra.Command, _ []string) {
					os.Exit(runServiceStatus(p, cliStdout, cliStderr))
				},
			}.ToCobra(),
			boa.CmdT[serviceRestartParams]{
				Use:         "restart",
				Short:       "Restart agentd gracefully, through systemd or launchd when it runs under one, keeping its launch arguments",
				ParamEnrich: common.DefaultParamEnricher(),
				RunFunc: func(p *serviceRestartParams, _ *cobra.Command, _ []string) {
					os.Exit(runServiceRestart(p, cliStdout, cliStderr))
				},
			}.ToCobra(),
		},
	}.ToCobra()
}

func serviceRequest(method, path string, in any, stderr io.Writer) (json.RawMessage, int) {
	if rc := RequireDaemonOrExit(stderr); rc != 0 {
		return nil, rc
	}
	var raw json.RawMessage
	if err := DaemonRequest(method, path, in, &raw, DaemonOpts{Timeout: 20 * time.Second, NoRetry: true, RetryOutput: stderr}); err != nil {
		var de *DaemonError
		if errors.As(err, &de) && json.Valid(de.Raw) {
			fmt.Fprintln(stderr, string(de.Raw))
		} else {
			fmt.Fprintln(stderr, "Error:", err)
		}
		return nil, MapDaemonErrorToRC(err)
	}
	return raw, 0
}

func runServiceStatus(p *serviceStatusParams, stdout, stderr io.Writer) int {
	path := "/v1/node/service"
	if p.Refresh {
		path += "?refresh=1"
	}
	raw, rc := serviceRequest(http.MethodGet, path, nil, stderr)
	if rc != 0 {
		return rc
	}
	if p.JSON {
		fmt.Fprintln(stdout, string(raw))
		return 0
	}
	var st serviceStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return rcIOFailure
	}
	printServiceStatus(st, stdout)
	return 0
}

func printServiceStatus(st serviceStatus, w io.Writer) {
	l := st.Launcher
	fmt.Fprintf(w, "Launcher:        %s\n", termtext.Cell(l.Kind))
	if l.Unit != "" {
		fmt.Fprintf(w, "Unit:            %s\n", termtext.Cell(l.Unit))
	}
	if l.Label != "" {
		fmt.Fprintf(w, "Label:           %s/%s\n", termtext.Cell(l.Domain), termtext.Cell(l.Label))
	}
	if l.UnitPath != "" {
		fmt.Fprintf(w, "Unit file:       %s\n", termtext.Cell(l.UnitPath))
	}
	if l.Kind == launcher.KindSystemdUser || l.Kind == launcher.KindSystemdSystem || l.Kind == launcher.KindLaunchd {
		fmt.Fprintf(w, "Managed:         %v\n", l.ManagedByTclaude)
	}
	if len(l.Argv) > 0 {
		fmt.Fprintf(w, "Unit command:    %s\n", termtext.Cell(strings.Join(l.Argv, " ")))
	}
	fmt.Fprintf(w, "Process command: %s\n", termtext.Cell(strings.Join(l.ProcessArgv, " ")))
	if l.RestartPolicy != "" {
		fmt.Fprintf(w, "Restart policy:  %s\n", termtext.Cell(l.RestartPolicy))
	}
	if l.KillMode != "" {
		fmt.Fprintf(w, "KillMode:        %s\n", termtext.Cell(l.KillMode))
	}
	for _, f := range l.EnvironmentFiles {
		fmt.Fprintf(w, "Env file:        %s\n", termtext.Cell(f))
	}
	for _, e := range l.Env {
		fmt.Fprintf(w, "  %s=%s\n", termtext.Cell(e.Key), termtext.Cell(e.Value))
	}
	fmt.Fprintf(w, "Restart method:  %s\n", termtext.Cell(l.RestartMethod))
	if st.RestartPending != "" {
		fmt.Fprintf(w, "Restart pending: %s\n", termtext.Cell(st.RestartPending))
	}
	for _, warning := range l.Warnings {
		fmt.Fprintln(w, "Warning:", termtext.Cell(warning))
	}
}

func runServiceRestart(p *serviceRestartParams, stdout, stderr io.Writer) int {
	raw, rc := serviceRequest(http.MethodPost, "/v1/node/service/restart", struct{}{}, stderr)
	if rc != 0 {
		return rc
	}
	if p.JSON {
		fmt.Fprintln(stdout, string(raw))
		return 0
	}
	var res serviceRestartResult
	if err := json.Unmarshal(raw, &res); err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return rcIOFailure
	}
	how := "re-executing in place with the same arguments"
	if res.Method == launcher.RestartSupervisor {
		how = "through " + res.Launcher
	}
	fmt.Fprintf(stdout, "agentd is restarting %s.\n", termtext.Cell(how))
	return 0
}
