package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
)

type teleportParams struct {
	Target           string `pos:"true" optional:"true" help:"Trusted peer or group:<node pool>"`
	Node             string `long:"node" optional:"true" help:"auto or group:<node pool>"`
	Group            string `long:"group" optional:"true" help:"Receiving group; automatic only when exactly one is authorized"`
	Require          string `long:"require" optional:"true" help:"Required node metadata matches"`
	Prefer           string `long:"prefer" optional:"true" help:"least-loaded or most-free-ram"`
	KeepPausedBackup bool   `long:"keep-paused-backup" help:"Keep a stopped origin backup with lease-loss recovery"`
	Clone            bool   `long:"clone" help:"Keep this agent running after the history clone lands"`
	Home             bool   `long:"home" help:"Return to the recorded origin, subject to live permissions and policy"`
	Note             string `long:"note" optional:"true" help:"Continuation briefing on the destination"`
	Credentials      string `long:"credentials" optional:"true" help:"local or proxy:<name>@<peer>; proxy refuses until available"`
	GitRef           string `long:"git-ref" optional:"true" help:"Allowlisted repo/ref checkout (requires receiver support)"`
	JSON             bool   `long:"json" help:"Output JSON"`
}

func teleportCmd() *cobra.Command {
	return boa.CmdT[teleportParams]{Use: "teleport", Short: "Move or clone yourself to a peer, continuing your history", ParamEnrich: common.DefaultParamEnricher(), SubCmds: []*cobra.Command{teleportReportCmd(), teleportRecoverCmd(), boa.CmdT[struct{}]{Use: "status", Short: "Inspect your incoming and outgoing teleport records", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(_ *struct{}, _ *cobra.Command, _ []string) {
		var out any
		if err := DaemonGet("/v1/whoami/teleports", &out); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(MapDaemonErrorToRC(err))
		}
		_ = json.NewEncoder(os.Stdout).Encode(out)
	}}.ToCobra()}, RunFunc: func(p *teleportParams, _ *cobra.Command, _ []string) { os.Exit(runTeleport(p, os.Stdout, os.Stderr)) }}.ToCobra()
}
func runTeleport(p *teleportParams, stdout, stderr io.Writer) int {
	if p.Clone && p.KeepPausedBackup {
		fmt.Fprintln(stderr, "clone and paused backup are mutually exclusive")
		return rcInvalidArg
	}
	if !bundletransfer.ValidCredentials(p.Credentials) {
		fmt.Fprintln(stderr, "credentials must be local or proxy:<name>@<peer>")
		return rcInvalidArg
	}
	if len(p.Note) > 4096 {
		fmt.Fprintln(stderr, "teleport note exceeds 4096 bytes")
		return rcInvalidArg
	}
	peer, node := p.Target, p.Node
	if strings.HasPrefix(peer, "group:") {
		if node != "" {
			fmt.Fprintln(stderr, "target pool and --node are mutually exclusive")
			return rcInvalidArg
		}
		node, peer = peer, ""
	}
	if p.Home && (peer != "" || node != "") || !p.Home && peer == "" && node == "" || peer != "" && node != "" {
		fmt.Fprintln(stderr, "choose one peer, node pool, --node auto, or --home")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	request := map[string]any{"peer": peer, "node": node, "group": p.Group, "require": p.Require, "prefer": p.Prefer, "keep_paused_backup": p.KeepPausedBackup, "clone": p.Clone, "home": p.Home, "note": p.Note, "credentials": p.Credentials, "git_ref": p.GitRef}
	var response map[string]any
	if err := DaemonRequest(http.MethodPost, "/v1/whoami/teleport", request, &response, DaemonOpts{Timeout: bundletransfer.DefaultTTL}); err != nil {
		var de *DaemonError
		if errors.As(err, &de) && p.JSON && len(de.Raw) > 0 {
			fmt.Fprintln(stdout, string(de.Raw))
		} else {
			fmt.Fprintln(stderr, err)
			if errors.As(err, &de) {
				var details struct {
					Placement struct {
						Candidates []struct {
							Peer   string `json:"peer"`
							Reason string `json:"reason"`
						} `json:"candidates"`
					} `json:"placement"`
				}
				if json.Unmarshal(de.Raw, &details) == nil {
					for _, c := range details.Placement.Candidates {
						fmt.Fprintf(stderr, "  %s: %s\n", c.Peer, c.Reason)
					}
				}
			}
		}
		return MapDaemonErrorToRC(err)
	}
	if !p.JSON {
		if id, ok := response["return_id"]; ok {
			fmt.Fprintf(stdout, "Return queued (%v). Findings will be delivered after this roaming copy stops.\n", id)
			return rcOK
		}
		var details struct {
			TeleportState string `json:"teleport_state"`
			Offer         struct {
				Descriptor struct {
					ID string `json:"id"`
				} `json:"offer"`
			} `json:"offer"`
		}
		raw, _ := json.Marshal(response)
		if json.Unmarshal(raw, &details) == nil {
			fmt.Fprintf(stdout, "Teleport %s (offer %s). Source remains active until confirmed running.\n", details.TeleportState, details.Offer.Descriptor.ID)
			return rcOK
		}
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(response); err != nil {
		return rcIOFailure
	}
	return rcOK
}

type teleportReportParams struct {
	Findings string `pos:"true" help:"Findings to deliver to the stopped origin backup"`
}

func teleportReportCmd() *cobra.Command {
	return boa.CmdT[teleportReportParams]{Use: "report", Short: "Report findings and return to your paused backup", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *teleportReportParams, _ *cobra.Command, _ []string) {
		os.Exit(runTeleportControl("/v1/whoami/teleport/report", map[string]any{"findings": p.Findings}, os.Stdout, os.Stderr))
	}}.ToCobra()
}

type teleportRecoverParams struct {
	Agent string `pos:"true" help:"Stable agent ID of the paused source backup"`
}

func teleportRecoverCmd() *cobra.Command {
	return boa.CmdT[teleportRecoverParams]{Use: "recover", Short: "Recover a paused backup after its online lease observation period", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *teleportRecoverParams, _ *cobra.Command, _ []string) {
		os.Exit(runTeleportControl("/v1/teleport/recover", map[string]any{"agent": p.Agent}, os.Stdout, os.Stderr))
	}}.ToCobra()
}
func runTeleportControl(path string, input any, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	var out any
	if err := DaemonPost(path, input, &out); err != nil {
		fmt.Fprintln(stderr, err)
		return MapDaemonErrorToRC(err)
	}
	if err := json.NewEncoder(stdout).Encode(out); err != nil {
		return rcIOFailure
	}
	return rcOK
}
