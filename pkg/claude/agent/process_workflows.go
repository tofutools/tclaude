package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"unicode"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/pwf"
)

// Workflow sharing (.pwf, docs/pwf.md): export a stored template with every
// template it references, inspect a file offline, and import one after
// reviewing what it would run. Importing stores templates only; it never
// starts a run.

type processTemplatesExportParams struct {
	ID       string `pos:"true" help:"Process-template id to export."`
	Version  string `long:"version" optional:"true" help:"Export this version (sha256:<hex> or a full ref) instead of the current one."`
	Output   string `long:"output" short:"o" optional:"true" help:"Write the .pwf here ('-' for stdout). Default: <id>.pwf in the current directory."`
	Title    string `long:"title" optional:"true" help:"Workflow title (default: the template's name)."`
	Summary  string `long:"summary" optional:"true" help:"One-paragraph summary (default: the template's description)."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (for example 30s). Capped at 300s; timeout means deny."`
}

func processTemplatesExportCmd() *cobra.Command {
	return boa.CmdT[processTemplatesExportParams]{
		Use:         "export <id>",
		Short:       "Export a process as a shareable .pwf workflow file",
		Long:        "Packs the template and every stored template its subprocess steps reference into a Portable Workflow Format file, signed with this node's federation key when it has one. Requires process.templates.read.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *processTemplatesExportParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *processTemplatesExportParams, _ *cobra.Command, _ []string) {
			os.Exit(runProcessTemplatesExport(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runProcessTemplatesExport(p *processTemplatesExportParams, stdout, stderr io.Writer) int {
	id := strings.TrimSpace(p.ID)
	if id == "" {
		fmt.Fprintln(stderr, "Error: a process-template id is required")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	q := url.Values{}
	for k, v := range map[string]string{"version": p.Version, "title": p.Title, "summary": p.Summary} {
		if v = strings.TrimSpace(v); v != "" {
			q.Set(k, v)
		}
	}
	path := "/v1/process/templates/" + url.PathEscape(id) + "/export"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	data, _, err := DaemonGetRaw(path)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	out := strings.TrimSpace(p.Output)
	if out == "-" {
		_, _ = stdout.Write(data)
		return rcOK
	}
	if out == "" {
		out = id + ".pwf"
	}
	if err := os.WriteFile(out, data, 0o644); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcIOFailure
	}
	archive, err := pwf.Open(bytes.NewReader(data), int64(len(data)), pwf.DefaultLimits())
	if err != nil {
		fmt.Fprintf(stderr, "Error: the daemon wrote an unreadable workflow: %v\n", err)
		return rcIOFailure
	}
	fmt.Fprintf(stdout, "Wrote %s (%d bytes, %d template(s)%s).\n", out, len(data), len(archive.Order), signedNote(archive))
	if n := len(archive.Workflow.Assignees); n > 0 {
		fmt.Fprintf(stdout, "Note: %d step(s) name a specific agent, which the importing node will not have; they wait until reassigned there.\n", n)
	}
	return rcOK
}

func signedNote(a *pwf.Archive) string {
	if len(a.Signatures) == 0 {
		return ", unsigned"
	}
	return ", signed by " + a.Signatures[0].Signature.KeyID
}

type processTemplatesInspectParams struct {
	File string `pos:"true" help:"Path to a .pwf file ('-' reads stdin)."`
	JSON bool   `long:"json" optional:"true" help:"Print workflow.json and the signature status as JSON."`
}

func processTemplatesInspectCmd() *cobra.Command {
	return boa.CmdT[processTemplatesInspectParams]{
		Use:         "inspect <file.pwf>",
		Short:       "Verify and summarize a .pwf workflow file offline",
		Long:        "Checks the file's integrity, templates and signatures and prints what the workflow needs: params, roles, the commands it would run, and steps naming a specific agent. Needs no daemon and changes nothing.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *processTemplatesInspectParams, _ *cobra.Command, _ []string) {
			os.Exit(runProcessTemplatesInspect(p, os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func readWorkflowFile(name string, stdin io.Reader) ([]byte, error) {
	if strings.TrimSpace(name) == "-" {
		return io.ReadAll(io.LimitReader(stdin, 32<<20))
	}
	return os.ReadFile(name)
}

func runProcessTemplatesInspect(p *processTemplatesInspectParams, stdin io.Reader, stdout, stderr io.Writer) int {
	data, err := readWorkflowFile(p.File, stdin)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	a, err := pwf.Open(bytes.NewReader(data), int64(len(data)), pwf.DefaultLimits())
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if p.JSON {
		sigs := []map[string]string{}
		for _, s := range a.Signatures {
			sigs = append(sigs, map[string]string{"keyId": s.Signature.KeyID, "status": string(s.Status)})
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"workflow": a.Workflow, "signatures": sigs})
		return rcOK
	}
	renderWorkflow(stdout, a.Workflow)
	if len(a.Signatures) == 0 {
		fmt.Fprintln(stdout, "Signature: none (valid but unverified)")
	}
	for _, s := range a.Signatures {
		fmt.Fprintf(stdout, "Signature: %s by key %s (this offline check does not say whose key it is; import shows that)\n", s.Status, shown(s.Signature.KeyID))
	}
	return rcOK
}

// workflowImportJSON mirrors the daemon's import preview (pwfImportView).
type workflowImportJSON struct {
	pwf.Workflow
	RootID     string `json:"rootId"`
	Signatures []struct {
		KeyID  string `json:"keyId"`
		Status string `json:"status"`
		Trust  string `json:"trust"`
	} `json:"signatures"`
	Templates []struct {
		Ref        string `json:"ref"`
		ID         string `json:"id"`
		Head       bool   `json:"head"`
		Status     string `json:"status"`
		CurrentRef string `json:"currentRef"`
	} `json:"templates"`
	Updates      int    `json:"updates"`
	Applied      bool   `json:"applied"`
	ReviewDigest string `json:"reviewDigest"`
}

type processTemplatesImportParams struct {
	File     string `pos:"true" help:"Path to a .pwf file ('-' reads stdin)."`
	Yes      bool   `long:"yes" short:"y" optional:"true" help:"Import after showing the review instead of stopping at the preview."`
	Replace  bool   `long:"replace" optional:"true" help:"Make the imported versions current where this node already has the template with different content."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (for example 30s). Capped at 300s; timeout means deny."`
}

func processTemplatesImportCmd() *cobra.Command {
	return boa.CmdT[processTemplatesImportParams]{
		Use:         "import <file.pwf>",
		Short:       "Review and import a .pwf workflow file",
		Long:        "Shows who signed the workflow, what it needs and every command it would run, and what importing changes here. Without --yes it stops at that review. Importing stores templates only and never starts a run; starting one later asks you to authorize its program profiles. Requires process.templates.manage.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *processTemplatesImportParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *processTemplatesImportParams, _ *cobra.Command, _ []string) {
			os.Exit(runProcessTemplatesImport(p, os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runProcessTemplatesImport(p *processTemplatesImportParams, stdin io.Reader, stdout, stderr io.Writer) int {
	data, err := readWorkflowFile(p.File, stdin)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	ask, err := ParseAskHuman(p.AskHuman)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var view workflowImportJSON
	if err := DaemonPostRawWithOptions("/v1/process/import", pwf.MediaType, data, nil, &view, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	renderWorkflow(stdout, view.Workflow)
	if len(view.Signatures) == 0 {
		fmt.Fprintln(stdout, "Signature: none (unverified)")
	}
	for _, s := range view.Signatures {
		fmt.Fprintf(stdout, "Signature: %s, key %s (%s)\n", shown(s.Status), shown(s.KeyID), shown(s.Trust))
	}
	fmt.Fprintln(stdout, "Templates:")
	for _, t := range view.Templates {
		line := fmt.Sprintf("  %-10s %s", shown(t.Status), shown(t.Ref))
		if t.CurrentRef != "" {
			line += " (here now: " + shown(t.CurrentRef) + ")"
		}
		fmt.Fprintln(stdout, line)
	}
	if !p.Yes {
		fmt.Fprintln(stdout, "Preview only; nothing was imported. Re-run with --yes to import.")
		return rcOK
	}
	if view.Updates > 0 && !p.Replace {
		fmt.Fprintf(stderr, "Error: %d template(s) already exist here with different content; re-run with --replace to make the imported versions current.\n", view.Updates)
		return rcInvalidArg
	}
	path := "/v1/process/import?apply=1&review=" + url.QueryEscape(view.ReviewDigest)
	if p.Replace {
		path += "&replace=1"
	}
	if err := DaemonPostRawWithOptions(path, pwf.MediaType, data, nil, &view, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Imported %q. Start it with: tclaude process run %s\n", view.Title, shown(view.RootID))
	return rcOK
}

// shown makes a file-controlled string safe to print in a review: anything
// with control characters (which could redraw the terminal and fake what the
// review says) is shown quoted, escapes visible.
func shown(v string) string {
	for _, r := range v {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return strconv.Quote(v)
		}
	}
	return v
}

func shownAll(vs []string) string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = shown(v)
	}
	return strings.Join(out, ", ")
}

// renderWorkflow prints the review a person needs before trusting a
// workflow: what it is, its inputs, the roles it asks for, every command it
// would run, and the steps that are not portable. Every string comes from
// the file, so each goes through shown.
func renderWorkflow(out io.Writer, wf pwf.Workflow) {
	fmt.Fprintf(out, "Workflow: %s\n", shown(wf.Title))
	if wf.Summary != "" {
		fmt.Fprintf(out, "  %s\n", shown(wf.Summary))
	}
	fmt.Fprintf(out, "Root: %s (%d template(s))\n", shown(wf.Root), len(wf.Templates))
	if len(wf.Params) > 0 {
		fmt.Fprintln(out, "Inputs:")
		for _, p := range wf.Params {
			req := ""
			if p.Required {
				req = ", required"
			}
			fmt.Fprintf(out, "  %s (%s%s)\n", shown(p.Name), shown(p.Type), req)
		}
	}
	if len(wf.Roles) > 0 {
		fmt.Fprintln(out, "Agent roles:")
		for _, r := range wf.Roles {
			fmt.Fprintf(out, "  %s: %s\n", shown(r.Name), shownAll(r.Steps))
		}
	}
	if len(wf.Humans) > 0 {
		fmt.Fprintf(out, "Asks you: %s\n", shownAll(wf.Humans))
	}
	if len(wf.Programs) == 0 {
		fmt.Fprintln(out, "Commands: none")
	} else {
		fmt.Fprintln(out, "Commands it would run (each needs its profile authorized when a run starts):")
		for _, p := range wf.Programs {
			args := make([]string, len(p.Args))
			for i, a := range p.Args {
				args[i] = shown(a)
			}
			fmt.Fprintf(out, "  %s [%s]: %s\n", shown(p.Step), shown(p.Profile), strings.TrimSpace(shown(p.Run)+" "+strings.Join(args, " ")))
		}
	}
	for _, a := range wf.Assignees {
		fmt.Fprintf(out, "Not portable: %s name agent %s\n", shownAll(a.Steps), strconv.Quote(a.Name))
	}
}
