package agentd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Every dimension the scope parser accepts must be projectable from an
// ActionContext. A dimension without a field would silently evaluate as
// "call site said nothing" forever — a grant an operator can write and the
// gate can never satisfy.
func TestActionContextCoversEveryScopeDimension(t *testing.T) {
	full := ActionContext{
		Group:           "g",
		TargetAgent:     "a",
		SpawnProfile:    "p",
		ProcessTemplate: "t",
	}
	for dim := range permissionScopeSelectors {
		if full.value(dim) == "" {
			t.Errorf("ActionContext.value(%q) = \"\": add a field for this dimension", dim)
		}
	}
}

func TestEvalPermissionScope(t *testing.T) {
	allow := func(scopes ...string) permVerdict {
		return permVerdict{Resolution: permAllow, Source: permSourceGroup, ScopeJSON: scopes}
	}
	for _, tc := range []struct {
		name          string
		verdict       permVerdict
		actx          ActionContext
		wantUnscoped  bool
		wantSatisfied bool
		wantMatched   string
	}{
		{name: "no scope rows is unscoped",
			verdict: allow(), wantUnscoped: true, wantSatisfied: true},
		{name: "empty scope row is unscoped",
			verdict: allow(""), wantUnscoped: true, wantSatisfied: true},
		{name: "empty object is unscoped",
			verdict: allow("{}"), wantUnscoped: true, wantSatisfied: true},
		{name: "single dimension matches",
			verdict: allow(`{"group":["dev"]}`), actx: ActionContext{Group: "dev"},
			wantSatisfied: true, wantMatched: "group=dev"},
		{name: "single dimension mismatches",
			verdict: allow(`{"group":["dev"]}`), actx: ActionContext{Group: "prod"}},
		{name: "context omits the constrained dimension",
			verdict: allow(`{"group":["dev"]}`)},
		{name: "matchers within a dimension OR",
			verdict: allow(`{"group":["dev","qa"]}`), actx: ActionContext{Group: "qa"},
			wantSatisfied: true, wantMatched: "group=dev,qa"},
		{name: "dimensions AND",
			verdict:       allow(`{"group":["dev"],"spawn_profile":["locked"]}`),
			actx:          ActionContext{Group: "dev", SpawnProfile: "locked"},
			wantSatisfied: true, wantMatched: "group=dev spawn_profile=locked"},
		{name: "one failing dimension fails the whole scope",
			verdict: allow(`{"group":["dev"],"spawn_profile":["locked"]}`),
			actx:    ActionContext{Group: "dev", SpawnProfile: "open"}},
		{name: "one missing dimension fails the whole scope",
			verdict: allow(`{"group":["dev"],"spawn_profile":["locked"]}`),
			actx:    ActionContext{Group: "dev"}},
		{name: "absent dimension is unconstrained",
			verdict:       allow(`{"group":["dev"]}`),
			actx:          ActionContext{Group: "dev", SpawnProfile: "anything"},
			wantSatisfied: true, wantMatched: "group=dev"},
		{name: "tier scopes union: first row",
			verdict: allow(`{"group":["dev"]}`, `{"group":["qa"]}`), actx: ActionContext{Group: "dev"},
			wantSatisfied: true, wantMatched: "group=dev"},
		{name: "tier scopes union: second row",
			verdict: allow(`{"group":["dev"]}`, `{"group":["qa"]}`), actx: ActionContext{Group: "qa"},
			wantSatisfied: true, wantMatched: "group=qa"},
		{name: "tier scopes union: neither row",
			verdict: allow(`{"group":["dev"]}`, `{"group":["qa"]}`), actx: ActionContext{Group: "prod"}},
		{name: "unscoped row absorbs the tier even when it sorts last",
			verdict: allow(`{"group":["dev"]}`, ""), actx: ActionContext{Group: "prod"},
			wantUnscoped: true, wantSatisfied: true},
		{name: "undecodable dimension fails closed, never unscoped",
			verdict: allow(`{"mystery":["x"]}`), actx: ActionContext{Group: "dev"}},
		{name: "malformed json fails closed, never unscoped",
			verdict: allow(`{`), actx: ActionContext{Group: "dev"}},
		{name: "undecodable row does not shadow a satisfiable sibling",
			verdict: allow(`{"mystery":["x"]}`, `{"group":["dev"]}`), actx: ActionContext{Group: "dev"},
			wantSatisfied: true, wantMatched: "group=dev"},
		{name: "selector fails closed until phase 5",
			verdict: allow(`{"target_agent":["@descendants"]}`), actx: ActionContext{TargetAgent: "agt_1"}},
		{name: "exact matcher beside a selector still matches",
			verdict: allow(`{"target_agent":["@descendants","agt_1"]}`), actx: ActionContext{TargetAgent: "agt_1"},
			wantSatisfied: true, wantMatched: "target_agent=@descendants,agt_1"},
		{name: "deny carries no scope and is never evaluated as one",
			verdict:      permVerdict{Resolution: permDeny, Source: permSourceOverride},
			wantUnscoped: true, wantSatisfied: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := evalPermissionScope(tc.verdict, "caller-conv", tc.actx)
			if got.Unscoped != tc.wantUnscoped {
				t.Errorf("Unscoped = %v, want %v", got.Unscoped, tc.wantUnscoped)
			}
			if got.Satisfied != tc.wantSatisfied {
				t.Errorf("Satisfied = %v, want %v", got.Satisfied, tc.wantSatisfied)
			}
			if got.Matched != tc.wantMatched {
				t.Errorf("Matched = %q, want %q", got.Matched, tc.wantMatched)
			}
		})
	}
}

// The matched scope lands on an audit row, so a tier where several scopes
// would admit the action must always name the same one.
func TestEvalPermissionScopeMatchedIsDeterministic(t *testing.T) {
	v := permVerdict{Resolution: permAllow, Source: permSourceGroup,
		ScopeJSON: []string{`{"group":["qa","dev"]}`, `{"group":["dev"]}`}}
	first := evalPermissionScope(v, "caller", ActionContext{Group: "dev"}).Matched
	for i := 0; i < 20; i++ {
		reordered := permVerdict{Resolution: permAllow, Source: permSourceGroup,
			ScopeJSON: []string{`{"group":["dev"]}`, `{"group":["qa","dev"]}`}}
		if got := evalPermissionScope(reordered, "caller", ActionContext{Group: "dev"}).Matched; got != first {
			t.Fatalf("matched scope depends on row order: %q vs %q", got, first)
		}
	}
	if first == "" {
		t.Fatal("expected a matched scope")
	}
}

// Precedence is unchanged by scopes: the winning tier decides, and the scope
// it carries out is the winning tier's — never a broader one from below.
func TestResolvePermissionVerdictFromCarriesWinningTierScope(t *testing.T) {
	const slug = PermGroupsSpawn
	for _, tc := range []struct {
		name           string
		src            permSources
		defaultAllowed bool
		wantResolution permResolution
		wantSource     permSource
		wantScopes     []string
	}{
		{
			name: "sudo scope wins over a narrower override",
			src: permSources{resolvable: true,
				sudo:     map[string]sudoPermSource{slug: {ID: 7, ScopeJSON: `{"group":["dev"]}`}},
				override: map[string]overridePermSource{slug: {Effect: db.PermEffectGrant, ScopeJSON: `{"group":["qa"]}`}},
				group:    map[string][]string{slug: {""}}},
			wantResolution: permAllow, wantSource: permSourceSudo, wantScopes: []string{`{"group":["dev"]}`},
		},
		{
			name: "override narrows below a broader group grant",
			src: permSources{resolvable: true,
				override: map[string]overridePermSource{slug: {Effect: db.PermEffectGrant, ScopeJSON: `{"group":["dev"]}`}},
				group:    map[string][]string{slug: {""}}},
			wantResolution: permAllow, wantSource: permSourceOverride, wantScopes: []string{`{"group":["dev"]}`},
		},
		{
			name: "deny beats a scoped group allow and carries no scope",
			src: permSources{resolvable: true,
				override: map[string]overridePermSource{slug: {Effect: db.PermEffectDeny}},
				group:    map[string][]string{slug: {`{"group":["dev"]}`}}},
			wantResolution: permDeny, wantSource: permSourceOverride,
		},
		{
			name: "every group row in the winning tier is carried",
			src: permSources{resolvable: true,
				group: map[string][]string{slug: {`{"group":["dev"]}`, `{"group":["qa"]}`}}},
			wantResolution: permAllow, wantSource: permSourceGroup,
			wantScopes: []string{`{"group":["dev"]}`, `{"group":["qa"]}`},
		},
		{
			name:           "config defaults stay unscoped",
			src:            permSources{resolvable: true},
			defaultAllowed: true,
			wantResolution: permAllow, wantSource: permSourceDefault,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := resolvePermissionVerdictFrom(tc.src, slug, tc.defaultAllowed)
			if v.Resolution != tc.wantResolution {
				t.Fatalf("resolution = %v, want %v", v.Resolution, tc.wantResolution)
			}
			if v.Source != tc.wantSource {
				t.Errorf("source = %q, want %q", v.Source, tc.wantSource)
			}
			if len(v.ScopeJSON) != len(tc.wantScopes) {
				t.Fatalf("scopes = %v, want %v", v.ScopeJSON, tc.wantScopes)
			}
			for i, want := range tc.wantScopes {
				if v.ScopeJSON[i] != want {
					t.Errorf("scope[%d] = %q, want %q", i, v.ScopeJSON[i], want)
				}
			}
		})
	}
}

// The boolean, context-free readers of the resolver (hasPermission and the
// spawn-time conferral checks) cannot evaluate a scope, so a scoped allow
// must not read as an allow to them. permUndecided, not permDeny: the owner
// bypass and the ask-human popup stay reachable.
func TestContextFreeResolutionFailsClosedOnScopedAllow(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    permVerdict
		want permResolution
	}{
		{"unscoped allow is unchanged",
			permVerdict{Resolution: permAllow, ScopeJSON: []string{""}}, permAllow},
		{"no scope rows is unchanged",
			permVerdict{Resolution: permAllow}, permAllow},
		{"scoped allow degrades to undecided",
			permVerdict{Resolution: permAllow, ScopeJSON: []string{`{"group":["dev"]}`}}, permUndecided},
		{"union with an unscoped row stays an allow",
			permVerdict{Resolution: permAllow, ScopeJSON: []string{`{"group":["dev"]}`, ""}}, permAllow},
		{"deny is unchanged",
			permVerdict{Resolution: permDeny}, permDeny},
		{"undecided is unchanged",
			permVerdict{Resolution: permUndecided}, permUndecided},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := contextFreeResolution(tc.v); got != tc.want {
				t.Errorf("contextFreeResolution = %v, want %v", got, tc.want)
			}
		})
	}
}

// Enforcement checklist. Every slug that declares ScopeDims must have a
// named authorization path that actually evaluates the scope — otherwise an
// operator can write a grant that reads as narrow in `permissions ls` and is
// a wildcard at the gate. This map is that checklist, and adding ScopeDims to
// a slug fails the test until its gate is listed here (which is the moment to
// check the gate really does evaluate).
//
// It is a review prompt, not a proof: it cannot verify the named gate. The
// gap it closes is the one that shipped routes.publish/routes.consume with a
// bespoke, scope-blind gate for the better part of a phase.
var scopedSlugEnforcementPaths = map[string]string{
	PermGroupsSpawn:       "requireGroupPermission — fills ActionContext{Group}",
	PermAgentRetire:       "requireCrossAgentPermission — target_agent awaits Phase 5, so a target_agent scope fails closed",
	PermAgentStanddown:    "requireCrossAgentPermission — same as agent.retire",
	PermProcessRunsManage: "requirePermission — run create supplies ActionContext{ProcessTemplate}",
	PermRoutesPublish:     "requireRoutePermissionForIdentity — central resolver plus ActionContext{Group}",
	PermRoutesConsume:     "requireRoutePermissionForIdentity — central resolver plus ActionContext{Group}",
}

func TestEveryScopedSlugDeclaresAnEnforcementPath(t *testing.T) {
	declared := map[string]bool{}
	for _, entry := range permissionRegistry {
		if len(entry.ScopeDims) == 0 {
			continue
		}
		declared[entry.Slug] = true
		if scopedSlugEnforcementPaths[entry.Slug] == "" {
			t.Errorf("slug %q declares scope dimensions %v but no enforcement path is recorded; "+
				"confirm its gate evaluates the scope and add it to scopedSlugEnforcementPaths",
				entry.Slug, entry.ScopeDims)
		}
	}
	for slug := range scopedSlugEnforcementPaths {
		if !declared[slug] {
			t.Errorf("scopedSlugEnforcementPaths lists %q, which no longer declares scope dimensions", slug)
		}
	}
}

// A cloned agent must not come out holding a wider grant than its source:
// self.clone is default-granted, so a scope-erasing copy would let any agent
// mint an unscoped duplicate of a grant an operator deliberately narrowed.
func TestApplyClonedIdentityPreservesGrantScopes(t *testing.T) {
	setupTestDB(t)
	const src = "clone-scope-src-0001"
	const dst = "clone-scope-dst-0001"
	require.NoError(t, db.GrantAgentPermissionWithScope(src, PermGroupsSpawn, `{"group":["alpha"]}`, "test"))
	require.NoError(t, db.SetAgentPermissionOverride(src, PermHumanClipboard, db.PermEffectDeny, "test"))

	perms, err := db.ListAgentPermissionOverrideRowsForConv(src)
	require.NoError(t, err)
	applyClonedIdentity(dst, "test", nil, perms, nil)

	rows, err := db.ListAgentPermissionOverrideRowsForConv(dst)
	require.NoError(t, err)
	got := map[string]db.AgentPermission{}
	for _, row := range rows {
		got[row.Slug] = row
	}
	require.Contains(t, got, PermGroupsSpawn)
	assert.Equal(t, `{"group":["alpha"]}`, got[PermGroupsSpawn].ScopeJSON,
		"the clone must inherit the source's scope, not a wildcard")
	assert.Equal(t, db.PermEffectGrant, got[PermGroupsSpawn].Effect)
	require.Contains(t, got, PermHumanClipboard)
	assert.Equal(t, db.PermEffectDeny, got[PermHumanClipboard].Effect, "denies still ride along")
}

// A tier this build cannot decode authorizes nothing at the gate, so the
// listing must not render it as an unscoped (i.e. unlimited) grant.
func TestPermissionProvenanceMarksUndecodableScopes(t *testing.T) {
	assert.Equal(t, "override [unreadable scope]",
		permissionProvenance(permSourceOverride, []string{`{"mystery":["x"]}`}))
	assert.Equal(t, "group", permissionProvenance(permSourceGroup, []string{"", `{"group":["dev"]}`}),
		"an unscoped row still makes the whole tier unscoped")
	assert.Equal(t, "group [group=dev]",
		permissionProvenance(permSourceGroup, []string{`{"group":["dev"]}`}))
}

// The scope clause is the authorization fact on an audit row; a describer
// that already filled the detail budget must not be able to push it off.
func TestJoinAuditDetailKeepsTheAppendedClause(t *testing.T) {
	long := strings.Repeat("x", 400)
	got := joinAuditDetail(long, "scope: groups.spawn [group=alpha]")
	assert.Contains(t, got, "scope: groups.spawn [group=alpha]")
	assert.LessOrEqual(t, len(got), 240)
}
