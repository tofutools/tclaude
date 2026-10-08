package federationcmd

import (
	"fmt"
	"io"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
)

type shareAgentParams struct {
	Agent        string `pos:"true" help:"Source agent selector; agents can use self."`
	Peer         string `pos:"true" help:"Trusted destination peer (label, name or instance ID)."`
	Group        string `long:"group" help:"Receiving group on the peer; it must grant agents.receive."`
	History      bool   `long:"history" help:"Clone with conversation history; the original keeps running."`
	AllowFlagged bool   `long:"allow-flagged" help:"Explicitly allow suspected credentials in configuration or transcripts."`
	JSON         bool   `long:"json" help:"Output JSON."`
}

func shareAgentCmd() *cobra.Command {
	return boa.CmdT[shareAgentParams]{Use: "share-agent", Short: "Offer an agent's configuration, optionally with conversation history", Long: "Shares configuration by default. --history clones the conversation while leaving the source running. The receiving operator previews and creates a fresh local agent; source permissions and ownership are advisory. Agent callers need agent.share with peer= scope; exporting another agent also requires agent.bundle.export.", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *shareAgentParams, _ *cobra.Command, _ []string) {
		os.Exit(runShareAgent(p, os.Stdout, os.Stderr))
	}}.ToCobra()
}
func runShareAgent(p *shareAgentParams, out, stderr io.Writer) int {
	in := map[string]any{"agent": p.Agent, "peer": p.Peer, "group": p.Group, "history": p.History, "allow_flagged": p.AllowFlagged}
	var response struct {
		Offer struct {
			Descriptor  bundletransfer.Descriptor `json:"offer"`
			Peer        string                    `json:"peer"`
			SenderAgent string                    `json:"sender_agent,omitempty"`
		} `json:"offer"`
		State    string                `json:"state"`
		Findings []agentbundle.Finding `json:"findings"`
		Warnings []string              `json:"warnings"`
	}
	if rc := offerPost(stderr, "/v1/federation/share-agent", in, &response); rc != 0 {
		return rc
	}
	if p.JSON {
		return printJSON(out, response)
	}
	fmt.Fprintf(out, "%s agent offer %s to %s/%s (expires %s). Receiver must explicitly import it.\n", response.State, response.Offer.Descriptor.ID, p.Peer, p.Group, response.Offer.Descriptor.ExpiresAt.Format("2006-01-02 15:04 MST"))
	for _, f := range response.Findings {
		fmt.Fprintf(stderr, "Flag: %s: %d matches at %v\n", f.Kind, f.Count, f.Locations)
	}
	for _, v := range response.Warnings {
		fmt.Fprintln(stderr, "Warning: "+v)
	}
	return 0
}
