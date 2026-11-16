package agentd_test

// sandbox_serve_test.go is the server behind scripts/sandbox/ui.sh: the real
// dashboard and daemon API over a seeded world in the isolated HOME newFlow sets
// up, for an agent to drive with its own browser before it ships a dashboard
// change. Unlike TestDocShotsServe it runs on the live clock and is writable:
// the operator's cookie is injected, so creating tasks, dragging, drawing and
// sending messages go through the production handlers. Tmux is the flow
// simulator, so nothing reaches the real tmux server, daemon or ~/.tclaude.
// Gated so `go test ./...` never serves anything.

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/claude/remoteaccess"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// sandboxFullFixture is the fixture that fills every page: the docs fleet plus
// seedSandboxFull.
const sandboxFullFixture = "full"

// TestSandboxServe seeds the fixture named by TCLAUDE_SANDBOX_FIXTURE (default
// full; any docshots fixture works too), serves it on 127.0.0.1 at
// TCLAUDE_SANDBOX_PORT (default: a random port), writes the URL to
// <TCLAUDE_SANDBOX_SERVE>/url and serves until a file named stop appears there.
// With TCLAUDE_SANDBOX_PEER set it links a live federation peer first
// (sandbox_peer_test.go).
func TestSandboxServe(t *testing.T) {
	dir := os.Getenv("TCLAUDE_SANDBOX_SERVE")
	if dir == "" {
		t.Skip("dashboard sandbox: run scripts/sandbox/ui.sh up")
	}
	fixture := os.Getenv("TCLAUDE_SANDBOX_FIXTURE")
	if fixture == "" {
		fixture = sandboxFullFixture
	}
	var ln net.Listener
	if port := os.Getenv("TCLAUDE_SANDBOX_PORT"); port != "" {
		// Loopback only: the sandbox has the operator's powers and no login.
		var err error
		if ln, err = net.Listen("tcp", net.JoinHostPort("127.0.0.1", port)); err != nil {
			t.Fatalf("listen on port %s: %v", port, err)
		}
	}
	mux, page := seedDocFixture(t, fixture)
	// TCLAUDE_SANDBOX_CONV_MONITOR=1 runs the daemon's transcript monitor, as
	// a real daemon does, so what a simulated agent writes to its transcript
	// (a Remote Access link) reaches the dashboard. Opt-in: the fixtures seed
	// titles their transcripts do not carry, which its first scan replaces.
	if os.Getenv("TCLAUDE_SANDBOX_CONV_MONITOR") != "" {
		agentd.StartConvMonitorForTest(t, 200*time.Millisecond)
	}
	if os.Getenv("TCLAUDE_SANDBOX_SWIPE_TUI") != "" {
		sandboxSwipeTUI(t)
	} else if os.Getenv("TCLAUDE_SANDBOX_LIVE_SHELL") != "" {
		sandboxLiveShell(t)
	}
	if sandboxLivePeer() {
		seedSandboxLivePeer(t, dir, mux)
	}
	if os.Getenv("TCLAUDE_SANDBOX_REMOTE") != "" {
		sandboxRemoteListener(t, dir)
	}
	serveDocFixture(t, dir, sandboxLogFailures(t, sandboxStageReferer(mux)), page, ln)
}

// sandboxRemoteListener also serves the seeded world on the remote-access
// listener (mTLS, a passphrase login, HTTP/2), with real remote-access material
// in the sandbox's HOME, and writes how to reach it to <dir>/remote.json: the
// URL, the client .p12 and its password, and the passphrase. A measurement
// (phone-boot-measure) boots the phone through it. TCLAUDE_SANDBOX_REMOTE=nocert
// asks for the client certificate without requiring it: Playwright presents
// one through a TLS proxy of its own, which does not hold up under a boot's
// hundreds of requests.
func sandboxRemoteListener(t *testing.T, dir string) {
	t.Helper()
	const passphrase, p12Password = "sandbox-passphrase", "sandbox-p12"
	res, err := remoteaccess.Setup(remoteaccess.SetupOptions{
		Bind: "127.0.0.1:8443", ExtraHosts: []string{"localhost"}, Passphrase: passphrase,
		ClientName: "sandbox-phone", P12Password: p12Password, RegenerateCerts: true,
	})
	if err != nil {
		t.Fatalf("sandbox: remote-access setup: %v", err)
	}
	m, err := remoteaccess.Load()
	if err != nil {
		t.Fatalf("sandbox: remote-access load: %v", err)
	}
	addr := agentd.ServeRemoteListenerForTest(t, m, os.Getenv("TCLAUDE_SANDBOX_REMOTE") == "nocert")
	out, _ := json.Marshal(map[string]string{"url": "https://" + addr, "p12": res.P12Path, "p12Password": p12Password, "passphrase": passphrase})
	if err := os.WriteFile(filepath.Join(dir, "remote.json"), out, 0o600); err != nil {
		t.Fatalf("sandbox: write remote.json: %v", err)
	}
	t.Logf("sandbox: remote-access listener on https://%s", addr)
}

// sandboxLogFailures writes each request answered 400 or above to the test
// log (ui.sh keeps it in sandbox-out/ui/server.log), so a page that fails to
// load in the sandbox says why.
func sandboxLogFailures(t *testing.T, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &sandboxStatus{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		if rec.code >= 400 {
			t.Logf("sandbox: %d %s %s (host %s, origin %q, referer %q)", rec.code, r.Method, r.URL.RequestURI(), r.Host, r.Header.Get("Origin"), r.Referer())
		}
	})
}

type sandboxStatus struct {
	http.ResponseWriter
	code int
}

func (s *sandboxStatus) WriteHeader(code int) { s.code = code; s.ResponseWriter.WriteHeader(code) }

// Unwrap lets http.ResponseController reach the real writer; Flush and Hijack
// serve the handlers that assert those interfaces directly (event streams, the
// drawing boards' live websocket).
func (s *sandboxStatus) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *sandboxStatus) Flush() { _ = http.NewResponseController(s.ResponseWriter).Flush() }

func (s *sandboxStatus) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	s.code = http.StatusSwitchingProtocols
	return http.NewResponseController(s.ResponseWriter).Hijack()
}

// sandboxStageReferer lets the dashboard load inside `tclaude agent show`. The
// stage presents each request as the browser's own to http://localhost:PORT:
// it rewrites Origin, and its pages send no Referer (Referrer-Policy:
// no-referrer). The dashboard pins a request to itself by Origin or Referer,
// so every asset GET would be refused. Here, and only here (a loopback test
// server over invented data whose handler already supplies the operator's
// cookie), a missing or foreign Referer is read as the server's own.
func sandboxStageReferer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") == "" {
			ref, err := url.Parse(r.Referer())
			if err != nil || ref.Host != r.Host {
				r = r.Clone(r.Context())
				r.Header.Set("Referer", "http://"+r.Host+"/")
			}
		}
		next.ServeHTTP(w, r)
	})
}

// sandboxSeeder sends the seeding requests through the production routes: the
// daemon API as the operator, and the dashboard's own /api routes.
type sandboxSeeder struct {
	t    *testing.T
	f    *testharness.Flow
	dash http.Handler
	// fixture is what the browser tests need to know about the seeded world,
	// written to <TCLAUDE_SANDBOX_SERVE>/fixture.json.
	fixture sandboxFixture
}

// sandboxFixture names the seeded entities a deep link opens.
type sandboxFixture struct {
	Group        string `json:"group"`
	Task         string `json:"task"`
	QuestionTask string `json:"questionTask"`
	DrawingBoard string `json:"drawingBoard"`
	ProcessRun   string `json:"processRun"`
	Template     string `json:"template"`
}

func (s *sandboxSeeder) writeFixture() {
	s.t.Helper()
	dir := os.Getenv("TCLAUDE_SANDBOX_SERVE")
	if dir == "" {
		return
	}
	raw, err := json.MarshalIndent(s.fixture, "", "  ")
	if err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixture.json"), raw, 0o600); err != nil {
		s.t.Fatal(err)
	}
}

// api calls the daemon API as the operator and decodes a 2xx reply into out.
func (s *sandboxSeeder) api(method, path string, body, out any) {
	s.t.Helper()
	s.check(method, path, testharness.Serve(s.f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(s.t, method, path, body))), out)
}

// dashAPI calls a dashboard /api route, as the browser does.
func (s *sandboxSeeder) dashAPI(method, path string, body, out any) {
	s.t.Helper()
	req := testharness.JSONRequest(s.t, method, path, body)
	req.Header.Set("Origin", "http://127.0.0.1:0")
	s.check(method, path, testharness.Serve(s.dash, req), out)
}

// upload posts files as multipart "file" parts, beside an optional JSON part.
func (s *sandboxSeeder) upload(path, jsonPart string, part any, files map[string][]byte, out any) {
	s.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if part != nil {
		raw, err := json.Marshal(part)
		if err != nil {
			s.t.Fatal(err)
		}
		if err := mw.WriteField(jsonPart, string(raw)); err != nil {
			s.t.Fatal(err)
		}
	}
	for name, data := range files {
		w, err := mw.CreateFormFile("file", name)
		if err != nil {
			s.t.Fatal(err)
		}
		_, _ = w.Write(data)
	}
	if err := mw.Close(); err != nil {
		s.t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	s.check(http.MethodPost, path, testharness.Serve(s.f.Mux, agentd.AsHumanPeer(req)), out)
}

func (s *sandboxSeeder) check(method, path string, rec *httptest.ResponseRecorder, out any) {
	s.t.Helper()
	if rec.Code < 200 || rec.Code >= 300 {
		s.t.Fatalf("sandbox seed %s %s: %d %s", method, path, rec.Code, rec.Body.String())
	}
	if out != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			s.t.Fatalf("sandbox seed %s %s: decode: %v", method, path, err)
		}
	}
}

// seedSandboxFull adds what the docs fleet leaves empty, so every page and tab
// has something realistic on it: a busy task board, drawing boards with
// drawings, automations of each kind, a process run, access grants and a
// linked federation peer.
func seedSandboxFull(t *testing.T, f *testharness.Flow) {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if cfg.Features == nil {
		cfg.Features = &config.FeaturesConfig{}
	}
	cfg.Features.Taskboard = new(true)
	cfg.Features.GroupsRouteMap = true
	if cfg.Agent == nil {
		cfg.Agent = &config.AgentConfig{}
	}
	cfg.Agent.DefaultPermissions = []string{"self.page", "self.pr", "self.rename", "self.schedule", "self.task"}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("config: %v", err)
	}
	s := &sandboxSeeder{t: t, f: f, dash: agentd.BuildDashboardHandlerForTest()}
	seedSandboxDrawingBoards(s)
	seedSandboxTaskboard(s)
	seedSandboxFleetState(s)
	seedSandboxLineage(s)
	seedSandboxAutomations(s)
	seedSandboxCronHistory(s)
	seedSandboxProcessRuns(s)
	seedSandboxAgreementGate(s)
	seedSandboxOversight(s)
	seedSandboxAccess(t)
	seedSandboxAttachment(s)
	seedSandboxOfflineSender(t)
	if !sandboxLivePeer() {
		seedSandboxFederation(t)
	}
	seedSandboxUsageLimit(s)
	seedSandboxPlugin(s)
	seedSandboxLog(t)
	seedSandboxAccessRequest(s)
	seedSandboxRemoteAccess(s)
	s.writeFixture()
}

// seedSandboxRemoteAccess arms Claude Code's Remote Access in checkout-ui's
// pane and reads it once through the dashboard route, as the operator's
// status read does: its menus then offer "Open in Claude app ↗" with the
// session link, and the other Claude Code agents "Turn on Remote Access &
// open".
func seedSandboxRemoteAccess(s *sandboxSeeder) {
	cc := s.f.World.CCs.GetByConvID(sbUI)
	if cc == nil {
		s.t.Fatalf("sandbox: checkout-ui has no Claude Code pane")
	}
	cc.Receive("/remote-control")
	cc.Receive("Enter")
	// The link comes from the bridge_status record Claude Code writes to its
	// transcript, which the follower records on the session row; the sandbox
	// records it as the follower would.
	if _, err := db.RecordSessionRCLink(sbUI, db.RCSessionURLPrefix+"session_01SandboxCheckoutUI", time.Now()); err != nil {
		s.t.Fatalf("sandbox: recording checkout-ui's Remote Access link: %v", err)
	}
	var st struct {
		SessionURL string `json:"session_url"`
	}
	s.dashAPI(http.MethodPost, "/api/agents/"+sbUI+"/remote-control", map[string]any{"intent": "link"}, &st)
	if st.SessionURL == "" {
		s.t.Fatalf("sandbox: checkout-ui's Remote Access link was not read")
	}
}

// The docs fleet's agents, by conversation.
const (
	sbLead     = "d0c00000-0000-4000-8000-000000000001"
	sbUI       = "d0c00000-0000-4000-8000-000000000002"
	sbSearch   = "d0c00000-0000-4000-8000-000000000003"
	sbCopy     = "d0c00000-0000-4000-8000-000000000004"
	sbPayments = "d0c00000-0000-4000-8000-000000000005"
	sbReviewer = "d0c00000-0000-4000-8000-000000000006"
	// sbPerf is the docs fleet's offline agent (perf-audit).
	sbPerf = "d0c00000-0000-4000-8000-000000000007"
	// sbMigrations is the sandbox's own errored agent (seedSandboxFleetState).
	sbMigrations = "d0c00000-0000-4000-8000-000000000008"
)

// sbShape is one Excalidraw element.
func sbShape(id, kind string, x, y, w, h float64, text string) map[string]any {
	el := map[string]any{"id": id, "type": kind, "x": x, "y": y, "width": w, "height": h,
		"version": 1, "versionNonce": len(id), "strokeColor": "#1e1e1e", "backgroundColor": "transparent",
		"fillStyle": "solid", "strokeWidth": 2, "roughness": 1, "opacity": 100}
	if text != "" {
		el["text"], el["originalText"], el["fontSize"], el["fontFamily"] = text, text, 20, 1
	}
	return el
}

func seedSandboxDrawingBoards(s *sandboxSeeder) {
	s.t.Helper()
	for _, b := range []struct {
		id, title, group string
		elements         []any
	}{
		{"checkout-flow", "Checkout flow", "webshop", []any{
			sbShape("cart", "rectangle", 0, 0, 160, 70, ""), sbShape("cart-label", "text", 40, 22, 80, 25, "Cart"),
			sbShape("address", "rectangle", 240, 0, 160, 70, ""), sbShape("address-label", "text", 270, 22, 100, 25, "Address"),
			sbShape("payment", "rectangle", 480, 0, 160, 70, ""), sbShape("payment-label", "text", 505, 22, 110, 25, "Payment"),
			sbShape("note", "ellipse", 240, 140, 160, 80, ""), sbShape("note-label", "text", 262, 168, 120, 25, "New step"),
		}},
		{"search-architecture", "Search architecture", "webshop", []any{
			sbShape("index", "rectangle", 0, 0, 180, 80, ""), sbShape("index-label", "text", 30, 25, 120, 25, "Index v2"),
			sbShape("ranker", "diamond", 260, -10, 160, 100, ""), sbShape("ranker-label", "text", 295, 28, 90, 25, "Rerank"),
		}},
		{"release-plan", "Release 2.4 plan", "webshop-release-2.4", nil},
	} {
		if s.fixture.DrawingBoard == "" {
			s.fixture.DrawingBoard = b.id
		}
		s.dashAPI(http.MethodPost, "/api/drawing-boards", map[string]any{"id": b.id, "title": b.title, "group": b.group}, nil)
		if len(b.elements) > 0 {
			s.dashAPI(http.MethodPatch, "/api/drawing-boards/"+b.id, map[string]any{"elements": b.elements}, nil)
		}
	}
}

type sbTask struct {
	ID string `json:"id"`
}

func seedSandboxTaskboard(s *sandboxSeeder) {
	s.t.Helper()
	const board = "/v1/groups/webshop/taskboard"
	for _, label := range []string{"In review", "QA"} {
		s.api(http.MethodPost, board+"/columns", map[string]any{"label": label}, nil)
	}
	pr := func(n int) string { return fmt.Sprintf("%s/pull/%d", docRepo, n) }
	tasks := []map[string]any{
		// Ideas and the queue, unassigned.
		{"title": "Dark mode for the storefront", "body": "Customers ask for it. Start with the **product page** and the cart."},
		{"title": "Gift cards", "body": "Needs a payments design first; see the checkout flow drawing."},
		{"title": "Faster image CDN", "stage": "idea", "ref": "https://linear.app/acme/issue/SHOP-230", "ref_label": "SHOP-230"},
		{"title": "Saved carts across devices", "stage": "next", "body": "- sync on login\n- merge on conflict\n- keep for 30 days"},
		{"title": "Accessibility pass on checkout", "stage": "next", "ref": "https://linear.app/acme/issue/SHOP-224", "ref_label": "SHOP-224"},
		{"title": "Remove the legacy search endpoint", "stage": "next", "after": []string{}},
		// In progress, held by the fleet.
		{"title": "Checkout page redesign", "to": sbUI, "ref": pr(1482), "add_refs": []string{"https://linear.app/acme/issue/SHOP-198"},
			"body": "The address step is done; the payment step stays as is."},
		{"title": "Search relevance v2", "to": sbSearch, "ref": pr(1477), "add_refs": []string{pr(1491), "https://linear.app/acme/issue/SHOP-205"}},
		{"title": "Onboarding copy refresh", "to": sbCopy, "ref": pr(1488)},
		{"title": "Retry failed card payments", "to": sbPayments, "ref": pr(1469), "body": "CI fails on the retry back-off test."},
		{"title": "Plan the 2.4 release", "to": sbLead, "start": false, "stage": "next"},
		// Blocked and done.
		{"title": "Upgrade the payment SDK", "status": "blocked", "block_reason": "Waiting for the vendor's 5.0 release."},
		{"title": "Ship the price-filter fix", "status": "done", "to": sbUI, "ref": pr(1455)},
		{"title": "Fix the cart badge count", "status": "done", "to": sbCopy},
		{"title": "Add search telemetry", "status": "done", "to": sbSearch, "ref": pr(1440)},
		{"title": "Old banner experiment", "status": "done"},
		// The backlog: parked, hidden until shown (tk_5d2547b0).
		{"title": "Loyalty points", "stage": "backlog", "ref": "https://linear.app/acme/issue/SHOP-118", "ref_label": "SHOP-118"},
		{"title": "Migrate image storage to R2", "stage": "backlog", "body": "After the CDN move; not before Q3."},
	}
	ids := make([]string, len(tasks))
	for i, body := range tasks {
		var out sbTask
		s.api(http.MethodPost, board+"/tasks", body, &out)
		ids[i] = out.ID
	}
	s.fixture.Group, s.fixture.Task = "webshop", ids[6]
	// The custom columns hold a PR waiting for review and one in QA.
	s.api(http.MethodPost, "/v1/taskboard/tasks/"+ids[6]+"/stage", map[string]any{"stage": "in-review"}, nil)
	s.api(http.MethodPost, "/v1/taskboard/tasks/"+ids[8]+"/stage", map[string]any{"stage": "qa"}, nil)
	// One done task is archived, so the archive has a row.
	s.api(http.MethodPost, "/v1/taskboard/tasks/"+ids[15]+"/archive", map[string]any{}, nil)
	// Epics (tk_0a821784): two open ones holding tasks, one still an idea.
	var checkout, search sbTask
	s.api(http.MethodPost, board+"/tasks", map[string]any{"title": "Checkout revamp", "kind": "epic", "stage": "next",
		"body": "Make checkout fast and accessible on mobile before the 2.4 release."}, &checkout)
	s.api(http.MethodPost, board+"/tasks", map[string]any{"title": "Search v2", "kind": "epic", "stage": "next"}, &search)
	s.api(http.MethodPost, board+"/tasks", map[string]any{"title": "Storefront personalisation", "kind": "epic"}, nil)
	for epic, children := range map[string][]string{checkout.ID: {ids[3], ids[4], ids[6], ids[13]}, search.ID: {ids[5], ids[7], ids[14]}} {
		for _, id := range children {
			s.api(http.MethodPatch, "/v1/taskboard/tasks/"+id, map[string]any{"parent": epic}, nil)
		}
	}

	// A question for the operator, with choices.
	var question sbTask
	s.api(http.MethodPost, board+"/tasks", map[string]any{
		"title": "Which carrier for international shipping?", "to": "operator",
		"question": map[string]any{
			"kind": "question", "title": "Which carrier for international shipping?",
			"body":      "Both cover the EU. DHL is cheaper per parcel; UPS has better tracking.",
			"options":   []map[string]any{{"key": "dhl", "label": "DHL"}, {"key": "ups", "label": "UPS"}, {"key": "both", "label": "Both, by region"}},
			"recommend": "dhl",
		},
	}, &question)
	s.fixture.QuestionTask = question.ID
	// An operator task another task waits on.
	s.api(http.MethodPost, board+"/tasks", map[string]any{
		"title": "Approve the new payment provider contract", "to": "operator", "blocks": ids[11],
	}, nil)

	// Files: an image and a text file on the redesign, and a drawing of the
	// checkout flow.
	s.upload("/v1/taskboard/tasks/"+ids[6]+"/attachments", "", nil, map[string][]byte{
		"notes.md": []byte("# Checkout redesign\n\n- address autocomplete\n- one-page summary\n"),
	}, nil)
	s.api(http.MethodPost, "/v1/taskboard/tasks/"+ids[6]+"/drawings", map[string]any{"board": "checkout-flow"}, nil)
	s.api(http.MethodPost, "/v1/taskboard/tasks/"+ids[7]+"/drawings", map[string]any{"board": "search-architecture", "comment": "The reranker sits after the index."}, nil)
	for _, c := range []struct{ task, body string }{
		{ids[6], "Looks good. Keep the payment step unchanged until payments-fix lands."},
		{ids[9], "The back-off test is flaky on CI; please fix it before review."},
		{ids[0], "Let's do this after the 2.4 release."},
	} {
		s.api(http.MethodPost, "/v1/taskboard/tasks/"+c.task+"/comments", map[string]any{"body": c.body}, nil)
	}
	// A quieter board in the release group.
	for _, title := range []string{"Write the 2.4 release notes", "Tag 2.4.0-rc1", "Smoke test the release candidate"} {
		s.api(http.MethodPost, "/v1/groups/webshop-release-2.4/taskboard/tasks", map[string]any{"title": title, "stage": "next"}, nil)
	}
}

func seedSandboxAutomations(s *sandboxSeeder) {
	s.t.Helper()
	for _, c := range []map[string]any{
		{"target": "group:webshop", "interval": "30m", "name": "standup-ping", "body": "Post a one-line status to the lead."},
		{"target": sbLead, "cron_expr": "0 9 * * 1-5", "name": "morning-plan", "body": "Plan the day from the task board."},
		{"target": sbReviewer, "interval": "2h", "name": "review-sweep", "body": "Review any PR waiting in In review.", "enabled": false},
	} {
		s.dashAPI(http.MethodPost, "/api/cron", c, nil)
	}
	s.dashAPI(http.MethodPost, "/api/standing-orders", map[string]any{
		"name": "reread-guidelines", "row_version": 0, "target": "global",
		"summary": "Re-read CONTRIBUTING.md after a compact.", "trigger_event": "session.start",
		"sources": []string{"compact", "resume"}, "timing": "same-continuation",
		"cadence": "always", "cooldown_seconds": 60, "enabled": true,
	}, nil)
	s.dashAPI(http.MethodPost, "/api/triggers", map[string]any{
		"name": "nudge-waiting-agents", "enabled": true, "scope": "global",
		"source": db.TriggerSourceAgentAwaitingInput, "draft_filter": db.TriggerDraftInclude,
		"for_seconds": 300, "actions": []map[string]any{{"type": "message", "message": map[string]any{
			"target": "agent", "body_template": "You have been waiting for input for five minutes ({{agent.id}}).",
		}}},
	}, nil)
	for _, j := range []db.ExportJob{
		{ConvID: sbLead, GroupName: "webshop", Title: "Sprint summary", Preset: "summary", Status: "done",
			ArtifactName: "sprint-summary.md", ArtifactSize: 2048, ContentType: "text/markdown"},
		{ConvID: sbSearch, GroupName: "webshop", Title: "Search v2 design notes", Preset: "summary", Status: "running"},
	} {
		j.CreatedAt, j.UpdatedAt = time.Now().Add(-25*time.Minute), time.Now().Add(-3*time.Minute)
		if _, err := db.InsertExportJob(&j); err != nil {
			s.t.Fatalf("export job: %v", err)
		}
	}
}

func seedSandboxProcessRuns(s *sandboxSeeder) {
	s.t.Helper()
	// release-train waits on its first node, a human decision.
	s.api(http.MethodPost, "/v1/process/runs", map[string]any{
		"id": "run_release_24", "templateId": "release-train", "group": "webshop", "authorizeProgramProfiles": []string{"safe"},
	}, nil)
	s.fixture.ProcessRun, s.fixture.Template = "run_release_24", "release-train"
	// An older run whose decision is already taken, so a run shows a finished
	// step; run_release_24 stays waiting for the browser tests to decide.
	s.api(http.MethodPost, "/v1/process/runs", map[string]any{
		"id": "run_release_23", "templateId": "release-train", "group": "webshop", "authorizeProgramProfiles": []string{"safe"},
	}, nil)
	s.api(http.MethodPost, "/v1/process/runs/run_release_23/decide", map[string]any{
		"nodeId": "checks-pass", "verdict": "yes", "evidence": "All release checks are green.",
	}, nil)
}

// seedSandboxOversight raises two concerns on run_release_24, which the
// webshop lead oversees: checkout-ui's, which the lead passed up so it waits
// on the operator's decision card, and search's, still with the lead.
func seedSandboxOversight(s *sandboxSeeder) {
	s.t.Helper()
	var raised struct {
		Concern struct {
			ID int64 `json:"id"`
		} `json:"concern"`
	}
	s.agentAPI(sbUI, http.MethodPost, "/v1/process/runs/run_release_24/concerns", map[string]any{
		"nodeId": "checks-pass",
		"reason": "The release checks pass, but the payment provider's sandbox has been down since this morning, so checkout was never tested end to end.",
	}, &raised)
	s.agentAPI(sbLead, http.MethodPost, fmt.Sprintf("/v1/process/runs/run_release_24/concerns/%d/answer", raised.Concern.ID), map[string]any{
		"action": "escalate", "note": "Shipping without an end-to-end checkout test is a product call.",
	}, nil)
	s.agentAPI(sbSearch, http.MethodPost, "/v1/process/runs/run_release_24/concerns", map[string]any{
		"reason": "The search index rebuild in this release takes about 40 minutes; we may want a maintenance window.",
	}, nil)
}

// agentAPI calls a daemon route as the agent conv, as its CLI does.
func (s *sandboxSeeder) agentAPI(conv, method, path string, body, out any) {
	s.t.Helper()
	s.check(method, path, testharness.Serve(s.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(s.t, method, path, body), conv)), out)
}

// seedSandboxAgreementGate stores a three-person agreement gate and runs it
// twice: run_ui_review_2 has two approvals and waits on the third person
// (the tally "2/3 agreed"), and run_ui_review_1 is agreed and done.
func seedSandboxAgreementGate(s *sandboxSeeder) {
	s.t.Helper()
	fs, err := store.NewFS(filepath.Join(s.f.World.HomeDir, ".tclaude", "processes"))
	if err != nil {
		s.t.Fatalf("process store: %v", err)
	}
	ask := func(text string) model.Performer { return model.Performer{Kind: model.PerformerHuman, Ask: text} }
	if _, err := fs.PutTemplate(s.t.Context(), &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "ui-review", Name: "UI review",
		Description: "Design, product and support must all approve a UI change.",
		Start:       "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{"next": "sign-off"}},
			"sign-off": {Type: model.NodeTypeDecision, Name: "Sign-off", Agree: "approve",
				Deciders: []model.Performer{
					ask("Design: does the new filter bar read well?"),
					ask("Product: is this the behaviour we want?"),
					ask("Support: will users understand it?"),
				},
				Next: model.Next{"approve": "shipped", "changes": "reworked"}},
			"shipped":  {Type: model.NodeTypeEnd, Name: "Shipped", Result: "success"},
			"reworked": {Type: model.NodeTypeEnd, Name: "Sent back", Result: "canceled"},
		},
	}); err != nil {
		s.t.Fatalf("process template: %v", err)
	}
	votes := map[string]int{"run_ui_review_1": 3, "run_ui_review_2": 2}
	for _, runID := range []string{"run_ui_review_1", "run_ui_review_2"} {
		s.api(http.MethodPost, "/v1/process/runs", map[string]any{
			"id": runID, "templateId": "ui-review", "group": "webshop", "authorizeProgramProfiles": []string{},
		}, nil)
		for k := 1; k <= votes[runID]; k++ {
			s.api(http.MethodPost, "/v1/process/runs/"+runID+"/decide", map[string]any{
				"nodeId": model.GateVoteID("sign-off", k), "verdict": "approve", "evidence": "Looks right to me.",
			}, nil)
		}
	}
}

func seedSandboxAccess(t *testing.T) {
	t.Helper()
	if err := db.GrantAgentPermission(sbLead, agentd.PermTaskboardWrite, "operator"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	agentID, err := db.AgentIDForConv(sbPayments)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertSudoGrantForAgent(&db.SudoGrant{
		AgentID: agentID, ConvID: sbPayments, Slug: "agent.spawn", GrantedAt: time.Now().Add(-10 * time.Minute),
		ExpiresAt: time.Now().Add(50 * time.Minute), GrantedBy: "operator", Reason: "spawn a test runner",
	}); err != nil {
		t.Fatalf("sudo grant: %v", err)
	}
}

// sandboxLivePeer reports `ui.sh up --peer`: a live peer replaces the
// unreachable one seedSandboxFederation trusts.
func sandboxLivePeer() bool { return os.Getenv("TCLAUDE_SANDBOX_PEER") != "" }

// seedSandboxFederation trusts one peer node, so the map and the fleet pages
// list it. There is no hub and nothing answers on its side, so it shows as
// unreachable and the hub-backed fleet pages stay empty; `ui.sh up --peer`
// links a live one instead (seedSandboxLivePeer).
func seedSandboxFederation(t *testing.T) {
	t.Helper()
	peer, err := proto.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.TrustFederationPeer(db.FederationPeer{
		InstanceID: peer.ID(), PubKey: peer.Pub, Label: "ci-runner", Name: "ci@runner", TrustedAt: time.Now().Add(-72 * time.Hour),
	}); err != nil {
		t.Fatalf("federation peer: %v", err)
	}
}

func seedSandboxUsageLimit(s *sandboxSeeder) {
	s.t.Helper()
	s.dashAPI(http.MethodPost, "/api/usage/limits", map[string]any{"scope": "global", "rule": map[string]any{
		"name": "monthly-cap", "period": "month", "budget": 400,
		"steps": []any{map[string]any{"at": 100, "action": "block_spawn"}},
	}}, nil)
}

// seedSandboxLog writes a few daemon log lines for Diagnostics → Logs.
func seedSandboxLog(t *testing.T) {
	t.Helper()
	path := common.OutputLogPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i, l := range []struct{ level, msg, extra string }{
		{"INFO", "agentd started", `"pid":4242`},
		{"INFO", "dashboard listening", `"addr":"127.0.0.1:7777"`},
		{"WARN", "nudge failed; queued for retry", `"to":"checkout-ui","retry_in":"30s"`},
		{"INFO", "cron fired", `"job":"standup-ping"`},
		{"ERROR", "gh pr checks failed", `"repo":"acme/webshop","pr":1469,"error":"exit status 1"`},
		{"INFO", "process run started", `"run":"run_release_24"`},
	} {
		at := time.Now().Add(time.Duration(i-6) * 7 * time.Minute).Format(time.RFC3339Nano)
		fmt.Fprintf(&b, `{"time":%q,"level":%q,"msg":%q,%s}`+"\n", at, l.level, l.msg, l.extra)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// seedSandboxPlugin installs a plugin, which also shows the Plugins tab (it is
// hidden while nothing is installed). Its checks only run `true`.
func seedSandboxPlugin(s *sandboxSeeder) {
	s.t.Helper()
	s.dashAPI(http.MethodPost, "/api/plugins", map[string]any{
		"name":  "webshop-dev-env",
		"descr": "Node and browsers for webshop",
		"steps": []map[string]any{{"name": "node-toolchain", "check": "true"}, {"name": "playwright-browsers", "check": "true"}},
	}, nil)
	// One whose second step's check fails, and one the operator switched off.
	s.dashAPI(http.MethodPost, "/api/plugins", map[string]any{
		"name":  "payments-sandbox",
		"descr": "Stripe test mode for checkout",
		"steps": []map[string]any{{"name": "stripe-cli", "check": "true"}, {"name": "test-webhooks", "check": "echo 'webhook endpoint not listening on :4242'; false", "run": "false"}},
	}, nil)
	s.dashAPI(http.MethodPost, "/api/plugins", map[string]any{
		"name":  "search-cluster",
		"descr": "Local OpenSearch",
		"steps": []map[string]any{{"name": "opensearch", "check": "false", "run": "false", "stop": "true"}},
	}, nil)
	s.dashAPI(http.MethodPost, "/api/plugins/check", nil, nil)
	s.dashAPI(http.MethodPost, "/api/plugins/search-cluster/deactivate", nil, nil)
}

// seedSandboxFleetState gives the docs fleet's agents their context-window
// use, and adds one agent whose last turn failed.
func seedSandboxFleetState(s *sandboxSeeder) {
	s.t.Helper()
	g, err := db.GetAgentGroupByName("webshop")
	if err != nil || g == nil {
		s.t.Fatalf("webshop group: %v", err)
	}
	seedDocAgent(s.t, s.f, g.ID, "webshop", docAgent{
		conv: sbMigrations, title: "db-migrations", descr: "Orders table migration", role: "dev",
		harness: "claude", model: "Sonnet 5.5", effort: "medium", status: "error",
		detail: "API Error: 529 overloaded_error", online: true, cost: 3.40,
		startBranch: "orders-migration", age: 70 * time.Minute, last: 6 * time.Minute,
	})
	for title, pct := range map[string]float64{
		"lead": 64, "checkout-ui": 38, "search-indexer": 87, "onboarding-copy": 12,
		"payments-fix": 52, "cold-reviewer": 21, "db-migrations": 93,
	} {
		if err := db.UpdateContextPct("lbl-"+title, pct); err != nil {
			s.t.Fatalf("context %s: %v", title, err)
		}
	}
}

// seedSandboxLineage has lead spawn four of webshop's workers (agent_lineage),
// so the phone's Agents view nests them under it; search-indexer and
// onboarding-copy were added by the operator and stay at the top level.
func seedSandboxLineage(s *sandboxSeeder) {
	s.t.Helper()
	lead, err := db.AgentIDForConv(sbLead)
	if err != nil || lead == "" {
		s.t.Fatalf("lead agent: %q %v", lead, err)
	}
	for i, conv := range []string{sbUI, sbPayments, sbReviewer, sbMigrations} {
		child, err := db.AgentIDForConv(conv)
		if err != nil || child == "" {
			s.t.Fatalf("worker agent %s: %q %v", conv, child, err)
		}
		if err := db.RecordAgentLineage(child, lead, time.Now().Add(time.Duration(i-4)*time.Hour)); err != nil {
			s.t.Fatalf("lineage %s: %v", conv, err)
		}
	}
}

// seedSandboxCronHistory gives the cron jobs a run history: morning-plan fires
// now through the dashboard's run-now (delivered, ok), and standup-ping keeps
// a failed run from earlier in the day.
func seedSandboxCronHistory(s *sandboxSeeder) {
	s.t.Helper()
	jobs, err := db.ListAgentCronJobs()
	if err != nil {
		s.t.Fatal(err)
	}
	ids := map[string]int64{}
	for _, j := range jobs {
		ids[j.Name] = j.ID
	}
	if ids["morning-plan"] == 0 || ids["standup-ping"] == 0 {
		s.t.Fatalf("cron jobs not seeded: %v", ids)
	}
	s.dashAPI(http.MethodPost, fmt.Sprintf("/api/cron/%d/run-now", ids["morning-plan"]), nil, nil)
	failed := time.Now().Add(-3 * time.Hour)
	if _, err := db.InsertAgentCronRun(&db.AgentCronRun{
		JobID: ids["standup-ping"], FiredAt: failed, Status: "send_failed",
		ErrorMsg: "queue message for checkout-ui: database is locked",
	}); err != nil {
		s.t.Fatal(err)
	}
	if err := db.UpdateAgentCronJobLastRun(ids["standup-ping"], failed, "send_failed"); err != nil {
		s.t.Fatal(err)
	}
}

// seedSandboxAttachment has checkout-ui send the operator a screenshot through
// notify-human's attachment upload.
// seedSandboxOfflineSender leaves a read notification from perf-audit, which
// is offline: the Mail reader's Find › finds it in Agents to power it on
// (smoke phone-agent-find). Read and a day old, so it sits below the rest.
func seedSandboxOfflineSender(t *testing.T) {
	t.Helper()
	at := time.Now().Add(-26 * time.Hour)
	if _, err := db.InsertHumanMessageWithAttachments(&db.HumanMessage{
		FromConv: sbPerf, FromTitle: "perf-audit", GroupName: "webshop",
		Subject:   "Lighthouse budget: paused",
		Body:      "The budget run is paused until the image CDN change lands. Power me on when it does.",
		CreatedAt: at, ReadAt: at,
	}, nil); err != nil {
		t.Fatalf("offline sender's notification: %v", err)
	}
}

func seedSandboxAttachment(s *sandboxSeeder) {
	s.t.Helper()
	if err := db.GrantAgentPermission(sbUI, agentd.PermHumanNotify, "operator"); err != nil {
		s.t.Fatalf("grant: %v", err)
	}
	// notify-human names the sender from a fresh read of its transcript, and
	// the docs fleet keeps titles in conv_index only. Write the title where
	// Claude Code's /rename does, or the reread reverts it to the summary.
	row, err := db.GetConvIndex(sbUI)
	if err != nil || row == nil || row.FullPath == "" {
		s.t.Fatalf("checkout-ui transcript: %v", err)
	}
	line, err := json.Marshal(map[string]string{"type": "custom-title", "customTitle": "checkout-ui", "sessionId": sbUI})
	if err != nil {
		s.t.Fatal(err)
	}
	transcript, err := os.OpenFile(row.FullPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		s.t.Fatal(err)
	}
	_, err = transcript.Write(append(line, '\n'))
	if cerr := transcript.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		s.t.Fatal(err)
	}
	metadata, err := json.Marshal(map[string]string{
		"subject": "Checkout buttons: A or B?",
		"body":    "Two takes on the address step's buttons. I lean to B: the primary action reads first on a phone.",
		"name":    "checkout-buttons.png",
	})
	if err != nil {
		s.t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/notify-human/attachment", bytes.NewReader(sandboxButtonsPNG(s.t)))
	req.Header.Set("Content-Type", "image/png")
	req.Header.Set("X-Tclaude-Notify-Metadata", base64.RawURLEncoding.EncodeToString(metadata))
	s.check(http.MethodPost, "/v1/notify-human/attachment", testharness.Serve(s.f.Mux, agentd.AsAgentPeer(req, sbUI)), nil)
}

// sandboxButtonsPNG draws two button layouts side by side.
func sandboxButtonsPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 640, 360))
	fill := func(r image.Rectangle, c color.RGBA) { draw.Draw(img, r, &image.Uniform{c}, image.Point{}, draw.Src) }
	fill(img.Bounds(), color.RGBA{0xf4, 0xf5, 0xf7, 0xff})
	for i, primaryFirst := range []bool{false, true} {
		x := 20 + i*320
		fill(image.Rect(x, 20, x+280, 340), color.RGBA{0xff, 0xff, 0xff, 0xff})
		primary, secondary := image.Rect(x+150, 270, x+260, 310), image.Rect(x+20, 270, x+130, 310)
		if primaryFirst {
			primary, secondary = image.Rect(x+20, 220, x+260, 260), image.Rect(x+20, 270, x+260, 310)
		}
		for row := 0; row < 4; row++ {
			fill(image.Rect(x+20, 50+row*40, x+260, 76+row*40), color.RGBA{0xe3, 0xe6, 0xeb, 0xff})
		}
		fill(secondary, color.RGBA{0xd0, 0xd4, 0xdb, 0xff})
		fill(primary, color.RGBA{0x2f, 0x6f, 0xeb, 0xff})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// seedSandboxAccessRequest has cold-reviewer ask for sudo through the real
// approvals registry, so Access shows a request waiting for the operator. The
// request blocks for its popup timeout, which the sandbox config stretches to
// a day; deciding it in the browser ends it as it would in production.
func seedSandboxAccessRequest(s *sandboxSeeder) {
	s.t.Helper()
	cfg, err := config.Load()
	if err != nil {
		s.t.Fatal(err)
	}
	if cfg.Agent == nil {
		cfg.Agent = &config.AgentConfig{}
	}
	if cfg.Agent.Sudo == nil {
		cfg.Agent.Sudo = &config.SudoConfig{}
	}
	cfg.Agent.Sudo.PopupTimeout = "24h"
	if err := config.Save(cfg); err != nil {
		s.t.Fatal(err)
	}
	// The gate refuses a popup without a dashboard URL. Set one only while the
	// request is raised: the dashboard pins browser requests to it, and the
	// sandbox's real URL is not known yet (seedDocFixture's warm-up does the same).
	restore := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	defer restore()
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := agentd.AsAgentPeer(testharness.JSONRequest(s.t, http.MethodPost, "/v1/sudo", map[string]any{
			"slugs": []string{agentd.PermAgentSpawn}, "duration": "30m",
			"reason": "spawn a second reviewer for the payments PR",
		}), sbReviewer)
		// No t.Logf: an operator's decision can land as the server stops, and
		// logging after the test returned panics.
		_ = testharness.Serve(s.f.Mux, req)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var snap struct {
			Pending int `json:"access_requests_pending"`
		}
		s.dashAPI(http.MethodGet, "/api/snapshot", nil, &snap)
		if snap.Pending > 0 {
			return
		}
		select {
		case <-done:
			s.t.Fatal("sandbox seed: the sudo request ended before it was pending")
		default:
		}
		if time.Now().After(deadline) {
			s.t.Fatal("sandbox seed: the sudo request never became pending")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// sandboxSwipeTUI (TCLAUDE_SANDBOX_SWIPE_TUI=1) attaches every terminal to
// a stand-in for Claude Code's fullscreen view (this binary, see
// RunSwipeTUIForTest), in a
// tmux server of the sandbox's own (its socket in the throwaway TMPDIR), with
// the client flags a dashboard terminal uses. It is how a phone swipe is
// measured in a real browser (scripts/sandbox/smoke/tests/phone-swipe-measure.spec.mjs).
func sandboxSwipeTUI(t *testing.T) {
	t.Helper()
	sandboxOwnTmux(t, "swipe", 46, 38, agentd.SwipeTUICommandForTest())
}

// sandboxLiveShell (TCLAUDE_SANDBOX_LIVE_SHELL=1) attaches every terminal to
// a real interactive shell, in a tmux server of the sandbox's own: how a
// terminal opening is seen working in a real browser (the simulated panes
// have nothing to attach to).
func sandboxLiveShell(t *testing.T) {
	t.Helper()
	sandboxOwnTmux(t, "shell", 46, 38, "env PS1='sandbox$ ' bash --noprofile --norc -i")
}

// sandboxOwnTmux starts command in session name of a tmux server of the
// sandbox's own (its socket in the throwaway TMPDIR) and points every
// dashboard terminal's attach at it, with the client flags a dashboard
// terminal uses.
func sandboxOwnTmux(t *testing.T, name string, width, height int, command string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "sw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	tmux := func(args ...string) *exec.Cmd {
		cmd := exec.Command("tmux", append([]string{"-u", "-S", sock, "-f", "/dev/null"}, args...)...)
		cmd.Env = append(os.Environ(), "TMUX=")
		return cmd
	}
	if out, err := tmux("new-session", "-d", "-s", name, "-x", fmt.Sprint(width), "-y", fmt.Sprint(height), command).CombinedOutput(); err != nil {
		t.Fatalf("sandbox tmux %s: %v: %s", name, err, out)
	}
	t.Cleanup(func() { _ = tmux("kill-server").Run() })
	attach := fmt.Sprintf("TMUX= exec tmux -u -S %s %s attach-session -f ignore-size -t %s", sock, agentd.WebTerminalTmuxFlagsForTest(), name)
	t.Cleanup(agentd.SetTermWSHookForTest(&agentd.TermWSHook{
		RewriteCommand: func(_, session string) (string, string) { return attach, "" },
	}))
}
