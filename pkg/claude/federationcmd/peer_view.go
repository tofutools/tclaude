package federationcmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

type viewParams struct {
	Node string `long:"node" help:"Pinned instance ID or local peer label"`
}

func peerViewCmd() *cobra.Command {
	return boa.CmdT[viewParams]{Use: "view <endpoint>", Short: "Read an authorized peer dashboard endpoint as JSON (operator only)", Args: cobra.ExactArgs(1), ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *viewParams, _ *cobra.Command, args []string) {
		os.Exit(runPeerView(p, args[0], os.Stdout, os.Stderr))
	}}.ToCobra()
}
func runPeerView(p *viewParams, endpoint string, stdout, stderr io.Writer) int {
	if rc := agent.RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	var raw json.RawMessage
	if err := agent.ReadPeerView(p.Node, endpoint, &raw); err != nil {
		return failPeerView(stderr, err)
	}
	return printJSON(stdout, raw)
}
func failPeerView(w io.Writer, err error) int {
	var de *agent.DaemonError
	if errors.As(err, &de) && json.Valid(de.Raw) {
		fmt.Fprintln(w, string(de.Raw))
		return agent.MapDaemonErrorToRC(err)
	}
	return fail(w, err)
}

type statusParams struct {
	JSON    bool `long:"json" help:"Output JSON"`
	Summary bool `long:"summary" help:"Only identity, connection and trusted peers; avoids catalogs and outbox"`
}

func runStatusOptions(p *statusParams, stdout, stderr io.Writer) int {
	if !p.Summary {
		return runStatus(&jsonParam{JSON: p.JSON}, stdout, stderr)
	}
	if rc := agent.RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	var raw json.RawMessage
	if err := agent.DaemonGet("/v1/federation/status?summary=1", &raw); err != nil {
		return fail(stderr, err)
	}
	if p.JSON {
		return printJSON(stdout, raw)
	}
	var st status
	if err := json.Unmarshal(raw, &st); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "Instance: %s (%s)\n", st.InstanceID, proto.StripControls(st.Name))
	for _, p := range st.Peers {
		fmt.Fprintf(stdout, "%s\t%s\t%s\tonline=%t\tlast_seen=%s\n", p.InstanceID, proto.StripControls(p.Label), p.Level, p.Online, p.LastSeen)
	}
	return 0
}

type nodeSummaryRow struct {
	InstanceID string          `json:"instance_id"`
	Label      string          `json:"label"`
	Level      string          `json:"level"`
	Online     bool            `json:"online"`
	LastSeen   time.Time       `json:"last_seen,omitempty"`
	Summary    json.RawMessage `json:"summary,omitempty"`
	Error      json.RawMessage `json:"error,omitempty"`
}

func runNodeSummaries(p *nodesParams, stdout, stderr io.Writer) int {
	if p.Watch || p.Match != "" {
		return fail(stderr, fmt.Errorf("live summaries cannot be combined with --watch or --match"))
	}
	if rc := agent.RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	var st status
	if err := agent.DaemonGet("/v1/federation/status?summary=1", &st); err != nil {
		return fail(stderr, err)
	}
	rows := []nodeSummaryRow{{InstanceID: st.InstanceID, Label: st.Name, Level: "self", Online: true}}
	for _, pe := range st.Peers {
		if pe.Trusted {
			rows = append(rows, nodeSummaryRow{InstanceID: pe.InstanceID, Label: pe.Label, Level: pe.Level, Online: pe.Online, LastSeen: pe.LastSeen})
		}
	}
	if p.Node != "" {
		selected := []nodeSummaryRow{}
		for _, row := range rows {
			if row.InstanceID == p.Node || row.Level == "self" && p.Node == "self" || row.Level != "self" && (row.Label == p.Node || len(p.Node) >= 8 && (strings.HasPrefix(row.InstanceID, p.Node) || strings.HasPrefix(row.InstanceID, proto.InstanceIDPrefix+p.Node))) {
				selected = append(selected, row)
			}
		}
		if len(selected) != 1 {
			return fail(stderr, fmt.Errorf("--node must match exactly one pinned identity or local label (matched %d)", len(selected)))
		}
		rows = selected
	}
	// Fan out explicitly in the CLI, never in the daemon. Each worker has one
	// bounded, non-retrying request; output order remains directory order.
	var wg sync.WaitGroup
	work := make(chan int)
	for worker := 0; worker < min(4, len(rows)); worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				row := &rows[i]
				var err error
				if row.Level == "self" {
					err = agent.DaemonRequest(http.MethodGet, "/v1/federation/node-summary", nil, &row.Summary, agent.DaemonOpts{NoRetry: true})
				} else {
					err = agent.ReadPeerView(row.InstanceID, "node-summary", &row.Summary)
				}
				if err == nil {
					row.Online = true
				}
				if err != nil {
					row.Summary = nil
					var de *agent.DaemonError
					if errors.As(err, &de) && json.Valid(de.Raw) {
						row.Error = append(json.RawMessage(nil), de.Raw...)
					} else {
						row.Error, _ = json.Marshal(map[string]string{"error": err.Error(), "code": "request_failed"})
					}
				}
			}
		}()
	}
	for i := range rows {
		work <- i
	}
	close(work)
	wg.Wait()
	failed := false
	for _, row := range rows {
		failed = failed || len(row.Error) > 0
	}
	if p.JSON {
		if rc := printJSON(stdout, rows); rc != 0 {
			return rc
		}
	} else {
		tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "NODE\tID\tTRUST\tSHARED GROUPS\tSHARED AGENTS\tREPORTED ONLINE\tWAITING\tHEALTH / ERROR")
		for _, row := range rows {
			var data map[string]any
			_ = json.Unmarshal(row.Summary, &data)
			cell := func(key string) any {
				if v, ok := data[key]; ok {
					return v
				}
				return "-"
			}
			health := cell("health")
			if len(row.Error) > 0 {
				var e struct {
					Code   string `json:"code"`
					Reason string `json:"reason"`
				}
				_ = json.Unmarshal(row.Error, &e)
				health = e.Code
				if e.Reason != "" {
					health = fmt.Sprint(health, " / ", e.Reason)
				}
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", summaryCell(row.Label), summaryCell(row.InstanceID), summaryCell(row.Level), summaryCell(cell("shared_groups")), summaryCell(cell("shared_agents")), summaryCell(cell("online_agents")), summaryCell(cell("waiting_for_input")), summaryCell(health))
		}
		_ = tw.Flush()
	}
	if failed {
		return 1
	}
	return 0
}

func summaryCell(v any) string {
	return strings.Join(strings.Fields(proto.StripControls(fmt.Sprint(v))), " ")
}
