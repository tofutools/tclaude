package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/table"
	"github.com/tofutools/tclaude/pkg/common"
)

// SpawnQueueEntry mirrors agentd's GET /v1/spawn-queue rows. It never carries
// the queued request itself.
type SpawnQueueEntry struct {
	ID            int64  `json:"id"`
	Group         string `json:"group"`
	Name          string `json:"name,omitempty"`
	RequesterKind string `json:"requester_kind"`
	RequestedBy   string `json:"requested_by"`
	RequesterID   string `json:"requester_agent_id,omitempty"`
	State         string `json:"state"`
	Position      int    `json:"position,omitempty"`
	Reason        string `json:"reason,omitempty"`
	CreatedAt     string `json:"created_at"`
	AgeSeconds    int64  `json:"age_seconds"`
}

// SpawnQueueCapacity is the instance's capacity policy and current use.
type SpawnQueueCapacity struct {
	MaxLiveAgents int    `json:"max_live_agents"`
	UsedAgents    *int   `json:"used_agents,omitempty"`
	MinFreeRAMMB  int64  `json:"min_free_ram_mb,omitempty"`
	MinFreeDiskMB int64  `json:"min_free_disk_mb,omitempty"`
	WhenFull      string `json:"when_full"`
	MaxQueued     int    `json:"max_queued"`
	Queued        int    `json:"queued"`
}

// SpawnQueueList is GET /v1/spawn-queue.
type SpawnQueueList struct {
	Capacity SpawnQueueCapacity `json:"capacity"`
	Entries  []SpawnQueueEntry  `json:"entries"`
}

func queueCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "queue",
		Short: "Inspect or cancel spawns waiting for capacity",
		Long: "When capacity.when_full is \"queue\", a spawn that would go past this instance's " +
			"capacity (capacity.max_live_agents, min_free_ram_mb, min_free_disk_mb) waits in a " +
			"first-in-first-out queue and launches when capacity frees. There is no ETA.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds:     []*cobra.Command{queueLsCmd(), queueRmCmd()},
	}.ToCobra()
}

type queueLsParams struct {
	All  bool `long:"all" help:"Also list ended entries (launched, failed, cancelled) still kept"`
	JSON bool `long:"json" help:"Output JSON"`
}

func queueLsCmd() *cobra.Command {
	return boa.CmdT[queueLsParams]{
		Use:         "ls",
		Short:       "List queued spawns you can see, and the capacity they wait for",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *queueLsParams, _ *cobra.Command, _ []string) {
			os.Exit(runQueueLs(p, cliStdout, cliStderr))
		},
	}.ToCobra()
}

func runQueueLs(p *queueLsParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	path := "/v1/spawn-queue"
	if p.All {
		path += "?all=1"
	}
	var list SpawnQueueList
	if err := DaemonGet(path, &list); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(list); err != nil {
			return rcIOFailure
		}
		return rcOK
	}
	fmt.Fprintln(stdout, capacitySummary(list.Capacity))
	if len(list.Entries) == 0 {
		fmt.Fprintln(stdout, "(no queued spawns)")
		return rcOK
	}
	fmt.Fprintln(stdout, renderSpawnQueue(list.Entries, true))
	return rcOK
}

func capacitySummary(c SpawnQueueCapacity) string {
	var parts []string
	switch {
	case c.MaxLiveAgents > 0 && c.UsedAgents != nil:
		parts = append(parts, fmt.Sprintf("agents %d/%d", *c.UsedAgents, c.MaxLiveAgents))
	case c.MaxLiveAgents > 0:
		parts = append(parts, fmt.Sprintf("max agents %d", c.MaxLiveAgents))
	default:
		parts = append(parts, "agents unlimited")
	}
	if c.MinFreeRAMMB > 0 {
		parts = append(parts, fmt.Sprintf("min free RAM %d MB", c.MinFreeRAMMB))
	}
	if c.MinFreeDiskMB > 0 {
		parts = append(parts, fmt.Sprintf("min free disk %d MB", c.MinFreeDiskMB))
	}
	parts = append(parts, "when full: "+c.WhenFull)
	if c.WhenFull == "queue" {
		parts = append(parts, fmt.Sprintf("queued %d/%d", c.Queued, c.MaxQueued))
	}
	return "Capacity: " + strings.Join(parts, ", ")
}

func renderSpawnQueue(entries []SpawnQueueEntry, withState bool) string {
	cols := []table.Column{
		{Header: "#", Width: 3},
		{Header: "QUEUE ID", Width: 8},
		{Header: "NAME", MinWidth: 8, Weight: 0.8, Truncate: true},
		{Header: "GROUP", MinWidth: 8, Weight: 0.6, Truncate: true},
		{Header: "REQUESTED BY", MinWidth: 10, Weight: 0.8, Truncate: true},
		{Header: "AGE", Width: 7},
	}
	if withState {
		cols = append(cols, table.Column{Header: "STATE", MinWidth: 8, Weight: 1.2, Truncate: true})
	}
	tbl := table.New(cols...)
	tbl.SetTerminalWidth(table.GetTerminalWidth())
	for _, e := range entries {
		pos := "-"
		if e.Position > 0 {
			pos = strconv.Itoa(e.Position)
		}
		cells := []string{pos, strconv.FormatInt(e.ID, 10), e.Name, e.Group, e.RequestedBy, relTimeAgo(e.CreatedAt)}
		if withState {
			state := e.State
			if e.Reason != "" && e.State != "queued" {
				state += ": " + e.Reason
			}
			cells = append(cells, state)
		}
		tbl.AddRow(table.Row{Cells: cells})
	}
	return tbl.Render()
}

// renderQueuedSpawnsSection adds the waiting spawns under `agent ls`. A daemon
// without the queue route, or an empty queue, prints nothing.
func renderQueuedSpawnsSection(stdout io.Writer) {
	var list SpawnQueueList
	if err := DaemonGet("/v1/spawn-queue", &list); err != nil || len(list.Entries) == 0 {
		return
	}
	fmt.Fprintf(stdout, "\nQueued spawns (waiting for capacity, no ETA; `tclaude agent queue ls`):\n")
	fmt.Fprintln(stdout, renderSpawnQueue(list.Entries, false))
}

type queueRmParams struct {
	ID int64 `pos:"true" help:"Queue ID of the waiting spawn (see 'tclaude agent queue ls')"`
}

func queueRmCmd() *cobra.Command {
	return boa.CmdT[queueRmParams]{
		Use:         "rm",
		Short:       "Cancel a queued spawn (its requester, an owner of its group, or the operator)",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *queueRmParams, _ *cobra.Command, _ []string) {
			os.Exit(runQueueRm(p, cliStdout, cliStderr))
		},
	}.ToCobra()
}

func runQueueRm(p *queueRmParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	path := "/v1/spawn-queue/" + strconv.FormatInt(p.ID, 10)
	if err := DaemonRequest(http.MethodDelete, path, nil, nil, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Cancelled queued spawn #%d\n", p.ID)
	return rcOK
}
