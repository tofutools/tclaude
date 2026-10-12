package agent

import (
	"encoding/json"
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
			snapshotCreateCmd(), snapshotListCmd(), snapshotShowCmd(), snapshotRemoveCmd(), snapshotExportCmd(), snapshotPruneCmd(),
		}}.ToCobra()
}

type snapshotCreateParams struct {
	Agent     string `pos:"true" help:"Agent selector to snapshot."`
	Name      string `long:"name" optional:"true" help:"Snapshot name: 1-64 of a-z, 0-9, '.', '_', '-'. Defaults to <agent>-<timestamp>."`
	Note      string `long:"note" optional:"true" help:"A short note stored with the snapshot."`
	Expires   string `long:"expires" optional:"true" help:"Expiry: a duration (72h, 30d), an RFC 3339 time, or never. Defaults to snapshots.default_retention."`
	NoHistory bool   `long:"no-history" help:"Capture the definition only, without the conversation."`
	JSON      bool   `long:"json" help:"Print the snapshot as JSON."`
}

func snapshotCreateCmd() *cobra.Command {
	return boa.CmdT[snapshotCreateParams]{Use: "create <agent>", Short: "Capture a named snapshot of an agent",
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
	body := map[string]any{"agent": p.Agent, "name": p.Name, "note": p.Note, "expires": p.Expires, "no_history": p.NoHistory}
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
	return boa.CmdT[snapshotNameParams]{Use: "show <name>", Short: "Show a snapshot", ParamEnrich: common.DefaultParamEnricher(),
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
	return boa.CmdT[snapshotNameParams]{Use: "rm <name>", Short: "Remove a snapshot (human only)", ParamEnrich: common.DefaultParamEnricher(),
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
	return boa.CmdT[snapshotExportParams]{Use: "export <name>", Short: "Write a snapshot's PAF to a file",
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
