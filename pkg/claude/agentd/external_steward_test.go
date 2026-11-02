package agentd

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// Whatever else a request carries, a verified external steward is an agent.
// requireHuman asks classify and nothing else, so this is what stops a route
// added later from letting one through.
func TestExternalStewardClassify_NeverHuman(t *testing.T) {
	for bits := range 16 {
		p := &peer{
			PID: 7, ConvID: "ext-1", ExternalSteward: true,
			HasClaudeAncestor: bits&1 != 0, HumanTokenValid: bits&2 != 0, DashboardHuman: bits&4 != 0,
		}
		if bits&8 != 0 {
			p.PID = 0
		}
		assert.Equal(t, classAgent, classify(p), "peer %+v", p)
		rec := httptest.NewRecorder()
		r := requestWithPeer(p)
		assert.False(t, requireHuman(rec, r, "test"), "peer %+v", p)
		assert.Equal(t, http.StatusForbidden, rec.Code)
	}
}

// The human can be decided in one place only. A new check that reads the raw
// fields would get around classify, and so around the test above.
func TestExternalSteward_OnlyClassifyReadsHumanFields(t *testing.T) {
	allowed := map[string]bool{
		"identity.go": true,
		// These two only pick an audit label, and never decide anything.
		"scribe.go": true, "steward_handover.go": true,
	}
	fset := token.NewFileSet()
	pkgs := parseAgentdSources(t, fset)
	for name, file := range pkgs {
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if ok && (sel.Sel.Name == "HumanTokenValid" || sel.Sel.Name == "DashboardHuman") && !allowed[name] {
				t.Errorf("%s reads %s; route it through classify()", fset.Position(sel.Pos()), sel.Sel.Name)
			}
			return true
		})
	}
}

func parseAgentdSources(t *testing.T, fset *token.FileSet) map[string]*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	out := map[string]*ast.File{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		out[name] = file
	}
	return out
}

// sourceRoutes reads every route registered with mux.HandleFunc in the source,
// so a route added later is in the sweep without anyone listing it.
func sourceRoutes(t *testing.T) (routes []sourceRoute) {
	t.Helper()
	fset := token.NewFileSet()
	files := parseAgentdSources(t, fset)
	gated := map[string]bool{}
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "requireHuman" {
						gated[fn.Name.Name] = true
					}
				}
				return true
			})
		}
	}
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle") {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			pattern, err := strconv.Unquote(lit.Value)
			require.NoError(t, err)
			handler := ""
			ast.Inspect(call.Args[1], func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && gated[id.Name] {
					handler = id.Name
				}
				return true
			})
			routes = append(routes, sourceRoute{pattern: pattern, gatedHandler: handler})
			return true
		})
	}
	return routes
}

type sourceRoute struct {
	pattern      string
	gatedHandler string
}

// humanGatedOpenReads are reads of a handler that gates only its writes.
var humanGatedOpenReads = map[string]bool{
	"GET /v1/agent/aliases": true, // same threat model as /v1/peers
}

var routeParam = regexp.MustCompile(`\{[^}]+\}`)

func (s sourceRoute) requests() []*http.Request {
	method, path, ok := strings.Cut(s.pattern, " ")
	if !ok {
		method, path = "", s.pattern
	}
	path = strings.ReplaceAll(routeParam.ReplaceAllString(path, "x"), "...", "")
	methods := []string{method}
	if method == "" {
		methods = []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}
	}
	var out []*http.Request
	for _, m := range methods {
		r := httptest.NewRequest(m, path, strings.NewReader(`{}`))
		out = append(out, r)
	}
	return out
}

// Every route whose handler gates on requireHuman, and every /v1 route that
// looks like answering a question, refuses the principal. The route list comes
// from the source, so a new requireHuman route is covered the day it is added.
func TestExternalSteward_RefusedByEveryHumanAndApprovalRoute(t *testing.T) {
	resetTestDB(t)
	require.NoError(t, config.Save(&config.Config{Features: &config.FeaturesConfig{Collab: true}, Steward: &config.StewardConfig{ExternalEnabled: true}}))
	approval := regexp.MustCompile(`(?i)approv|decision|access-request|answer|confirm`)
	mux := buildMux()
	gated, answering := 0, 0
	for _, route := range sourceRoutes(t) {
		isV1 := strings.Contains(route.pattern, "/v1/")
		isApproval := isV1 && approval.MatchString(route.pattern)
		if !isV1 || (route.gatedHandler == "" && !isApproval) {
			continue
		}
		if route.gatedHandler != "" {
			gated++
		}
		if isApproval {
			answering++
		}
		for _, r := range route.requests() {
			if humanGatedOpenReads[r.Method+" "+r.URL.Path] {
				continue
			}
			rec := httptest.NewRecorder()
			p := &peer{PID: 1, HasClaudeAncestor: true, ConvID: "ext-sweep", ExternalSteward: true}
			mux.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), peerKey{}, p)))
			assert.Falsef(t, rec.Code >= 200 && rec.Code < 300, "%s %s (%s) let an external steward through: %s",
				r.Method, r.URL.Path, route.gatedHandler, rec.Body.String())
		}
	}
	// The sweep is worth nothing if the source walk stops finding routes.
	assert.Greater(t, gated, 20, "human-gated routes found")
	assert.Greater(t, answering, 3, "approval routes found")
}

func TestExternalNoTmuxPanes(t *testing.T) {
	assert.True(t, externalNoTmuxPanes(fmt.Errorf("run: %w", exec.ErrNotFound)), "tmux is not installed")
	assert.True(t, externalNoTmuxPanes(errors.New("exit status 1: no server running on /tmp/tmux-1000/tclaude")))
	assert.True(t, externalNoTmuxPanes(errors.New("exit status 1: error connecting to /tmp/tmux-1000/tclaude")))
	assert.True(t, externalNoTmuxPanes(errors.New("exit status 1: no current target")), "an idle server")
	assert.False(t, externalNoTmuxPanes(errors.New("exit status 1: protocol version mismatch (client 8, server 7)")),
		"a live server this client cannot read")
	assert.False(t, externalNoTmuxPanes(errTmuxCommandTimeout), "a wedged tmux proves nothing")
}

// A popup opened anywhere else would be a way past the ceiling. These gates either
// call refuseExternalStewardCeiling or sit behind one that does; a new one has to
// be looked at before it joins this list.
func TestExternalSteward_EveryPopupIsAccountedFor(t *testing.T) {
	want := map[string]bool{
		"requirePermissionEx": true, "requireCrossAgentPermission": true, // the ceiling
		"requestResumeDirectoryRecreationApproval": true, "requestCodexDriveRollbackApproval": true, // agent.resume, after its gate
		"handleSudoRequest": true, // sudo, which refuses the principal
	}
	fset := token.NewFileSet()
	got := map[string]bool{}
	for _, file := range parseAgentdSources(t, fset) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Name.Name == "admitApproval" {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "admitApproval" {
						got[fn.Name.Name] = true
					}
				}
				return true
			})
		}
	}
	assert.Equal(t, want, got)
}

// The list itself is pinned, because the tests below walk it: dropping a slug
// has to be a change to this test too, not just a line that goes quiet.
func TestExternalSteward_NeverListIsPinned(t *testing.T) {
	assert.ElementsMatch(t, []string{
		"groups.members.spawn", "agent.clone", "groups.members.clone", "groups.clone",
		"agent.morph", "groups.members.morph", "agent.resume", "groups.members.resume",
		"groups.settings.default-dir", "groups.settings.environment", "groups.settings.default-spawn-target",
		"approvals.answer", "human.inbox.read", "human.inbox.answer",
		"sessions.attach", "sessions.takeover", "sessions.shell",
		"permissions.grant", "permissions.revoke", "config.import", "roles.manage",
		"groups.owners.manage", "groups.settings.member-permissions", "groups.settings.owner-scopes",
		"groups.members.spawn.permissions", "groups.members.spawn.owner", "groups.members.spawn.all",
		"profiles.manage", "templates.manage", "node.credentials.receive",
		"self.schedule", "agent.schedule", "groups.members.schedule", "groups.messages.schedule",
		"triggers.manage", "groups.triggers.manage",
	}, externalStewardNever)
}

// Nothing makes a never-listed slug usable: not a standing grant, not a group
// grant, and not the popup, which does not even open, on either gate that has
// one.
func TestExternalSteward_NeverListRefusedAtTheGate(t *testing.T) {
	require.NotEmpty(t, externalStewardNever)
	gates := map[string]func(http.ResponseWriter, *http.Request, string) bool{
		"requirePermission": func(w http.ResponseWriter, r *http.Request, slug string) bool {
			_, ok := requirePermission(w, r, slug)
			return ok
		},
		"requireCrossAgentPermission": func(w http.ResponseWriter, r *http.Request, slug string) bool {
			_, ok := requireCrossAgentPermission(w, r, slug, "some-other-conv")
			return ok
		},
	}
	for name, gate := range gates {
		for _, slug := range externalStewardNever {
			require.Truef(t, IsKnownPermSlug(slug), "%s is not a registered slug", slug)
			rec := httptest.NewRecorder()
			r := requestWithPeer(&peer{PID: 1, HasClaudeAncestor: true, ConvID: "ext-1", ExternalSteward: true})
			r.Header.Set("X-Tclaude-Ask-Human", "30s")
			assert.Falsef(t, gate(rec, r, slug), "%s %s", name, slug)
			assert.Equalf(t, http.StatusForbidden, rec.Code, "%s %s", name, slug)
			assert.Containsf(t, rec.Body.String(), externalStewardCeilingCode, "%s %s", name, slug)
		}
	}
	src := permSources{resolvable: true, externalSteward: true,
		override: map[string]overridePermSource{}, sudo: map[string]sudoPermSource{}, group: map[string][]string{}}
	for _, slug := range externalStewardNever {
		src.override[slug] = overridePermSource{Effect: "grant"}
		src.group[slug] = []string{""}
		assert.Equalf(t, permDeny, resolvePermissionVerdictFrom(src, slug, true).Resolution, slug)
		// The federation gate rebuilds its sources for peer-scoped grants.
		src.group[slug] = []string{`{"peer":["some-peer"]}`}
		assert.Equalf(t, permDeny, resolveRemotePermissionVerdictFrom(src, slug).Resolution, slug)
	}
}
