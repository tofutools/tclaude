package ccworkflows

import (
	"fmt"
	"strings"
)

// Mermaid renders a run's phase → agent fan-out as a mermaid flowchart
// definition: the portable, vendor-free diagram artifact. The returned text is
// a complete mermaid graph you can paste into GitHub markdown (which renders
// mermaid natively), mermaid.live, an editor preview, or a design doc — no
// rendering library is bundled into tclaude itself.
//
// It is a thin PROJECTION of the same typed RunState the CLI tree view and the
// dashboard already consume — there is no parallel data path. The shape mirrors
// the run's two levels: phase nodes chained in sequence (P1 --> P2 --> …), each
// fanning out to the agent leaves that ran under it. Node fill encodes status
// (done / running / failed / queued) via classDef, so the same colour language
// as the tree carries into the diagram. Agents whose phase is unknown (an
// in-flight dynamic fan-out, or phaseIndex 0) hang off a single "Unassigned"
// node so nothing is silently dropped.
//
// IDs are synthesised positionally (P<index>, a<n>, unassigned) and never
// derived from run text, so an adversarial label/title cannot break out of a
// node into raw mermaid syntax; the human-facing text is quoted and escaped by
// mermaidLabel.
func Mermaid(rs *RunState) string {
	var b strings.Builder
	// A %% comment header: ignored by every renderer, but it makes the exported
	// text self-identifying when read/diffed as a plain artifact.
	fmt.Fprintf(&b, "%%%% Run %s [%s]", rs.RunID, rs.Status)
	if rs.WorkflowName != "" {
		fmt.Fprintf(&b, " — %s", rs.WorkflowName)
	}
	b.WriteByte('\n')
	b.WriteString("flowchart TD\n")

	// No structure at all → a single placeholder node, still a valid diagram.
	if len(rs.Phases) == 0 && len(rs.Agents) == 0 {
		b.WriteString("  empty[\"(no phases or agents)\"]\n")
		return b.String()
	}

	known := make(map[int]bool, len(rs.Phases))
	for _, p := range rs.Phases {
		known[p.Index] = true
	}

	// Phase nodes, in the run's phase order (RunState.Phases is index-sorted).
	for _, p := range rs.Phases {
		title := p.Title
		if title == "" {
			title = fmt.Sprintf("Phase %d", p.Index)
		} else {
			title = fmt.Sprintf("Phase %d: %s", p.Index, title)
		}
		fmt.Fprintf(&b, "  P%d[%s]%s\n", p.Index, mermaidLabel(title), classSuffix(p.Status))
	}
	// Phase sequence edges: P1 --> P2 --> … in order.
	for i := 1; i < len(rs.Phases); i++ {
		fmt.Fprintf(&b, "  P%d --> P%d\n", rs.Phases[i-1].Index, rs.Phases[i].Index)
	}

	// Agent leaves, each linked to its phase (or to the shared Unassigned node).
	hasUnassigned := false
	for _, a := range rs.Agents {
		if !known[a.PhaseIndex] {
			hasUnassigned = true
		}
	}
	if hasUnassigned {
		b.WriteString("  unassigned[\"Unassigned\"]\n")
	}
	for i, a := range rs.Agents {
		label := a.Label
		if label == "" {
			label = shortMermaidID(a.ID)
		}
		fmt.Fprintf(&b, "  a%d[%s]%s\n", i, mermaidLabel(label), classSuffix(a.State))
		if known[a.PhaseIndex] {
			fmt.Fprintf(&b, "  P%d --> a%d\n", a.PhaseIndex, i)
		} else {
			fmt.Fprintf(&b, "  unassigned --> a%d\n", i)
		}
	}

	// Status palette — light fills with a matching stroke, readable on either a
	// light or dark mermaid theme. Always emitted so any :::class reference
	// resolves regardless of which states the run happened to use.
	b.WriteString("  classDef done fill:#d4f4dd,stroke:#1f7a1f,color:#0b3d0b;\n")
	b.WriteString("  classDef running fill:#fff3cd,stroke:#b8860b,color:#5c4400;\n")
	b.WriteString("  classDef failed fill:#f8d7da,stroke:#a71d2a,color:#5c0a13;\n")
	b.WriteString("  classDef queued fill:#e2e3e5,stroke:#6c757d,color:#343a40;\n")
	return b.String()
}

// classSuffix maps an AgentState to a `:::class` reference for a node, or "" for
// an empty/unrecognised state (which renders with the default node style). The
// class names match the classDef block emitted by Mermaid.
func classSuffix(s AgentState) string {
	switch s {
	case AgentDone:
		return ":::done"
	case AgentRunning:
		return ":::running"
	case AgentFailed:
		return ":::failed"
	case AgentQueued:
		return ":::queued"
	default:
		return ""
	}
}

// mermaidLabel renders s as a safe quoted mermaid node label: "…". Newlines and
// runs of whitespace collapse to a single space, embedded double quotes become
// single quotes (so they can't close the label early), and the text is capped
// so one pathological label can't bloat the diagram. The result always includes
// the surrounding quotes.
func mermaidLabel(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	s = strings.ReplaceAll(s, "\"", "'")
	const max = 80
	r := []rune(s)
	if len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	return "\"" + s + "\""
}

// shortMermaidID trims a long agent id for an unlabeled node (mirrors the
// CLI/web shortId, kept local so ccworkflows stays self-contained).
func shortMermaidID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}
