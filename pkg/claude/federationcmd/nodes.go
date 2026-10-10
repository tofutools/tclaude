package federationcmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

type nodesParams struct {
	Summary bool   `long:"summary" help:"Fetch live map summaries for self and linked peers (operator only)"`
	Node    string `long:"node" optional:"true" help:"Fetch one live summary by pinned ID or local label (operator only)"`
	Watch   bool   `long:"watch" help:"Stream fleet health transitions (operator only; noisy signals require nodes health)"`
	Match   string `long:"match" optional:"true" help:"All required matches: os=darwin,arch=arm64,label=gpu,harness=codex"`
	JSON    bool   `long:"json" help:"Output JSON"`
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
	return boa.CmdT[nodesParams]{Use: "nodes", Short: "List shared peer node capabilities and resource summaries", ParamEnrich: common.DefaultParamEnricher(), SubCmds: []*cobra.Command{NodeGroupsCmd(), nodeHealthCmd()}, RunFunc: func(p *nodesParams, _ *cobra.Command, _ []string) { os.Exit(runNodes(p, os.Stdout, os.Stderr)) }}.ToCobra()
}
func runNodes(p *nodesParams, stdout, stderr io.Writer) int {
	if p.Summary || p.Node != "" {
		return runNodeSummaries(p, stdout, stderr)
	}
	if _, err := proto.ParseNodeMatch(p.Match); err != nil {
		return fail(stderr, err)
	}
	if rc := agent.RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	if p.Watch {
		return watchNodes(p, stdout, stderr)
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
	Add    []string `long:"add" optional:"true" help:"Add labels to the local set"`
	Remove []string `long:"remove" optional:"true" help:"Remove labels from the local set"`
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

func watchNodes(p *nodesParams, stdout, stderr io.Writer) int {
	if p.Match != "" {
		return fail(stderr, fmt.Errorf("--watch cannot be combined with --match"))
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	conn, err := agent.DaemonStreamGet(ctx, "/v1/federation/nodes/watch")
	if err != nil {
		return fail(stderr, err)
	}
	defer func() { _ = conn.Close() }()
	var rows []remoteNode
	if err := agent.DaemonGet("/v1/federation/nodes", &rows); err != nil {
		return fail(stderr, err)
	}
	if p.JSON {
		if err := json.NewEncoder(stdout).Encode(map[string]any{"nodes": rows}); err != nil {
			return fail(stderr, err)
		}
	} else {
		printNodes(stdout, rows)
	}
	dec := json.NewDecoder(conn)
	for {
		var e struct {
			At       time.Time `json:"at"`
			Instance string    `json:"instance"`
			Peer     string    `json:"peer"`
			Kind     string    `json:"kind"`
			Message  string    `json:"message"`
		}
		if err := dec.Decode(&e); err != nil {
			if ctx.Err() != nil {
				return 0
			}
			return fail(stderr, fmt.Errorf("fleet watch disconnected: %w; run --watch again", err))
		}
		if p.JSON {
			if err := json.NewEncoder(stdout).Encode(e); err != nil {
				return fail(stderr, err)
			}
		} else {
			if _, err := fmt.Fprintf(stdout, "%s %s %s: %s\n", e.At.Local().Format(time.RFC3339), e.Peer, e.Kind, e.Message); err != nil {
				return fail(stderr, err)
			}
		}
	}
}

type nodeHealthParams struct {
	Peer string `long:"peer" optional:"true" help:"Trusted peer label or instance ID; omit for defaults"`
	Set  string `long:"set" optional:"true" help:"Replace this policy with a JSON object (operator only)"`
}

func nodeHealthCmd() *cobra.Command {
	return boa.CmdT[nodeHealthParams]{Use: "health", Short: "Read or replace fleet notification policy", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *nodeHealthParams, _ *cobra.Command, _ []string) {
		if rc := agent.RequireDaemonOrExit(os.Stderr); rc != 0 {
			os.Exit(rc)
		}
		path := "/v1/federation/nodes/health?peer=" + url.QueryEscape(p.Peer)
		var result any
		if p.Set != "" {
			var policy map[string]any
			if err := json.Unmarshal([]byte(p.Set), &policy); err != nil {
				os.Exit(fail(os.Stderr, err))
			}
			if policy == nil {
				os.Exit(fail(os.Stderr, fmt.Errorf("--set requires a JSON object")))
			}
			if err := agent.DaemonPost(path, policy, &result); err != nil {
				os.Exit(fail(os.Stderr, err))
			}
		} else {
			if err := agent.DaemonGet(path, &result); err != nil {
				os.Exit(fail(os.Stderr, err))
			}
		}
		os.Exit(printJSON(os.Stdout, result))
	}}.ToCobra()
}
