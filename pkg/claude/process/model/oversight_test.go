package model

import (
	"strings"
	"testing"
)

func oversightTemplate(header, nodeExtra string) string {
	return `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: watched
` + header + `
start: begin
nodes:
  begin:
    type: start
    next: { next: work }
  work:
    type: task
` + nodeExtra + `
    performer: { kind: agent, lead: true, prompt: do it }
    next: { next: done }
  done:
    type: end
    result: success
`
}

// A template names its runs' overseer (lead, starter, operator or
// role:<role>) and how long its steps may stay stuck (patience, on the
// template or a step); both round-trip, and a bad value is refused.
func TestTemplateOverseerAndPatience(t *testing.T) {
	parsed, err := Parse([]byte(oversightTemplate("overseer: role:qa\npatience: 45m", "    patience: 2h")))
	if err != nil || parsed.Diagnostics.HasErrors() {
		t.Fatalf("parse: %v %v", err, parsed.Diagnostics)
	}
	if parsed.Template.Overseer != "role:qa" || parsed.Template.Patience != "45m" || parsed.Template.Nodes["work"].Patience != "2h" {
		t.Fatalf("fields not read: %+v / %+v", parsed.Template.Overseer, parsed.Template.Nodes["work"])
	}
	out, err := CanonicalYAML(parsed.Template)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	for _, want := range []string{"overseer: role:qa", "patience: 45m", "patience: 2h"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("canonical YAML lost %q:\n%s", want, out)
		}
	}
	for _, bad := range []struct{ header, node, code string }{
		{"overseer: whoever", "", "invalid_overseer"},
		{"overseer: role:Not A Role", "", "invalid_overseer"},
		{"patience: soon", "", "invalid_duration"},
		{"", "    patience: -5m", "invalid_duration"},
	} {
		parsed, err := Parse([]byte(oversightTemplate(bad.header, bad.node)))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		found := false
		for _, d := range parsed.Diagnostics.Errors() {
			found = found || d.Code == bad.code
		}
		if !found {
			t.Errorf("%q %q: want %s, got %v", bad.header, bad.node, bad.code, parsed.Diagnostics)
		}
	}
}
