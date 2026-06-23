package agentd

import (
	"net/http"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

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
