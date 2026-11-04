package agentd_test

import (
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// The collab-fleet docshots fixture (tcl-8nfzbb): a fleet the size of a real
// operator's, for rendering the Collab view's home and side panel. 39 groups
// with a directory (so each could start a group shell), two group shells
// running, five agents of group tclaude running out of sight, and a few
// boards and saved pages, most of them closed.

// collabFleetGroups are the groups, tclaude first.
var collabFleetGroups = []string{
	"tclaude", "tclaude-website", "awb", "home-lab", "dotfiles", "infra", "data-pipeline", "webshop",
	"webshop-perf", "search", "payments", "onboarding", "mobile-app", "design-system", "docs", "billing",
	"analytics", "auth", "notifications", "email", "release", "security", "observability", "ci",
	"benchmarks", "ml-eval", "recsys", "pricing", "support-bot", "crm", "partners", "growth",
	"localization", "accessibility", "storage", "edge", "maps", "legacy-api", "sandbox",
}

// collabFleetAgents are the five agents of group tclaude, each running.
var collabFleetAgents = []struct{ conv, title, status, detail string }{
	{"c011ab00-0000-4000-8000-000000000001", "steward", "idle", ""},
	{"c011ab00-0000-4000-8000-000000000002", "launch-uniform", "working", "Edit"},
	{"c011ab00-0000-4000-8000-000000000003", "relaunch-table", "working", "Bash"},
	{"c011ab00-0000-4000-8000-000000000004", "open-questions", "idle", ""},
	{"c011ab00-0000-4000-8000-000000000005", "live-page", "awaiting_permission", ""},
}

// docShellPane is a running group shell's pane: alive, and deaf.
type docShellPane struct{}

func (docShellPane) Receive(string) {}
func (docShellPane) IsAlive() bool  { return true }
func (docShellPane) Shutdown()      {}

// docGroupShellSession is the tmux session a group's shell runs in
// (groupTermSessionName).
func docGroupShellSession(group string) string {
	sum := sha256.Sum256([]byte(group))
	return fmt.Sprintf("tclaude-groupterm-%x", sum[:8])
}

// seedCollabFleet seeds the fleet; quiet leaves out everything running (the
// agents and the group shells), for the home's quiet state.
func seedCollabFleet(t *testing.T, f *testharness.Flow, quiet bool) {
	t.Helper()
	if err := config.Save(&config.Config{Federation: &config.FederationConfig{Name: "dev@box"}}); err != nil {
		t.Fatalf("config: %v", err)
	}
	ids := map[string]int64{}
	for _, name := range collabFleetGroups {
		g := f.HaveGroup(name)
		ids[name] = g.ID
		if _, err := db.SetAgentGroupDefaultCwd(name, docHome+"/git/"+name); err != nil {
			t.Fatalf("group cwd %s: %v", name, err)
		}
	}
	for _, name := range []string{"tclaude", "home-lab"} {
		if quiet {
			break
		}
		f.World.Tmux.Register(docGroupShellSession(name), docHome+"/git/"+name, docShellPane{})
	}
	for _, a := range collabFleetAgents {
		if quiet {
			break
		}
		label, tmux := "lbl-"+a.title, "tmux-"+a.title
		f.HaveAliveSession(a.conv, label, tmux, docHome+"/git/tclaude")
		f.HaveConvWithTitle(a.conv, a.title)
		f.HaveMemberWithRole("tclaude", a.conv, "dev")
		row, err := db.LoadSession(label)
		if err != nil || row == nil {
			t.Fatalf("load session %s: %v", a.title, err)
		}
		row.Status, row.StatusDetail = a.status, a.detail
		row.LastHook = time.Now().Add(-time.Minute)
		if err := db.SaveSession(row); err != nil {
			t.Fatalf("save session %s: %v", a.title, err)
		}
	}
	boards := []struct{ id, title, group string }{
		{"collab-plan", "Collab plan", "tclaude"},
		{"launch-map", "Launch-resolution map", "tclaude"},
		{"release-train", "Release train", "release"},
		{"net-diagram", "Network diagram", "home-lab"},
		{"pricing-v3", "Pricing v3", "pricing"},
	}
	for _, b := range boards {
		if _, err := db.CreateCollabBoard(b.id, b.title, "docshots", 100); err != nil {
			t.Fatalf("board %s: %v", b.id, err)
		}
		if err := db.SetCollabBoardGroup(b.id, ids[b.group]); err != nil {
			t.Fatalf("board group %s: %v", b.id, err)
		}
	}
	pages := []struct{ id, name, title, group string }{
		{"pg_collab_home", "collab-home", "Collab home mock", "tclaude"},
		{"pg_rail", "rail", "Rail mock", "tclaude"},
		{"pg_status", "status", "Status page", "home-lab"},
	}
	for _, p := range pages {
		row := db.GroupPage{ID: p.id, GroupID: ids[p.group], Owner: fmt.Sprintf("group:%d", ids[p.group]), Name: p.name, Title: p.title, Kind: "static"}
		if err := db.SaveOpenGroupPage(row); err != nil {
			t.Fatalf("page %s: %v", p.id, err)
		}
		if err := db.CloseGroupPage(p.id, "closed by the operator"); err != nil {
			t.Fatalf("close page %s: %v", p.id, err)
		}
	}
}
