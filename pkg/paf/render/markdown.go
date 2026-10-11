package render

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
	"github.com/tofutools/tclaude/pkg/paf"
)

// Markdown writes the archive as one Markdown document: archive facts,
// then each agent's definition, environment, losses and conversation.
func Markdown(w io.Writer, a *paf.Archive, source string, opts Options) error {
	s := Summarize(a, source)
	bw := bufio.NewWriter(w)
	fmt.Fprintf(bw, "# %s\n\n", oneLine(documentTitle(s)))
	fmt.Fprintf(bw, "%s\n\n## Archive\n\n", provenanceNote(s))
	for _, f := range archiveFacts(s) {
		fmt.Fprintf(bw, "- %s: %s\n", f[0], oneLine(f[1]))
	}
	for _, ag := range s.Agents {
		fmt.Fprintf(bw, "\n## Agent: %s\n\n", oneLine(ag.Name))
		for _, f := range agentFacts(ag) {
			fmt.Fprintf(bw, "- %s: %s\n", f[0], oneLine(f[1]))
		}
		for _, f := range definitionTexts(ag.Definition) {
			fmt.Fprintf(bw, "\n### %s\n\n%s\n", f[0], fence(f[1]))
		}
		if ag.LandingRung != "" || len(ag.Losses) > 0 {
			fmt.Fprintf(bw, "\n### Losses\n\n")
			if ag.LandingRung != "" {
				fmt.Fprintf(bw, "Landing rung when written: %s\n\n", oneLine(ag.LandingRung))
			}
			for _, l := range ag.Losses {
				fmt.Fprintf(bw, "- %s\n", oneLine(lossLine(l)))
			}
		}
		t, from, err := Transcript(a, ag.Key, opts)
		if err != nil {
			fmt.Fprintf(bw, "\n### Conversation\n\n_Not shown: %s._\n", oneLine(err.Error()))
			continue
		}
		fmt.Fprintf(bw, "\n### Conversation\n\nFrom the %s, %d entries.\n", from, len(t.Entries))
		if err := bw.Flush(); err != nil {
			return err
		}
		if err := transcript.WriteMarkdownEntries(t, w, 4); err != nil {
			return err
		}
	}
	return bw.Flush()
}

func documentTitle(s Summary) string {
	if s.Group != nil {
		return "Group " + s.Group.Name + " (PAF archive)"
	}
	if len(s.Agents) == 1 && s.Agents[0].Name != "" {
		return s.Agents[0].Name + " (PAF archive)"
	}
	return "PAF archive"
}

func provenanceNote(s Summary) string {
	note := "Rendered by tclaude from a verified PAF, without running any harness or model."
	if s.Source != paf.Format {
		note = "Rendered by tclaude from a " + s.Source + " archive, converted to PAF on read and verified, without running any harness or model."
	}
	if len(s.Unsupported) > 0 {
		note += " This reader does not understand: " + strings.Join(s.Unsupported, ", ") + "; parts of the content may be misread."
	}
	return note
}

// archiveFacts are label/value pairs describing the archive itself.
func archiveFacts(s Summary) [][2]string {
	f := [][2]string{
		{"Format", fmt.Sprintf("%s %s, kind %s", s.Format, s.PafVersion, s.Kind)},
		{"Created", s.CreatedAt.UTC().Format(time.RFC3339)},
		{"Producer", strings.TrimSpace(s.Producer.Name + " " + s.Producer.Version)},
		{"Entries", fmt.Sprintf("%d, %s", s.Entries, humanBytes(s.Bytes))},
		{"Signatures", signaturesLine(s.Signatures)},
	}
	if len(s.Requires) > 0 {
		f = append(f, [2]string{"Requires", strings.Join(s.Requires, ", ")})
	}
	if len(s.Unsupported) > 0 {
		f = append(f, [2]string{"Not understood", strings.Join(s.Unsupported, ", ")})
	}
	if len(s.Extensions) > 0 {
		var parts []string
		for _, e := range s.Extensions {
			p := e.ID + " " + e.Version + " (" + strings.TrimSpace(e.System+" "+e.SystemVersion) + ")"
			if e.Required {
				p += ", required"
			}
			if !e.Understood {
				p += ", not understood"
			}
			parts = append(parts, p)
		}
		f = append(f, [2]string{"Extensions", strings.Join(parts, "; ")})
	}
	if s.Group != nil {
		f = append(f, [2]string{"Group", s.Group.Name + " (" + strings.Join(s.Group.Members, ", ") + ")"})
	}
	if p := s.Privacy; p != nil {
		var parts []string
		if p.Sensitivity != "" {
			parts = append(parts, "sensitivity "+p.Sensitivity)
		}
		if p.Audience != "" {
			parts = append(parts, "audience "+p.Audience)
		}
		for _, fd := range p.Findings {
			parts = append(parts, fmt.Sprintf("%d %s finding(s)", fd.Count, fd.Kind))
		}
		if p.Redactions > 0 {
			parts = append(parts, fmt.Sprintf("%d redaction(s)", p.Redactions))
		}
		if len(parts) > 0 {
			f = append(f, [2]string{"Privacy", strings.Join(parts, "; ")})
		}
	}
	return f
}

func signaturesLine(sigs []SignatureSummary) string {
	if len(sigs) == 0 {
		return "none (unsigned)"
	}
	var parts []string
	for _, s := range sigs {
		p := s.Status + " " + s.Alg + " key " + s.KeyID
		if s.Signer != "" {
			p += " (" + s.Signer + ")"
		}
		if s.Trust != "" {
			p += ", " + s.Trust
		}
		if s.Error != "" {
			p += ": " + s.Error
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, "; ") + ". A valid signature proves the archive is unchanged since that key signed it, not who holds the key."
}

func agentFacts(ag AgentSummary) [][2]string {
	f := [][2]string{{"Harness", ag.Harness}}
	add := func(label, v string) {
		if v != "" {
			f = append(f, [2]string{label, v})
		}
	}
	add("Title", ag.Title)
	add("Model", strings.TrimSpace(ag.Model+" "+effortNote(ag.Effort)))
	add("Working directory", ag.Cwd)
	if ws := ag.Workspace; ws != nil {
		add("Repository", strings.TrimSpace(ws.RepoURL+" "+ws.Branch+" "+ws.Commit))
	}
	add("Approval", ag.Approval)
	add("Sandbox", strings.Join(ag.Sandbox, ", "))
	add("Groups", strings.Join(ag.Groups, ", "))
	add("Lineage", strings.Join(ag.Lineage, "; "))
	add("Placeholders", strings.Join(ag.Placeholders, ", "))
	if n := ag.Native; n != nil {
		add("Native history", fmt.Sprintf("%s, %s", n.Format, humanBytes(n.Bytes)))
	}
	var ctx []string
	for _, c := range ag.Context {
		ctx = append(ctx, fmt.Sprintf("%s, %s", c.Scope, humanBytes(c.Bytes)))
	}
	if ag.Handoff {
		ctx = append(ctx, "handoff")
	}
	add("Neutral context", strings.Join(ctx, "; "))
	return f
}

func effortNote(e string) string {
	if e == "" {
		return ""
	}
	return "(effort " + e + ")"
}

// definitionTexts are the definition's free-text fields, in reading order.
func definitionTexts(d *paf.Definition) [][2]string {
	if d == nil {
		return nil
	}
	var f [][2]string
	add := func(label, v string) {
		if strings.TrimSpace(v) != "" {
			f = append(f, [2]string{label, v})
		}
	}
	add("Role", d.Role)
	add("Description", d.Description)
	add("Role guidance", d.RoleGuidance)
	add("Startup context", d.StartupContext)
	add("Initial message", d.InitialMessage)
	if d.Task != nil {
		add("Task", strings.TrimSpace(d.Task.Label+" "+d.Task.URL))
	}
	add("License", d.License)
	return f
}

func lossLine(l paf.LossItem) string {
	s := fmt.Sprintf("%s (%s)", l.Kind, l.Severity)
	if l.Count > 0 {
		s += fmt.Sprintf(" x%d", l.Count)
	}
	if l.Detail != "" {
		s += ": " + l.Detail
	}
	return s
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

// oneLine keeps an archive-supplied value on its list line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// fence wraps text in a code fence longer than any backtick run inside it,
// so archive text cannot close the fence early.
func fence(s string) string {
	ticks := "```"
	for strings.Contains(s, ticks) {
		ticks += "`"
	}
	return ticks + "\n" + s + "\n" + ticks
}

// ArchiveFacts are label/value pairs describing the archive, as the
// documents show them.
func ArchiveFacts(s Summary) [][2]string { return archiveFacts(s) }

// AgentFacts are label/value pairs describing one agent.
func AgentFacts(ag AgentSummary) [][2]string { return agentFacts(ag) }

// LossLine formats one recorded loss.
func LossLine(l paf.LossItem) string { return lossLine(l) }
