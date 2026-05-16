package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/table"
	"github.com/tofutools/tclaude/pkg/common"
)

// groups_export.go holds the `tclaude agent groups export | import |
// transfers` subcommands — the CLI face of per-group export / import.
// Each is a thin client over the daemon's /v1/groups endpoints; all the
// real work (table collection, conv-id remap, path rewriting, the import
// transaction) lives in the daemon.

// --- groups export ---

type groupsExportParams struct {
	Name string `pos:"true" help:"Group to export"`
	Out  string `long:"out" short:"o" optional:"true" help:"Write the .zip archive here. Default: group-<name>-<timestamp>.zip in the current directory. Pass '-' to stream the archive to stdout."`
}

func groupsExportCmd() *cobra.Command {
	return boa.CmdT[groupsExportParams]{
		Use:   "export",
		Short: "Export a whole group to a portable .zip archive",
		Long: "Export a group — the group row, its members and owners, " +
			"permissions, enrollment, cron jobs, message history and audit " +
			"trail, plus every member's conversation .jsonl — into a single " +
			"self-contained .zip. The archive is portable across machines, " +
			"users and OSes (Linux <-> macOS); import it elsewhere with " +
			"`tclaude agent groups import`. It contains full conversation " +
			"content, so treat it as sensitive. Gated server-side on the " +
			"groups.export permission (default human-only).",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *groupsExportParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Name).SetAlternativesFunc(completeGroupNames)
			return nil
		},
		RunFunc: func(p *groupsExportParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupsExport(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGroupsExport(p *groupsExportParams, stdout, stderr io.Writer) int {
	if p.Name == "" {
		fmt.Fprintf(stderr, "Error: group name is required\n")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	archive, headers, err := DaemonGetRaw("/v1/groups/" + url.PathEscape(p.Name) + "/export")
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}

	// Stream to stdout when asked — lets the archive be piped.
	if p.Out == "-" {
		if _, err := stdout.Write(archive); err != nil {
			fmt.Fprintf(stderr, "Error: writing archive to stdout: %v\n", err)
			return rcIOFailure
		}
		return rcOK
	}

	outPath := p.Out
	if outPath == "" {
		// Prefer the server-suggested filename; fall back to a local one.
		outPath = headers.Get("X-Tclaude-Export-Filename")
		if outPath == "" {
			outPath = fmt.Sprintf("group-%s-%s.zip", p.Name, time.Now().Format("20060102-150405"))
		}
	}
	if err := os.WriteFile(outPath, archive, 0o600); err != nil {
		fmt.Fprintf(stderr, "Error: writing %q: %v\n", outPath, err)
		return rcIOFailure
	}
	abs, _ := filepath.Abs(outPath)
	fmt.Fprintf(stdout, "Exported group %q to %s (%s)\n", p.Name, abs, humanBytes(len(archive)))
	return rcOK
}

// --- groups import ---

type groupsImportParams struct {
	File   string `pos:"true" help:"Path to the .zip export archive"`
	Into   string `long:"into" help:"Target working directory the imported agents are bound to. Required — on a different machine the source path will not exist, so you choose where the group lives now."`
	As     string `long:"as" optional:"true" help:"Import the group under this name instead of its exported name. Required when a group with the exported name already exists locally."`
	DryRun bool   `long:"dry-run" help:"Inspect the archive and report what would be imported — manifest summary plus group-name and conv-id collisions — WITHOUT importing anything. --into is not needed with --dry-run."`
	JSON   bool   `long:"json" help:"Output the import (or --dry-run inspection) as JSON"`
}

func groupsImportCmd() *cobra.Command {
	return boa.CmdT[groupsImportParams]{
		Use:   "import",
		Short: "Import a group from a .zip archive produced by `groups export`",
		Long: "Recreate a group from an export archive: the group, its agents, " +
			"permissions, ownership, enrollment, cron jobs, messages and every " +
			"conversation .jsonl. Imported agents land OFFLINE — no tmux " +
			"session is started; wake them with the usual resume tooling.\n\n" +
			"--into picks the local working directory the imported agents are " +
			"bound to (their conversation files and any embedded paths are " +
			"rewritten to it). A conv-id that already exists locally is given a " +
			"fresh id and an agent title suffixed '-i-N' so the copy is " +
			"distinguishable; non-colliding conv-ids are preserved. If the " +
			"group name is already taken, the import is refused — pass --as to " +
			"choose a different name.\n\n" +
			"--dry-run inspects the archive and prints what WOULD be imported — " +
			"the manifest summary plus the group-name and conv-id collisions — " +
			"without writing anything; the dashboard's import preview uses the " +
			"same check. Gated server-side on the groups.import permission " +
			"(default human-only).",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *groupsImportParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupsImport(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGroupsImport(p *groupsImportParams, stdout, stderr io.Writer) int {
	if p.File == "" {
		fmt.Fprintf(stderr, "Error: path to the export archive is required\n")
		return rcInvalidArg
	}
	// --into is the target directory the imported agents bind to — needed
	// for a real import, but irrelevant to --dry-run (which writes nothing).
	if !p.DryRun && strings.TrimSpace(p.Into) == "" {
		fmt.Fprintf(stderr, "Error: --into <dir> is required (the working directory for the imported agents)\n")
		return rcInvalidArg
	}
	archive, err := os.ReadFile(p.File)
	if err != nil {
		fmt.Fprintf(stderr, "Error: reading %q: %v\n", p.File, err)
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}

	if p.DryRun {
		return runGroupsImportDryRun(p, archive, stdout, stderr)
	}

	// Resolve --into to an absolute path CLI-side so the value the daemon
	// stores does not depend on the daemon's working directory.
	into, err := filepath.Abs(p.Into)
	if err != nil {
		fmt.Fprintf(stderr, "Error: resolving --into %q: %v\n", p.Into, err)
		return rcInvalidArg
	}

	path := "/v1/groups/import?into=" + url.QueryEscape(into)
	if p.As != "" {
		path += "&as=" + url.QueryEscape(p.As)
	}

	var resp struct {
		Group          string            `json:"group"`
		GroupID        int64             `json:"group_id"`
		TargetDir      string            `json:"target_dir"`
		AgentCount     int               `json:"agent_count"`
		MessageCount   int               `json:"message_count"`
		ConvRemaps     map[string]string `json:"conv_remaps"`
		Retitled       map[string]string `json:"retitled"`
		SkippedAliases []string          `json:"skipped_head_aliases"`
		FileWarnings   []string          `json:"file_warnings"`
	}
	if err := DaemonPostRaw(path, "application/zip", archive, &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}

	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(resp); err != nil {
			return rcIOFailure
		}
		return rcOK
	}

	fmt.Fprintf(stdout, "Imported group %q (id=%d) into %s\n", resp.Group, resp.GroupID, resp.TargetDir)
	fmt.Fprintf(stdout, "  %d agent(s), %d message(s)\n", resp.AgentCount, resp.MessageCount)
	if len(resp.ConvRemaps) > 0 {
		fmt.Fprintf(stdout, "  %d conv-id(s) collided locally and were remapped to fresh ids:\n", len(resp.ConvRemaps))
		for old, fresh := range resp.ConvRemaps {
			title := resp.Retitled[fresh]
			fmt.Fprintf(stdout, "    %s -> %s  (title %q)\n", short(old), short(fresh), title)
		}
	}
	for _, h := range resp.SkippedAliases {
		fmt.Fprintf(stdout, "  note: head alias %q already existed locally — left untouched\n", h)
	}
	for _, w := range resp.FileWarnings {
		fmt.Fprintf(stdout, "  warning: %s\n", w)
	}
	return rcOK
}

// runGroupsImportDryRun drives `groups import --dry-run`: it POSTs the
// archive to the inspect endpoint, which analyses it WITHOUT importing
// anything, and prints the manifest summary plus the group-name and
// conv-id collisions the human would hit. The dashboard's import preview
// calls the very same endpoint.
func runGroupsImportDryRun(p *groupsImportParams, archive []byte, stdout, stderr io.Writer) int {
	path := "/v1/groups/import/inspect"
	if p.As != "" {
		path += "?as=" + url.QueryEscape(p.As)
	}
	var insp struct {
		SourceGroup     string `json:"source_group"`
		FormatVersion   int    `json:"format_version"`
		SchemaVersion   int    `json:"schema_version"`
		SourceHome      string `json:"source_home"`
		SourceOS        string `json:"source_os"`
		ExportedAt      string `json:"exported_at"`
		AgentCount      int    `json:"agent_count"`
		MessageCount    int    `json:"message_count"`
		ConvCount       int    `json:"conv_count"`
		MissingConvs    int    `json:"missing_convs"`
		TargetName      string `json:"target_name"`
		TargetNameValid bool   `json:"target_name_valid"`
		TargetNameError string `json:"target_name_error"`
		GroupNameTaken  bool   `json:"group_name_taken"`
		ConvCollisions  []struct {
			ConvID string `json:"conv_id"`
			Title  string `json:"title"`
		} `json:"conv_collisions"`
	}
	if err := DaemonPostRaw(path, "application/zip", archive, &insp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}

	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(insp); err != nil {
			return rcIOFailure
		}
		return rcOK
	}

	fmt.Fprintf(stdout, "Dry run — nothing was imported.\n\n")
	fmt.Fprintf(stdout, "Archive contents:\n")
	fmt.Fprintf(stdout, "  source group:   %s\n", insp.SourceGroup)
	fmt.Fprintf(stdout, "  agents:         %d\n", insp.AgentCount)
	fmt.Fprintf(stdout, "  messages:       %d\n", insp.MessageCount)
	fmt.Fprintf(stdout, "  conversations:  %d", insp.ConvCount)
	if insp.MissingConvs > 0 {
		fmt.Fprintf(stdout, " (%d with no .jsonl content)", insp.MissingConvs)
	}
	fmt.Fprintln(stdout)
	if insp.SourceOS != "" || insp.SourceHome != "" {
		fmt.Fprintf(stdout, "  source machine: %s, home %s\n", insp.SourceOS, insp.SourceHome)
	}
	if insp.ExportedAt != "" {
		fmt.Fprintf(stdout, "  exported at:    %s\n", insp.ExportedAt)
	}
	fmt.Fprintf(stdout, "  format version: %d\n", insp.FormatVersion)

	fmt.Fprintf(stdout, "\nWould import as: %s\n", insp.TargetName)
	switch {
	case !insp.TargetNameValid:
		fmt.Fprintf(stdout, "  REFUSED: invalid group name — %s\n", insp.TargetNameError)
	case insp.GroupNameTaken:
		fmt.Fprintf(stdout, "  REFUSED: a group named %q already exists — pass --as to import under a free name\n", insp.TargetName)
	default:
		fmt.Fprintf(stdout, "  OK: the name is free\n")
	}

	if len(insp.ConvCollisions) > 0 {
		fmt.Fprintf(stdout, "\n%d conv-id(s) already exist locally — each would be imported as a fresh copy (-i-N):\n",
			len(insp.ConvCollisions))
		for _, c := range insp.ConvCollisions {
			fmt.Fprintf(stdout, "  %s  (%s)\n", short(c.ConvID), c.Title)
		}
	} else {
		fmt.Fprintf(stdout, "\nNo conv-id collisions — every conversation id would be preserved.\n")
	}
	return rcOK
}

// --- groups transfers ---

type groupsTransfersParams struct {
	Limit int  `long:"limit" short:"n" optional:"true" help:"Max entries to show (default 50; 0 = all)"`
	JSON  bool `long:"json" help:"Output JSON"`
}

func groupsTransfersCmd() *cobra.Command {
	return boa.CmdT[groupsTransfersParams]{
		Use:   "transfers",
		Short: "Show the group export / import audit log",
		Long: "List recorded group exports and imports — when, which group, " +
			"the source machine an import came from, the resulting group and " +
			"target directory, and how many conv-ids had to be remapped. " +
			"Read-only; open to any caller.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *groupsTransfersParams, _ *cobra.Command, _ []string) {
			os.Exit(runGroupsTransfers(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGroupsTransfers(p *groupsTransfersParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	limit := p.Limit
	if limit == 0 && !p.JSON {
		limit = 50
	}
	path := "/v1/groups/transfers"
	if limit > 0 {
		path += "?limit=" + strconv.Itoa(limit)
	}
	var entries []struct {
		ID            int64  `json:"id"`
		Kind          string `json:"kind"`
		At            string `json:"at"`
		FormatVersion int    `json:"format_version"`
		SourceGroup   string `json:"source_group"`
		SourceOS      string `json:"source_os"`
		ResultGroup   string `json:"result_group"`
		TargetDir     string `json:"target_dir"`
		ConvRemaps    string `json:"conv_remaps"`
		AgentCount    int    `json:"agent_count"`
		MessageCount  int    `json:"message_count"`
	}
	if err := DaemonGet(path, &entries); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(entries); err != nil {
			return rcIOFailure
		}
		return rcOK
	}
	if len(entries) == 0 {
		fmt.Fprintln(stdout, "(no exports or imports recorded)")
		return rcOK
	}
	tbl := table.New(
		table.Column{Header: "WHEN", Width: 19},
		table.Column{Header: "KIND", Width: 6},
		table.Column{Header: "GROUP", MinWidth: 8, Weight: 0.8, Truncate: true},
		table.Column{Header: "AGENTS", Width: 6, Align: table.AlignRight},
		table.Column{Header: "DETAIL", MinWidth: 10, Weight: 1.4, Truncate: true},
	)
	tbl.SetTerminalWidth(table.GetTerminalWidth())
	for _, e := range entries {
		when := e.At
		if t, err := time.Parse(time.RFC3339, e.At); err == nil {
			when = t.Local().Format("2006-01-02 15:04:05")
		}
		group := e.SourceGroup
		detail := ""
		if e.Kind == "import" {
			if e.ResultGroup != "" && e.ResultGroup != e.SourceGroup {
				group = e.SourceGroup + " -> " + e.ResultGroup
			} else {
				group = e.ResultGroup
			}
			detail = "into " + e.TargetDir
			if n := countRemaps(e.ConvRemaps); n > 0 {
				detail += fmt.Sprintf("  (%d conv-id(s) remapped)", n)
			}
		}
		tbl.AddRow(table.Row{Cells: []string{
			when, e.Kind, group, strconv.Itoa(e.AgentCount), detail,
		}})
	}
	fmt.Fprintln(stdout, tbl.Render())
	return rcOK
}

// countRemaps reports how many conv-id remaps a transfer-log row's
// conv_remaps JSON object holds.
func countRemaps(remapJSON string) int {
	if remapJSON == "" || remapJSON == "{}" {
		return 0
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(remapJSON), &m); err != nil {
		return 0
	}
	return len(m)
}

// humanBytes renders a byte count compactly for CLI output.
func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
