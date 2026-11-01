package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/tofutools/tclaude/pkg/claude/common/snapshotlib"
	"github.com/tofutools/tclaude/pkg/common"
)

func snapshotCmd() *cobra.Command {
	return boa.CmdT[struct{}]{Use: "snapshot", Short: "Capture agents as named snapshots in the local library",
		Long: "A snapshot is a named, point-in-time PAF of an agent (definition, environment and conversation) kept in\n" +
			"this node's library. It is a fork point: spawning from it creates new agents and leaves the original alone.\n" +
			"See docs/paf.md.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			snapshotCreateCmd(), snapshotListCmd(), snapshotShowCmd(), snapshotRemoveCmd(), snapshotExportCmd(), snapshotPruneCmd(), snapshotPublishCmd(),
			snapshotShareCmd(), snapshotBoardCmd(), snapshotPolicyCmd(), snapshotHeldCmd(), snapshotRecallsCmd(),
		}}.ToCobra()
}

type snapshotCreateParams struct {
	Agent       string `pos:"true" help:"Agent selector to snapshot."`
	Name        string `long:"name" optional:"true" help:"Snapshot name: 1-64 of a-z, 0-9, '.', '_', '-'. Defaults to <agent>-<timestamp>."`
	Note        string `long:"note" optional:"true" help:"A short note stored with the snapshot."`
	Expires     string `long:"expires" optional:"true" help:"Expiry: a duration (72h, 30d), an RFC 3339 time, or never. Defaults to snapshots.default_retention."`
	NoHistory   bool   `long:"no-history" help:"Capture the definition only, without the conversation."`
	WithSummary bool   `long:"with-summary" help:"First ask the agent to write a summary of its earlier work and keep it with the snapshot, so a landing on a smaller context window keeps it. Costs the agent a turn; waits up to --summary-wait."`
	SummaryWait string `long:"summary-wait" optional:"true" help:"With --with-summary: how long to wait for the agent's summary (default 10m)."`
	JSON        bool   `long:"json" help:"Print the snapshot as JSON."`
	CarriedWorkflowParams
}

func snapshotCreateCmd() *cobra.Command {
	return boa.CmdT[snapshotCreateParams]{Use: "create", Short: "Capture a named snapshot of an agent",
		Long: "Capture the agent's definition, environment and conversation (native history plus a harness-neutral\n" +
			"context) as a PAF in the local library. An idle agent is captured whole; a busy one up to its last\n" +
			"complete turn. Credential findings are recorded, since a snapshot stays on this node. Needs\n" +
			"agent.bundle.export on the agent.",
		ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *snapshotCreateParams, _ *cobra.Command, _ []string) {
			os.Exit(runSnapshotCreate(p, os.Stdout, os.Stderr))
		}}.ToCobra()
}

func runSnapshotCreate(p *snapshotCreateParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var meta snapshotlib.Meta
	body := map[string]any{"agent": p.Agent, "name": p.Name, "note": p.Note, "expires": p.Expires, "no_history": p.NoHistory, "workflow": p.Workflow, "own_workflows": p.Workflows}
	if p.WithSummary {
		id, rc := awaitAgentSummary(p.Agent, p.SummaryWait, stderr)
		if rc != rcOK {
			return rc
		}
		body["summary"] = id
	}
	if err := DaemonPost("/v1/snapshots", body, &meta); err != nil {
		fmt.Fprintln(stderr, err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		return printJSON(stdout, meta)
	}
	fmt.Fprintf(stdout, "Snapshot %s of %s (%s)\n", meta.Name, meta.AgentName, describeSnapshot(meta))
	for _, f := range meta.Findings {
		fmt.Fprintf(stdout, "Flag: %s: %d occurrence(s); redact before sharing\n", f.Kind, f.Count)
	}
	return rcOK
}

func describeSnapshot(m snapshotlib.Meta) string {
	parts := []string{m.Harness}
	if m.Model != "" {
		parts = append(parts, m.Model)
	}
	if m.History {
		parts = append(parts, "with history, "+m.State)
	} else {
		parts = append(parts, "definition only")
	}
	parts = append(parts, humanSize(m.Bytes))
	if m.ExpiresAt != nil {
		parts = append(parts, "expires "+m.ExpiresAt.Local().Format(time.RFC3339))
	}
	return strings.Join(parts, ", ")
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func printJSON(out io.Writer, v any) int {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return rcIOFailure
	}
	return rcOK
}

type snapshotListParams struct {
	JSON bool `long:"json" help:"Print the library as JSON."`
}

func snapshotListCmd() *cobra.Command {
	return boa.CmdT[snapshotListParams]{Use: "ls", Short: "List the snapshot library", ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *snapshotListParams, _ *cobra.Command, _ []string) { os.Exit(runSnapshotList(p, os.Stdout, os.Stderr)) }}.ToCobra()
}

func runSnapshotList(p *snapshotListParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp struct {
		Snapshots []snapshotlib.Meta `json:"snapshots"`
	}
	if err := DaemonGet("/v1/snapshots", &resp); err != nil {
		fmt.Fprintln(stderr, err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		return printJSON(stdout, resp.Snapshots)
	}
	if len(resp.Snapshots) == 0 {
		fmt.Fprintln(stdout, "No snapshots.")
		return rcOK
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tAGENT\tHARNESS\tCREATED\tSIZE\tEXPIRES\tFLAGS")
	for _, m := range resp.Snapshots {
		expires := "never"
		if m.ExpiresAt != nil {
			expires = m.ExpiresAt.Local().Format("2006-01-02 15:04")
		}
		flags := 0
		for _, f := range m.Findings {
			flags += f.Count
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\n", m.Name, termClean(m.AgentName), m.Harness, m.CreatedAt.Local().Format("2006-01-02 15:04"), humanSize(m.Bytes), expires, flags)
	}
	_ = tw.Flush()
	return rcOK
}

// termClean keeps stored text from controlling the terminal.
func termClean(s string) string { return consentSafe(s) }

type snapshotNameParams struct {
	Name string `pos:"true" help:"Snapshot name."`
	JSON bool   `long:"json" help:"Print as JSON."`
}

func snapshotShowCmd() *cobra.Command {
	return boa.CmdT[snapshotNameParams]{Use: "show", Short: "Show a snapshot", ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *snapshotNameParams, _ *cobra.Command, _ []string) { os.Exit(runSnapshotShow(p, os.Stdout, os.Stderr)) }}.ToCobra()
}

func runSnapshotShow(p *snapshotNameParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var m snapshotlib.Meta
	if err := DaemonGet("/v1/snapshots/"+url.PathEscape(p.Name), &m); err != nil {
		fmt.Fprintln(stderr, err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		return printJSON(stdout, m)
	}
	fmt.Fprintf(stdout, "%s: %s\n", m.Name, describeSnapshot(m))
	fmt.Fprintf(stdout, "  Agent:    %s %s\n", termClean(m.AgentName), m.AgentID)
	fmt.Fprintf(stdout, "  Created:  %s\n", m.CreatedAt.Local().Format(time.RFC3339))
	if m.Note != "" {
		fmt.Fprintf(stdout, "  Note:     %s\n", termClean(m.Note))
	}
	for _, f := range m.Findings {
		fmt.Fprintf(stdout, "  Flag:     %s: %d\n", f.Kind, f.Count)
	}
	return rcOK
}

func snapshotRemoveCmd() *cobra.Command {
	return boa.CmdT[snapshotNameParams]{Use: "rm", Short: "Remove a snapshot (human only)", ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *snapshotNameParams, _ *cobra.Command, _ []string) {
			if rc := RequireDaemonOrExit(os.Stderr); rc != rcOK {
				os.Exit(rc)
			}
			if err := DaemonDelete("/v1/snapshots/"+url.PathEscape(p.Name), nil); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(MapDaemonErrorToRC(err))
			}
			fmt.Println("Removed", p.Name)
		}}.ToCobra()
}

type snapshotExportParams struct {
	Name string `pos:"true" help:"Snapshot name."`
	File string `long:"file" short:"f" help:"Destination .paf file."`
}

func snapshotExportCmd() *cobra.Command {
	return boa.CmdT[snapshotExportParams]{Use: "export", Short: "Write a snapshot's PAF to a file",
		Long:        "Write the snapshot's PAF to a file. It holds the agent's conversation: redact and consent before sharing it (tclaude paf redact, tclaude paf consent).",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *snapshotExportParams, _ *cobra.Command, _ []string) {
			if rc := RequireDaemonOrExit(os.Stderr); rc != rcOK {
				os.Exit(rc)
			}
			if err := downloadSnapshot(p.Name, p.File); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(MapDaemonErrorToRC(err))
			}
			fmt.Println("Exported", p.File)
		}}.ToCobra()
}

func snapshotPruneCmd() *cobra.Command {
	return boa.CmdT[struct{}]{Use: "prune", Short: "Remove expired snapshots (human only)", ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(_ *struct{}, _ *cobra.Command, _ []string) {
			if rc := RequireDaemonOrExit(os.Stderr); rc != rcOK {
				os.Exit(rc)
			}
			var resp struct {
				Removed []string `json:"removed"`
			}
			if err := DaemonPost("/v1/snapshots/prune", map[string]any{}, &resp); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(MapDaemonErrorToRC(err))
			}
			fmt.Printf("Removed %d expired snapshot(s)\n", len(resp.Removed))
			for _, n := range resp.Removed {
				fmt.Println(" ", n)
			}
		}}.ToCobra()
}

// downloadSnapshot streams a snapshot's PAF to a temporary file beside
// dest and renames it into place.
func downloadSnapshot(name, dest string) error {
	f, err := os.CreateTemp(filepath.Dir(dest), ".paf-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	err = downloadAgentArchive("/v1/snapshots/"+url.PathEscape(name)+"/export", f)
	if err == nil {
		err = f.Chmod(0o600)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

// snapshotSpawnFlags are the spawn flags --from-snapshot applies. Every
// other launch flag would be silently ignored, so it is refused instead:
// the snapshot carries the launch shape.
var snapshotSpawnFlags = map[string]bool{"from-snapshot": true, "count": true, "name": true, "cwd": true, "harness": true, "model": true, "confirm-history-loss": true}

func runSpawnFromSnapshot(p *SpawnParams, cmd *cobra.Command, stdout, stderr io.Writer) int {
	if p.FromSnapshot == "" {
		fmt.Fprintln(stderr, "Error: --count needs --from-snapshot")
		return rcInvalidArg
	}
	var refused []string
	inherited := cmd.InheritedFlags()
	cmd.Flags().Visit(func(f *pflag.Flag) {
		if !snapshotSpawnFlags[f.Name] && inherited.Lookup(f.Name) == nil {
			refused = append(refused, "--"+f.Name)
		}
	})
	if len(refused) > 0 {
		fmt.Fprintf(stderr, "Error: --from-snapshot takes its launch shape from the snapshot; %s cannot be combined with it\n", strings.Join(refused, ", "))
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	group := p.Group
	if group == "" {
		var rc int
		if group, rc = implicitSpawnGroup(stderr); rc != rcOK {
			return rc
		}
	}
	body := map[string]any{"group": group, "name": p.Name, "count": p.Count, "harness": p.Harness, "model": p.Model, "confirm_history_loss": p.ConfirmHistoryLoss}
	if p.Cwd != "" {
		cwd, err := filepath.Abs(p.Cwd)
		if err != nil {
			fmt.Fprintln(stderr, "Error:", err)
			return rcInvalidArg
		}
		body["cwd"] = cwd
	} else {
		// Recorded paths are reused when the operator names none, as
		// `paf import --keep-paths` does.
		body["keep_paths"] = true
	}
	var res snapshotSpawnResult
	if err := DaemonPost("/v1/snapshots/"+url.PathEscape(p.FromSnapshot)+"/spawn", body, &res); err != nil {
		// A partial failure still created agents; list them so a retry
		// does not duplicate them unknowingly.
		var de *DaemonError
		if errors.As(err, &de) && de.Code == "partial" {
			var partial struct {
				Result snapshotSpawnResult `json:"result"`
			}
			if json.Unmarshal(de.Raw, &partial) == nil {
				printSnapshotSpawned(stdout, stderr, partial.Result)
			}
		}
		printBundleError(stderr, err)
		return MapDaemonErrorToRC(err)
	}
	printSnapshotSpawned(stdout, stderr, res)
	return rcOK
}

type snapshotSpawnResult struct {
	Snapshot string          `json:"snapshot"`
	Spawned  []SpawnResponse `json:"spawned"`
	Warnings []string        `json:"warnings"`
}

func printSnapshotSpawned(stdout, stderr io.Writer, res snapshotSpawnResult) {
	for _, w := range res.Warnings {
		fmt.Fprintln(stderr, "Warning:", w)
	}
	for _, s := range res.Spawned {
		fmt.Fprintf(stdout, "Spawned %s (%s) from snapshot %s in group %s; attach: %s\n", s.Label, s.AgentID, res.Snapshot, s.Group, s.AttachCmd)
	}
}
