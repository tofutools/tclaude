package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

type codexAppServerStatusParams struct {
	Target string `long:"target" optional:"true" help:"Show ANOTHER agent's Codex-drive diagnostics instead of self. Requires agent.context-info, or ownership of a group containing the target."`
	JSON   bool   `long:"json" help:"Output JSON"`
}

type codexAppServerStatus struct {
	ConvID            string `json:"conv_id"`
	Harness           string `json:"harness"`
	Drive             string `json:"drive"`
	DriveSource       string `json:"drive_source"`
	Health            string `json:"health"`
	RuntimeState      string `json:"runtime_state,omitempty"`
	CodexVersion      string `json:"codex_version,omitempty"`
	SupportedVersions string `json:"supported_versions,omitempty"`
	Generation        string `json:"generation,omitempty"`
	LaunchID          string `json:"launch_id,omitempty"`
	ServerPID         int    `json:"server_pid,omitempty"`
	ServerIdentity    string `json:"server_identity"`
	SocketIdentity    string `json:"socket_identity"`
	ClientConnection  string `json:"client_connection"`
	ThreadBinding     string `json:"thread_binding"`
	ThreadID          string `json:"thread_id,omitempty"`
	ApprovalOwner     string `json:"approval_owner"`
	StatusObservedAt  string `json:"status_observed_at,omitempty"`
	UsageObservedAt   string `json:"usage_observed_at,omitempty"`
	ContextSource     string `json:"context_source"`
	MessageDelivery   string `json:"message_delivery"`
	Detail            string `json:"detail,omitempty"`
	Recovery          string `json:"recovery"`
	Rollback          string `json:"rollback"`
	CallerConv        string `json:"caller_conv,omitempty"`
	CallerAgentID     string `json:"caller_agent_id,omitempty"`
}

func codexAppServerCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "codex-app-server",
		Short:       "Inspect the selected Codex drive and its safe app-server diagnostics",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds:     []*cobra.Command{codexAppServerStatusCmd()},
	}.ToCobra()
}

func codexAppServerStatusCmd() *cobra.Command {
	return boa.CmdT[codexAppServerStatusParams]{
		Use:   "status",
		Short: "Show drive selection, connection, binding, freshness, recovery, and rollback",
		Long: "Shows whether the agent uses the default send-keys drive or the optional Codex app-server drive, " +
			"including launch-resolution provenance and safe identity/proof state. Private socket and log paths are never returned. " +
			"By default this inspects the calling agent; --target uses the same read permission as agent context-info.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *codexAppServerStatusParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Target).SetAlternativesFunc(completeConvSelectors)
			return nil
		},
		RunFunc: func(p *codexAppServerStatusParams, _ *cobra.Command, _ []string) {
			os.Exit(runCodexAppServerStatus(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runCodexAppServerStatus(p *codexAppServerStatusParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	path := "/v1/whoami/codex-app-server"
	if target := strings.TrimSpace(p.Target); target != "" {
		path = "/v1/agent/" + url.PathEscape(target) + "/codex-app-server"
	}
	var status codexAppServerStatus
	if err := DaemonGet(path, &status); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(status); err != nil {
			return rcIOFailure
		}
		return rcOK
	}
	fmt.Fprintf(stdout, "conv:       %s\n", short(status.ConvID))
	fmt.Fprintf(stdout, "harness:    %s\n", status.Harness)
	fmt.Fprintf(stdout, "drive:      %s (%s)\n", status.Drive, status.DriveSource)
	fmt.Fprintf(stdout, "health:     %s\n", status.Health)
	if status.CodexVersion != "" {
		fmt.Fprintf(stdout, "version:    %s (supported %s)\n", status.CodexVersion, status.SupportedVersions)
	}
	if status.Generation != "" {
		fmt.Fprintf(stdout, "generation: %s (launch %s)\n", short(status.Generation), short(status.LaunchID))
	}
	fmt.Fprintf(stdout, "server:     %s", status.ServerIdentity)
	if status.ServerPID > 0 {
		fmt.Fprintf(stdout, " (pid %d)", status.ServerPID)
	}
	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "socket:     %s\n", status.SocketIdentity)
	fmt.Fprintf(stdout, "client:     %s\n", status.ClientConnection)
	fmt.Fprintf(stdout, "thread:     %s", status.ThreadBinding)
	if status.ThreadID != "" {
		fmt.Fprintf(stdout, " (%s)", short(status.ThreadID))
	}
	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "approvals:  %s\n", status.ApprovalOwner)
	if status.StatusObservedAt != "" {
		fmt.Fprintf(stdout, "status:     observed %s\n", status.StatusObservedAt)
	}
	if status.UsageObservedAt != "" {
		fmt.Fprintf(stdout, "usage:      observed %s\n", status.UsageObservedAt)
	}
	fmt.Fprintf(stdout, "context:    %s\n", status.ContextSource)
	fmt.Fprintf(stdout, "messages:   %s\n", status.MessageDelivery)
	if status.Detail != "" {
		fmt.Fprintf(stdout, "detail:     %s\n", status.Detail)
	}
	fmt.Fprintf(stdout, "recovery:   %s\n", status.Recovery)
	fmt.Fprintf(stdout, "rollback:   %s\n", status.Rollback)
	return rcOK
}
