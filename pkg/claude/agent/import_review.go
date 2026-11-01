package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf16"

	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/configbundle"
)

// A review's summary stays at the top of a popup, where no cut can reach it, so
// the file must not be able to make it long either.
const (
	reviewSummaryItems = 20
	reviewSummaryChars = 160
)

// importSignature is one checked signature, as the daemon reports it.
type importSignature struct {
	KeyID  string `json:"keyId"`
	Status string `json:"status"`
	Trust  string `json:"trust"`
}

type carriedWorkflow struct {
	Key  string `json:"key"`
	Root string `json:"root"`
}

// agentImportView is the part of an agent import preview that is read here. The
// whole preview is printed as JSON.
type agentImportView struct {
	Agent struct {
		Name    string `json:"name"`
		Role    string `json:"role"`
		Harness string `json:"harness"`
	} `json:"agent"`
	Cwd              string                     `json:"cwd"`
	Group            string                     `json:"group"`
	History          bool                       `json:"history"`
	Signatures       []importSignature          `json:"signatures"`
	Findings         []agentbundle.Finding      `json:"findings"`
	Unresolved       []configbundle.Placeholder `json:"unresolved"`
	CarriedWorkflows []carriedWorkflow          `json:"carried_workflows"`
	Permissions      []struct {
		Decision string `json:"decision"`
	} `json:"permissions"`
	ManagedScripts bool `json:"managed_script_approval_required"`
	importOutcome
}

// renderAgentImport prints what a person reviews of an agent import: the whole
// resolved definition, then what it leaves for them to settle.
func renderAgentImport(out io.Writer, result []byte, applied bool) {
	var value any
	_ = json.Unmarshal(result, &value)
	pretty, _ := json.MarshalIndent(value, "", "  ")
	fmt.Fprintln(out, escapeInvisible(string(pretty)))
	var view agentImportView
	_ = json.Unmarshal(result, &view)
	if applied {
		view.print(out)
	} else {
		view.Confirm.print(out)
	}
	for _, p := range view.Unresolved {
		fmt.Fprintf(out, "Unresolved %s %s (original: %q): --set %s=value or --keep-paths\n", shown(p.Item), shown(p.Field), p.Original, shown(p.Name))
	}
	for _, w := range view.CarriedWorkflows {
		fmt.Fprintf(out, "Also carries workflow %s, not imported here: review it with process-templates import --workflow %s\n", shown(w.Root), shown(w.Key))
	}
}

// AgentImportReview renders the daemon's agent import preview as the CLI does,
// for an approval popup.
func AgentImportReview(preview []byte) (ImportReview, error) {
	var view agentImportView
	if err := json.Unmarshal(preview, &view); err != nil {
		return ImportReview{}, err
	}
	var full, summary strings.Builder
	renderAgentImport(&full, preview, false)
	fmt.Fprintf(&summary, "Agent: %s, on %s\n", clipped(view.Agent.Name), clipped(view.Agent.Harness))
	if view.Agent.Role != "" {
		fmt.Fprintf(&summary, "Role: %s\n", clipped(view.Agent.Role))
	}
	if view.Group != "" {
		fmt.Fprintf(&summary, "Group here: %s\n", clipped(view.Group))
	}
	if view.Cwd != "" {
		fmt.Fprintf(&summary, "Works in: %s\n", clipped(view.Cwd))
	}
	if view.History {
		fmt.Fprintln(&summary, "Lands with its conversation history.")
	} else {
		fmt.Fprintln(&summary, "Lands without conversation history.")
	}
	summarizeSignatures(&summary, view.Signatures)
	summarizeFindings(&summary, len(view.Findings), func(i int) (string, int) { return view.Findings[i].Kind, view.Findings[i].Count })
	summarizeConfirm(&summary, view.Confirm)
	if n := len(view.Permissions); n > 0 {
		fmt.Fprintf(&summary, "Carries %d permission row(s), decided below.\n", n)
	}
	if view.ManagedScripts {
		fmt.Fprintln(&summary, "Ships managed instructions with code, which run only with their own approval. See below.")
	}
	summarizeWorkflows(&summary, view.CarriedWorkflows)
	return ImportReview{Summary: summary.String(), Full: full.String()}, nil
}

func summarizeSignatures(out io.Writer, signatures []importSignature) {
	switch len(signatures) {
	case 0:
		fmt.Fprintln(out, "Signature: none (unverified)")
	case 1:
		s := signatures[0]
		fmt.Fprintf(out, "Signature: %s, key %s (%s)\n", shown(s.Status), shown(s.KeyID), shown(s.Trust))
	default:
		fmt.Fprintf(out, "Signatures: %d, see below\n", len(signatures))
	}
}

func summarizeFindings(out io.Writer, count int, finding func(int) (string, int)) {
	if count == 0 {
		return
	}
	fmt.Fprintln(out, "Suspected credentials in the file:")
	for i := range min(count, reviewSummaryItems) {
		kind, n := finding(i)
		fmt.Fprintf(out, "  %s: %d\n", clipped(kind), n)
	}
	if count > reviewSummaryItems {
		fmt.Fprintf(out, "  and %d more kinds\n", count-reviewSummaryItems)
	}
}

func summarizeConfirm(out io.Writer, c *importConfirm) {
	if c == nil || len(c.Items) == 0 {
		return
	}
	if c.CanKeep {
		fmt.Fprintf(out, "Elevated items, %d:\n", len(c.Items))
	} else {
		fmt.Fprintf(out, "Elevated items, %d, none of which this caller can keep:\n", len(c.Items))
	}
	for _, item := range c.Items[:min(len(c.Items), reviewSummaryItems)] {
		fmt.Fprintf(out, "  %s\n", clipped(item.Label))
	}
	if len(c.Items) > reviewSummaryItems {
		fmt.Fprintf(out, "  and %d more\n", len(c.Items)-reviewSummaryItems)
	}
}

func summarizeWorkflows(out io.Writer, workflows []carriedWorkflow) {
	if n := len(workflows); n > 0 {
		fmt.Fprintf(out, "The file also ships %d workflow(s); this import stores none of them.\n", n)
	}
}

// clipped is shown for the summary: the same string, of a bounded length.
func clipped(v string) string {
	if r := []rune(v); len(r) > reviewSummaryChars {
		v = string(r[:reviewSummaryChars]) + "…"
	}
	return shown(v)
}

// escapeInvisible writes the format characters JSON encoding lets through as
// escapes, so a file cannot reorder or hide what its strings say.
func escapeInvisible(s string) string {
	var b strings.Builder
	for _, r := range s {
		if !unicode.Is(unicode.Cf, r) {
			b.WriteRune(r)
			continue
		}
		for _, unit := range utf16.Encode([]rune{r}) {
			fmt.Fprintf(&b, "\\u%04x", unit)
		}
	}
	return b.String()
}
