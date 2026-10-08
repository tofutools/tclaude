package federationcmd

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

type nodesParams struct {
	Match string `long:"match" help:"All required matches: os=darwin,arch=arm64,label=gpu,harness=codex"`
	JSON  bool   `long:"json" help:"Output JSON"`
}
type remoteNode struct {
	*proto.NodeMetadata
	Peer       string    `json:"peer"`
	Instance   string    `json:"instance"`
	Online     bool      `json:"online"`
	ReceivedAt time.Time `json:"received_at"`
	Stale      bool      `json:"stale"`
}

func nodesCmd() *cobra.Command {
	return boa.CmdT[nodesParams]{Use: "nodes", Short: "List shared peer node capabilities and resource summaries", ParamEnrich: common.DefaultParamEnricher(), SubCmds: []*cobra.Command{NodeGroupsCmd()}, RunFunc: func(p *nodesParams, _ *cobra.Command, _ []string) { os.Exit(runNodes(p, os.Stdout, os.Stderr)) }}.ToCobra()
}
func runNodes(p *nodesParams, stdout, stderr io.Writer) int {
	if _, err := proto.ParseNodeMatch(p.Match); err != nil {
		return fail(stderr, err)
	}
	if rc := agent.RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	var rows []remoteNode
	if err := agent.DaemonGet("/v1/federation/nodes?match="+url.QueryEscape(p.Match), &rows); err != nil {
		return fail(stderr, err)
	}
	if p.JSON {
		return printJSON(stdout, rows)
	}
	printNodes(stdout, rows)
	return 0
}
func printNodes(w io.Writer, rows []remoteNode) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "PEER\tPLATFORM\tTCLAUDE\tHARNESSES\tLABELS\tCPU LOAD / CORES\tRAM FREE\tAGENTS / MAX\tSTATE")
	for _, n := range rows {
		if n.NodeMetadata == nil {
			continue
		}
		hs := []string{}
		for _, h := range n.Harnesses {
			s := h.Name
			if h.Version != "" {
				s += "=" + h.Version
			}
			hs = append(hs, s)
		}
		cpu, ram, agents := "unavailable", "unavailable", "unavailable"
		if n.Resources.CPU.LoadAverage != nil {
			cpu = fmt.Sprintf("%.2f / %d", n.Resources.CPU.LoadAverage[0], n.Resources.CPU.LogicalCores)
		}
		if n.Resources.RAM != nil {
			ram = fmt.Sprintf("%.1f GiB", float64(n.Resources.RAM.AvailableBytes)/(1<<30))
			if n.Resources.RAM.AvailableEstimated {
				ram += " (est.)"
			}
		}
		if n.Resources.Agents != nil {
			cap := fmt.Sprint(n.MaxLiveAgents)
			if n.MaxLiveAgents == 0 {
				cap = "unlimited"
			}
			agents = fmt.Sprintf("%d / %s", n.Resources.Agents.LiveAgents, cap)
		}
		state := "current"
		if n.Stale {
			state = "stale"
		}
		if !n.Online {
			state = "offline"
		}
		if n.Resources.Status == "warming" {
			state += " (warming)"
		}
		fmt.Fprintf(tw, "%s\t%s %s %s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", n.Peer, n.OS, dash(n.OSVersion), n.Arch, dash(n.TclaudeVersion), strings.Join(hs, ","), strings.Join(n.Labels, ","), cpu, ram, agents, state)
	}
	_ = tw.Flush()
}

type nodeLabelsParams struct {
	Add    []string `long:"add" help:"Add labels to the local set"`
	Remove []string `long:"remove" help:"Remove labels from the local set"`
	JSON   bool     `long:"json" help:"Output JSON"`
}

func nodeLabelsCmd() *cobra.Command {
	return boa.CmdT[nodeLabelsParams]{Use: "node-labels", Short: "Read or update this node's labels (operator only)", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *nodeLabelsParams, _ *cobra.Command, _ []string) {
		os.Exit(runNodeLabels(p, os.Stdout, os.Stderr))
	}}.ToCobra()
}
func runNodeLabels(p *nodeLabelsParams, stdout, stderr io.Writer) int {
	if rc := agent.RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	if len(p.Add)+len(p.Remove) > 0 {
		var result any
		if err := agent.DaemonPost("/v1/federation/node-labels", map[string]any{"add": p.Add, "remove": p.Remove}, &result); err != nil {
			return fail(stderr, err)
		}
	}
	var result struct {
		Labels []string `json:"labels"`
	}
	if err := agent.DaemonGet("/v1/federation/node-labels", &result); err != nil {
		return fail(stderr, err)
	}
	if p.JSON {
		return printJSON(stdout, result)
	}
	fmt.Fprintln(stdout, strings.Join(result.Labels, ","))
	return 0
}
