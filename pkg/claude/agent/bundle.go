package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/configbundle"
	"github.com/tofutools/tclaude/pkg/common"
)

type bundleExportParams struct {
	CarryPermissions bool   `long:"carry-permissions" help:"Request receiver-approved permission carry; off by default."`
	Agent            string `pos:"true" help:"Agent selector to export."`
	File             string `long:"file" short:"f" help:"Destination ZIP archive."`
	History          bool   `long:"history" help:"Include native conversation history when supported."`
	AllowFlagged     bool   `long:"allow-flagged" help:"Explicitly allow credential-shaped text; inspect before sharing."`
}
type bundleImportParams struct {
	CarryPermissions          bool     `long:"carry-permissions" help:"Request receiver-approved permission carry; off by default."`
	AllowSensitivePermissions bool     `long:"allow-sensitive-permissions" help:"Explicitly allow permission administration, sandbox, human interaction and federation administration grants; requires unrestricted trust."`
	File                      string   `long:"file" short:"f" help:"Bundle ZIP archive; '-' reads stdin."`
	Apply                     bool     `long:"apply" help:"Create a fresh local agent through normal spawn checks."`
	Group                     string   `long:"group" optional:"true" help:"Receiving group (required with --apply). Source memberships are advisory."`
	Name                      string   `long:"name" optional:"true" help:"Override the imported agent name."`
	Cwd                       string   `long:"cwd" optional:"true" help:"Remap the working directory."`
	Worktree                  string   `long:"worktree" optional:"true" help:"Remap an optional worktree hint."`
	KeepPaths                 bool     `long:"keep-paths" help:"Reuse recorded paths that exist locally; --cwd and --set override them."`
	Set                       []string `long:"set" optional:"true" help:"Resolve profile path placeholder with name=value (repeatable)."`
	SkipHistory               bool     `long:"skip-history" help:"Import configuration without native history."`
	JSON                      bool     `long:"json" help:"Print the full machine-readable preview or result."`
}

func bundleCmd() *cobra.Command {
	return boa.CmdT[struct{}]{Use: "bundle", Short: "Transfer portable agent configuration and optional history", ParamEnrich: common.DefaultParamEnricher(), SubCmds: []*cobra.Command{
		boa.CmdT[bundleExportParams]{Use: "export", Short: "Export an agent to a portable ZIP archive", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *bundleExportParams, _ *cobra.Command, _ []string) { os.Exit(runBundleExport(p, os.Stderr)) }}.ToCobra(),
		boa.CmdT[bundleImportParams]{Use: "import", Short: "Preview an agent bundle, or spawn it with --apply", Long: "Preview by default. --apply creates a fresh agent in --group through normal spawn checks. Permission carry requires --carry-permissions on export and import, and receiver authorization. Ownership and source memberships never travel. Native history support depends on the harness.", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *bundleImportParams, _ *cobra.Command, _ []string) {
			os.Exit(runBundleImport(p, os.Stdin, os.Stdout, os.Stderr))
		}}.ToCobra(),
	}}.ToCobra()
}
func printBundleFindings(out io.Writer, findings []agentbundle.Finding) {
	for _, f := range findings {
		fmt.Fprintf(out, "Flag: %s: %d occurrence(s); first locations: %s\n", f.Kind, f.Count, strings.Join(f.Locations, ", "))
	}
}
func printBundleError(out io.Writer, err error) {
	fmt.Fprintln(out, err)
	var de *DaemonError
	if errors.As(err, &de) {
		var detail struct {
			Findings []agentbundle.Finding `json:"findings"`
			Preview  json.RawMessage       `json:"preview"`
		}
		if json.Unmarshal(de.Raw, &detail) == nil {
			printBundleFindings(out, detail.Findings)
			if len(detail.Preview) > 0 {
				fmt.Fprintln(out, string(detail.Preview))
			}
		}
	}
}
func runBundleExport(p *bundleExportParams, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	q := url.Values{"agent": {p.Agent}}
	if p.CarryPermissions {
		q.Set("carry_permissions", "true")
	}
	if p.History {
		q.Set("history", "true")
	}
	if p.AllowFlagged {
		q.Set("allow_flagged", "true")
	}
	archive, err := os.CreateTemp("", ".agent-cli-export-")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return rcIOFailure
	}
	defer func() { _ = archive.Close(); _ = os.Remove(archive.Name()) }()
	err = downloadAgentArchive("/v1/agent-bundle/export?"+q.Encode(), archive)
	if err != nil {
		printBundleError(stderr, err)
		return MapDaemonErrorToRC(err)
	}
	b, err := agentbundle.DecodeFile(archive, localAgentBundleLimit(), "")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return rcIOFailure
	}
	defer func() { _ = b.Close() }()
	for _, warning := range b.Manifest.Warnings {
		fmt.Fprintln(stderr, "Warning:", warning)
	}
	printBundleFindings(stderr, b.Manifest.Findings)
	file, err := os.OpenFile(p.File, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return rcIOFailure
	}
	err = file.Chmod(0600)
	if err == nil {
		_, err = archive.Seek(0, io.SeekStart)
		if err == nil {
			_, err = io.Copy(file, archive)
		}
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return rcIOFailure
	}
	fmt.Fprintln(stderr, "Exported", p.File)
	return rcOK
}
func runBundleImport(p *bundleImportParams, stdin io.Reader, out, stderr io.Writer) int {
	reader := stdin
	if p.File != "-" {
		f, err := os.Open(p.File)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return rcInvalidArg
		}
		defer f.Close()
		reader = f
	}
	archive, err := os.CreateTemp("", ".agent-cli-import-")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return rcIOFailure
	}
	defer func() { _ = archive.Close(); _ = os.Remove(archive.Name()) }()
	limit := localAgentBundleLimit()
	n, err := io.Copy(archive, io.LimitReader(reader, limit+1))
	if err != nil || n > limit {
		fmt.Fprintf(stderr, "archive exceeds or cannot be read within federation.agent_transfer_max_bytes=%d\n", limit)
		return rcInvalidArg
	}
	b, err := agentbundle.DecodeFile(archive, limit, "")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return rcInvalidArg
	}
	_ = b.Close()
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	q := url.Values{}
	if p.CarryPermissions {
		q.Set("carry_permissions", "true")
	}
	if p.AllowSensitivePermissions {
		q.Set("allow_sensitive_permissions", "true")
	}
	for key, value := range map[string]string{"group": p.Group, "name": p.Name, "cwd": p.Cwd, "worktree": p.Worktree} {
		if value != "" {
			q.Set(key, value)
		}
	}
	for _, v := range p.Set {
		q.Add("set", v)
	}
	if p.Apply {
		q.Set("apply", "true")
	}
	if p.KeepPaths {
		q.Set("keep_paths", "true")
	}
	if p.SkipHistory {
		q.Set("skip_history", "true")
	}
	var result json.RawMessage
	if err := uploadAgentArchive("/v1/agent-bundle/import?"+q.Encode(), archive, &result); err != nil {
		printBundleError(stderr, err)
		return MapDaemonErrorToRC(err)
	}
	// The complete resolved definition is reviewable in both modes, including
	// launch authority fields and advisory permission provenance.
	var formatted strings.Builder
	if p.JSON {
		fmt.Fprintln(out, string(result))
	} else {
		var value any
		_ = json.Unmarshal(result, &value)
		pretty, _ := json.MarshalIndent(value, "", "  ")
		formatted.Write(pretty)
		fmt.Fprintln(out, formatted.String())
		var preview struct {
			Unresolved []configbundle.Placeholder `json:"unresolved"`
		}
		_ = json.Unmarshal(result, &preview)
		for _, p := range preview.Unresolved {
			fmt.Fprintf(out, "Unresolved %s %s (original: %q): --set %s=value or --keep-paths\n", p.Item, p.Field, p.Original, p.Name)
		}
		if !p.Apply {
			fmt.Fprintln(out, "Preview only. Use --apply --group NAME to create an agent.")
		}
	}
	return rcOK
}
