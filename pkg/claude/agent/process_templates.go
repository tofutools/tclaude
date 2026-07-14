package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/common"
)

// `tclaude agent process-templates` is the socket-authenticated authoring
// client for the same process-template REST handlers used by the dashboard.
// It intentionally has no run/instantiate/delete verb: authoring content must
// never execute it as a side effect.
func processTemplatesCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "process-templates",
		Short:       "Author process templates through agentd",
		Long:        "List, show, validate, and CAS-save process-template YAML through agentd. Reads require process.templates.read; save requires process.templates.manage. Saving never executes or instantiates a process.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			processTemplatesLsCmd(),
			processTemplatesShowCmd(),
			processTemplatesValidateCmd(),
			processTemplatesSaveCmd(),
		},
	}.ToCobra()
}

type processTemplateVersionJSON struct {
	Ref          string `json:"ref"`
	SemanticHash string `json:"semanticHash"`
	SourceHash   string `json:"sourceHash"`
	Actor        string `json:"actor,omitempty"`
}

type processTemplateListJSON struct {
	ID            string                     `json:"id"`
	Name          string                     `json:"name,omitempty"`
	Description   string                     `json:"description,omitempty"`
	VersionCount  int                        `json:"versionCount"`
	LatestVersion processTemplateVersionJSON `json:"latestVersion"`
}

type processTemplateShowJSON struct {
	Source       string `json:"source"`
	SourceHash   string `json:"sourceHash"`
	SemanticHash string `json:"semanticHash"`
	CurrentRef   string `json:"currentRef"`
}

type processTemplateDiagJSON struct {
	Scope    string `json:"scope"`
	TargetID string `json:"targetId,omitempty"`
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`
}

type processTemplateValidateJSON struct {
	SourceHash   string                    `json:"sourceHash"`
	SemanticHash string                    `json:"semanticHash"`
	Diagnostics  []processTemplateDiagJSON `json:"diagnostics"`
}

type processTemplateSourceRequest struct {
	Source     string `json:"source"`
	SourceHash string `json:"sourceHash,omitempty"`
}

func processTemplatesLsCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "ls",
		Short:       "List stored process templates",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(_ *struct{}, _ *cobra.Command, _ []string) {
			os.Exit(runProcessTemplatesLs(os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runProcessTemplatesLs(stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var response struct {
		Templates []processTemplateListJSON `json:"templates"`
	}
	if err := DaemonRequest(http.MethodGet, "/v1/process/templates", nil, &response, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if len(response.Templates) == 0 {
		fmt.Fprintln(stdout, "No process templates.")
		return rcOK
	}
	for _, tmpl := range response.Templates {
		label := tmpl.Name
		if label == "" {
			label = "(unnamed)"
		}
		fmt.Fprintf(stdout, "%s\t%s\tversions=%d\tref=%s", tmpl.ID, label, tmpl.VersionCount, tmpl.LatestVersion.Ref)
		if tmpl.LatestVersion.Actor != "" {
			fmt.Fprintf(stdout, "\tactor=%s", tmpl.LatestVersion.Actor)
		}
		fmt.Fprintln(stdout)
		if tmpl.Description != "" {
			fmt.Fprintf(stdout, "  %s\n", tmpl.Description)
		}
	}
	return rcOK
}

type processTemplatesShowParams struct {
	ID string `pos:"true" help:"Process-template id to show."`
}

func processTemplatesShowCmd() *cobra.Command {
	return boa.CmdT[processTemplatesShowParams]{
		Use:         "show <id>",
		Short:       "Emit canonical YAML with CAS metadata",
		Long:        "Writes valid process-template YAML. The leading YAML comments carry currentRef, sourceHash, and semanticHash for the validate then CAS-save workflow.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *processTemplatesShowParams, _ *cobra.Command, _ []string) {
			os.Exit(runProcessTemplatesShow(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runProcessTemplatesShow(p *processTemplatesShowParams, stdout, stderr io.Writer) int {
	id := strings.TrimSpace(p.ID)
	if id == "" {
		fmt.Fprintln(stderr, "Error: a process-template id is required")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var response processTemplateShowJSON
	if err := DaemonRequest(http.MethodGet, "/v1/process/templates/"+url.PathEscape(id), nil, &response, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	// Metadata is emitted as comments so redirecting stdout produces a valid,
	// directly editable YAML file rather than a mixed prose/document stream.
	fmt.Fprintf(stdout, "# tclaude currentRef: %s\n", response.CurrentRef)
	fmt.Fprintf(stdout, "# tclaude sourceHash: %s\n", response.SourceHash)
	fmt.Fprintf(stdout, "# tclaude semanticHash: %s\n", response.SemanticHash)
	fmt.Fprint(stdout, response.Source)
	if response.Source != "" && !strings.HasSuffix(response.Source, "\n") {
		fmt.Fprintln(stdout)
	}
	return rcOK
}

type processTemplatesValidateParams struct {
	File string `long:"file" short:"f" help:"Path to process-template YAML ('-' reads stdin)."`
}

func processTemplatesValidateCmd() *cobra.Command {
	return boa.CmdT[processTemplatesValidateParams]{
		Use:         "validate --file <path>",
		Short:       "Validate process-template YAML through agentd",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *processTemplatesValidateParams, _ *cobra.Command, _ []string) {
			os.Exit(runProcessTemplatesValidate(p, os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runProcessTemplatesValidate(p *processTemplatesValidateParams, stdin io.Reader, stdout, stderr io.Writer) int {
	source, rc := loadProcessTemplateYAML(p.File, stdin, stderr)
	if rc != rcOK {
		return rc
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var response processTemplateValidateJSON
	if err := DaemonRequest(http.MethodPost, "/v1/process/validate", processTemplateSourceRequest{Source: source}, &response, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	hasErrors := renderProcessTemplateDiagnostics(response.Diagnostics, stdout)
	fmt.Fprintf(stdout, "sourceHash=%s\nsemanticHash=%s\n", response.SourceHash, response.SemanticHash)
	if hasErrors {
		return rcInvalidArg
	}
	fmt.Fprintln(stdout, "Valid process template.")
	return rcOK
}

type processTemplatesSaveParams struct {
	File             string `long:"file" short:"f" help:"Path to process-template YAML ('-' reads stdin)."`
	ExpectSourceHash string `long:"expect-source-hash" optional:"true" help:"sourceHash from the latest show. Omit only when creating a new id."`
	AskHuman         string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (for example 30s). Capped at 300s; timeout means deny."`
}

func processTemplatesSaveCmd() *cobra.Command {
	return boa.CmdT[processTemplatesSaveParams]{
		Use:         "save --file <path> [--expect-source-hash <hash>]",
		Short:       "CAS-save process-template YAML",
		Long:        "Creates a new template when --expect-source-hash is omitted and the id does not exist. For edits, pass the sourceHash emitted by show; a stale hash is refused with re-show and merge guidance. Saving has no execution side effects.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *processTemplatesSaveParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *processTemplatesSaveParams, _ *cobra.Command, _ []string) {
			os.Exit(runProcessTemplatesSave(p, os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runProcessTemplatesSave(p *processTemplatesSaveParams, stdin io.Reader, stdout, stderr io.Writer) int {
	source, rc := loadProcessTemplateYAML(p.File, stdin, stderr)
	if rc != rcOK {
		return rc
	}
	parsed, err := model.Parse([]byte(source))
	if err != nil {
		fmt.Fprintf(stderr, "Error: --file is not parseable process-template YAML: %v\n", err)
		return rcInvalidArg
	}
	id := strings.TrimSpace(parsed.Template.ID)
	if id == "" {
		fmt.Fprintln(stderr, "Error: process-template YAML must declare id")
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
	request := processTemplateSourceRequest{Source: source, SourceHash: strings.TrimSpace(p.ExpectSourceHash)}
	var response struct {
		Ref          string                    `json:"ref"`
		SemanticHash string                    `json:"semanticHash"`
		SourceHash   string                    `json:"sourceHash"`
		Actor        string                    `json:"actor"`
		Diagnostics  []processTemplateDiagJSON `json:"diagnostics"`
	}
	err = DaemonRequest(http.MethodPost, "/v1/process/templates/"+url.PathEscape(id), request, &response, DaemonOpts{AskHuman: ask})
	if err != nil {
		if renderProcessTemplateConflict(err, id, stderr) {
			return rcIOFailure
		}
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	renderProcessTemplateDiagnostics(response.Diagnostics, stdout)
	fmt.Fprintf(stdout, "Saved process template %q.\nref=%s\nsourceHash=%s\nsemanticHash=%s\nactor=%s\n",
		id, response.Ref, response.SourceHash, response.SemanticHash, response.Actor)
	return rcOK
}

func renderProcessTemplateConflict(err error, id string, stderr io.Writer) bool {
	var daemonErr *DaemonError
	if !errors.As(err, &daemonErr) || daemonErr.Status != http.StatusConflict || daemonErr.Code != "process_template_conflict" {
		return false
	}
	var conflict struct {
		Error             string `json:"error"`
		Code              string `json:"code"`
		CurrentSourceHash string `json:"currentSourceHash"`
		CurrentRef        string `json:"currentRef"`
	}
	_ = json.Unmarshal(daemonErr.Raw, &conflict)
	fmt.Fprintf(stderr, "Error: %s (code=%s)\ncurrentRef=%s\ncurrentSourceHash=%s\n",
		conflict.Error, daemonErr.Code, conflict.CurrentRef, conflict.CurrentSourceHash)
	fmt.Fprintf(stderr, "Re-run 'tclaude agent process-templates show %s', merge your edit into that YAML, validate it, then save with --expect-source-hash %s. Never blind-overwrite a conflict.\n",
		id, conflict.CurrentSourceHash)
	return true
}

func renderProcessTemplateDiagnostics(diagnostics []processTemplateDiagJSON, out io.Writer) bool {
	hasErrors := false
	for _, diagnostic := range diagnostics {
		target := diagnostic.Scope
		if diagnostic.TargetID != "" {
			target += ":" + diagnostic.TargetID
		}
		fmt.Fprintf(out, "%s %s [%s] %s\n", strings.ToUpper(diagnostic.Severity), diagnostic.Code, target, diagnostic.Message)
		hasErrors = hasErrors || diagnostic.Severity == string(model.SeverityError)
	}
	return hasErrors
}

func loadProcessTemplateYAML(file string, stdin io.Reader, stderr io.Writer) (string, int) {
	if strings.TrimSpace(file) == "" {
		fmt.Fprintln(stderr, "Error: --file is required (path to process-template YAML, or - to read stdin)")
		return "", rcInvalidArg
	}
	var (
		data []byte
		err  error
	)
	if file == "-" {
		data, err = io.ReadAll(stdin)
	} else {
		data, err = os.ReadFile(file)
	}
	if err != nil {
		fmt.Fprintf(stderr, "Error: read process-template YAML: %v\n", err)
		return "", rcIOFailure
	}
	if len(data) > 4<<20 {
		fmt.Fprintln(stderr, "Error: process-template YAML exceeds the 4 MiB limit")
		return "", rcInvalidArg
	}
	return string(data), rcOK
}
