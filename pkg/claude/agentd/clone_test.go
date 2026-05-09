package agentd

import (
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// uniqueCloneAlias always appends `-clone-N`. N is the smallest
// integer not already used by any matching alias system-wide; clone-
// of-a-clone strips the existing `-clone-N` before recomputing.

func TestUniqueCloneAlias_NoExistingCloneStartsAt1(t *testing.T) {
	setupTestDB(t)
	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "a", Alias: "worker"})

	got := uniqueCloneAlias("worker")
	if got != "worker-clone-1" {
		t.Errorf("first clone: got %q, want %q", got, "worker-clone-1")
	}
}

func TestUniqueCloneAlias_GlobalCounterAcrossGroups(t *testing.T) {
	// The new clone should pick the smallest free N regardless of
	// which groups the existing clones live in.
	setupTestDB(t)
	g1, _ := db.CreateAgentGroup("team-a", "")
	g2, _ := db.CreateAgentGroup("team-b", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: g1, ConvID: "a", Alias: "worker"})
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: g1, ConvID: "b", Alias: "worker-clone-1"})
	// "worker-clone-2" lives in a DIFFERENT group; the next clone must
	// still skip past it.
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: g2, ConvID: "c", Alias: "worker-clone-2"})

	got := uniqueCloneAlias("worker")
	if got != "worker-clone-3" {
		t.Errorf("global counter: got %q, want %q", got, "worker-clone-3")
	}
}

func TestUniqueCloneAlias_FillsHoles(t *testing.T) {
	// If 1 and 3 are taken but 2 is free, pick 2.
	setupTestDB(t)
	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "a", Alias: "worker-clone-1"})
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "b", Alias: "worker-clone-3"})

	got := uniqueCloneAlias("worker")
	if got != "worker-clone-2" {
		t.Errorf("hole-filling: got %q, want %q", got, "worker-clone-2")
	}
}

func TestUniqueCloneAlias_CloneOfACloneStripsSuffix(t *testing.T) {
	// Cloning `worker-clone-3` should yield `worker-clone-N` (bumped),
	// not `worker-clone-3-clone-1` (nested).
	setupTestDB(t)
	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "a", Alias: "worker"})
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "b", Alias: "worker-clone-3"})

	got := uniqueCloneAlias("worker-clone-3")
	if got != "worker-clone-1" {
		// 1 and 2 are free; we expect the smallest, which is 1.
		t.Errorf("clone-of-clone: got %q, want %q", got, "worker-clone-1")
	}
}

func TestUniqueCloneAlias_EmptyOriginalAliasUsesPrefix(t *testing.T) {
	// Original had no alias in this group. The clone gets `clone-N`
	// (no leading dash, no base name).
	setupTestDB(t)
	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "a", Alias: "clone-1"})

	got := uniqueCloneAlias("")
	if got != "clone-2" {
		t.Errorf("empty base: got %q, want %q", got, "clone-2")
	}
}

func TestUniqueCloneAlias_DifferentBasesIndependent(t *testing.T) {
	// Existing aliases for "frontend-clone-N" don't affect counter
	// for "worker-clone-N".
	setupTestDB(t)
	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "a", Alias: "frontend-clone-1"})
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "b", Alias: "frontend-clone-2"})

	got := uniqueCloneAlias("worker")
	if got != "worker-clone-1" {
		t.Errorf("independent base: got %q, want %q", got, "worker-clone-1")
	}
}
