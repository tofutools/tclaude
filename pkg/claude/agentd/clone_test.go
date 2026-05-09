package agentd

import (
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestUniqueCloneAlias_NoCollision(t *testing.T) {
	setupTestDB(t)
	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "a", Alias: "worker"})

	got := uniqueCloneAlias(gID, "worker-clone")
	if got != "worker-clone" {
		t.Errorf("no collision: got %q, want %q", got, "worker-clone")
	}
}

func TestUniqueCloneAlias_FirstCollisionGets2(t *testing.T) {
	setupTestDB(t)
	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "a", Alias: "worker"})
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "b", Alias: "worker-clone"})

	got := uniqueCloneAlias(gID, "worker-clone")
	if got != "worker-clone-2" {
		t.Errorf("first collision: got %q, want %q", got, "worker-clone-2")
	}
}

func TestUniqueCloneAlias_StackedCollisions(t *testing.T) {
	setupTestDB(t)
	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "a", Alias: "worker"})
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "b", Alias: "worker-clone"})
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "c", Alias: "worker-clone-2"})
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "d", Alias: "worker-clone-3"})

	got := uniqueCloneAlias(gID, "worker-clone")
	if got != "worker-clone-4" {
		t.Errorf("stacked collisions: got %q, want %q", got, "worker-clone-4")
	}
}

func TestUniqueCloneAlias_OtherGroupsDontCount(t *testing.T) {
	setupTestDB(t)
	g1, _ := db.CreateAgentGroup("team-a", "")
	g2, _ := db.CreateAgentGroup("team-b", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: g2, ConvID: "x", Alias: "worker-clone"})

	got := uniqueCloneAlias(g1, "worker-clone")
	if got != "worker-clone" {
		t.Errorf("collision in unrelated group should not affect g1: got %q", got)
	}
}

func TestUniqueCloneAlias_EmptyGroupBase(t *testing.T) {
	// When the original had no alias, the clone's base is "clone"
	// (no leading dash). Make sure auto-increment still works.
	setupTestDB(t)
	gID, _ := db.CreateAgentGroup("team", "")
	_ = db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: gID, ConvID: "a", Alias: "clone"})

	got := uniqueCloneAlias(gID, "clone")
	if got != "clone-2" {
		t.Errorf("got %q, want %q", got, "clone-2")
	}
}
