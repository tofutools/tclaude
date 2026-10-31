package agent

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"text/tabwriter"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"

	"github.com/tofutools/tclaude/pkg/common"
)

func snapshotPolicyCmd() *cobra.Command {
	return boa.CmdT[struct{}]{Use: "policy", Short: "Send scheduled resilience snapshots to a federation peer (human only)",
		Long: "A resilience policy snapshots an agent, or every agent of a group, on a schedule and sends each snapshot,\n" +
			"encrypted, to a trusted federation peer that holds it for you, or puts it on a Board sealed to the members\n" +
			"you name. Snapshots are taken only while an agent is idle, and only when its conversation changed. A peer\n" +
			"must grant this node snapshots.receive. The newest --keep snapshots per agent are kept for at most\n" +
			"--max-age-days (30 at most on a board). A board never takes a snapshot with suspected credentials.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds:     []*cobra.Command{snapshotPolicySetCmd(), snapshotPolicyListCmd(), snapshotPolicyRemoveCmd(), snapshotPolicyRunCmd()},
	}.ToCobra()
}

type snapshotPolicySetParams struct {
	Target     string   `pos:"true" help:"Agent selector, or group:<name> for every agent of a group."`
	Every      string   `long:"every" help:"How often to snapshot, such as 30m or 6h; at least 10m."`
	To         string   `long:"to" help:"Destination: peer:<instance id or label>, or board:<board id>."`
	Recipients []string `long:"recipient" optional:"true" help:"For a board: a member to seal snapshots to (instance id or peer label); repeatable. This node is always added."`
	Keep       int      `long:"keep" help:"Snapshots the peer keeps per agent (1-20)." default:"5"`
	MaxAgeDays int      `long:"max-age-days" optional:"true" help:"Days the peer keeps a snapshot (default 30, at most 365)."`
	JSON       bool     `long:"json" help:"Print the policy as JSON."`
}

func snapshotPolicySetCmd() *cobra.Command {
	return boa.CmdT[snapshotPolicySetParams]{Use: "set", Short: "Add a resilience snapshot policy",
		Long: "Adding a policy is your consent to send the agents' conversations, encrypted, to that peer. Suspected\n" +
			"credentials are not removed: they are counted in policy ls and you are told once per policy.\n\n" +
			"  tclaude agent snapshot policy set reviewer --every 1h --to peer:bob --keep 5\n" +
			"  tclaude agent snapshot policy set group:team --every 6h --to board:BOARD --recipient bob --keep 3",
		ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *snapshotPolicySetParams, _ *cobra.Command, _ []string) {
			os.Exit(runSnapshotPolicySet(p, os.Stdout, os.Stderr))
		}}.ToCobra()
}

type resiliencePolicyOut struct {
	ID           string `json:"id"`
	Target       string `json:"target"`
	EverySeconds int64  `json:"every_seconds"`
	Dest         struct {
		Kind string `json:"kind"`
		Peer string `json:"peer"`
	} `json:"dest"`
	Keep       int    `json:"keep"`
	MaxAgeDays int    `json:"max_age_days"`
	PeerLabel  string `json:"peer_label"`
	Agents     []struct {
		Agent       string    `json:"agent"`
		LastGood    time.Time `json:"last_good"`
		LastBytes   int64     `json:"last_bytes"`
		Pending     string    `json:"pending_offer"`
		NextDue     time.Time `json:"next_due"`
		Deferred    string    `json:"deferred"`
		LastError   string    `json:"last_error"`
		Failures    int       `json:"failures"`
		Findings    int       `json:"findings"`
		LastAttempt time.Time `json:"last_attempt"`
	} `json:"agents"`
}

func runSnapshotPolicySet(p *snapshotPolicySetParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var out resiliencePolicyOut
	body := map[string]any{"target": p.Target, "every": p.Every, "to": p.To, "recipients": p.Recipients, "keep": p.Keep, "max_age_days": p.MaxAgeDays}
	if err := DaemonPost("/v1/resilience/policies", body, &out); err != nil {
		fmt.Fprintln(stderr, err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		return printJSON(stdout, out)
	}
	fmt.Fprintf(stdout, "Policy %s: %s every %s to %s, keep %d\n", out.ID, termClean(out.Target), time.Duration(out.EverySeconds)*time.Second, termClean(p.To), out.Keep)
	return rcOK
}

type snapshotPolicyListParams struct {
	JSON bool `long:"json" help:"Print the policies and their status as JSON."`
}

func snapshotPolicyListCmd() *cobra.Command {
	return boa.CmdT[snapshotPolicyListParams]{Use: "ls", Short: "List resilience policies and each agent's last snapshot",
		ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *snapshotPolicyListParams, _ *cobra.Command, _ []string) {
			os.Exit(runSnapshotPolicyList("", p.JSON, os.Stdout, os.Stderr))
		}}.ToCobra()
}

// runSnapshotPolicyList prints the policies; run, when set, runs that
// policy first.
func runSnapshotPolicyList(run string, asJSON bool, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp struct {
		Policies []resiliencePolicyOut `json:"policies"`
	}
	var err error
	if run != "" {
		err = DaemonPost("/v1/resilience/policies/"+url.PathEscape(run)+"/run", nil, &resp)
	} else {
		err = DaemonGet("/v1/resilience/policies", &resp)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return MapDaemonErrorToRC(err)
	}
	if asJSON {
		return printJSON(stdout, resp.Policies)
	}
	if len(resp.Policies) == 0 {
		fmt.Fprintln(stdout, "No resilience policies.")
		return rcOK
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "POLICY\tTARGET\tEVERY\tTO\tKEEP\tAGENT\tLAST GOOD\tSIZE\tFLAGS\tSTATUS")
	for _, p := range resp.Policies {
		to := p.PeerLabel
		if to == "" {
			to = p.Dest.Peer
		}
		head := fmt.Sprintf("%s\t%s\t%s\t%s\t%d", p.ID, termClean(p.Target), time.Duration(p.EverySeconds)*time.Second, termClean(to), p.Keep)
		if len(p.Agents) == 0 {
			fmt.Fprintf(tw, "%s\t-\tnever\t-\t0\tnot run yet\n", head)
		}
		for _, a := range p.Agents {
			good, size := "never", "-"
			if !a.LastGood.IsZero() {
				good, size = a.LastGood.Local().Format("2006-01-02 15:04"), humanSize(a.LastBytes)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n", head, a.Agent, good, size, a.Findings, termClean(policyAgentStatus(a.Pending, a.Deferred, a.LastError, a.Failures, a.NextDue)))
		}
	}
	_ = tw.Flush()
	return rcOK
}

func policyAgentStatus(pending, deferred, lastError string, failures int, next time.Time) string {
	switch {
	case lastError != "":
		return fmt.Sprintf("failing (%d): %s", failures, lastError)
	case pending != "":
		return "sending"
	case deferred != "":
		return "deferred: " + deferred
	case !next.IsZero():
		return "next " + next.Local().Format("15:04")
	}
	return "ok"
}

type snapshotPolicyIDParams struct {
	ID   string `pos:"true" help:"Policy id, as policy ls shows it."`
	JSON bool   `long:"json" help:"Print as JSON."`
}

func snapshotPolicyRemoveCmd() *cobra.Command {
	return boa.CmdT[snapshotPolicyIDParams]{Use: "rm", Short: "Remove a resilience policy",
		Long:        "Stops the schedule. Snapshots the peer already holds stay there until its retention removes them.",
		ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *snapshotPolicyIDParams, _ *cobra.Command, _ []string) {
			if rc := RequireDaemonOrExit(os.Stderr); rc != rcOK {
				os.Exit(rc)
			}
			if err := DaemonDelete("/v1/resilience/policies/"+url.PathEscape(p.ID), nil); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(MapDaemonErrorToRC(err))
			}
			fmt.Println("Removed policy", p.ID)
		}}.ToCobra()
}

func snapshotPolicyRunCmd() *cobra.Command {
	return boa.CmdT[snapshotPolicyIDParams]{Use: "run", Short: "Snapshot a policy's idle agents now",
		ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *snapshotPolicyIDParams, _ *cobra.Command, _ []string) {
			os.Exit(runSnapshotPolicyList(p.ID, p.JSON, os.Stdout, os.Stderr))
		}}.ToCobra()
}

type snapshotRecallsParams struct {
	JSON bool `long:"json" help:"Print as JSON."`
}

func snapshotRecallsCmd() *cobra.Command {
	return boa.CmdT[snapshotRecallsParams]{Use: "recalls", Short: "List this node's recalls of resilience snapshots (human only)",
		Long: "A recall (tclaude agent recover --recall) asks a peer to return a snapshot it holds of one of this node's\n" +
			"agents. It is requested, then ready once the snapshot is back, and recovered once landed; an unanswered\n" +
			"recall expires after an hour.",
		ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *snapshotRecallsParams, _ *cobra.Command, _ []string) {
			os.Exit(runSnapshotRecalls(p, os.Stdout, os.Stderr))
		}}.ToCobra()
}

func runSnapshotRecalls(p *snapshotRecallsParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp struct {
		Recalls []struct {
			ID          string    `json:"id"`
			Peer        string    `json:"peer"`
			SourceAgent string    `json:"source_agent"`
			State       string    `json:"state"`
			TakenAt     time.Time `json:"taken_at"`
			ExpiresAt   time.Time `json:"expires_at"`
		} `json:"recalls"`
	}
	if err := DaemonGet("/v1/resilience/recalls", &resp); err != nil {
		fmt.Fprintln(stderr, err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		return printJSON(stdout, resp.Recalls)
	}
	if len(resp.Recalls) == 0 {
		fmt.Fprintln(stdout, "No recalls.")
		return rcOK
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RECALL\tAGENT\tPEER\tSTATE\tTAKEN")
	for _, r := range resp.Recalls {
		taken := "-"
		if !r.TakenAt.IsZero() {
			taken = r.TakenAt.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.ID, r.SourceAgent, r.Peer, termClean(r.State), taken)
	}
	_ = tw.Flush()
	return rcOK
}

type snapshotHeldParams struct {
	RemovePeer string `long:"rm-peer" optional:"true" help:"Delete every snapshot kept for this peer (instance id or label)."`
	JSON       bool   `long:"json" help:"Print as JSON."`
}

func snapshotHeldCmd() *cobra.Command {
	return boa.CmdT[snapshotHeldParams]{Use: "held", Short: "List resilience snapshots this node keeps for peers (human only)",
		Long: "Peers with the snapshots.receive grant send this node encrypted resilience snapshots of their agents.\n" +
			"They are sealed to this node's key too, so it could decrypt them, but tclaude stores them encrypted and\n" +
			"never previews or imports them. Each is removed when its sender's keep count or maximum age says so; a\n" +
			"peer may use at most 2 GiB. --rm-peer deletes a peer's copies, for example after revoking the grant.",
		ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *snapshotHeldParams, _ *cobra.Command, _ []string) {
			os.Exit(runSnapshotHeld(p, os.Stdout, os.Stderr))
		}}.ToCobra()
}

func runSnapshotHeld(p *snapshotHeldParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	if p.RemovePeer != "" {
		var out struct {
			Peer    string `json:"peer"`
			Removed int    `json:"removed"`
		}
		if err := DaemonDelete("/v1/resilience/held/"+url.PathEscape(p.RemovePeer), &out); err != nil {
			fmt.Fprintln(stderr, err)
			return MapDaemonErrorToRC(err)
		}
		if p.JSON {
			return printJSON(stdout, out)
		}
		fmt.Fprintf(stdout, "Removed %d snapshot(s) kept for %s\n", out.Removed, out.Peer)
		return rcOK
	}
	var resp struct {
		Held []struct {
			Peer        string    `json:"peer"`
			SourceAgent string    `json:"source_agent"`
			AgentName   string    `json:"agent_name"`
			Harness     string    `json:"harness"`
			TakenAt     time.Time `json:"taken_at"`
			ExpiresAt   time.Time `json:"expires_at"`
			Bytes       int64     `json:"bytes"`
		} `json:"held"`
	}
	if err := DaemonGet("/v1/resilience/held", &resp); err != nil {
		fmt.Fprintln(stderr, err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		return printJSON(stdout, resp.Held)
	}
	if len(resp.Held) == 0 {
		fmt.Fprintln(stdout, "No held snapshots.")
		return rcOK
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PEER\tAGENT\tNAME\tHARNESS\tTAKEN\tSIZE\tEXPIRES")
	for _, h := range resp.Held {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", h.Peer, h.SourceAgent, termClean(h.AgentName), termClean(h.Harness), h.TakenAt.Local().Format("2006-01-02 15:04"), humanSize(h.Bytes), h.ExpiresAt.Local().Format("2006-01-02"))
	}
	_ = tw.Flush()
	return rcOK
}
