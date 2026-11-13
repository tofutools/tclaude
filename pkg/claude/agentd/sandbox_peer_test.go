package agentd_test

// sandbox_peer_test.go is `scripts/sandbox/ui.sh up --peer`: a live federation
// peer for the dashboard sandbox. TestSandboxServe, given TCLAUDE_SANDBOX_PEER,
// starts a tclaude-hub in its own process and a second daemon as a child
// process (TestSandboxPeerInstance, on TestFleetJourney's fixture-node
// plumbing), links the two through the hub as trusted peers and lets the
// sandbox read the peer's seeded groups and task board. The Fleet map, the
// peer's per-node pages and the hub-backed Fleet pages then show real peer
// data. Every process stays under the sandbox's throwaway HOME and TMPDIR, on
// loopback ports of its own.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/client"
	"github.com/tofutools/tclaude/pkg/federation/hub"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// The live peer's seeded world, as the tour's assertions name it.
const (
	sbPeerName  = "ci@runner"
	sbPeerLabel = "ci-runner"
	sbPeerGroup = "ci-fleet"
	sbPeerAgent = "nightly-builder"
	sbPeerTask  = "Quarantine the flaky checkout e2e test"
)

// sandboxPeerFixture is what the browser tests need to know about the live
// peer, written under "peer" in fixture.json.
type sandboxPeerFixture struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Label     string `json:"label"`
	Group     string `json:"group"`
	Agent     string `json:"agent"`
	Task      string `json:"task"`
	TaskTitle string `json:"taskTitle"`
}

// TestSandboxPeerInstance is the peer daemon of `ui.sh up --peer`, started by
// TestSandboxServe as a child process.
func TestSandboxPeerInstance(t *testing.T) {
	if os.Getenv("TCLAUDE_SANDBOX_PEER_CHILD") != "1" {
		t.Skip("fixture process for TestSandboxServe's live peer (ui.sh up --peer)")
	}
	serveFleetFixtureInstance(t, seedSandboxPeer)
}

// seedSandboxPeer fills the peer: a CI fleet of agents in two groups and a
// task board, through the production routes where one exists.
func seedSandboxPeer(f *testharness.Flow) {
	t := f.T
	t.Helper()
	cfg, err := config.Load()
	require.NoError(t, err)
	if cfg.Features == nil {
		cfg.Features = &config.FeaturesConfig{}
	}
	cfg.Features.Taskboard = new(true)
	cfg.Federation = &config.FederationConfig{Name: sbPeerName}
	require.NoError(t, config.Save(cfg))
	// No branch has a PR, and nothing shells out to git or gh for one.
	t.Cleanup(agentd.SetGitInfoResolverWithChecksForTest(
		func(_, _ string) (string, string, int, string, string, string, bool) {
			return docRepo, "main", 0, "", "", "", true
		}))

	fleet := f.HaveGroup(sbPeerGroup)
	night := f.HaveGroup("release-checks")
	for _, a := range []struct {
		group *db.AgentGroup
		agent docAgent
	}{
		{fleet, docAgent{conv: "c1000000-0000-4000-8000-000000000001", title: sbPeerAgent, descr: "Runs the nightly build and e2e suite", role: "lead", owner: true,
			harness: "claude", model: "Sonnet 5.5", effort: "medium", status: "working", detail: "Bash", online: true, cost: 3.40,
			startBranch: "main", age: 9 * time.Hour, last: 6 * time.Second}},
		{fleet, docAgent{conv: "c1000000-0000-4000-8000-000000000002", title: "flake-hunter", descr: "Reruns and quarantines flaky tests", role: "dev",
			harness: "codex", model: "gpt-6.1-sol", effort: "high", status: "idle", online: true, cost: 1.90,
			startBranch: "flaky-e2e", age: 4 * time.Hour, last: 7 * time.Minute}},
		{fleet, docAgent{conv: "c1000000-0000-4000-8000-000000000003", title: "cache-warmer", descr: "Keeps the build cache warm", role: "dev",
			harness: "claude", model: "Haiku 5.5", online: false, cost: 0.35,
			startBranch: "main", age: 30 * time.Hour, last: 20 * time.Hour}},
		{night, docAgent{conv: "c1000000-0000-4000-8000-000000000004", title: "rc-verifier", descr: "Smoke tests each release candidate", role: "reviewer",
			harness: "gemini", model: "gemini-3-pro", status: "awaiting_permission", online: true, cost: 0.80,
			startBranch: "main", age: 2 * time.Hour, last: 40 * time.Second}},
	} {
		seedDocAgent(t, f, a.group.ID, a.group.Name, a.agent)
	}

	s := &sandboxSeeder{t: t, f: f}
	const board = "/v1/groups/" + sbPeerGroup + "/taskboard"
	for _, task := range []map[string]any{
		{"title": sbPeerTask, "to": "c1000000-0000-4000-8000-000000000002", "body": "It fails one run in twenty on the payment step."},
		{"title": "Nightly build of main", "to": "c1000000-0000-4000-8000-000000000001"},
		{"title": "Move the e2e suite to the new runners", "stage": "next"},
		{"title": "Cache the Playwright browsers", "stage": "idea"},
		{"title": "Upgrade the runner images", "status": "blocked", "block_reason": "Waiting for the base image release."},
		{"title": "Pin the Go toolchain", "status": "done"},
	} {
		s.api(http.MethodPost, board+"/tasks", task, nil)
	}
}

// seedSandboxLivePeer starts the hub and the peer, links them with the
// sandbox (served by mux) and waits until the sandbox reads the peer's
// snapshot, so the first page load already shows it. Everything stops at the
// test's cleanup: the peer when its stdin closes, the hub with this process.
func seedSandboxLivePeer(t *testing.T, dir string, mux http.Handler) {
	t.Helper()
	agentd.ResetFederationForTest()
	t.Cleanup(agentd.ResetFederationForTest)

	store, err := hub.OpenStore(filepath.Join(t.TempDir(), "hub.sqlite"))
	require.NoError(t, err)
	h, err := hub.New(store, hub.Config{})
	require.NoError(t, err)
	hubSrv := httptest.NewServer(h.Handler())
	t.Cleanup(func() { h.Close(); hubSrv.Close(); _ = store.Close() })

	// The seeding requests reach the sandbox on a loopback port of their own,
	// as its own pages (the dashboard pins a request by Origin or Referer).
	self := httptest.NewServer(sandboxStageReferer(mux))
	t.Cleanup(self.Close)
	local := &journeyNode{url: self.URL, client: &http.Client{Timeout: 10 * time.Second}}
	// Registered first so it runs after the peer's own cleanup has waited for
	// it: a stale pid must not send ui.sh down after a recycled one.
	pidPath := filepath.Join(dir, "peer.pid")
	t.Cleanup(func() { _ = os.Remove(pidPath) })
	peer := startFixtureNode(t, fixtureNodeOptions{
		testName: "TestSandboxPeerInstance", env: []string{"TCLAUDE_SANDBOX_PEER_CHILD=1"}, timeout: "0",
		// ui.sh keeps the log and waits for the pid at down.
		logPath: filepath.Join(dir, "peer.log"),
		onStart: func(pid int) {
			require.NoError(t, os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0o600))
		},
	})

	hubURL := "ws" + strings.TrimPrefix(hubSrv.URL, "http")
	for _, node := range []*journeyNode{local, peer} {
		invite, err := store.CreateInvite("", time.Hour)
		require.NoError(t, err)
		node.call(t, "POST", "/api/federation/config", map[string]any{"enabled": true, "hub_url": hubURL, "invite": invite}, 200)
	}
	localID := local.call(t, "GET", "/api/federation/status", nil, 200)["instance_id"].(string)
	peerID := peer.call(t, "GET", "/api/federation/status", nil, 200)["instance_id"].(string)
	journeyAwait(t, "both nodes on the hub", func() bool {
		return len(journeyRows(t, local.call(t, "GET", "/api/federation/status", nil, 200)["peers"])) == 1 &&
			len(journeyRows(t, peer.call(t, "GET", "/api/federation/status", nil, 200)["peers"])) == 1
	})

	// The sandbox's operator runs the hub too: it claims the hub's admin, as
	// `tclaude-hub admin claim` sets up, so the Hub page and hub runs work. A
	// new claim token changes the admin generation, so hub status answers 409
	// until it has learned it (503 while the link is still coming up).
	claim, err := store.PrepareAdminClaim(false, time.Now())
	require.NoError(t, err)
	journeyAwait(t, "hub status as the sandbox", func() bool {
		return local.status(t, "GET", "/api/federation/hub/status", nil) == http.StatusOK
	})
	local.call(t, "POST", "/api/federation/hub/claim", map[string]any{"token": claim}, 200)

	// The peer enrolls with the sandbox on a node profile, which trusts both
	// ways at the restricted level, as `tclaude federation enroll` does.
	local.call(t, "POST", "/api/federation/profiles", map[string]any{"name": "ci-runners", "definition": map[string]any{"trust_level": "restricted"}}, 200)
	token := local.call(t, "POST", "/api/federation/enroll-tokens", map[string]any{"profile": "ci-runners", "uses": 1, "ttl_seconds": 600}, 200)["token"].(string)
	enroll := map[string]any{"master": localID, "token": token}
	enroll["preview_token"] = peer.call(t, "POST", "/api/federation/enroll/preview", enroll, 200)["preview_token"]
	peer.call(t, "POST", "/api/federation/enroll", enroll, 200)
	// Each side names the other, as an operator does on Fleet config → Peers.
	local.call(t, "POST", "/api/federation/peers/trust", map[string]any{"instance": peerID, "level": "restricted", "label": sbPeerLabel}, 200)
	peer.call(t, "POST", "/api/federation/peers/trust", map[string]any{"instance": localID, "level": "restricted", "label": "dev-box"}, 200)

	// The peer shares its CI fleet with the sandbox: roster, presence and
	// task board, and which harnesses it runs.
	for _, g := range []struct{ slug, scope string }{
		{agentd.PermGroupsRosterRead, "group=" + sbPeerGroup},
		{agentd.PermGroupsPresenceRead, "group=" + sbPeerGroup},
		{agentd.PermTaskboardRead, "group=" + sbPeerGroup},
		{agentd.PermGroupsRosterRead, "group=release-checks"},
		{agentd.PermNodeHarnessesRead, ""},
	} {
		peer.call(t, "POST", "/api/federation/grants", map[string]any{"peer": localID, "slug": g.slug, "scope": g.scope}, 200)
	}
	journeyAwait(t, "the peer's snapshot", func() bool {
		return local.status(t, "GET", "/api/peer/"+peerID+"/snapshot", nil) == http.StatusOK
	})
	agents := journeyRows(t, local.call(t, "GET", "/api/peer/"+peerID+"/snapshot", nil, 200)["agents"])
	require.NotEmpty(t, agents, "the peer shares no agents with the sandbox")
	// The tour opens the peer's task on its board, by id.
	taskID := ""
	for _, task := range journeyRows(t, local.call(t, "GET", "/api/peer/"+peerID+"/v1/groups/"+sbPeerGroup+"/taskboard", nil, 200)["tasks"]) {
		if task["title"] == sbPeerTask {
			taskID, _ = task["id"].(string)
		}
	}
	require.NotEmpty(t, taskID, "the peer's task board does not show %q", sbPeerTask)

	seedSandboxStranger(t, store, hubURL, local)
	writeSandboxPeerFixture(t, dir, sandboxPeerFixture{ID: peerID, Name: sbPeerName, Label: sbPeerLabel, Group: sbPeerGroup, Agent: sbPeerAgent, Task: taskID, TaskTitle: sbPeerTask})
	t.Logf("sandbox: live peer %s (%s) at %s, hub at %s", sbPeerName, peerID, peer.url, hubSrv.URL)
}

// seedSandboxStranger connects a bare hub client nobody trusts, so Fleet
// config → Peers has an instance waiting to be trusted and the map has one to
// show under "Show untrusted". It runs until the test's cleanup.
func seedSandboxStranger(t *testing.T, store *hub.Store, hubURL string, local *journeyNode) {
	t.Helper()
	identity, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, store.Admit(identity.ID()))
	cl, err := client.New(client.Options{URL: hubURL, Identity: identity, Name: "guest@laptop"})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { cl.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	journeyAwait(t, "the stranger on the hub", func() bool {
		for _, row := range journeyRows(t, local.call(t, "GET", "/api/federation/status", nil, 200)["peers"]) {
			if row["instance_id"] == identity.ID() {
				return true
			}
		}
		return false
	})
}

// writeSandboxPeerFixture adds the peer to fixture.json, beside whatever the
// fixture wrote there.
func writeSandboxPeerFixture(t *testing.T, dir string, p sandboxPeerFixture) {
	t.Helper()
	path := filepath.Join(dir, "fixture.json")
	doc := map[string]any{}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		require.NoError(t, json.Unmarshal(raw, &doc))
	case !errors.Is(err, os.ErrNotExist):
		t.Fatal(err)
	}
	doc["peer"] = p
	out, err := json.MarshalIndent(doc, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, out, 0o600))
}
