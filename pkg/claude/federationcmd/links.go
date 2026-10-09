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
)

type linksParams struct {
	Group string `long:"group" help:"Exact local group name; omit to list all groups"`
	JSON  bool   `long:"json" help:"Output JSON"`
}
type federationGroupLinks struct {
	GroupID int64  `json:"group_id"`
	Name    string `json:"name"`
	Links   []struct {
		Peer      string     `json:"peer"`
		Label     string     `json:"label"`
		Level     string     `json:"level"`
		Kind      string     `json:"kind"`
		Direction string     `json:"direction"`
		Slugs     []string   `json:"slugs,omitempty"`
		Pool      string     `json:"pool,omitempty"`
		Remote    string     `json:"remote,omitempty"`
		Online    bool       `json:"online"`
		LastSeen  *time.Time `json:"last_seen,omitempty"`
	} `json:"federation_links"`
}

func linksCmd() *cobra.Command {
	return boa.CmdT[linksParams]{Use: "links", Short: "Read per-group federation links (local operator only)", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *linksParams, _ *cobra.Command, _ []string) { os.Exit(runLinks(p, os.Stdout, os.Stderr)) }}.ToCobra()
}
func runLinks(p *linksParams, stdout, stderr io.Writer) int {
	if rc := agent.RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	var result struct {
		Groups []federationGroupLinks `json:"groups"`
	}
	path := "/v1/federation/links"
	if p.Group != "" {
		path += "?group=" + url.QueryEscape(p.Group)
	}
	if err := agent.DaemonGet(path, &result); err != nil {
		return fail(stderr, err)
	}
	if p.JSON {
		return printJSON(stdout, result)
	}
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "GROUP\tID\tPEER\tKIND\tDIRECTION\tPOOL / REMOTE\tSLUGS\tTRUST\tSTATE\tLAST SEEN")
	for _, g := range result.Groups {
		if len(g.Links) == 0 {
			fmt.Fprintf(tw, "%s\t%d\t-\t-\t-\t-\t-\t-\tno links\t-\n", summaryCell(g.Name), g.GroupID)
			continue
		}
		for _, link := range g.Links {
			peer := link.Label
			if peer == "" {
				peer = link.Peer
			}
			state := "offline"
			if link.Online {
				state = "online"
			}
			detail := link.Pool
			if link.Remote != "" {
				detail = link.Remote
			}
			lastSeen := "-"
			if link.LastSeen != nil {
				lastSeen = link.LastSeen.Format(time.RFC3339)
			}
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", summaryCell(g.Name), g.GroupID, summaryCell(peer), summaryCell(link.Kind), summaryCell(link.Direction), summaryCell(detail), summaryCell(strings.Join(link.Slugs, ",")), summaryCell(link.Level), state, lastSeen)
		}
	}
	if err := tw.Flush(); err != nil {
		return fail(stderr, err)
	}
	return 0
}
