package agentd

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// bareAPIFetchBaseline counts, per dashboard module, the fetch calls whose
// first argument is a literal /api or /v1 path. Such a request always reaches
// the page's node, so a control on another node's group or agent must go
// through node-api.js (nodeFetch) instead. The counts may only fall: a module
// moving its requests to nodeFetch lowers its entry, and none adds a literal
// one. (A path held in a variable is not counted.)
var bareAPIFetchBaseline = map[string]int{
	"dock-save-dnd.js":              2,
	"fleet-admin-island.js":         1,
	"group-reorder.js":              1,
	"groups-island.js":              1,
	"groups-notification-reader.js": 1,
	"helpers.js":                    1,
	"list-paging.js":                1,
	"mail-island.js":                1,
	"mail.js":                       7,
	"management-actions.js":         17,
	"modal-templates.js":            1,
	"palette.js":                    2,
	"pr-checks-hover.js":            1,
	"pr-link-hover.js":              1,
	"process-editor.js":             3,
	"process-scribe-dock.js":        1,
	"profiles.js":                   1,
	"refresh.js":                    1,
	"remote-admin.js":               1,
	"row-action-handler.js":         13,
	"sandbox-profiles.js":           2,
	"slop-volume.js":                2,
	"vegas.js":                      3,
}

var bareAPIFetch = regexp.MustCompile("fetch\\(\\s*[`'\"]/(api|v1)/")

func TestDashboardBareAPIFetchesOnlyFall(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("dashboard", "js", "*.js"))
	if err != nil || len(files) == 0 {
		t.Fatalf("dashboard modules: %v", err)
	}
	seen := map[string]bool{}
	for name := range bareAPIFetchBaseline {
		seen[name] = true
	}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(file)
		got, want := len(bareAPIFetch.FindAll(raw, -1)), bareAPIFetchBaseline[name]
		switch {
		case got > want:
			t.Errorf("%s: %d fetch('/api…') calls, baseline %d: send the request through node-api.js nodeFetch(node) so it reaches the node the control is on", name, got, want)
		case got < want:
			t.Errorf("%s: %d fetch('/api…') calls, baseline %d: lower its bareAPIFetchBaseline entry", name, got, want)
		}
		delete(seen, name)
	}
	for name := range seen {
		t.Errorf("bareAPIFetchBaseline names %s, which is gone: drop its entry", name)
	}
}

// grantSlug matches the grants the dashboard's grant tables name: a
// permission slug, or feature:<peer view feature>.
var grantSlug = regexp.MustCompile(`'(feature:[a-z0-9_.-]+|[a-z0-9_-]+(?:\.[a-z0-9_-]+)+)'`)

// TestDashboardGrantTablesNameKnownGrants checks the JS grant tables
// (peer-view-limits.js) against the Go side: every slug is a registered
// permission and every feature one the peer view declares.
func TestDashboardGrantTablesNameKnownGrants(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("dashboard", "js", "peer-view-limits.js"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	slugs := map[string]bool{}
	for _, slug := range permissionRegistry {
		slugs[slug.Slug] = true
	}
	features := map[string]bool{}
	for _, rule := range peerViewRules() {
		features[rule.feature] = true
	}
	seen := 0
	for _, table := range []string{"export const GROUP_GRANT_ACTS", "export const AGENT_GRANT_ACTS", "const STOP_AND_RESUME", "export const AGENT_GRANT_VARIANTS"} {
		start := strings.Index(src, table+" = Object.freeze(")
		if start < 0 {
			t.Fatalf("%s not found", table)
		}
		end := strings.Index(src[start:], ");\n")
		if end < 0 {
			t.Fatalf("%s has no end", table)
		}
		for _, m := range grantSlug.FindAllStringSubmatch(src[start:start+end], -1) {
			seen++
			if feature, ok := strings.CutPrefix(m[1], "feature:"); ok {
				if !features[feature] {
					t.Errorf("%s names feature %q, which no peer view rule declares", table, feature)
				}
			} else if !slugs[m[1]] {
				t.Errorf("%s names %q, which is not a registered permission", table, m[1])
			}
		}
	}
	if seen < 20 {
		t.Fatalf("only %d grants found: the tables' shape changed", seen)
	}
}
