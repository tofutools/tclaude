package agentd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// auditRequests (and logRequest) wrap every response in *statusRec for
// status-code capture. statusRec embeds the http.ResponseWriter
// *interface*, and Go's method promotion through an embedded interface
// only forwards methods declared on that interface — never extra
// methods (like Hijack) the concrete value underneath happens to
// implement. Unless statusRec forwards Hijack explicitly, any handler
// behind auditRequests that needs to hijack the connection (e.g. a
// WebSocket upgrade, like the dashboard's in-browser terminal) fails
// with "response does not implement http.Hijacker" — a real-world
// regression that http.NewRecorder-based tests can't catch, since
// httptest.NewRecorder doesn't implement Hijacker either way.
func TestStatusRec_PreservesHijackerThroughAuditRequests(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	srv := httptest.NewServer(auditRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade behind auditRequests: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
	})))
	defer srv.Close()

	wsURL := "ws" + srv.URL[len("http"):]
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		status := ""
		if resp != nil {
			status = resp.Status
		}
		t.Fatalf("dial through auditRequests failed: %v (status=%s)", err, status)
	}
	_ = conn.Close()
}

// A dashboard request is the operator IFF it carries a valid dashboard
// session — attribution keys on the session, NOT the response status, so
// a post-auth policy 403 (operator cleared the cookie gate, handler then
// refused) stays "operator" while an unauthenticated / cross-origin probe
// is recorded as "unauthenticated".
func TestAuditActor_DashboardAttributionByteSession(t *testing.T) {
	// No cookie → unauthenticated, regardless of any later status.
	r := httptest.NewRequest(http.MethodPost, "/api/groups/crew/spawn", nil)
	kind, conv, label := auditActor(r, db.AuditSourceDashboard)
	if kind != db.AuditActorUnknown || label != "unauthenticated" || conv != "" {
		t.Errorf("uncookied dashboard request = (%q,%q,%q), want (unknown,\"\",unauthenticated)", kind, conv, label)
	}

	// Valid cookie + matching Origin → operator, even though this request
	// would (in a real handler) go on to be policy-refused with a 403.
	t.Cleanup(SetPopupBaseURLForTest("http://127.0.0.1:0"))
	initDashboardToken()
	r2 := httptest.NewRequest(http.MethodPost, "/api/sudo", nil)
	r2.AddCookie(&http.Cookie{Name: dashboardCookieName, Value: dashboardSessionToken})
	r2.Header.Set("Origin", popupBaseURL)
	kind, _, label = auditActor(r2, db.AuditSourceDashboard)
	if kind != db.AuditActorHuman || label != "operator" {
		t.Errorf("authed dashboard request = (%q,%q), want (human,operator)", kind, label)
	}
}

// matchAuditRoute is the allowlist gate: it must map real command routes
// on both surfaces to the right verb + source, and leave reads and
// look-alike sibling routes unmatched.
func TestMatchAuditRoute(t *testing.T) {
	cases := []struct {
		name       string
		method     string
		path       string
		wantOK     bool
		wantVerb   string // route.verb ("" when derived from {verb})
		wantSource string
		wantVar    map[string]string
	}{
		{"cli spawn", http.MethodPost, "/v1/groups/crew/spawn", true, "spawn", db.AuditSourceCLI, map[string]string{"name": "crew"}},
		{"dashboard spawn", http.MethodPost, "/api/groups/crew/spawn", true, "spawn", db.AuditSourceDashboard, map[string]string{"name": "crew"}},
		{"cli message", http.MethodPost, "/v1/messages", true, "message", db.AuditSourceCLI, nil},
		{"dashboard message singular", http.MethodPost, "/api/message", true, "message", db.AuditSourceDashboard, nil},
		{"cli agent verb", http.MethodPost, "/v1/agent/worker/reincarnate", true, "", db.AuditSourceCLI, map[string]string{"conv": "worker", "verb": "reincarnate"}},
		{"dashboard agents verb", http.MethodPost, "/api/agents/worker/retire", true, "", db.AuditSourceDashboard, map[string]string{"conv": "worker", "verb": "retire"}},
		{"cli permissions grant", http.MethodPost, "/v1/permissions/grant", true, "permissions.grant", db.AuditSourceCLI, nil},
		{"cli cron add", http.MethodPost, "/v1/cron", true, "cron.add", db.AuditSourceCLI, nil},

		// Reply + message deletions (the originally-missing routes).
		{"cli reply", http.MethodPost, "/v1/messages/7/reply", true, "reply", db.AuditSourceCLI, map[string]string{"id": "7"}},
		{"cli message delete", http.MethodDelete, "/v1/messages/7", true, "message.delete", db.AuditSourceCLI, map[string]string{"id": "7"}},
		{"cli inbox prune", http.MethodPost, "/v1/inbox/prune", true, "inbox.prune", db.AuditSourceCLI, nil},
		{"dashboard mailbox delete", http.MethodPost, "/api/mailbox/delete", true, "message.delete", db.AuditSourceDashboard, nil},
		{"dashboard mailbox wipe", http.MethodPost, "/api/mailbox/wipe", true, "mailbox.wipe", db.AuditSourceDashboard, nil},

		// Self-lifecycle (whoami): verb derived from {verb}.
		{"cli self reincarnate", http.MethodPost, "/v1/whoami/reincarnate", true, "", db.AuditSourceCLI, map[string]string{"verb": "reincarnate"}},
		{"cli self rename", http.MethodPost, "/v1/whoami/rename", true, "", db.AuditSourceCLI, map[string]string{"verb": "rename"}},

		// Template instantiate (both surfaces share the route).
		{"cli template instantiate", http.MethodPost, "/v1/templates/crew-tpl/instantiate", true, "template.instantiate", db.AuditSourceCLI, map[string]string{"name": "crew-tpl"}},
		{"dashboard template instantiate", http.MethodPost, "/api/templates/crew-tpl/instantiate", true, "template.instantiate", db.AuditSourceDashboard, map[string]string{"name": "crew-tpl"}},
		{"process run create", http.MethodPost, "/v1/process/runs", true, "process.run.create", db.AuditSourceCLI, nil},
		{"dashboard process run create", http.MethodPost, "/api/process/runs", true, "process.run.create", db.AuditSourceDashboard, nil},
		{"process signal", http.MethodPost, "/v1/process/runs/run-42/nodes/wait/signal", true, "process.signal", db.AuditSourceCLI, map[string]string{"id": "run-42", "node": "wait"}},
		{"dashboard process signal", http.MethodPost, "/api/process/runs/run-42/nodes/wait/signal", true, "process.signal", db.AuditSourceDashboard, map[string]string{"id": "run-42", "node": "wait"}},

		// Security / daemon admin (dashboard).
		{"remote-access add-client", http.MethodPost, "/api/remote-access/add-client", true, "remote-access.add-client", db.AuditSourceDashboard, nil},
		{"remote-access setup", http.MethodPost, "/api/remote-access/setup", true, "remote-access.setup", db.AuditSourceDashboard, nil},
		{"power shutdown", http.MethodPost, "/api/shutdown", true, "power.shutdown", db.AuditSourceDashboard, nil},
		{"power on", http.MethodPost, "/api/power-on", true, "power.on", db.AuditSourceDashboard, nil},

		// Reads + non-command routes must not match.
		{"GET members is a read", http.MethodGet, "/v1/groups/crew/members", false, "", "", nil},
		{"GET whoami context is a read", http.MethodGet, "/v1/whoami/context", false, "", "", nil},
		{"snapshot poll", http.MethodGet, "/api/snapshot", false, "", "", nil},
		{"static asset", http.MethodGet, "/static/js/audit.js", false, "", "", nil},
		{"unknown prefix", http.MethodPost, "/internal/thing", false, "", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			route, vars, source, ok := matchAuditRoute(tc.method, tc.path)
			if ok != tc.wantOK {
				t.Fatalf("matchAuditRoute(%s %s) ok=%v, want %v", tc.method, tc.path, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if route.verb != tc.wantVerb {
				t.Errorf("verb = %q, want %q", route.verb, tc.wantVerb)
			}
			if source != tc.wantSource {
				t.Errorf("source = %q, want %q", source, tc.wantSource)
			}
			if strings.Contains(tc.name, "process run create") && route.describe != nil {
				t.Error("process run create must keep a nil describer so params are never buffered")
			}
			for k, want := range tc.wantVar {
				if vars[k] != want {
					t.Errorf("vars[%q] = %q, want %q", k, vars[k], want)
				}
			}
		})
	}
}

// The agent/{conv}/{verb} route's {verb} is just a path segment, so a
// sibling route that shares the shape — /v1/agent/aliases/{handle} —
// must NOT be recorded with the handle as a verb. describeAgentVerb
// blanks an unknown verb so recordAuditRow drops the row.
func TestDescribeAgentVerb_DropsNonVerbSiblings(t *testing.T) {
	// A head-alias handle ("myhead") is not a lifecycle verb → blanked.
	f := auditFields{Verb: "myhead"}
	describeAgentVerb(&auditCtx{vars: map[string]string{"conv": "aliases", "verb": "myhead"}, fields: &f})
	if f.Verb != "" {
		t.Errorf("non-verb sibling should be blanked, got verb=%q", f.Verb)
	}

	// A real lifecycle verb is kept.
	f2 := auditFields{Verb: "retire"}
	describeAgentVerb(&auditCtx{vars: map[string]string{"conv": "worker", "verb": "retire"}, fields: &f2})
	if f2.Verb != "retire" {
		t.Errorf("real verb should be kept, got verb=%q", f2.Verb)
	}
}

// describeWhoamiVerb mirrors describeAgentVerb but for the self-lifecycle
// route: it must drop a POST to a read sibling (whoami/context) and keep a
// real self-verb. A self-rename additionally lifts the new title into the
// detail.
func TestDescribeWhoamiVerb(t *testing.T) {
	// A read sibling reached by POST is not a self-verb → blanked.
	f := auditFields{Verb: "context"}
	describeWhoamiVerb(&auditCtx{vars: map[string]string{"verb": "context"}, fields: &f})
	if f.Verb != "" {
		t.Errorf("non-verb sibling should be blanked, got verb=%q", f.Verb)
	}

	// A real self-verb is kept.
	f2 := auditFields{Verb: "reincarnate"}
	describeWhoamiVerb(&auditCtx{vars: map[string]string{"verb": "reincarnate"}, fields: &f2})
	if f2.Verb != "reincarnate" {
		t.Errorf("real self-verb should be kept, got verb=%q", f2.Verb)
	}

	// A self-rename carries the new title.
	f3 := auditFields{Verb: "rename"}
	describeWhoamiVerb(&auditCtx{
		vars:   map[string]string{"verb": "rename"},
		body:   []byte(`{"title":"new name"}`),
		fields: &f3,
	})
	if f3.Verb != "rename" {
		t.Errorf("rename verb should be kept, got verb=%q", f3.Verb)
	}
	if !strings.Contains(f3.Detail, "new name") {
		t.Errorf("rename detail should carry the new title, got %q", f3.Detail)
	}
}

// describeRemoteAccessSetup must capture the non-secret fields and NEVER
// the passphrase / p12 password from the body.
func TestDescribeRemoteAccessSetup_RedactsSecrets(t *testing.T) {
	f := auditFields{}
	describeRemoteAccessSetup(&auditCtx{
		body: []byte(`{"bind":"0.0.0.0:8443","client_name":"laptop",` +
			`"passphrase":"hunter2","p12_password":"s3cr3t","regenerate":true}`),
		fields: &f,
	})
	if !strings.Contains(f.Detail, "0.0.0.0:8443") || !strings.Contains(f.Detail, "laptop") {
		t.Errorf("detail should carry bind + client name, got %q", f.Detail)
	}
	if strings.Contains(f.Detail, "hunter2") || strings.Contains(f.Detail, "s3cr3t") {
		t.Fatalf("SECRET LEAK: detail must not contain the passphrase/p12 password, got %q", f.Detail)
	}
}

// recordApprovalDecision writes one popup-sourced audit row attributed to
// the operator, naming the requesting agent as the target and the decided
// permission in the detail.
func TestRecordApprovalDecision_WritesPopupRow(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	db.ResetForTest()

	req := &approvalRequest{
		perm:        "tool.bash",
		convID:      "019ec010-1111-1111-1111-111111111111",
		agentID:     "agt_stable_requester",
		convTitle:   "worker",
		method:      http.MethodPost,
		path:        "/v1/messages",
		targetGroup: "crew",
	}
	recordApprovalDecision(req, outcomeApprove)

	rows, err := db.ListAuditLog(db.AuditLogFilter{Verb: "approval.approve"})
	if err != nil {
		t.Fatalf("list audit log: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 approval.approve row, got %d", len(rows))
	}
	row := rows[0]
	if row.ActorKind != db.AuditActorHuman || row.ActorLabel != "operator" {
		t.Errorf("actor = (%q,%q), want (human,operator)", row.ActorKind, row.ActorLabel)
	}
	if row.TargetConv != req.convID || row.TargetLabel != "worker" {
		t.Errorf("target = (%q,%q), want (%q,worker)", row.TargetConv, row.TargetLabel, req.convID)
	}
	if row.TargetAgent != req.agentID {
		t.Errorf("target agent = %q, want captured %q", row.TargetAgent, req.agentID)
	}
	if row.GroupName != "crew" {
		t.Errorf("group = %q, want crew", row.GroupName)
	}
	if row.Source != db.AuditSourcePopup {
		t.Errorf("source = %q, want %q", row.Source, db.AuditSourcePopup)
	}
	if !strings.Contains(row.Detail, "tool.bash") {
		t.Errorf("detail should name the permission, got %q", row.Detail)
	}

	// A deny writes the mirror verb.
	recordApprovalDecision(req, outcomeDeny)
	denies, err := db.ListAuditLog(db.AuditLogFilter{Verb: "approval.deny"})
	if err != nil {
		t.Fatalf("list deny rows: %v", err)
	}
	if len(denies) != 1 {
		t.Fatalf("want 1 approval.deny row, got %d", len(denies))
	}

	// An "always allow" writes its own distinct verb (JOH-367), so the audit
	// trail tells a one-off approval apart from a persistent grant.
	recordApprovalDecision(req, outcomeApproveAlways)
	always, err := db.ListAuditLog(db.AuditLogFilter{Verb: "approval.approve-always"})
	if err != nil {
		t.Fatalf("list always rows: %v", err)
	}
	if len(always) != 1 {
		t.Fatalf("want 1 approval.approve-always row, got %d", len(always))
	}
}

// recordApprovalRequest writes one agent-attributed audit row naming the
// requester as the actor, the requested permission in the detail, and the
// surface (cli/dashboard) as the source — the counterpart to the operator's
// decision row (JOH-392).
func TestRecordApprovalRequest_WritesAgentRow(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	db.ResetForTest()

	req := &approvalRequest{
		perm:        "human.clipboard",
		convID:      "019ec010-2222-2222-2222-222222222222",
		agentID:     "agt_stable_requester",
		convTitle:   "worker",
		method:      http.MethodPost,
		path:        "/v1/clipboard",
		targetGroup: "crew",
	}
	recordApprovalRequest(req)

	rows, err := db.ListAuditLog(db.AuditLogFilter{Verb: "approval.request"})
	if err != nil {
		t.Fatalf("list audit log: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 approval.request row, got %d", len(rows))
	}
	row := rows[0]
	// The actor is the requesting AGENT, not the operator — this is the
	// gap the decision-only trail left (JOH-392).
	if row.ActorKind != db.AuditActorAgent || row.ActorConv != req.convID || row.ActorLabel != "worker" {
		t.Errorf("actor = (%q,%q,%q), want (agent,%q,worker)", row.ActorKind, row.ActorConv, row.ActorLabel, req.convID)
	}
	if row.ActorAgent != req.agentID {
		t.Errorf("actor agent = %q, want captured %q", row.ActorAgent, req.agentID)
	}
	if row.GroupName != "crew" {
		t.Errorf("group = %q, want crew", row.GroupName)
	}
	// Source reflects the surface the agent's call arrived on (/v1 → cli),
	// not the popup surface the human decides on.
	if row.Source != db.AuditSourceCLI {
		t.Errorf("source = %q, want %q", row.Source, db.AuditSourceCLI)
	}
	if !strings.Contains(row.Detail, "human.clipboard") {
		t.Errorf("detail should name the permission, got %q", row.Detail)
	}

	// A requester with no resolved title falls back to the short conv-id so
	// the actor column is never blank. Assert the label directly — a Search
	// term would also match the full actor_conv and pass even if the label
	// were left empty.
	req.convTitle = ""
	recordApprovalRequest(req)
	rows, err = db.ListAuditLog(db.AuditLogFilter{Verb: "approval.request"})
	if err != nil {
		t.Fatalf("list fallback rows: %v", err)
	}
	var fallback *db.AuditLogEntry
	for i := range rows {
		if rows[i].ActorLabel != "worker" {
			fallback = &rows[i]
			break
		}
	}
	if fallback == nil {
		t.Fatalf("want a fallback-labeled approval.request row distinct from the titled one")
	}
	if fallback.ActorLabel != short8(req.convID) {
		t.Errorf("fallback actor label = %q, want %q (short conv-id)", fallback.ActorLabel, short8(req.convID))
	}
}
