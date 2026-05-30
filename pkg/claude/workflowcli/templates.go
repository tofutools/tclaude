package workflowcli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/table"
	"github.com/tofutools/tclaude/pkg/claude/workflow"
	"github.com/tofutools/tclaude/pkg/common"
)

// Template discovery is a pure client-side read of the project / user / example
// sources — templates are plain files on disk, not DB rows, so these verbs hit
// no daemon. Project context is the caller's cwd (see projectDirs).

// --- templates ---

type templatesParams struct {
	JSON bool `long:"json" help:"Output JSON"`
}

func templatesCmd() *cobra.Command {
	return boa.CmdT[templatesParams]{
		Use:         "templates",
		Short:       "List discoverable workflow templates (project / user / example)",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *templatesParams, _ *cobra.Command, _ []string) {
			os.Exit(runTemplates(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runTemplates(p *templatesParams, stdout, stderr io.Writer) int {
	entries := workflow.List(projectDirs()...)
	if entries == nil {
		entries = []workflow.ListEntry{} // emit [] not null for the empty case
	}
	if p.JSON {
		return writeJSON(stdout, stderr, entries)
	}
	renderTemplateList(entries, stdout)
	return rcOK
}

// renderTemplateList renders the discoverable-templates table. Shared by the
// templates verb and (later) ls so the two surfaces stay identical. A template
// that failed to load is still listed, flagged with ⚠, so it's visible rather
// than silently missing.
func renderTemplateList(entries []workflow.ListEntry, w io.Writer) {
	if len(entries) == 0 {
		fmt.Fprintln(w, "(no workflow templates found in project, user, or example sources)")
		return
	}
	tbl := table.New(
		table.Column{Header: "REF", MinWidth: 10, Weight: 0.8, Truncate: true},
		table.Column{Header: "NAME", MinWidth: 8, Weight: 0.6, Truncate: true},
		table.Column{Header: "SOURCE", Width: 8},
		table.Column{Header: "NODES", Width: 5},
		table.Column{Header: "DESCRIPTION", MinWidth: 10, Weight: 1.4, Truncate: true},
	)
	tbl.SetTerminalWidth(table.GetTerminalWidth())
	for _, e := range entries {
		desc := e.Description
		nodes := strconv.Itoa(e.NodeCount)
		switch {
		case e.Err != "":
			desc = "⚠ " + e.Err
			nodes = "-"
		case len(e.Warnings) > 0:
			desc = "⚠ " + desc
		}
		tbl.AddRow(table.Row{Cells: []string{e.Ref, e.Name, string(e.Source), nodes, desc}})
	}
	fmt.Fprintln(w, tbl.Render())
}

// --- show ---

type showParams struct {
	Ref  string `pos:"true" help:"Template reference: a bare name, or project:/user:/example: qualified"`
	JSON bool   `long:"json" help:"Output JSON"`
}

func showCmd() *cobra.Command {
	return boa.CmdT[showParams]{
		Use:         "show",
		Short:       "Render a template: metadata, params, node summary, and mermaid chart",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *showParams, _ *cobra.Command, _ []string) {
			os.Exit(runShow(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runShow(p *showParams, stdout, stderr io.Writer) int {
	// A malformed ref (empty / dotted / path-separated name) is an invalid
	// argument, not a missing template — exit accordingly so agents can tell
	// "you typed a bad ref" from "no such template", matching the invalid_arg
	// vs not_found split agent.MapDaemonErrorToRC uses for the daemon verbs.
	if refLooksInvalid(p.Ref) {
		fmt.Fprintf(stderr, "Error: invalid template reference %q\n", p.Ref)
		return rcInvalidArg
	}
	tmpl, err := workflow.Resolve(p.Ref, projectDirs()...)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcNotFound
	}
	if p.JSON {
		return writeJSON(stdout, stderr, templateToJSON(tmpl))
	}
	renderTemplate(tmpl, stdout)
	return rcOK
}

// refLooksInvalid reports whether ref is malformed: an empty name, a dotted
// name, or one carrying path separators — the cases the workflow package
// rejects before any filesystem lookup. It mirrors that package's validRefName
// over the name part (after stripping a recognised source: prefix) so the CLI
// can return rcInvalidArg for these rather than collapsing them into
// rcNotFound.
func refLooksInvalid(ref string) bool {
	name := ref
	if src, rest, found := strings.Cut(ref, ":"); found {
		switch workflow.Source(src) {
		case workflow.SourceProject, workflow.SourceUser, workflow.SourceExample:
			name = rest
		}
	}
	name = strings.TrimSpace(name)
	return name == "" || name == "." || name == ".." ||
		strings.ContainsAny(name, `/\`) || strings.Contains(name, "..")
}

// --- rendering / JSON shaping ---

type showParamJSON struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
	Default  string `json:"default,omitempty"`
}

type showNodeJSON struct {
	ID              string   `json:"id"`
	Label           string   `json:"label"`
	ExecutorKind    string   `json:"executor_kind"`
	Agent           string   `json:"agent,omitempty"`
	VerifyKind      string   `json:"verify_kind,omitempty"`
	AllowedOutcomes []string `json:"allowed_outcomes,omitempty"`
}

type showJSON struct {
	Ref         string          `json:"ref"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Source      string          `json:"source"`
	Dir         string          `json:"dir,omitempty"`
	Entry       []string        `json:"entry"`
	Params      []showParamJSON `json:"params,omitempty"`
	Nodes       []showNodeJSON  `json:"nodes"`
	Mermaid     string          `json:"mermaid"`
	Warnings    []string        `json:"warnings,omitempty"`
}

func templateToJSON(t *workflow.Template) showJSON {
	// Entry and Nodes are non-omitempty contract fields: emit [] not null when
	// empty so JSON consumers can iterate them unconditionally.
	entry := t.Entry
	if entry == nil {
		entry = []string{}
	}
	out := showJSON{
		Ref:         t.Ref,
		Name:        t.Name,
		Description: t.Description,
		Source:      string(t.Source),
		Dir:         t.Dir,
		Entry:       entry,
		Mermaid:     t.Mermaid,
		Warnings:    t.Warnings,
		Nodes:       []showNodeJSON{},
	}
	for _, p := range t.Params {
		out.Params = append(out.Params, showParamJSON{
			Name: p.Name, Required: p.IsRequired(), Default: p.Default,
		})
	}
	for _, id := range sortedNodeIDs(t) {
		n := t.Nodes[id]
		nj := showNodeJSON{
			ID:              id,
			Label:           t.DisplayLabel(id),
			AllowedOutcomes: t.AllowedOutcomes(id),
		}
		if n != nil {
			nj.ExecutorKind = string(n.Executor.Kind)
			nj.Agent = n.Executor.Agent
			nj.VerifyKind = string(n.Verify.Kind)
		}
		out.Nodes = append(out.Nodes, nj)
	}
	return out
}

func renderTemplate(t *workflow.Template, w io.Writer) {
	fmt.Fprintf(w, "%s\n", t.Ref)
	if t.Name != "" && t.Name != t.Ref {
		fmt.Fprintf(w, "  name:    %s\n", t.Name)
	}
	if t.Description != "" {
		fmt.Fprintf(w, "  desc:    %s\n", t.Description)
	}
	fmt.Fprintf(w, "  source:  %s\n", t.Source)
	if t.Dir != "" {
		fmt.Fprintf(w, "  dir:     %s\n", t.Dir)
	}
	if len(t.Entry) > 0 {
		fmt.Fprintf(w, "  entry:   %s\n", strings.Join(t.Entry, ", "))
	}

	if len(t.Params) > 0 {
		fmt.Fprintln(w, "\nparams:")
		for _, p := range t.Params {
			req := "optional"
			if p.IsRequired() {
				req = "required"
			}
			line := fmt.Sprintf("  %s (%s)", p.Name, req)
			if p.Default != "" {
				line += fmt.Sprintf(", default=%s", p.Default)
			}
			fmt.Fprintln(w, line)
		}
	}

	if len(t.Warnings) > 0 {
		fmt.Fprintln(w, "\n⚠ warnings:")
		for _, warn := range t.Warnings {
			fmt.Fprintf(w, "  - %s\n", warn)
		}
	}

	fmt.Fprintln(w, "\nnodes:")
	tbl := table.New(
		table.Column{Header: "ID", MinWidth: 6, Weight: 0.6, Truncate: true},
		table.Column{Header: "LABEL", MinWidth: 8, Weight: 1.0, Truncate: true},
		table.Column{Header: "EXECUTOR", Width: 10},
		table.Column{Header: "VERIFY", Width: 8},
		table.Column{Header: "OUTCOMES", MinWidth: 8, Weight: 0.8, Truncate: true},
	)
	tbl.SetTerminalWidth(table.GetTerminalWidth())
	for _, id := range sortedNodeIDs(t) {
		n := t.Nodes[id]
		exec, verify := "", ""
		if n != nil {
			exec = string(n.Executor.Kind)
			if n.Executor.Agent != "" {
				exec += ":" + n.Executor.Agent
			}
			verify = string(n.Verify.Kind)
		}
		tbl.AddRow(table.Row{Cells: []string{
			id, t.DisplayLabel(id), exec, verify, strings.Join(t.AllowedOutcomes(id), ", "),
		}})
	}
	fmt.Fprintln(w, tbl.Render())

	if strings.TrimSpace(t.Mermaid) != "" {
		fmt.Fprintln(w, "\nflow:")
		fmt.Fprintln(w, t.Mermaid)
	}
}

// sortedNodeIDs returns the template's node ids in deterministic order.
func sortedNodeIDs(t *workflow.Template) []string {
	ids := make([]string, 0, len(t.Nodes))
	for id := range t.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// writeJSON encodes v as indented JSON to stdout, reporting an encode/write
// failure on stderr. Shared by every --json path.
func writeJSON(stdout, stderr io.Writer, v any) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(stderr, "Error: encoding JSON: %v\n", err)
		return rcIOFailure
	}
	return rcOK
}
