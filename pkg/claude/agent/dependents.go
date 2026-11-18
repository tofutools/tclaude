package agent

import (
	"fmt"
	"io"
	"net/url"
	"strings"
)

// setKeepDependents adds the opt-out to a retire or delete request.
func setKeepDependents(q url.Values, keep bool) {
	if keep {
		q.Set("keep_dependents", "1")
	}
}

// dependentsReply is the daemon's account of what a retire or delete did to
// what refers to the agent (agentd's agentCascade).
type dependentsReply struct {
	Kept    bool `json:"kept"`
	Removed []struct {
		Kind string `json:"kind"`
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Via  string `json:"via"`
	} `json:"removed"`
	RunsCancelled []struct {
		ID          string `json:"id"`
		TemplateRef string `json:"template_ref"`
	} `json:"runs_cancelled"`
	StepsCleared []struct {
		Template string `json:"template"`
		Node     string `json:"node"`
		Slot     string `json:"slot"`
	} `json:"steps_cleared"`
	Warnings []string `json:"warnings"`
}

var dependentKindWords = map[string]string{
	"cron":           "cron job",
	"standing_order": "standing order",
	"trigger_rule":   "automation",
}

// print writes one indented line per thing the cascade touched.
func (d *dependentsReply) print(w io.Writer) {
	if d == nil {
		return
	}
	if d.Kept {
		fmt.Fprintln(w, "  dependents: kept (--keep-dependents)")
	}
	for _, dep := range d.Removed {
		name := dep.Name
		if name == "" {
			name = fmt.Sprintf("#%d", dep.ID)
		}
		via := "its own"
		if dep.Via == "target" {
			via = "addressed to it"
		}
		fmt.Fprintf(w, "  removed %s %s (%s)\n", dependentKindWords[dep.Kind], name, via)
	}
	for _, run := range d.RunsCancelled {
		fmt.Fprintf(w, "  cancelled run %s (%s)\n", run.ID, run.TemplateRef)
	}
	for _, step := range d.StepsCleared {
		fmt.Fprintf(w, "  cleared template %s step %s (%s)\n", step.Template, step.Node, step.Slot)
	}
	for _, warning := range d.Warnings {
		fmt.Fprintf(w, "  warning: %s\n", strings.TrimSpace(warning))
	}
}
