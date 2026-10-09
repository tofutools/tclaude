package federationcmd

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

type sessionsParams struct {
	Peer   string `pos:"true" optional:"true" help:"Trusted peer label or instance id (default all peers)"`
	JSON   bool   `long:"json" help:"Output JSON"`
	Notify bool   `long:"notify" help:"Keep running and print/bell when a visible session starts waiting (Ctrl-C to stop)"`
}

type remoteSession struct {
	proto.CatalogSession
	Address    string    `json:"address"`
	Peer       string    `json:"peer"`
	Instance   string    `json:"instance"`
	Groups     []string  `json:"groups"`
	ObservedAt time.Time `json:"observed_at"`
	Stale      bool      `json:"stale"`
}

func sessionsCmd() *cobra.Command {
	return boa.CmdT[sessionsParams]{Use: "sessions", Short: "List shared remote agent sessions and waiting states", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *sessionsParams, cmd *cobra.Command, _ []string) {
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer stop()
		os.Exit(runSessions(ctx, p, os.Stdout, os.Stderr))
	}}.ToCobra()
}

func runSessions(ctx context.Context, p *sessionsParams, stdout, stderr io.Writer) int {
	if p.JSON && p.Notify {
		fmt.Fprintln(stderr, "--json and --notify cannot be combined")
		return 1
	}
	if rc := agent.RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	previous := map[string]string{}
	initial := true
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		var rows []remoteSession
		if err := agent.DaemonGet("/v1/federation/sessions?peer="+url.QueryEscape(p.Peer), &rows); err != nil {
			return fail(stderr, err)
		}
		if p.JSON {
			return printJSON(stdout, rows)
		}
		if initial {
			printSessions(stdout, rows)
		}
		previous = notifySessionTransitions(stdout, rows, previous, initial)
		if !p.Notify {
			return 0
		}
		initial = false
		select {
		case <-ctx.Done():
			return 0
		case <-ticker.C:
		}
	}
}

// Remember stale states, but never alert from them. Reconnecting with the
// same waiting snapshot therefore does not announce a fictitious transition.
func notifySessionTransitions(w io.Writer, rows []remoteSession, previous map[string]string, initial bool) map[string]string {
	current := map[string]string{}
	for _, s := range rows {
		key := s.Instance + "/" + s.Agent
		state := s.Session + "/" + s.WaitingReason
		if s.WaitingObservedSince != nil {
			state += "/" + s.WaitingObservedSince.UTC().Format(time.RFC3339Nano)
		}
		current[key] = state
		old, known := previous[key]
		if !initial && !s.Stale && s.WaitingReason != "" && (!known || old != state) {
			fmt.Fprintf(w, "\a%s (%s) started waiting: %s\n", s.Address, s.Name, s.WaitingReason)
		}
	}
	return current
}

func printSessions(w io.Writer, rows []remoteSession) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION / ATTACH TARGET\tNAME\tGROUPS\tHARNESS\tSTATE\tWAITING\tOBSERVED WAIT\tFRESHNESS")
	for _, s := range rows {
		wait := "-"
		if s.WaitingReason != "" && s.WaitingObservedSince != nil {
			end := time.Now()
			if s.Stale {
				end = s.ObservedAt
			}
			duration := end.Sub(*s.WaitingObservedSince).Round(time.Second)
			if duration < 0 {
				duration = 0
			}
			wait = "waiting ≥" + duration.String()
		}
		freshness := "current"
		if s.Stale {
			freshness = "stale"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.Address, s.Name, strings.Join(s.Groups, ","), dash(s.Harness), dash(s.State), dash(s.WaitingReason), wait, freshness)
	}
	_ = tw.Flush()
}
