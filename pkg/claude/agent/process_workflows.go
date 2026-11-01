package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"slices"
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
	ID               string   `pos:"true" help:"Process-template id to export."`
	Version          string   `long:"version" optional:"true" help:"Export this version (sha256:<hex> or a full ref) instead of the current one."`
	Output           string   `long:"output" short:"o" optional:"true" help:"Write the .pwf here ('-' for stdout). Default: <id>.pwf in the current directory."`
	Title            string   `long:"title" optional:"true" help:"Workflow title (default: the template's name)."`
	Summary          string   `long:"summary" optional:"true" help:"One-paragraph summary (default: the template's description)."`
	Author           string   `long:"author" optional:"true" help:"Who the workflow is from, shown to whoever reviews it (only a signature proves it)."`
	Licence          string   `long:"licence" optional:"true" help:"The licence it is shared under, for example MIT."`
	Strict           bool     `long:"strict" optional:"true" help:"Refuse to export a workflow that is not portable: paths from this machine, steps naming a specific agent, or failing validation."`
	AssigneesToRoles bool     `long:"assignees-to-roles" optional:"true" help:"Stop naming a specific agent in exported steps: they ask for the role it holds here (when it has exactly one), or for the profile they already have."`
	AssigneeRole     []string `long:"assignee-role" optional:"true" help:"Export steps naming an agent with a role instead, as <agent>=<role> (repeatable). Wins over --assignees-to-roles."`
	AskHuman         string   `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (for example 30s). Capped at 300s; timeout means deny."`
}

func processTemplatesExportCmd() *cobra.Command {
	return boa.CmdT[processTemplatesExportParams]{
		Use:         "export",
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
	for k, v := range map[string]string{"version": p.Version, "title": p.Title, "summary": p.Summary, "author": p.Author, "licence": p.Licence} {
		if v = strings.TrimSpace(v); v != "" {
			q.Set(k, v)
		}
	}
	path := "/v1/process/templates/" + url.PathEscape(id) + "/export"
	for _, m := range p.AssigneeRole {
		from, to, ok := strings.Cut(m, "=")
		if !ok || strings.TrimSpace(from) == "" || strings.TrimSpace(to) == "" {
			fmt.Fprintf(stderr, "Error: --assignee-role %q: want <agent>=<role>\n", m)
			return rcInvalidArg
		}
		q.Add("assignee", strings.TrimSpace(from)+"="+strings.TrimSpace(to))
	}
	if p.AssigneesToRoles {
		reportAssigneeRoles(path, q, stderr)
		q.Set("assignees", "roles")
	}
	if p.Strict {
		q.Set("strict", "1")
	}
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
		if archive, err := pwf.Open(bytes.NewReader(data), int64(len(data)), pwf.DefaultLimits()); err == nil {
			warnWorkflowProblems(stderr, archive)
			warnWorkflowPaths(stderr, archive.Paths)
		}
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
		fmt.Fprintf(stdout, "Note: %d step(s) name a specific agent, which the importing node will not have; they fall to their profile there, or wait until reassigned.\n", n)
	}
	warnWorkflowProblems(stderr, archive)
	warnWorkflowPaths(stderr, archive.Paths)
	return rcOK
}

// reportAssigneeRoles says which role --assignees-to-roles picks for each
// agent: the role comes from this node's groups, so the author should see it.
func reportAssigneeRoles(path string, q url.Values, stderr io.Writer) {
	check := url.Values{"check": {"1"}}
	for _, k := range []string{"version", "title", "summary"} {
		if v := q.Get(k); v != "" {
			check.Set(k, v)
		}
	}
	data, _, err := DaemonGetRaw(path + "?" + check.Encode())
	if err != nil {
		return
	}
	var view struct {
		Assignees []struct {
			Name string `json:"name"`
			Role string `json:"role"`
		} `json:"assignees"`
	}
	if json.Unmarshal(data, &view) != nil {
		return
	}
	for _, a := range view.Assignees {
		if a.Role != "" && !slices.ContainsFunc(q["assignee"], func(m string) bool { return strings.HasPrefix(m, a.Name+"=") }) {
			fmt.Fprintf(stderr, "Agent %s is no longer named: its steps ask for role %s, or for the profile they already have.\n", shown(a.Name), shown(a.Role))
		}
	}
}

// warnWorkflowProblems warns that an export fails validation. The export
// still succeeds: sharing an unfinished workflow is allowed, as saving one is.
func warnWorkflowProblems(stderr io.Writer, archive *pwf.Archive) {
	if len(archive.Problems) == 0 {
		return
	}
	fmt.Fprintln(stderr, "Warning: the workflow fails validation. It was exported as it is, but it cannot run until these are fixed:")
	renderProblems(stderr, archive.Problems)
}

// warnWorkflowPaths warns about paths in an export. Like a validation problem
// it does not stop the export; the author decides whether to fix them.
func warnWorkflowPaths(stderr io.Writer, paths []pwf.PathFinding) {
	if len(paths) == 0 {
		return
	}
	fmt.Fprintln(stderr, "Warning: the workflow carries paths from this machine. It was exported with them; replace them with a param or a relative path before sharing:")
	renderPaths(stderr, paths)
}

// renderPaths lists machine-specific paths. They come from the file, so each
// goes through shown.
func renderPaths(out io.Writer, paths []pwf.PathFinding) {
	for _, p := range paths {
		why := "may not exist elsewhere"
		if p.Kind == pwf.PathHome {
			why = "reveals a username"
		}
		fmt.Fprintf(out, "  %s: %s (%s)\n", shown(p.Where), shown(p.Path), why)
	}
}

// renderProblems lists what keeps a workflow from running as carried. Every
// string comes from the file (template ids, node paths), so each goes through
// shown.
func renderProblems(out io.Writer, problems []string) {
	for _, p := range problems {
		fmt.Fprintf(out, "  %s\n", shown(p))
	}
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
		Use:         "inspect",
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
		problems := a.Problems
		if problems == nil {
			problems = []string{}
		}
		paths := a.Paths
		if paths == nil {
			paths = []pwf.PathFinding{}
		}
		_ = enc.Encode(map[string]any{"workflow": a.Workflow, "signatures": sigs, "problems": problems, "paths": paths})
		return rcOK
	}
	renderWorkflow(stdout, a.Workflow, len(a.Workflow.Templates))
	if len(a.Problems) > 0 {
		fmt.Fprintln(stdout, "Fails validation (it imports, but cannot run until fixed):")
		renderProblems(stdout, a.Problems)
	}
	if len(a.Paths) > 0 {
		fmt.Fprintln(stdout, "Paths from the machine it was exported on:")
		renderPaths(stdout, a.Paths)
	}
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
	RootID     string            `json:"rootId"`
	Problems   []string          `json:"problems"`
	Paths      []pwf.PathFinding `json:"paths"`
	Signatures []struct {
		KeyID  string `json:"keyId"`
		Status string `json:"status"`
		Trust  string `json:"trust"`
	} `json:"signatures"`
	Templates []struct {
		Ref        string `json:"ref"`
		ID         string `json:"id"`
		ImportRef  string `json:"importRef"`
		ImportID   string `json:"importId"`
		Head       bool   `json:"head"`
		Status     string `json:"status"`
		CurrentRef string `json:"currentRef"`
	} `json:"templates"`
	RoleMap []struct {
		Name   string   `json:"name"`
		Steps  []string `json:"steps"`
		Target string   `json:"target"`
		Mapped bool     `json:"mapped"`
		Known  bool     `json:"known"`
	} `json:"roleMap"`
	Group        string `json:"group"`
	Pinned       bool   `json:"pinned"`
	Refused      string `json:"refused"`
	Updates      int    `json:"updates"`
	Applied      bool   `json:"applied"`
	ReviewDigest string `json:"reviewDigest"`
}

type processTemplatesImportParams struct {
	File     string   `pos:"true" help:"Path to a .pwf file ('-' reads stdin)."`
	Yes      bool     `long:"yes" short:"y" optional:"true" help:"Import after showing the review instead of stopping at the preview."`
	Replace  bool     `long:"replace" optional:"true" help:"Make the imported versions current where this node already has the template with different content."`
	Rename   bool     `long:"rename" optional:"true" help:"Import a template whose id this node already uses with different content under a new id (<id>-2, ...) instead of replacing it; references inside the workflow follow."`
	MapRole  []string `long:"map-role" optional:"true" help:"Use a local role for one the workflow asks for, as <workflow role>=<local role> (repeatable). Rewrites the imported steps' role and fallback profile."`
	Group    string   `long:"group" optional:"true" help:"Pin the imported process to this group's saved processes."`
	AskHuman string   `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (for example 30s). Capped at 300s; timeout means deny."`
}

func processTemplatesImportCmd() *cobra.Command {
	return boa.CmdT[processTemplatesImportParams]{
		Use:         "import",
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
	choices, err := workflowImportChoices(p)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	var view workflowImportJSON
	path := "/v1/process/import"
	if len(choices) > 0 {
		path += "?" + choices.Encode()
	}
	if err := DaemonPostRawWithOptions(path, pwf.MediaType, data, nil, &view, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	renderImportReview(stdout, &view)
	if view.Refused != "" {
		if p.Yes {
			fmt.Fprintf(stderr, "Error: %s\n", view.Refused)
			return rcInvalidArg
		}
		fmt.Fprintf(stdout, "Refused here: %s\n", view.Refused)
		return rcOK
	}
	if !p.Yes {
		fmt.Fprintln(stdout, "Preview only; nothing was imported. Re-run with --yes to import.")
		return rcOK
	}
	if view.Updates > 0 && !p.Replace {
		fmt.Fprintf(stderr, "Error: %d template(s) already exist here with different content; re-run with --replace to make the imported versions current, or --rename to import them under new ids.\n", view.Updates)
		return rcInvalidArg
	}
	choices.Set("apply", "1")
	choices.Set("review", view.ReviewDigest)
	if p.Replace {
		choices.Set("replace", "1")
	}
	path = "/v1/process/import?" + choices.Encode()
	if err := DaemonPostRawWithOptions(path, pwf.MediaType, data, nil, &view, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Imported %q. Start it with: tclaude process run %s\n", view.Title, shown(view.RootID))
	if view.Pinned {
		fmt.Fprintf(stdout, "Pinned to group %s.\n", shown(view.Group))
	}
	return rcOK
}

// workflowImportChoices are the importer's choices as the import's query.
// The daemon applies them to the preview and the apply alike, so the review
// shows exactly what an apply stores.
func workflowImportChoices(p *processTemplatesImportParams) (url.Values, error) {
	q := url.Values{}
	for _, m := range p.MapRole {
		from, to, ok := strings.Cut(m, "=")
		if !ok || strings.TrimSpace(from) == "" || strings.TrimSpace(to) == "" {
			return nil, fmt.Errorf("--map-role %q: want <workflow role>=<local role>", m)
		}
		q.Add("map", strings.TrimSpace(from)+"="+strings.TrimSpace(to))
	}
	if p.Rename {
		q.Set("rename", "1")
	}
	if g := strings.TrimSpace(p.Group); g != "" {
		q.Set("group", g)
	}
	return q, nil
}

// renderImportChanges prints the role mapping and what importing stores
// here, renamed and rewritten templates included.
func renderImportChanges(out io.Writer, view *workflowImportJSON) {
	if len(view.RoleMap) > 0 {
		fmt.Fprintln(out, "Roles here:")
		for _, r := range view.RoleMap {
			line := "  " + shown(r.Name)
			if r.Mapped {
				line += " -> " + shown(r.Target)
			}
			if !r.Known {
				line += " (nothing here has this role or profile yet; map it with --map-role " + shown(r.Name) + "=<local>)"
			}
			fmt.Fprintln(out, line)
		}
	}
	fmt.Fprintln(out, "Templates:")
	for _, t := range view.Templates {
		line := fmt.Sprintf("  %-10s %s", shown(t.Status), shown(t.Ref))
		switch {
		case t.ImportID != "" && t.ImportID != t.ID:
			line += " as " + shown(t.ImportRef) + " (renamed)"
		case t.ImportRef != "":
			line += " as " + shown(t.ImportRef) + " (roles mapped)"
		}
		if t.CurrentRef != "" {
			line += " (here now: " + shown(t.CurrentRef) + "; --rename imports it under a new id instead)"
		}
		fmt.Fprintln(out, line)
	}
	if view.Group != "" {
		fmt.Fprintf(out, "Pins the process to group %s.\n", shown(view.Group))
	}
}

// renderImportReview prints what a person reviews before importing: who signed
// the workflow, what it needs and every command it would run, and what
// importing changes here.
func renderImportReview(out io.Writer, view *workflowImportJSON) {
	renderWorkflow(out, view.Workflow, len(view.Templates))
	if len(view.Signatures) == 0 {
		fmt.Fprintln(out, "Signature: none (unverified)")
	}
	for _, s := range view.Signatures {
		fmt.Fprintf(out, "Signature: %s, key %s (%s)\n", shown(s.Status), shown(s.KeyID), shown(s.Trust))
	}
	if len(view.Problems) > 0 {
		fmt.Fprintln(out, "Fails validation (it imports, but cannot run until fixed here):")
		renderProblems(out, view.Problems)
	}
	if len(view.Paths) > 0 {
		fmt.Fprintln(out, "Paths from the machine it was exported on (steps using them may need changing here):")
		renderPaths(out, view.Paths)
	}
	renderImportChanges(out, view)
}

// ImportReview is a workflow import's review for an approval popup.
type ImportReview struct {
	// Summary is what a person must not miss, for the top of the popup, where
	// no cut can reach it: the signer, how many commands, what changes here.
	Summary string
	// Full is the review the CLI prints.
	Full string
	// Refused says why this node would refuse the file.
	Refused string
}

// WorkflowImportReview renders the daemon's import preview as the CLI does, so
// the human approving an import sees what the agent asking for it sees.
func WorkflowImportReview(preview []byte) (ImportReview, error) {
	var view workflowImportJSON
	if err := json.Unmarshal(preview, &view); err != nil {
		return ImportReview{}, err
	}
	var full, summary strings.Builder
	renderImportReview(&full, &view)
	switch {
	case len(view.Signatures) == 0:
		fmt.Fprintln(&summary, "Signature: none (unverified)")
	case len(view.Signatures) == 1:
		s := view.Signatures[0]
		fmt.Fprintf(&summary, "Signature: %s, key %s (%s)\n", shown(s.Status), shown(s.KeyID), shown(s.Trust))
	default:
		fmt.Fprintf(&summary, "Signatures: %d, see below\n", len(view.Signatures))
	}
	fmt.Fprintf(&summary, "Commands it can run: %d\n", len(view.Programs))
	changes := map[string]int{}
	for _, t := range view.Templates {
		changes[t.Status]++
	}
	var counts []string
	for _, status := range slices.Sorted(maps.Keys(changes)) {
		counts = append(counts, fmt.Sprintf("%d %s", changes[status], shown(status)))
	}
	fmt.Fprintf(&summary, "Templates: %s\n", strings.Join(counts, ", "))
	if view.Group != "" {
		fmt.Fprintf(&summary, "Pins the process to group %s.\n", shown(view.Group))
	}
	return ImportReview{Summary: summary.String(), Full: full.String(), Refused: shown(view.Refused)}, nil
}

// shown makes a file-controlled string safe to print in a review: anything
// with control characters (which could redraw the terminal and fake what the
// review says) or format characters (bidi overrides, which make a browser show
// a command other than the one that runs) is shown quoted, escapes visible.
func shown(v string) string {
	for _, r := range v {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
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
func renderWorkflow(out io.Writer, wf pwf.Workflow, templates int) {
	fmt.Fprintf(out, "Workflow: %s\n", shown(wf.Title))
	if wf.Summary != "" {
		fmt.Fprintf(out, "  %s\n", shown(wf.Summary))
	}
	if wf.Author != "" {
		fmt.Fprintf(out, "Author: %s (as the file says)\n", shown(wf.Author))
	}
	if wf.Licence != "" {
		fmt.Fprintf(out, "Licence: %s\n", shown(wf.Licence))
	}
	fmt.Fprintf(out, "Root: %s (%d template(s))\n", shown(wf.Root), templates)
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
