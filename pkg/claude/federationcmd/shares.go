package federationcmd

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common"
)

type modelShare struct {
	Peer         string     `json:"peer"`
	PeerName     string     `json:"peer_name"`
	Gateway      string     `json:"gateway"`
	Dialect      string     `json:"dialect"`
	Models       []string   `json:"models"`
	MaxUSD       float64    `json:"max_usd"`
	SpentUSD     float64    `json:"spent_usd"`
	ExpiresAt    *time.Time `json:"expires_at"`
	Unrestricted bool       `json:"unrestricted"`
	UsedUp       bool       `json:"used_up"`
}

type sharesParams struct {
	Given bool `long:"given" optional:"true" help:"List what this node shares with peers instead of what peers share with it"`
	JSON  bool `long:"json" optional:"true" help:"Print JSON"`
}

func sharesCmd() *cobra.Command {
	return boa.CmdT[sharesParams]{Use: "shares", Short: "Usage sharing: model gateways peers let this node use (--given: what it shares)", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *sharesParams, _ *cobra.Command, _ []string) {
		os.Exit(runShares(p, os.Stdout, os.Stderr))
	}}.ToCobra()
}

func runShares(p *sharesParams, stdout, stderr io.Writer) int {
	if rc := agent.RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	path := "/v1/models/shares/received"
	if p.Given {
		path = "/v1/models/shares"
	}
	var resp struct {
		Shares  []modelShare      `json:"shares"`
		Errors  map[string]string `json:"errors,omitempty"`
		Offline bool              `json:"offline,omitempty"`
	}
	if err := agent.DaemonGet(path, &resp); err != nil {
		return fail(stderr, err)
	}
	if p.JSON {
		return printJSON(stdout, resp)
	}
	if resp.Offline {
		fmt.Fprintln(stderr, "federation is offline: peers cannot be asked")
	}
	if len(resp.Shares) == 0 {
		if p.Given {
			fmt.Fprintln(stdout, "This node shares no model gateway with any peer.")
		} else {
			fmt.Fprintln(stdout, "No peer shares a model gateway with this node.")
		}
	} else {
		who := "FROM"
		if p.Given {
			who = "TO"
		}
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintf(tw, "%s\tGATEWAY\tDIALECT\tSPENT\tCAP\tENDS\tMODELS\tUSE WITH\n", who)
		for _, s := range resp.Shares {
			capText, ends := "none", "never"
			if s.MaxUSD > 0 {
				capText = fmt.Sprintf("$%.2f", s.MaxUSD)
				if s.UsedUp {
					capText += " (used up)"
				}
			} else if s.Unrestricted {
				capText = "none (unrestricted)"
			}
			if s.ExpiresAt != nil {
				ends = s.ExpiresAt.Local().Format("2006-01-02 15:04")
			}
			use := "--model-proxy " + s.Gateway + "@" + shareNodeName(s)
			if p.Given {
				use = "-"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t$%.2f\t%s\t%s\t%s\t%s\n", shareNodeName(s), s.Gateway, s.Dialect, s.SpentUSD, capText, ends, strings.Join(s.Models, ","), use)
		}
		_ = tw.Flush()
	}
	peers := make([]string, 0, len(resp.Errors))
	for peer := range resp.Errors {
		peers = append(peers, peer)
	}
	sort.Strings(peers)
	for _, peer := range peers {
		fmt.Fprintf(stderr, "could not ask %s: %s\n", peer, resp.Errors[peer])
	}
	return 0
}

func shareNodeName(s modelShare) string {
	if s.PeerName != "" {
		return s.PeerName
	}
	return s.Peer
}
