package ccworkflows

import (
	"strings"
	"testing"
)

// mustContainAll fails the test for any wanted substring missing from got,
// printing the full diagram once so a failure is debuggable.
func mustContainAll(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("mermaid output missing %q", w)
		}
	}
	if t.Failed() {
		t.Logf("full mermaid output:\n%s", got)
	}
}

func TestMermaidPhaseFanout(t *testing.T) {
	rs := &RunState{
		RunID:        "wf_abc",
		WorkflowName: "review",
		Status:       RunCompleted,
		Phases: []Phase{
			{Index: 1, Title: "Review", Status: AgentDone},
			{Index: 2, Title: "Verify", Status: AgentRunning},
		},
		Agents: []Agent{
			{ID: "a1", Label: "review:bugs", PhaseIndex: 1, State: AgentDone},
			{ID: "a2", Label: "review:perf", PhaseIndex: 1, State: AgentDone},
			{ID: "a3", Label: "verify:bugs", PhaseIndex: 2, State: AgentRunning},
		},
	}
	got := Mermaid(rs)

	mustContainAll(t, got,
		"flowchart TD",
		`%% Run wf_abc [completed] — review`,
		`P1["Phase 1: Review"]:::done`,
		`P2["Phase 2: Verify"]:::running`,
		"P1 --> P2",                 // phase sequence edge
		`a0["review:bugs"]:::done`,  // agent leaves
		`a2["verify:bugs"]:::running`,
		"P1 --> a0",
		"P1 --> a1",
		"P2 --> a2",
		"classDef done",
		"classDef running",
	)
	// A clean two-phase run must not invent an Unassigned bucket.
	if strings.Contains(got, "unassigned") {
		t.Errorf("did not expect an unassigned node for a fully-mapped run:\n%s", got)
	}
}

func TestMermaidUnassignedAgents(t *testing.T) {
	rs := &RunState{
		RunID:  "wf_x",
		Status: RunRunning,
		Phases: []Phase{{Index: 1, Title: "Scan", Status: AgentRunning}},
		Agents: []Agent{
			{ID: "a1", Label: "scan", PhaseIndex: 1, State: AgentRunning},
			// PhaseIndex 0 / unknown → must hang off the shared Unassigned node,
			// never silently dropped.
			{ID: "deadbeefcafef00d", Label: "", PhaseIndex: 0, State: AgentQueued},
		},
	}
	got := Mermaid(rs)
	mustContainAll(t, got,
		`unassigned["Unassigned"]`,
		"unassigned --> a1",
		`a1["deadbeef"]:::queued`, // empty label falls back to a short id
		"P1 --> a0",
	)
}

func TestMermaidEmpty(t *testing.T) {
	got := Mermaid(&RunState{RunID: "wf_empty", Status: RunUnknown})
	mustContainAll(t, got, "flowchart TD", `empty["(no phases or agents)"]`)
	if strings.Contains(got, "-->") {
		t.Errorf("empty run should have no edges:\n%s", got)
	}
}

func TestMermaidLabelEscaping(t *testing.T) {
	rs := &RunState{
		RunID:  "wf_esc",
		Status: RunCompleted,
		Phases: []Phase{{Index: 1, Title: "A \"quoted\"\nmulti-line   title", Status: AgentDone}},
		Agents: []Agent{{ID: "a1", Label: strings.Repeat("x", 200), PhaseIndex: 1, State: AgentDone}},
	}
	got := Mermaid(rs)
	// Embedded double quotes become single quotes so they can't close the label;
	// newlines/whitespace runs collapse to a single space.
	mustContainAll(t, got, `P1["Phase 1: A 'quoted' multi-line title"]:::done`)
	if strings.Contains(got, "\"quoted\"") {
		t.Errorf("raw embedded double-quotes must not survive into a label:\n%s", got)
	}
	// The 200-char label is capped (80 runes incl. the ellipsis).
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "a0[") {
			if !strings.Contains(line, "…") {
				t.Errorf("over-long label should be truncated with an ellipsis: %q", line)
			}
		}
	}
}

func TestMermaidUntitledPhase(t *testing.T) {
	rs := &RunState{
		RunID:  "wf_u",
		Status: RunCompleted,
		Phases: []Phase{{Index: 3, Title: "", Status: AgentDone}},
	}
	got := Mermaid(rs)
	mustContainAll(t, got, `P3["Phase 3"]:::done`)
}

// TestMermaidFromRealRun renders an on-disk fixture run end-to-end to guard the
// projection against the authoritative RunState the rest of the package builds.
func TestMermaidFromRealRun(t *testing.T) {
	rs, err := LoadRun(fixtureSessionDir(), "wf_0fa30e48-d43")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	got := Mermaid(rs)
	mustContainAll(t, got,
		"flowchart TD",
		`P1["Phase 1: Research"]:::done`,
		`P2["Phase 2: Synthesize"]:::done`,
		"P1 --> P2",
	)
	// Every agent in the run must appear as a node.
	for i := range rs.Agents {
		node := "a" + itoa(i) + "["
		if !strings.Contains(got, node) {
			t.Errorf("agent %d not rendered as a node %q:\n%s", i, node, got)
		}
	}
}

// itoa is a tiny local int→string to keep the test free of strconv churn.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
