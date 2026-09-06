package agentd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

type recordingSpawnAuthorityReader struct {
	snapshot spawnAuthorityFactsSnapshot
	err      error
	reads    int
	seen     []spawnAuthorityPrincipal
}

func (r *recordingSpawnAuthorityReader) Read(_ context.Context, principal spawnAuthorityPrincipal, _ permissionReadPolicy) (spawnAuthorityFactsSnapshot, error) {
	r.reads++
	r.seen = append(r.seen, principal)
	if r.reads > 1 {
		return spawnAuthorityFactsSnapshot{}, errors.New("authority facts were read more than once")
	}
	return r.snapshot, r.err
}

func capturedSpawnSources() permSources {
	return permSources{
		resolvable: true,
		sudo:       map[string]sudoPermSource{},
		override:   map[string]overridePermSource{},
		group:      map[string][]string{},
		groupRows:  map[string][]db.AgentGroupPermission{},
	}
}

func validAgentSpawnRequest() spawnAuthorityRequest {
	return spawnAuthorityRequest{
		Principal: spawnAuthorityPrincipal{Kind: authorityPrincipalAgent, AgentID: "agt_test", ConvID: "conv-test"},
		Origin:    spawnAuthorityOrigin{Kind: spawnAuthorityOriginHTTP},
		Action: ActionContext{
			Group: "team", SpawnProfile: "worker", SandboxProfile: "strict", structuralGroup: "team",
		},
	}
}

func TestSpawnAuthorityRejectsInvalidPrincipalAndOriginWithoutReading(t *testing.T) {
	base := validAgentSpawnRequest()
	tests := []struct {
		name string
		edit func(*spawnAuthorityRequest)
	}{
		{"invalid principal kind", func(r *spawnAuthorityRequest) { r.Principal.Kind = authorityPrincipalInvalid }},
		{"operator carries agent identity", func(r *spawnAuthorityRequest) { r.Principal.Kind = authorityPrincipalOperator }},
		{"agent missing stable id", func(r *spawnAuthorityRequest) { r.Principal.AgentID = "" }},
		{"agent id has wrong shape", func(r *spawnAuthorityRequest) { r.Principal.AgentID = "conversation-like" }},
		{"agent missing current conversation", func(r *spawnAuthorityRequest) { r.Principal.ConvID = "" }},
		{"invalid origin kind", func(r *spawnAuthorityRequest) { r.Origin.Kind = spawnAuthorityOriginInvalid }},
		{"http carries trigger causation", func(r *spawnAuthorityRequest) { r.Origin.RuleID = 1 }},
		{"trigger missing firing", func(r *spawnAuthorityRequest) {
			r.Origin = spawnAuthorityOrigin{Kind: spawnAuthorityOriginTrigger, RuleID: 1}
		}},
		{"cron missing run", func(r *spawnAuthorityRequest) {
			r.Origin = spawnAuthorityOrigin{Kind: spawnAuthorityOriginCron, RuleID: 1, CronJobID: 1}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := base
			tt.edit(&req)
			reader := &recordingSpawnAuthorityReader{}
			decision, err := (spawnAuthorityEvaluator{reader: reader}).EvaluateSpawn(context.Background(), req, permissionReadLegacy)
			require.NoError(t, err)
			assert.Equal(t, spawnAuthorityInvalid, decision.Outcome)
			assert.Zero(t, reader.reads)
		})
	}
}

func TestSpawnAuthorityOperatorAdaptersAreExplicit(t *testing.T) {
	reader := &recordingSpawnAuthorityReader{err: errors.New("operator must not read agent facts")}
	evaluator := spawnAuthorityEvaluator{reader: reader}
	for _, origin := range []spawnAuthorityOrigin{
		{Kind: spawnAuthorityOriginHTTP},
		{Kind: spawnAuthorityOriginTrigger, RuleID: 3, FiringID: 7},
		{Kind: spawnAuthorityOriginCron, RuleID: 4, CronJobID: 4, CronRunID: 9},
	} {
		decision, err := evaluator.EvaluateSpawn(context.Background(), spawnAuthorityRequest{
			Principal: spawnAuthorityPrincipal{Kind: authorityPrincipalOperator}, Origin: origin,
		}, permissionReadLegacy)
		require.NoError(t, err)
		assert.Equal(t, spawnAuthorityAllowed, decision.Outcome)
		assert.Equal(t, permSourceOperator, decision.Source)
		assert.True(t, decision.AllowAnyGroup)
	}
	assert.Zero(t, reader.reads)

	operator, ok := managedSpawnPrincipal(&db.TriggerRule{OperatorAuthored: true}, "")
	require.True(t, ok)
	assert.Equal(t, authorityPrincipalOperator, operator.Kind)
	agent, ok := managedSpawnPrincipal(&db.TriggerRule{OwnerAgent: "agt_owner"}, "owner-conv")
	require.True(t, ok)
	assert.Equal(t, spawnAuthorityPrincipal{Kind: authorityPrincipalAgent, AgentID: "agt_owner", ConvID: "owner-conv"}, agent)
	for name, input := range map[string]struct {
		rule *db.TriggerRule
		conv string
	}{
		"operator with owner id":     {&db.TriggerRule{OperatorAuthored: true, OwnerAgent: "agt_owner"}, ""},
		"operator with owner conv":   {&db.TriggerRule{OperatorAuthored: true}, "owner-conv"},
		"agent missing stable id":    {&db.TriggerRule{}, "owner-conv"},
		"agent missing current conv": {&db.TriggerRule{OwnerAgent: "agt_owner"}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			_, ok := managedSpawnPrincipal(input.rule, input.conv)
			assert.False(t, ok)
		})
	}
}

func TestSpawnAuthorityHTTPAuthenticatedOperatorAdapter(t *testing.T) {
	setupTestDB(t)
	reader := &recordingSpawnAuthorityReader{err: errors.New("operator must not read agent facts")}
	old := spawnAuthorityFacts
	spawnAuthorityFacts = reader
	t.Cleanup(func() { spawnAuthorityFacts = old })

	w := httptest.NewRecorder()
	r := requestWithPeer(&peer{PID: 99, HumanTokenValid: true})
	caller, ok := requireSpawnPermission(w, r, &db.AgentGroup{Name: "foreign"}, ActionContext{SandboxProfile: "strict"})
	require.True(t, ok, "body=%s", w.Body.String())
	assert.Empty(t, caller)
	assert.Zero(t, reader.reads)
}

func TestSpawnAuthorityAutomationIdentityMustBeCurrentAndActive(t *testing.T) {
	setupTestDB(t)
	agentID, _, err := db.EnsureAgentForConv("current-conv", "test")
	require.NoError(t, err)
	evaluator := newSpawnAuthorityEvaluator()
	origin := spawnAuthorityOrigin{Kind: spawnAuthorityOriginTrigger, RuleID: 1, FiringID: 2}

	decision, err := evaluator.EvaluateSpawn(context.Background(), spawnAuthorityRequest{
		Principal: spawnAuthorityPrincipal{Kind: authorityPrincipalAgent, AgentID: agentID, ConvID: "current-conv"}, Origin: origin,
	}, permissionReadLegacy)
	require.NoError(t, err)
	assert.Equal(t, spawnAuthorityNotGranted, decision.Outcome, "known active identity reaches policy evaluation")

	for name, principal := range map[string]spawnAuthorityPrincipal{
		"mismatched conversation": {Kind: authorityPrincipalAgent, AgentID: agentID, ConvID: "old-conv"},
		"missing agent":           {Kind: authorityPrincipalAgent, AgentID: "agt_missing", ConvID: "missing-conv"},
	} {
		t.Run(name, func(t *testing.T) {
			decision, err := evaluator.EvaluateSpawn(context.Background(), spawnAuthorityRequest{Principal: principal, Origin: origin}, permissionReadLegacy)
			require.NoError(t, err)
			assert.Equal(t, spawnAuthorityInvalid, decision.Outcome)
		})
	}

	retired, err := db.RetireAgentByID(agentID, "test", "authority test")
	require.NoError(t, err)
	require.True(t, retired)
	decision, err = evaluator.EvaluateSpawn(context.Background(), spawnAuthorityRequest{
		Principal: spawnAuthorityPrincipal{Kind: authorityPrincipalAgent, AgentID: agentID, ConvID: "current-conv"}, Origin: origin,
	}, permissionReadLegacy)
	require.NoError(t, err)
	assert.Equal(t, spawnAuthorityInvalid, decision.Outcome)
}

func TestSpawnAuthorityUsesOneCaptureForIndependentAlternativesAndSudoCounterfactual(t *testing.T) {
	src := capturedSpawnSources()
	src.sudo[PermAgentSpawn] = sudoPermSource{ID: 41, ScopeJSON: `{"group":["team"]}`}
	src.override[PermGroupsMembersSpawn] = overridePermSource{Effect: db.PermEffectGrant, ScopeJSON: `{"sandbox_profile":["strict"]}`}
	reader := &recordingSpawnAuthorityReader{snapshot: spawnAuthorityFactsSnapshot{PrincipalVerified: true, Sources: src, Defaults: map[string]bool{}}}
	decision, err := (spawnAuthorityEvaluator{reader: reader}).EvaluateSpawn(context.Background(), validAgentSpawnRequest(), permissionReadLegacy)
	require.NoError(t, err)
	assert.Equal(t, 1, reader.reads)
	assert.Equal(t, spawnAuthorityAllowed, decision.Outcome)
	assert.Equal(t, PermAgentSpawn, decision.AuthorizedSlug)
	assert.Equal(t, permSourceSudo, decision.Source)
	assert.Equal(t, int64(41), decision.SudoGrantID)
	assert.Zero(t, decision.LoadBearingSudo, "the independent group alternative survives without sudo")
	assert.True(t, decision.Alternatives[PermAgentSpawn].Allowed)
	assert.True(t, decision.Alternatives[PermGroupsMembersSpawn].Allowed, "the group alternative is evaluated independently")

	src.diagnostics = []permissionReadDiagnostic{{Tier: "sudo", Err: errors.New("sudo unavailable")}}
	src.ownerReadErr = errors.New("owner unavailable")
	reader = &recordingSpawnAuthorityReader{snapshot: spawnAuthorityFactsSnapshot{PrincipalVerified: true, Sources: src, Defaults: map[string]bool{}}}
	decision, err = (spawnAuthorityEvaluator{reader: reader}).EvaluateSpawn(context.Background(), validAgentSpawnRequest(), permissionReadLegacy)
	require.NoError(t, err)
	require.Len(t, decision.Diagnostics, 2)
	assert.Equal(t, "sudo", decision.Diagnostics[0].Tier)
	assert.Equal(t, "owner", decision.Diagnostics[1].Tier)

	onlySudo := capturedSpawnSources()
	onlySudo.sudo[PermAgentSpawn] = sudoPermSource{ID: 41, ScopeJSON: `{"group":["team"]}`}
	reader = &recordingSpawnAuthorityReader{snapshot: spawnAuthorityFactsSnapshot{PrincipalVerified: true, Sources: onlySudo, Defaults: map[string]bool{}}}
	decision, err = (spawnAuthorityEvaluator{reader: reader}).EvaluateSpawn(context.Background(), validAgentSpawnRequest(), permissionReadLegacy)
	require.NoError(t, err)
	assert.Equal(t, int64(41), decision.SudoGrantID)
	assert.Equal(t, int64(41), decision.LoadBearingSudo, "sudo is load-bearing only when no alternative survives its removal")
	assert.Equal(t, 1, reader.reads)

	reader = &recordingSpawnAuthorityReader{snapshot: spawnAuthorityFactsSnapshot{
		PrincipalVerified: true, Sources: onlySudo, Defaults: map[string]bool{PermAgentSpawn: true},
	}}
	decision, err = (spawnAuthorityEvaluator{reader: reader}).EvaluateSpawn(context.Background(), validAgentSpawnRequest(), permissionReadLegacy)
	require.NoError(t, err)
	assert.Equal(t, int64(41), decision.SudoGrantID, "selected source remains the sudo grant")
	assert.Zero(t, decision.LoadBearingSudo, "the same captured default allows the counterfactual")
	assert.Equal(t, 1, reader.reads)
}

func TestSpawnAuthorityGlobalAndGroupGrantsRemainIndependent(t *testing.T) {
	req := validAgentSpawnRequest()
	src := capturedSpawnSources()
	src.override[PermAgentSpawn] = overridePermSource{Effect: db.PermEffectDeny}
	src.group[PermGroupsMembersSpawn] = []string{`{"group":["team"]}`}
	reader := &recordingSpawnAuthorityReader{snapshot: spawnAuthorityFactsSnapshot{PrincipalVerified: true, Sources: src, Defaults: map[string]bool{}}}
	decision, err := (spawnAuthorityEvaluator{reader: reader}).EvaluateSpawn(context.Background(), req, permissionReadLegacy)
	require.NoError(t, err)
	assert.Equal(t, PermGroupsMembersSpawn, decision.AuthorizedSlug)
	assert.Equal(t, permSourceGroup, decision.Source)
	assert.False(t, decision.AllowAnyGroup)
	assert.Equal(t, permDeny, decision.Alternatives[PermAgentSpawn].Resolution)

	src = capturedSpawnSources()
	src.ownedGroups = []db.OwnedGroupScopes{{Name: "team", OwnerScopesJSON: `{"groups.members.spawn":{"sandbox_profile":["strict"]}}`}}
	reader = &recordingSpawnAuthorityReader{snapshot: spawnAuthorityFactsSnapshot{
		PrincipalVerified: true, Sources: src, Defaults: map[string]bool{PermAgentSpawn: true},
	}}
	decision, err = (spawnAuthorityEvaluator{reader: reader}).EvaluateSpawn(context.Background(), req, permissionReadLegacy)
	require.NoError(t, err)
	assert.Equal(t, PermAgentSpawn, decision.AuthorizedSlug, "the broader independent global grant wins first")
	assert.True(t, decision.AllowAnyGroup)
	assert.False(t, decision.MatchedDims[ScopeDimSandboxProfile], "owner narrowing must not narrow a broader global grant")
	assert.Equal(t, permSourceOwner, decision.Alternatives[PermGroupsMembersSpawn].Source)
}

func TestSpawnAuthorityHTTPUsesCapturedPinnedAndUnpinnedEvidence(t *testing.T) {
	for _, tc := range []struct {
		name      string
		scopeJSON string
		pinned    bool
	}{
		{"pinned", `{"sandbox_profile":["strict"]}`, true},
		{"unpinned", `{"group":["team"]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupTestDB(t)
			_, _, err := db.EnsureAgentForConv("http-conv", "test")
			require.NoError(t, err)
			src := capturedSpawnSources()
			src.group[PermGroupsMembersSpawn] = []string{tc.scopeJSON}
			reader := &recordingSpawnAuthorityReader{snapshot: spawnAuthorityFactsSnapshot{PrincipalVerified: true, Sources: src, Defaults: map[string]bool{}}}
			old := spawnAuthorityFacts
			spawnAuthorityFacts = reader
			t.Cleanup(func() { spawnAuthorityFacts = old })

			action := ActionContext{Group: "team", SpawnProfile: "worker", SandboxProfile: "strict"}
			w := httptest.NewRecorder()
			r := requestWithPeer(&peer{PID: 99, HasClaudeAncestor: true, ConvID: "http-conv"})
			caller, ok := requireSpawnPermission(w, r, &db.AgentGroup{Name: "team"}, action)
			require.True(t, ok, "body=%s", w.Body.String())
			assert.Equal(t, "http-conv", caller)
			// Simulate the admission source disappearing before launch. The DB
			// also has no corresponding grant, so any fallback reread loses the
			// pin and makes the pinned case fail.
			reader.snapshot = spawnAuthorityFactsSnapshot{PrincipalVerified: true, Sources: capturedSpawnSources(), Defaults: map[string]bool{}}
			assert.Equal(t, tc.pinned, scopePinsDimension(r, caller, PermGroupsMembersSpawn,
				action, ScopeDimSandboxProfile),
				"the production caller's original action must consume captured evidence")
			assert.Equal(t, 1, reader.reads, "launch evidence must not reread authority facts")
			assert.False(t, scopePinsDimension(r, caller, PermGroupsMembersSpawn,
				ActionContext{Group: "other", SpawnProfile: "worker", SandboxProfile: "strict", structuralGroup: "other"}, ScopeDimSandboxProfile),
				"private evidence must not bleed into another action")
		})
	}
}

func TestSpawnAuthorityHTTPDenialAndFailureReadOnce(t *testing.T) {
	for _, tc := range []struct {
		name       string
		reader     *recordingSpawnAuthorityReader
		wantStatus int
	}{
		{"denied", &recordingSpawnAuthorityReader{snapshot: spawnAuthorityFactsSnapshot{PrincipalVerified: true, Sources: capturedSpawnSources(), Defaults: map[string]bool{}}}, http.StatusForbidden},
		{"reader failure", &recordingSpawnAuthorityReader{err: errors.New("fact read failed")}, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupTestDB(t)
			_, _, err := db.EnsureAgentForConv("http-conv", "test")
			require.NoError(t, err)
			old := spawnAuthorityFacts
			spawnAuthorityFacts = tc.reader
			t.Cleanup(func() { spawnAuthorityFacts = old })
			w := httptest.NewRecorder()
			r := requestWithPeer(&peer{PID: 99, HasClaudeAncestor: true, ConvID: "http-conv"})
			r.Header.Set(authorizedPermissionHeader, PermAgentSpawn)
			r.Header.Set(authorizedSudoGrantHeader, "999")
			_, ok := requireSpawnPermission(w, r, &db.AgentGroup{Name: "team"}, ActionContext{SandboxProfile: "strict"})
			assert.False(t, ok)
			assert.Equal(t, tc.wantStatus, w.Code, "body=%s", w.Body.String())
			assert.Equal(t, 1, tc.reader.reads)
			assert.Empty(t, authorizedPermissionForRequest(r, ""), "forged internal evidence is cleared")
			assert.Zero(t, authorizedSudoGrantIDForRequest(r))
		})
	}
}

func TestSpawnAuthorityPrivateRefusalEvidenceIsActionBound(t *testing.T) {
	setupTestDB(t)
	_, _, err := db.EnsureAgentForConv("http-conv", "test")
	require.NoError(t, err)
	require.NoError(t, db.GrantAgentPermission("http-conv", PermGroupsMembersSpawn, "test"))

	r := requestWithPeer(&peer{PID: 99, HasClaudeAncestor: true, ConvID: "http-conv"})
	refusedAction := ActionContext{Group: "team", SpawnProfile: "worker", SandboxProfile: "strict", structuralGroup: "team"}
	r = r.WithContext(context.WithValue(r.Context(), spawnAuthorityRefusalContextKey{}, spawnAuthorityRefusalContext{
		ConvID: "http-conv", Slug: PermGroupsMembersSpawn, Action: spawnActionKey(refusedAction),
	}))
	w := httptest.NewRecorder()
	_, ok := requirePermissionEx(w, r, PermGroupsMembersSpawn,
		ActionContext{Group: "other", SpawnProfile: "worker", SandboxProfile: "strict", structuralGroup: "other"})
	require.True(t, ok, "evidence for one spawn action must not suppress evaluation of another; body=%s", w.Body.String())

	r = requestWithPeer(&peer{PID: 99, HasClaudeAncestor: true, ConvID: "http-conv"})
	r = r.WithContext(context.WithValue(r.Context(), spawnAuthorityRefusalContextKey{}, spawnAuthorityRefusalContext{
		ConvID: "http-conv", Slug: PermGroupsMembersSpawn, Action: spawnActionKey(refusedAction),
	}))
	w = httptest.NewRecorder()
	_, ok = requirePermissionEx(w, r, PermGroupsMembersSpawn, refusedAction)
	assert.False(t, ok, "the exact captured refusal must suppress a changed standing-policy reread")
	assert.Equal(t, http.StatusForbidden, w.Code)
}
