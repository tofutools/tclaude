package workflowcli

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/workflow"
)

// The embedded "implement-microservice" example template is the fixture for
// the client-side template verbs — no daemon, no filesystem setup needed.
const exampleRef = "example:implement-microservice"

func TestRunShow_HumanOutput(t *testing.T) {
	var out, errBuf bytes.Buffer
	rc := runShow(&showParams{Ref: exampleRef}, &out, &errBuf)
	if rc != rcOK {
		t.Fatalf("runShow rc = %d, want %d (stderr: %s)", rc, rcOK, errBuf.String())
	}
	s := out.String()
	for _, want := range []string{
		exampleRef,     // header
		"params:",      //
		"service_name", // required param
		"env",          // optional param
		"nodes:",       // node summary section
		"plan",         // a node id
		"flow:",        // mermaid section
		"flowchart TD", // the raw chart
	} {
		if !strings.Contains(s, want) {
			t.Errorf("show output missing %q\n---\n%s", want, s)
		}
	}
}

func TestRunShow_JSON(t *testing.T) {
	var out, errBuf bytes.Buffer
	rc := runShow(&showParams{Ref: exampleRef, JSON: true}, &out, &errBuf)
	if rc != rcOK {
		t.Fatalf("runShow --json rc = %d, want %d (stderr: %s)", rc, rcOK, errBuf.String())
	}
	var got showJSON
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("show --json produced invalid JSON: %v\n%s", err, out.String())
	}
	if got.Ref != exampleRef {
		t.Errorf("Ref = %q, want %q", got.Ref, exampleRef)
	}
	if got.Name != "implement-microservice" {
		t.Errorf("Name = %q, want implement-microservice", got.Name)
	}
	if got.Source != string(workflow.SourceExample) {
		t.Errorf("Source = %q, want %q", got.Source, workflow.SourceExample)
	}
	if len(got.Nodes) != 6 {
		t.Errorf("len(Nodes) = %d, want 6", len(got.Nodes))
	}
	if got.Mermaid == "" {
		t.Error("Mermaid is empty")
	}
	if !slices.Contains(got.Entry, "plan") {
		t.Errorf("Entry = %v, want it to contain plan", got.Entry)
	}

	// Params: service_name required, env optional with a default.
	params := map[string]showParamJSON{}
	for _, p := range got.Params {
		params[p.Name] = p
	}
	if p, ok := params["service_name"]; !ok || !p.Required {
		t.Errorf("service_name param = %+v (ok=%v), want required", p, ok)
	}
	if p, ok := params["env"]; !ok || p.Required || p.Default != "staging" {
		t.Errorf("env param = %+v (ok=%v), want optional default=staging", p, ok)
	}

	// Nodes carry their ids; the node summary must surface every node.
	ids := map[string]bool{}
	for _, n := range got.Nodes {
		ids[n.ID] = true
	}
	for _, want := range []string{"plan", "implement", "test", "review", "deploy", "done"} {
		if !ids[want] {
			t.Errorf("node %q missing from JSON nodes", want)
		}
	}
}

func TestRunShow_UnknownRef(t *testing.T) {
	var out, errBuf bytes.Buffer
	rc := runShow(&showParams{Ref: "example:does-not-exist"}, &out, &errBuf)
	if rc != rcNotFound {
		t.Fatalf("runShow unknown rc = %d, want %d", rc, rcNotFound)
	}
	if errBuf.Len() == 0 {
		t.Error("expected an error message on stderr for an unknown ref")
	}
}

// A malformed ref (path traversal, dotted, empty) is an invalid argument, not a
// missing template — it must exit rcInvalidArg, distinct from the rcNotFound a
// well-formed-but-absent ref gets above.
func TestRunShow_InvalidRef(t *testing.T) {
	for _, ref := range []string{"../escape", "user:../etc/passwd", "", ".", "example:.."} {
		var out, errBuf bytes.Buffer
		rc := runShow(&showParams{Ref: ref}, &out, &errBuf)
		if rc != rcInvalidArg {
			t.Errorf("runShow(%q) rc=%d, want rcInvalidArg(%d)", ref, rc, rcInvalidArg)
		}
	}
}

// renderTemplateList must flag a template that failed to load (Err) or carries
// topology warnings with a ⚠ marker, rather than hiding the problem.
func TestRenderTemplateList_FlagsErrorsAndWarnings(t *testing.T) {
	var buf bytes.Buffer
	renderTemplateList([]workflow.ListEntry{
		{Ref: "user:broken", Name: "broken", Source: workflow.SourceUser, Err: "bad yaml"},
		{Ref: "user:smelly", Name: "smelly", Source: workflow.SourceUser, NodeCount: 3, Warnings: []string{"loop"}, Description: "smelly"},
	}, &buf)
	s := buf.String()
	if !strings.Contains(s, "⚠") {
		t.Errorf("expected a ⚠ flag for the broken/warned templates\n%s", s)
	}
	if !strings.Contains(s, "bad yaml") {
		t.Errorf("a failed-to-load template should surface its error\n%s", s)
	}
}

func TestRunTemplates_JSONIncludesExample(t *testing.T) {
	var out, errBuf bytes.Buffer
	rc := runTemplates(&templatesParams{JSON: true}, &out, &errBuf)
	if rc != rcOK {
		t.Fatalf("runTemplates --json rc = %d, want %d", rc, rcOK)
	}
	var entries []workflow.ListEntry
	if err := json.Unmarshal(out.Bytes(), &entries); err != nil {
		t.Fatalf("templates --json produced invalid JSON: %v\n%s", err, out.String())
	}
	found := false
	for _, e := range entries {
		if e.Ref == exampleRef {
			found = true
			if e.NodeCount != 6 {
				t.Errorf("example NodeCount = %d, want 6", e.NodeCount)
			}
		}
	}
	if !found {
		t.Errorf("templates --json did not include %q; got %d entries", exampleRef, len(entries))
	}
}
