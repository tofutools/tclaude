package agent

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// TestRenderPermissionsState_LeadsWithAgentID locks in the JOH-325 (D3)
// display cleanup: the per-agent overrides roster leads its ID column with
// the stable agent_id projected onto each conv key (state.AgentIDs), not
// the conv-id prefix. Storage was already agent-keyed — this is the
// display half.
func TestRenderPermissionsState_LeadsWithAgentID(t *testing.T) {
	// Hermetic: titles now arrive on the wire (state.Titles), so this
	// renderer touches no DB at all — point HOME at a fresh temp store
	// anyway so a regression back to a local lookup can't reach the real
	// ~/.tclaude.
	t.Setenv("HOME", t.TempDir())
	db.ResetForTest()

	const conv = "11112222-3333-4444-5555-666677778888"
	const agentID = "agt_032fdfcfbb0578a5a1cf6493db7264fb"

	t.Run("known agent_id leads the ID column", func(t *testing.T) {
		state := permissionsState{
			Defaults:  []string{"groups.create"},
			Overrides: map[string]map[string]string{conv: {"groups.spawn": "grant", "human.notify": "deny"}},
			AgentIDs:  map[string]string{conv: agentID},
		}
		var buf bytes.Buffer
		if rc := renderPermissionsState(state, &buf); rc != rcOK {
			t.Fatalf("renderPermissionsState rc = %d, want %d", rc, rcOK)
		}
		out := buf.String()
		if want := agentID[:12]; !strings.Contains(out, want) {
			t.Errorf("roster must lead with the short agent_id %q; got:\n%s", want, out)
		}
		if convPrefix := conv[:8]; strings.Contains(out, convPrefix) {
			t.Errorf("roster must not show the conv prefix %q in the ID column when an agent_id is known; got:\n%s", convPrefix, out)
		}
	})

	t.Run("missing agent_id falls back to the conv prefix", func(t *testing.T) {
		state := permissionsState{
			Overrides: map[string]map[string]string{conv: {"groups.spawn": "grant"}},
			// AgentIDs intentionally empty — the daemon couldn't project one.
		}
		var buf bytes.Buffer
		if rc := renderPermissionsState(state, &buf); rc != rcOK {
			t.Fatalf("renderPermissionsState rc = %d, want %d", rc, rcOK)
		}
		out := buf.String()
		if convPrefix := conv[:8]; !strings.Contains(out, convPrefix) {
			t.Errorf("roster must fall back to the conv prefix %q when no agent_id is known; got:\n%s", convPrefix, out)
		}
	})
}
