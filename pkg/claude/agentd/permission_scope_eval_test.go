package agentd

import (
	"testing"

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
