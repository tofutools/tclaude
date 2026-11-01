package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

type guideFillParams struct {
	Form string   `long:"form" help:"The open form to fill: spawn or config"`
	Set  []string `long:"set" collection:"array" help:"A field and its value, as id=value, using the ids guide capture --form prints; repeat it, in order"`
	JSON bool     `long:"json" help:"Output JSON"`
}

type guideFillField struct {
	Field  string   `json:"field"`
	Status string   `json:"status"`
	Widens string   `json:"widens,omitempty"`
	Moved  []string `json:"moved,omitempty"`
}

func guideFillCmd() *cobra.Command {
	return boa.CmdT[guideFillParams]{
		Use:   "fill",
		Short: "Propose values in the operator's open form",
		Long: "Puts values into the operator's open spawn or Config form, through the form's own validation. It never submits " +
			"and presses nothing. Filled fields stay highlighted and can be undone in one click, and a change that widens access " +
			"shows a warning with your name. It refuses a password or marked field and any field the operator edited. Each field " +
			"reports what became of it. Needs dashboard.forms.fill.\n\n" +
			"A select takes an option's label or value, and a checkbox true or false. Fields apply in order, so set a harness before its model.\n\n" +
			"Example: tclaude agent guide fill --form spawn --set agent-spawn-name=reviewer --set agent-spawn-harness=codex",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *guideFillParams, _ *cobra.Command, _ []string) {
			os.Exit(runGuideFill(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runGuideFill(p *guideFillParams, stdout, stderr io.Writer) int {
	fields := make([]map[string]string, 0, len(p.Set))
	for _, set := range p.Set {
		field, value, ok := strings.Cut(set, "=")
		if !ok || field == "" {
			fmt.Fprintf(stderr, "Error: --set takes id=value, got %q\n", set)
			return rcInvalidArg
		}
		fields = append(fields, map[string]string{"field": field, "value": value})
	}
	if len(fields) == 0 {
		fmt.Fprintln(stderr, "Error: name at least one field with --set id=value")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp struct {
		Fields []guideFillField `json:"fields"`
	}
	body := map[string]any{"form": p.Form, "fields": fields}
	if err := DaemonRequest(http.MethodPost, "/v1/dashboard/fill", body, &resp, DaemonOpts{Timeout: guideCaptureTimeout}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		if de, ok := err.(*DaemonError); ok && de.Code == "permission" {
			fmt.Fprintln(stderr, "Filling exists, but you do not hold dashboard.forms.fill. Ask for it: tclaude agent sudo request dashboard.forms.fill")
		}
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(resp)
		return rcOK
	}
	for _, field := range resp.Fields {
		line := fmt.Sprintf("%-10s %s", field.Status, field.Field)
		if field.Widens != "" {
			line += " (the operator sees a warning: " + field.Widens + ")"
		}
		fmt.Fprintln(stdout, line)
		if len(field.Moved) > 0 {
			fmt.Fprintln(stdout, "           which also moved: "+strings.Join(field.Moved, ", "))
		}
	}
	return rcOK
}
