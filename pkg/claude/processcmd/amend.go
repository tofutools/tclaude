package processcmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/common"
)

type processAmendParams struct {
	RunID    string `pos:"true" help:"Process run id"`
	File     string `long:"file" optional:"true" help:"The amended template (YAML), from --print and edited"`
	Print    bool   `long:"print" optional:"true" help:"Print the run's own template as it stands, to edit"`
	List     bool   `long:"list" optional:"true" help:"List the run's amendments"`
	SaveBack int64  `long:"save-back" optional:"true" help:"Save this applied amendment (the run's latest) as its template's next version (process.templates.manage)"`
	Note     string `long:"note" optional:"true" help:"Why, shown to the operator and recorded in the run log"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout"`
}

func processAmendCmd() *cobra.Command {
	return boa.CmdT[processAmendParams]{
		Use:   "amend",
		Short: "Change one run's own copy of its template, where the run has not been yet",
		Long: "Amends a running process run: the run gets its own copy of its template, changed only in steps that " +
			"have not started (a step being worked on may still be rerouted, to insert a step after it). Print the " +
			"copy with --print, edit it, and submit it with --file. With process.runs.manage or " +
			"process.templates.manage it applies at once; the run's overseer proposes it, and the operator applies " +
			"or rejects it from a card that says what it changes. If the run moves on before then, applying it is " +
			"refused and it has to be proposed again. Programs and subprocesses cannot be added or changed. The " +
			"template itself is untouched until an applied amendment is saved back with --save-back " +
			"(process.templates.manage); list the run's amendments with --list.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *processAmendParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *processAmendParams, _ *cobra.Command, _ []string) {
			exitProcessRuntime(runProcessAmend(p, os.Stdout, os.Stderr), os.Stderr)
		},
	}.ToCobra()
}

type processAmendmentJSON struct {
	ID           int64  `json:"id"`
	State        string `json:"state"`
	ProposedName string `json:"proposedName"`
	ProposedBy   string `json:"proposedBy"`
	Note         string `json:"note"`
	DecidedName  string `json:"decidedName"`
	DecisionNote string `json:"decisionNote"`
	SavedRef     string `json:"savedTemplateRef"`
	CreatedAt    string `json:"createdAt"`
	Summary      struct {
		Added    []string `json:"added"`
		Removed  []string `json:"removed"`
		Changed  []string `json:"changed"`
		Rerouted []string `json:"rerouted"`
		Settings bool     `json:"settings"`
	} `json:"summary"`
}

func (a processAmendmentJSON) summaryText() string {
	var parts []string
	for _, p := range []struct {
		label string
		ids   []string
	}{{"adds", a.Summary.Added}, {"removes", a.Summary.Removed}, {"changes", a.Summary.Changed}, {"reroutes", a.Summary.Rerouted}} {
		if len(p.ids) > 0 {
			parts = append(parts, p.label+" "+strings.Join(p.ids, ", "))
		}
	}
	if a.Summary.Settings {
		parts = append(parts, "changes template settings")
	}
	return strings.Join(parts, "; ")
}

func runProcessAmend(p *processAmendParams, stdout, stderr io.Writer) error {
	if err := requireProcessRuntime(stderr); err != nil {
		return err
	}
	runID := strings.TrimSpace(p.RunID)
	modes := 0
	for _, on := range []bool{p.File != "", p.Print, p.List, p.SaveBack > 0} {
		if on {
			modes++
		}
	}
	if runID == "" || modes != 1 {
		return fmt.Errorf("a run id and exactly one of --file, --print, --list or --save-back are required")
	}
	ask, err := agent.ParseAskHuman(p.AskHuman)
	if err != nil {
		return err
	}
	opts := agent.DaemonOpts{AskHuman: ask, RetryOutput: stderr}
	base := "/v1/process/runs/" + url.PathEscape(runID)
	switch {
	case p.Print:
		var response struct {
			Template json.RawMessage `json:"template"`
		}
		if err := agent.DaemonRequest(http.MethodGet, base+"/template", nil, &response, opts); err != nil {
			return err
		}
		var tmpl model.Template
		if err := json.Unmarshal(response.Template, &tmpl); err != nil {
			return err
		}
		out, err := yaml.Marshal(&tmpl)
		if err != nil {
			return err
		}
		_, err = stdout.Write(out)
		return err
	case p.SaveBack > 0:
		var response struct {
			Ref string `json:"ref"`
		}
		if err := agent.DaemonRequest(http.MethodPost, fmt.Sprintf("%s/amendments/%d/save-back", base, p.SaveBack), map[string]any{}, &response, opts); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Saved amendment #%d of %s back to its template as %s. Runs started from now on follow it.\n", p.SaveBack, runID, response.Ref)
		return nil
	case p.List:
		var response struct {
			Amendments []processAmendmentJSON `json:"amendments"`
		}
		if err := agent.DaemonRequest(http.MethodGet, base+"/amendments", nil, &response, opts); err != nil {
			return err
		}
		if len(response.Amendments) == 0 {
			fmt.Fprintf(stdout, "Run %s has no amendments.\n", runID)
		}
		for _, a := range response.Amendments {
			fmt.Fprintf(stdout, "#%d  %-8s  %s  by %s: %s\n", a.ID, a.State, a.CreatedAt, firstNonEmpty(a.ProposedName, a.ProposedBy), a.summaryText())
			if a.Note != "" {
				fmt.Fprintf(stdout, "    why: %s\n", a.Note)
			}
			if a.SavedRef != "" {
				fmt.Fprintf(stdout, "    saved back as %s\n", a.SavedRef)
			}
			if a.DecisionNote != "" {
				fmt.Fprintf(stdout, "    %s: %s\n", a.State, a.DecisionNote)
			}
		}
		return nil
	}
	data, err := os.ReadFile(p.File)
	if err != nil {
		return err
	}
	parsed, err := model.ParseAuthoring(data)
	if err != nil {
		return err
	}
	if errs := parsed.Diagnostics.Errors(); len(errs) > 0 {
		return fmt.Errorf("the amended template does not validate: %v", errs)
	}
	template, err := json.Marshal(parsed.Template)
	if err != nil {
		return err
	}
	var response struct {
		Amendment processAmendmentJSON `json:"amendment"`
		Run       processRunJSON       `json:"run"`
	}
	request := map[string]any{"template": json.RawMessage(template), "note": p.Note}
	if err := agent.DaemonRequest(http.MethodPost, base+"/amendments", request, &response, opts); err != nil {
		return err
	}
	a := response.Amendment
	if a.State == "applied" {
		fmt.Fprintf(stdout, "Amended process run %s (#%d): %s.\n", runID, a.ID, a.summaryText())
		printProcessRun(stdout, &response.Run)
	} else {
		fmt.Fprintf(stdout, "Proposed amendment #%d to %s: %s. The operator applies or rejects it.\n", a.ID, runID, a.summaryText())
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
