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
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
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
	if sandboxLivePeer() {
		seedSandboxLivePeer(t, dir, mux)
	}
	serveDocFixture(t, dir, sandboxLogFailures(t, sandboxStageReferer(mux)), page, ln)
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
	seedSandboxAutomations(s)
	seedSandboxProcessRuns(s)
	seedSandboxAccess(t)
	if !sandboxLivePeer() {
		seedSandboxFederation(t)
	}
	seedSandboxUsageLimit(s)
	seedSandboxPlugin(s)
	seedSandboxLog(t)
	s.writeFixture()
}

// The docs fleet's agents, by conversation.
const (
	sbLead     = "d0c00000-0000-4000-8000-000000000001"
	sbUI       = "d0c00000-0000-4000-8000-000000000002"
	sbSearch   = "d0c00000-0000-4000-8000-000000000003"
	sbCopy     = "d0c00000-0000-4000-8000-000000000004"
	sbPayments = "d0c00000-0000-4000-8000-000000000005"
	sbReviewer = "d0c00000-0000-4000-8000-000000000006"
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
		"steps": []map[string]any{{"name": "node-toolchain", "check": "true"}, {"name": "playwright-browsers", "check": "true"}},
	}, nil)
}
