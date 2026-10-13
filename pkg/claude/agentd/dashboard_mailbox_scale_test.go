package agentd_test

import (
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// TestDashboardMailboxScale times the Messages-tab endpoints against a
// large synthetic installation: 100k agents (95k retired), 100k messages,
// 500 groups, 2k human notifications. It asserts nothing beyond HTTP 200 —
// it is a measuring tool for perf work on the mailbox queries, opt-in
// because seeding takes several seconds and ~400 MB of temp disk:
//
//	TCLAUDE_MAILBOX_SCALE=1 go test ./pkg/claude/agentd/ -run TestDashboardMailboxScale -v -count=1 -timeout 1200s
//
// TCLAUDE_MAILBOX_SCALE=roster times only the sidebar roster (for profiling).
func TestDashboardMailboxScale(t *testing.T) {
	mode := os.Getenv("TCLAUDE_MAILBOX_SCALE")
	if mode == "" {
		t.Skip("set TCLAUDE_MAILBOX_SCALE=1 to run the Messages-tab scale benchmark")
	}
	_ = newFlow(t)
	seedMailboxScale(t)
	dash := dashHandlerForTest(t)
	get := func(path string) {
		st := time.Now()
		rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet, path, nil))
		if rec.Code != 200 {
			t.Errorf("%s -> %d %s", path, rec.Code, rec.Body.String()[:min(200, rec.Body.Len())])
		}
		t.Logf("%-55s %8v  %7d KB", path, time.Since(st), rec.Body.Len()/1024)
	}
	if mode == "roster" {
		get("/api/mailboxes")
		get("/api/mailboxes")
		get("/api/mailboxes")
		return
	}
	get("/api/mailboxes")
	get("/api/mailboxes")
	get("/api/mailboxes?include_retired=1")
	get("/api/mailbox?id=all")
	get("/api/mailbox?id=all&include_retired=1")
	get("/api/mailbox?id=all&page=50")
	get("/api/mailbox?id=human")
	get("/api/mailbox?id=" + scaleConv(scaleAgents-10))
	get("/api/mailbox?id=group:group-7")
	get("/api/mailbox?id=group:group-7&include_retired=1")
	get("/api/mailbox?id=all&q=zzz")
	get("/api/mailbox?id=all&q=title")
}

// Scale fixture shape: agents [0, scaleAgents-scaleLive) are retired.
const (
	scaleAgents = 100000
	scaleMsgs   = 100000
	scaleGroups = 500
	scaleLive   = 5000
)

// scaleConv is the conv-id of scale-fixture agent i.
func scaleConv(i int) string { return fmt.Sprintf("%08x-aaaa-bbbb-cccc-%012x", i, i) }

// seedMailboxScale bulk-inserts the scale fixture straight into SQLite (the
// production insert paths would take minutes at this size).
func seedMailboxScale(t *testing.T) {
	t.Helper()
	nAgents, nMsgs, nGroups, nLive := scaleAgents, scaleMsgs, scaleGroups, scaleLive
	conv := scaleConv
	d, err := db.Open()
	if err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewSource(1))
	now := time.Now()
	st := time.Now()
	tx, _ := d.Begin()
	for i := 0; i < nAgents; i++ {
		var retired any
		if i < nAgents-nLive {
			retired = now.Add(-time.Hour).UnixNano()
		}
		if _, err := tx.Exec(`INSERT INTO agents(agent_id,current_conv_id,created_at,retired_at,pending_name) VALUES (?,?,?,?,?)`,
			fmt.Sprintf("agt_%08x", i), conv(i), now.UnixNano(), retired, fmt.Sprintf("worker-%d", i)); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO agent_conversations(conv_id,agent_id,linked_at) VALUES (?,?,?)`, conv(i), fmt.Sprintf("agt_%08x", i), now.UnixNano()); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO conv_index(conv_id,project_dir,full_path,custom_title) VALUES (?,?,?,?)`, conv(i), "/p", "/p/x.jsonl", fmt.Sprintf("agent title %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	for g := 1; g <= nGroups; g++ {
		if _, err := tx.Exec(`INSERT INTO agent_groups(id,name,created_at) VALUES (?,?,?)`, g, fmt.Sprintf("group-%d", g), now.UnixNano()); err != nil {
			t.Fatal(err)
		}
		for m := 0; m < 10; m++ {
			_, _ = tx.Exec(`INSERT OR IGNORE INTO agent_group_members(group_id,agent_id,joined_at) VALUES (?,?,?)`, g, fmt.Sprintf("agt_%08x", nAgents-nLive+r.Intn(nLive)), now.UnixNano())
		}
	}
	// Agents talk mostly within their own era (neighbouring agent ids), so
	// most traffic belongs to retired agents, as on a long-lived install.: message i is between agents near i.
	for i := 0; i < nMsgs; i++ {
		a := min(nAgents-1, max(0, i*nAgents/nMsgs+r.Intn(200)-100))
		b := min(nAgents-1, max(0, i*nAgents/nMsgs+r.Intn(200)-100))
		if _, err := tx.Exec(`INSERT INTO agent_messages(group_id,from_conv,to_conv,subject,body,created_at) VALUES (?,?,?,?,?,?)`,
			1+r.Intn(nGroups), conv(a), conv(b), "subj", strings.Repeat("lorem ipsum ", 50+r.Intn(400)), now.Add(time.Duration(i-nMsgs)*time.Minute).UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2000; i++ {
		_, _ = tx.Exec(`INSERT INTO human_messages(from_conv,body,created_at) VALUES (?,?,?)`, conv(nAgents-1-i%nLive), strings.Repeat("note ", 100), now.UnixNano())
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	t.Logf("seeded in %v", time.Since(st))
}
