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
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/paf/legacy"
)

type bundleExportParams struct {
	OmitManagedInstructions bool   `long:"omit-managed-instructions" help:"Human only: omit managed skills and rules from this export."`
	CarryPermissions        bool   `long:"carry-permissions" help:"Request receiver-approved permission carry; off by default."`
	Agent                   string `pos:"true" help:"Agent selector to export."`
	File                    string `long:"file" short:"f" help:"Destination file: a .paf, or with --format v1 an agent bundle ZIP."`
	History                 bool   `long:"history" help:"Include native conversation history when supported."`
	AllowFlagged            bool   `long:"allow-flagged" help:"Explicitly allow credential-shaped text; inspect before sharing."`
	Format                  string `long:"format" optional:"true" alts:"paf,v1" help:"Archive format: paf (the default; Portable Agent Format, see docs/paf.md) or v1 (agent bundle v1, for importing into an older tclaude)."`
	NoNative                bool   `long:"no-native" help:"PAF only: leave the native history out and carry the conversation in harness-neutral form only."`
	NoContext               bool   `long:"no-context" help:"PAF only: leave the harness-neutral copy of the conversation out."`
	WithSummary             bool   `long:"with-summary" help:"PAF only: first ask the agent to write a summary of its earlier work and carry it, so a landing on a smaller context window keeps it. Costs the agent a turn; waits up to --summary-wait."`
	SummaryWait             string `long:"summary-wait" optional:"true" help:"With --with-summary: how long to wait for the agent's summary (default 10m)."`
	CarriedWorkflowParams
}

// CarriedWorkflowParams choose the workflows an export carries (docs/paf.md,
// "The envelope"). Carrying one needs process.templates.read.
type CarriedWorkflowParams struct {
	Workflow  []string `long:"workflow" optional:"true" help:"Carry this process template and everything it references (repeatable). Needs process.templates.read."`
	Workflows bool     `long:"workflows" help:"Carry the workflows this has: an agent's active runs, or a group's saved processes. Needs process.templates.read."`
}

func (p CarriedWorkflowParams) addTo(q url.Values) {
	for _, id := range p.Workflow {
		q.Add("workflow", id)
	}
	if p.Workflows {
		q.Set("workflows", "own")
	}
}

// pafExportParams are bundleExportParams for `tclaude paf export`, which
// always writes a PAF.
type pafExportParams struct {
	OmitManagedInstructions bool   `long:"omit-managed-instructions" help:"Human only: omit managed skills and rules from this export."`
	Agent                   string `pos:"true" help:"Agent selector to export."`
	File                    string `long:"file" short:"f" help:"Destination .paf file."`
	History                 bool   `long:"history" help:"Include the conversation history."`
	NoNative                bool   `long:"no-native" help:"Leave the native history out and carry the conversation in harness-neutral form only."`
	NoContext               bool   `long:"no-context" help:"Leave the harness-neutral copy of the conversation out; only the native history travels."`
	AllowFlagged            bool   `long:"allow-flagged" help:"Explicitly allow credential-shaped text; inspect before sharing."`
	WithSummary             bool   `long:"with-summary" help:"First ask the agent to write a summary of its earlier work and carry it, so a landing on a smaller context window keeps it. Costs the agent a turn; waits up to --summary-wait."`
	SummaryWait             string `long:"summary-wait" optional:"true" help:"With --with-summary: how long to wait for the agent's summary (default 10m)."`
	CarriedWorkflowParams
}

// PAFExportCmd is `tclaude paf export`: agent bundle export writing PAF.
func PAFExportCmd() *cobra.Command {
	return boa.CmdT[pafExportParams]{Use: "export", Short: "Export an agent as a PAF file",
		Long: "Export an agent's definition, environment and, with --history, its conversation as a Portable Agent Format file. " +
			"The conversation travels as the native history plus a harness-neutral copy, so it can land on any harness. " +
			"Credential-shaped text refuses the export unless --allow-flagged. Needs agent.bundle.export.",
		ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *pafExportParams, _ *cobra.Command, _ []string) {
			os.Exit(runBundleExport(&bundleExportParams{OmitManagedInstructions: p.OmitManagedInstructions, Agent: p.Agent, File: p.File, History: p.History, AllowFlagged: p.AllowFlagged, Format: "paf", NoNative: p.NoNative, NoContext: p.NoContext, WithSummary: p.WithSummary, SummaryWait: p.SummaryWait, CarriedWorkflowParams: p.CarriedWorkflowParams}, os.Stderr))
		}}.ToCobra()
}

// PAFImportCmd is `tclaude paf import`: agent bundle import, which reads PAF
// and legacy bundles alike.
func PAFImportCmd() *cobra.Command {
	return boa.CmdT[bundleImportParams]{Use: "import", Short: "Preview a PAF, or land it as a new agent with --apply",
		Long: "Preview by default: the agent, the landing harness and model, how its conversation lands (native, translated or not at all) " +
			"with the expected losses, and the security posture. --apply creates a fresh agent in --group through normal spawn checks. " +
			"--harness and --model land it somewhere other than where it came from; the conversation is then translated within the " +
			"landing model's context budget. What the file asks for beyond a spawn here is listed and dropped unless a human keeps it " +
			"with --keep-defaults, --keep or --keep-all and the digest the preview prints; the sandbox and sudo are never kept. " +
			"Needs agent.bundle.import.",
		ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *bundleImportParams, _ *cobra.Command, _ []string) {
			os.Exit(runBundleImport(p, os.Stdin, os.Stdout, os.Stderr))
		}}.ToCobra()
}

type bundleImportParams struct {
	ApproveManagedScripts     string   `long:"approve-managed-scripts" optional:"true" help:"Human only: approve this transfer's script files using managed_instruction_digest from its preview."`
	OmitManagedInstructions   bool     `long:"omit-managed-instructions" help:"Human only: omit the carried skills and rules; receiver rules still apply."`
	CarryPermissions          bool     `long:"carry-permissions" help:"Request receiver-approved permission carry; off by default."`
	AllowSensitivePermissions bool     `long:"allow-sensitive-permissions" help:"Explicitly allow permission administration, sandbox, human interaction and federation administration grants; requires unrestricted trust."`
	File                      string   `long:"file" short:"f" help:"Agent bundle ZIP or .paf archive; '-' reads stdin."`
	Apply                     bool     `long:"apply" help:"Create a fresh local agent through normal spawn checks."`
	NoStart                   bool     `long:"no-start" help:"With --apply, land the agent offline with its conversation; tclaude agent resume starts it."`
	Group                     string   `long:"group" optional:"true" help:"Receiving group (required with --apply). Source memberships are advisory."`
	Name                      string   `long:"name" optional:"true" help:"Override the imported agent name."`
	Cwd                       string   `long:"cwd" optional:"true" help:"Remap the working directory."`
	Worktree                  string   `long:"worktree" optional:"true" help:"Remap an optional worktree hint."`
	KeepPaths                 bool     `long:"keep-paths" help:"Reuse recorded paths that exist locally; --cwd and --set override them."`
	Set                       []string `long:"set" optional:"true" help:"Resolve profile path placeholder with name=value (repeatable)."`
	SkipHistory               bool     `long:"skip-history" help:"Import configuration without native history."`
	ConfirmHistoryLoss        bool     `long:"confirm-history-loss" help:"Accept a landing that cuts 25% or more of the conversation to fit the landing model; the full history stays in the file and is saved beside the agent."`
	Harness                   string   `long:"harness" optional:"true" help:"Land on this harness instead of the source one; the conversation is translated."`
	Model                     string   `long:"model" optional:"true" help:"Landing model; the conversation is budgeted to its context window."`
	Agent                     string   `long:"agent" optional:"true" help:"Which agent to land from a .paf that ships several for a workflow."`
	Role                      string   `long:"role" optional:"true" help:"Land under this role instead of the one the file records, such as the role a workflow asks for."`
	ImportKeepParams
	JSON bool `long:"json" help:"Print the full machine-readable preview or result."`
}

func bundleCmd() *cobra.Command {
	return boa.CmdT[struct{}]{Use: "bundle", Short: "Transfer portable agent configuration and optional history", ParamEnrich: common.DefaultParamEnricher(), SubCmds: []*cobra.Command{
		boa.CmdT[bundleExportParams]{Use: "export", Short: "Export an agent to a portable .paf file (or a legacy v1 ZIP with --format v1)", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *bundleExportParams, _ *cobra.Command, _ []string) { os.Exit(runBundleExport(p, os.Stderr)) }}.ToCobra(),
		boa.CmdT[bundleImportParams]{Use: "import", Short: "Preview an agent bundle, or spawn it with --apply", Long: "Preview by default. --apply creates a fresh agent in --group through normal spawn checks. Permission carry requires --carry-permissions on export and import, and receiver authorization. What a file asks for beyond a spawn here is dropped unless a human keeps it (--keep-defaults, --keep, --keep-all). Ownership and source memberships never travel. Native history support depends on the harness.", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *bundleImportParams, _ *cobra.Command, _ []string) {
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
	legacyFormat := p.Format == "v1" || p.Format == "bundle"
	if p.WithSummary {
		if legacyFormat {
			fmt.Fprintln(stderr, "Error: --with-summary needs a PAF export; agent bundle v1 cannot carry a summary")
			return rcInvalidArg
		}
		id, rc := awaitAgentSummary(p.Agent, p.SummaryWait, stderr)
		if rc != rcOK {
			return rc
		}
		q.Set("summary", id)
	}
	if p.OmitManagedInstructions {
		q.Set("omit_managed_instructions", "true")
	}
	if p.CarryPermissions {
		q.Set("carry_permissions", "true")
	}
	if p.History {
		q.Set("history", "true")
	}
	if p.Format != "" {
		q.Set("format", p.Format)
	}
	if p.AllowFlagged {
		q.Set("allow_flagged", "true")
	}
	if p.NoNative {
		q.Set("no_native", "true")
	}
	if p.NoContext {
		q.Set("no_context", "true")
	}
	p.addTo(q)
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
	b, _, err := legacy.DecodeBundleFile(archive, legacy.DecodeLimits{MaxBytes: localAgentBundleLimit()}, "")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return rcIOFailure
	}
	defer func() { _ = b.Close() }()
	for _, warning := range b.Manifest.Warnings {
		if p.NoNative && warning == legacy.WarningContextOnly {
			continue // asked for; `tclaude paf import` lands it
		}
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
	b, _, err := legacy.DecodeBundleFile(archive, legacy.DecodeLimits{MaxBytes: limit, AgentKey: p.Agent}, "")
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
	for key, value := range map[string]string{"group": p.Group, "name": p.Name, "cwd": p.Cwd, "worktree": p.Worktree, "harness": p.Harness, "model": p.Model, "agent": p.Agent, "role": p.Role} {
		if value != "" {
			q.Set(key, value)
		}
	}
	for _, v := range p.Set {
		q.Add("set", v)
	}
	if p.NoStart && !p.Apply {
		fmt.Fprintln(stderr, "Error: --no-start applies only with --apply")
		return rcInvalidArg
	}
	if p.Apply {
		q.Set("apply", "true")
	}
	if p.NoStart {
		q.Set("start", "false")
	}
	if p.KeepPaths {
		q.Set("keep_paths", "true")
	}
	if p.SkipHistory {
		q.Set("skip_history", "true")
	}
	if p.ConfirmHistoryLoss {
		q.Set("confirm_history_loss", "true")
	}
	if p.OmitManagedInstructions {
		q.Set("omit_managed_instructions", "true")
	}
	if p.ApproveManagedScripts != "" {
		q.Set("approve_managed_scripts", p.ApproveManagedScripts)
	}
	p.addTo(q)
	var result json.RawMessage
	if err := uploadAgentArchive("/v1/agent-bundle/import?"+q.Encode(), archive, &result); err != nil {
		printBundleError(stderr, err)
		return MapDaemonErrorToRC(err)
	}
	// The complete resolved definition is reviewable in both modes, including
	// launch authority fields and advisory permission provenance.
	if p.JSON {
		fmt.Fprintln(out, string(result))
	} else {
		renderAgentImport(out, result, p.Apply)
		if !p.Apply {
			fmt.Fprintln(out, "Preview only. Use --apply --group NAME to create an agent.")
		}
	}
	return rcOK
}
