package federationcmd

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

type auditParams struct {
	Peer  string `long:"peer" optional:"true" help:"Filter by peer label or immutable instance ID"`
	Since string `long:"since" optional:"true" help:"Show activity since an RFC3339 timestamp or duration ago (e.g. 24h)"`
	Limit int    `long:"limit" default:"200" help:"Maximum rows, 1..1000"`
	JSON  bool   `long:"json" help:"Output metadata as JSON"`
}

func auditCmd() *cobra.Command {
	return boa.CmdT[auditParams]{Use: "audit", Short: "Show local metadata for incoming and outgoing remote activity", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *auditParams, _ *cobra.Command, _ []string) { os.Exit(runAudit(p, os.Stdout, os.Stderr)) }}.ToCobra()
}
func runAudit(p *auditParams, stdout, stderr io.Writer) int {
	limit := p.Limit
	if limit < 1 || limit > 1000 {
		return fail(stderr, fmt.Errorf("--limit must be 1..1000"))
	}
	query := url.Values{"limit": {strconv.Itoa(limit)}}
	if p.Peer != "" {
		query.Set("peer", p.Peer)
	}
	if p.Since != "" {
		since, err := time.Parse(time.RFC3339Nano, p.Since)
		if err != nil {
			duration, e := time.ParseDuration(p.Since)
			if e != nil || duration <= 0 {
				return fail(stderr, fmt.Errorf("--since must be an RFC3339 timestamp or positive duration, e.g. 24h"))
			}
			since = time.Now().Add(-duration)
		}
		query.Set("since", since.UTC().Format(time.RFC3339Nano))
	}
	if rc := agent.RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	var rows []db.FederationActivity
	if err := agent.DaemonGet("/v1/federation/audit?"+query.Encode(), &rows); err != nil {
		return fail(stderr, err)
	}
	if p.JSON {
		return printJSON(stdout, rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "no federation activity")
		return 0
	}
	cell := func(value string) string { return strings.Join(strings.Fields(proto.StripControls(value)), " ") }
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "AT\tDIRECTION\tPEER\tKIND\tSTATE\tACTOR\tTARGET\tGROUP\tSOURCE")
	for _, r := range rows {
		state := r.State
		if r.Status != 0 {
			state = strings.TrimSpace(state + " HTTP " + strconv.Itoa(r.Status))
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.At.UTC().Format(time.RFC3339), cell(r.Direction), cell(r.Peer), cell(r.Kind), cell(state), cell(r.Actor), cell(r.Target), cell(r.Group), cell(r.ID))
	}
	_ = tw.Flush()
	return 0
}
