package agentd

import (
	"net/http"
	"net/http/httptest"
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

		// Reads + non-command routes must not match.
		{"GET members is a read", http.MethodGet, "/v1/groups/crew/members", false, "", "", nil},
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
