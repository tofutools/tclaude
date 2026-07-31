package agentd_test

import (
	"fmt"
	"net/http"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// resolverConflictProfile is the TCL-883 shape: a Proxy filter engine plus a
// filesystem grant that binds the system resolver socket into the sandbox, which
// takes name authority away from the engine. On the Linux tclaude layer that is
// a typed *harness.SandboxCapabilityError; other targets deploy no proxy engine
// and are unaffected by the same profile, which is exactly what makes it the
// right fixture for a MIXED request.
func resolverConflictProfile(name string) map[string]any {
	return map[string]any{
		"name":        name,
		"environment": []any{},
		"filesystem": []any{
			map[string]any{
				"path": "/run/systemd/resolve", "access": "read",
			},
		},
		"network": map[string]any{
			"baseline": "deny",
			"engine":   "proxy",
			"allow": []any{
				map[string]any{"domain": "example.com", "ports": []any{443}},
			},
		},
	}
}

type refusalJSON struct {
	Kind    string `json:"kind"`
	Harness string `json:"harness"`
	Message string `json:"message"`
}

type enforcementTargetJSON struct {
	Implementation string                      `json:"implementation"`
	Harness        string                      `json:"harness"`
	Platform       string                      `json:"platform"`
	Predicted      bool                        `json:"predicted"`
	Axes           harness.PredictedAccessAxes `json:"axes"`
	Refusal        *refusalJSON                `json:"refusal"`
}

// TestSandboxProfileEnforcementRendersMixedRefusedAndCleanTargets is the ticket's
// central claim: ONE request naming a conflicted target and a clean target
// returns rows for both, instead of failing the whole prediction.
//
// Falsifiability: with the refusal branch in handleSandboxProfileEnforcement
// reverted (delete the `if refusal := ...; refusal != nil` block so the error
// falls through to writeError), this test fails at the first require on
// rec.Code — the response is 400 and carries ZERO targets, so neither the
// refused row nor the clean row exists. The pre-change value of len(Targets) is
// unreachable (no JSON body at all); the post-change value is 2.
func TestSandboxProfileEnforcementRendersMixedRefusedAndCleanTargets(t *testing.T) {
	f := newFlow(t)
	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles",
		resolverConflictProfile("resolver-conflict"))
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	rec = profileReq(t, f, http.MethodGet,
		"/v1/sandbox-profiles/resolver-conflict/enforcement?"+
			"for=tclaude-layer%2Fclaude%2Flinux&"+
			"for=harness-builtin%2Fclaude%2Flinux", nil)
	require.Equalf(t, http.StatusOK, rec.Code,
		"a per-target capability conflict must not fail the whole request; body=%s",
		rec.Body.String())

	var got struct {
		Profile string                  `json:"profile"`
		Targets []enforcementTargetJSON `json:"targets"`
	}
	testharness.DecodeJSON(t, rec, &got)
	assert.Equal(t, "resolver-conflict", got.Profile)
	require.Len(t, got.Targets, 2,
		"both the refused and the unaffected target must render a row")

	refused := got.Targets[0]
	assert.Equal(t, "tclaude-layer", refused.Implementation)
	assert.False(t, refused.Predicted,
		"a refused target reports no prediction")
	require.NotNil(t, refused.Refusal,
		"the refusal is its own field, never an axis verdict")
	// Discriminating, not a bare "something was refused": the exact capability
	// this target lacks, and the remedy the operator can act on.
	assert.Equal(t, harness.SandboxCapabilityNetworkAllowlist, refused.Refusal.Kind)
	assert.Contains(t, refused.Refusal.Message, "proxy_engine_name_authority")
	assert.Contains(t, refused.Refusal.Message,
		"/run/systemd/resolve/io.systemd.Resolve")
	assert.Contains(t, refused.Refusal.Message, "Packet filter engine",
		"the refusal must carry its named remedy, not just the diagnosis")
	// The refused target carries the ZERO axes. Consumers therefore MUST branch
	// on Refusal first; this assertion is what makes that contract testable
	// rather than incidental.
	assert.Empty(t, refused.Axes.Network.Outcome,
		"a refused target must not fabricate an axis outcome")
	assert.Empty(t, refused.Axes.Filesystem.Outcome)

	clean := got.Targets[1]
	assert.Equal(t, "harness-builtin", clean.Implementation)
	assert.True(t, clean.Predicted,
		"the unaffected target keeps its normal rows")
	assert.Nil(t, clean.Refusal)
	assert.NotEmpty(t, clean.Axes.Filesystem.Outcome,
		"the clean target must carry real per-axis verdicts")
	assert.NotEmpty(t, clean.Axes.Network.Outcome)
}

// TestSandboxProfileEnforcementRefusalNeverMasksAWholeRequestError pins the RULE
// for what becomes a row and what stays a 400.
//
// An unparseable --for target is a malformed REQUEST, detected before any
// prediction runs, so there is no valid target to attach a row to. It must stay
// a whole-request 400 even when a sibling target in the same request would have
// produced a legitimate refusal row — otherwise a typo silently renders as a
// capability verdict about a target that does not exist.
//
// Falsifiability: broaden sandboxProfilePredictionRefusal to return a refusal
// for ANY error (drop the errors.As guard and synthesize one). That alone does
// not flip this test, which is the point — parse failures never reach that
// function. To make it fail you must move parsing behind the refusal branch;
// doing so turns the pre-change 400 into a 200 with a row, and the first require
// below fails on Code (200 != 400).
func TestSandboxProfileEnforcementRefusalNeverMasksAWholeRequestError(t *testing.T) {
	f := newFlow(t)
	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles",
		resolverConflictProfile("resolver-conflict-parse"))
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	for _, tc := range []struct {
		name    string
		query   string
		message string
	}{{
		name:    "unknown implementation",
		query:   "for=tclaude-layer%2Fclaude%2Flinux&for=not-an-implementation",
		message: "implementation: off, harness-builtin, tclaude-layer, stacked",
	}, {
		name:    "unsupported platform",
		query:   "for=tclaude-layer%2Fclaude%2Flinux&for=tclaude-layer%2Fclaude%2Fwindows",
		message: "platform: linux, darwin",
	}, {
		name:    "stacked outside linux",
		query:   "for=tclaude-layer%2Fclaude%2Flinux&for=stacked%2Fclaude%2Fdarwin",
		message: "stacked sandbox prediction is supported only on linux",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			rec := profileReq(t, f, http.MethodGet,
				"/v1/sandbox-profiles/resolver-conflict-parse/enforcement?"+tc.query, nil)
			require.Equalf(t, http.StatusBadRequest, rec.Code,
				"a malformed --for target must stay a whole-request error, not become a row; body=%s",
				rec.Body.String())
			assert.Contains(t, rec.Body.String(), tc.message)
			assert.NotContains(t, rec.Body.String(), "\"refusal\"",
				"an unparseable target must never render a refusal row")
			assert.NotContains(t, rec.Body.String(), "proxy_engine_name_authority",
				"the sibling target's real conflict must not leak into the request error")
		})
	}
}

// TestSandboxProfilePreviewAndLaunchAgreeOnTheRefusedTarget is the parity
// assertion required by the map. Preview and launch must reach the same verdict
// from the same evaluation, and they must express it DIFFERENTLY: the preview
// renders a per-target row so the operator can still read the rest of the
// request, while the launch path refuses outright and produces no plan at all.
//
// If a future change makes the launch path emit rows instead of refusing, the
// second half of this test fails on Code (200 != 400) — that is the
// launch-parity invariant this ticket must not break.
//
// Falsifiability: reverting the preview refusal branch flips the FIRST half
// (preview 200-with-row becomes 400). Adding a refusal branch to
// sandbox_profile_plan.go's PredictAccessEnforcement call flips the SECOND half
// (launch 400 becomes 200). Each half fails independently, so neither can pass
// vacuously because of the other.
func TestSandboxProfilePreviewAndLaunchAgreeOnTheRefusedTarget(t *testing.T) {
	f := newFlow(t)
	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles",
		resolverConflictProfile("resolver-conflict-parity"))
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	// Preview: a row, and the request as a whole succeeds.
	rec = profileReq(t, f, http.MethodGet,
		"/v1/sandbox-profiles/resolver-conflict-parity/enforcement?"+
			"for=tclaude-layer%2Fclaude%2Flinux", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var preview struct {
		Targets []enforcementTargetJSON `json:"targets"`
	}
	testharness.DecodeJSON(t, rec, &preview)
	require.Len(t, preview.Targets, 1)
	require.NotNil(t, preview.Targets[0].Refusal)
	previewMessage := preview.Targets[0].Refusal.Message

	// Launch: the SAME conflict, refusing outright. No plan, no rows.
	rec = profileReq(t, f, http.MethodPost, "/v1/sandbox-profile-plan", map[string]any{
		"sandbox_profile": "resolver-conflict-parity",
		"for":             "tclaude-layer/claude/linux",
	})
	require.Equalf(t, http.StatusBadRequest, rec.Code,
		"LAUNCH PARITY: the plan path must keep refusing outright, never emit rows; body=%s",
		rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "\"refusal\"",
		"the launch path has no per-target row shape")
	// Same evaluation, therefore the same capability sentence on both sides.
	assert.Contains(t, rec.Body.String(), "proxy_engine_name_authority")
	assert.Contains(t, previewMessage, "proxy_engine_name_authority")
}

// TestSandboxProfileDraftEnforcementRendersMixedRefusedAndCleanTargets covers the
// editor's own endpoint, which predicts an unsaved draft against several targets
// at once. Same claim as the saved-profile case, different handler and a
// different wire shape.
//
// Falsifiability: revert either refusal branch in
// handleSandboxProfileDraftEnforcement and the first require fails — the draft
// endpoint answers 400 with no targets, so the pre-change len(Targets) is
// unreachable where the post-change value is 2.
func TestSandboxProfileDraftEnforcementRendersMixedRefusedAndCleanTargets(t *testing.T) {
	f := newFlow(t)
	draft := resolverConflictProfile("resolver-conflict-draft")
	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profile-enforcement", map[string]any{
		"draft": draft,
		"targets": []any{
			map[string]any{
				"implementation": "tclaude-layer", "harness": "claude", "platform": "linux",
			},
			map[string]any{
				"implementation": "harness-builtin", "harness": "claude", "platform": "linux",
			},
		},
	})
	require.Equalf(t, http.StatusOK, rec.Code,
		"one conflicted target must not blank the editor's whole preview; body=%s",
		rec.Body.String())

	var got struct {
		Targets []struct {
			Target struct {
				Implementation string `json:"implementation"`
			} `json:"target"`
			Predicted       bool                        `json:"predicted"`
			Axes            harness.PredictedAccessAxes `json:"axes"`
			Refusal         *refusalJSON                `json:"refusal"`
			ContextRefusals []*refusalJSON              `json:"context_refusals"`
		} `json:"targets"`
	}
	testharness.DecodeJSON(t, rec, &got)
	require.Len(t, got.Targets, 2)

	refused := got.Targets[0]
	assert.Equal(t, "tclaude-layer", refused.Target.Implementation)
	assert.False(t, refused.Predicted)
	require.NotNil(t, refused.Refusal)
	assert.Equal(t, harness.SandboxCapabilityNetworkAllowlist, refused.Refusal.Kind)
	assert.Contains(t, refused.Refusal.Message, "proxy_engine_name_authority")
	assert.Empty(t, refused.Axes.Network.Outcome,
		"a refused draft target must not fabricate an axis outcome")

	clean := got.Targets[1]
	assert.Equal(t, "harness-builtin", clean.Target.Implementation)
	assert.True(t, clean.Predicted)
	assert.Nil(t, clean.Refusal)
	assert.NotEmpty(t, clean.Axes.Network.Outcome)
}

// TestSandboxProfileRefusalIsDistinguishableFromAMissingAxis is the test the
// lead asked for, and the one most likely to be missed.
//
// sandbox-profiles-data.js already substitutes
// {outcome:'not_enforced', detail:'No enforcement verdict was returned.'} for an
// axis an OLD daemon omitted. If a refusal were encoded as a missing or odd axis
// outcome it would be indistinguishable from that fallback, and a refusal would
// silently render as "not enforced" — the exact failure this ticket exists to
// remove, moved one layer down.
//
// This pins the two apart at the wire: a refusal is a NON-EMPTY dedicated field
// with a capability kind, while a missing axis is an ABSENT axis with no such
// field. A refusal folded into the axis path cannot satisfy both halves.
//
// Falsifiability: encode the refusal as an axis instead — set
// Axes.Network = {Outcome: "not_enforced", Detail: capability.Message} and drop
// the Refusal field. The refused half then fails on `require.NotNil(refusal)`
// AND on the outcome comparison, because the refused target's network outcome
// becomes "not_enforced", identical to the old-daemon fallback value asserted
// two lines below. The pre-change refused outcome is "" and the fallback value
// is "not_enforced"; they DIFFER, which is what the test requires.
func TestSandboxProfileRefusalIsDistinguishableFromAMissingAxis(t *testing.T) {
	f := newFlow(t)
	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles",
		resolverConflictProfile("resolver-conflict-discriminate"))
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	rec = profileReq(t, f, http.MethodGet,
		"/v1/sandbox-profiles/resolver-conflict-discriminate/enforcement?"+
			"for=tclaude-layer%2Fclaude%2Flinux", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var got struct {
		Targets []enforcementTargetJSON `json:"targets"`
	}
	testharness.DecodeJSON(t, rec, &got)
	require.Len(t, got.Targets, 1)
	target := got.Targets[0]

	require.NotNil(t, target.Refusal,
		"a refusal must be carried by its own field, so a consumer can branch on it")
	assert.NotEmpty(t, target.Refusal.Kind,
		"the field names the missing capability; the axis fallback has no such value")

	// The missing-axis fallback's value. A refused target must NOT report it,
	// or the dashboard's fallback and a real refusal become the same rendering.
	const missingAxisFallbackOutcome = harness.AccessPredictionNotEnforced
	assert.NotEqual(t, missingAxisFallbackOutcome, target.Axes.Network.Outcome,
		"a refusal must not arrive wearing the missing-axis fallback's outcome")
	assert.NotEqual(t, missingAxisFallbackOutcome, target.Axes.Filesystem.Outcome)
	assert.NotEqual(t, missingAxisFallbackOutcome, target.Axes.UnixSockets.Outcome)

	// And the converse direction: a target that really does predict carries
	// real axis outcomes and NO refusal field, so the two states are disjoint
	// in both directions rather than merely different in one.
	rec = profileReq(t, f, http.MethodGet,
		"/v1/sandbox-profiles/resolver-conflict-discriminate/enforcement?"+
			"for=harness-builtin%2Fclaude%2Flinux", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	// A FRESH destination, deliberately. encoding/json reuses the elements of a
	// non-empty destination slice and only overwrites the fields the payload
	// actually contains, so decoding a clean target over a refused one would
	// leave the previous refusal in place and this assertion would fail against
	// a value the daemon never sent.
	var predictedOnly struct {
		Targets []enforcementTargetJSON `json:"targets"`
	}
	testharness.DecodeJSON(t, rec, &predictedOnly)
	require.Len(t, predictedOnly.Targets, 1)
	assert.Nil(t, predictedOnly.Targets[0].Refusal)
	assert.NotEmpty(t, predictedOnly.Targets[0].Axes.Network.Outcome)
}

// TestSandboxProfileDraftEnforcementRefusesOneContextAndKeepsTheOthers is the
// call-site-4 claim: the capability conflict is formed ACROSS SCOPES, in one
// effective assignment context only, and refusing that context must not darken
// the verdicts of the contexts that never had the conflict.
//
// Setup: the draft is the global profile and carries the Proxy filter engine.
// One group's profile adds the resolver-reaching filesystem grant — the conflict
// exists only where those two compose. The other group adds nothing.
//
// Falsifiability: revert describePredictedDraftSandboxProfile to aggregate over
// `predictions` instead of `survivors` (and to return the error rather than a
// per-context refusal). Returning the error reverts to a whole-request 400 and
// the first require fails. Aggregating over `predictions` instead — keeping the
// refusal slice — makes the refused context's ZERO axes participate: the
// aggregate Filesystem.Outcome becomes "" (the zero value ranks as `enforced`
// and drags the tier text to "2 effective contexts") instead of the surviving
// context's real verdict, so the clean-context assertions below fail. Pre-change
// aggregate tier and post-change aggregate tier DIFFER.
func TestSandboxProfileDraftEnforcementRefusesOneContextAndKeepsTheOthers(t *testing.T) {
	f := newFlow(t)
	for _, name := range []string{"crew-conflicted", "crew-clean"} {
		_, err := db.CreateAgentGroup(name, "")
		require.NoError(t, err)
	}
	for _, body := range []map[string]any{{
		// The global layer: engine only, no filesystem grant of its own.
		"name": "global-proxy", "filesystem": []any{}, "environment": []any{},
		"network": map[string]any{
			"baseline": "deny", "engine": "proxy",
			"allow": []any{map[string]any{"domain": "example.com", "ports": []any{443}}},
		},
	}, {
		// The group layer that forms the conflict when composed with the global.
		"name": "group-resolver-grant", "environment": []any{},
		"filesystem": []any{
			map[string]any{"path": "/run/systemd/resolve", "access": "read"},
		},
	}, {
		"name": "group-harmless", "filesystem": []any{}, "environment": []any{},
	}} {
		rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", body)
		require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	}
	require.NoError(t, db.SetGlobalSandboxProfile("global-proxy"))
	_, err := db.SetAgentGroupSandboxProfile("crew-conflicted", "group-resolver-grant")
	require.NoError(t, err)
	_, err = db.SetAgentGroupSandboxProfile("crew-clean", "group-harmless")
	require.NoError(t, err)
	globalProfile, err := db.GetSandboxProfile("global-proxy")
	require.NoError(t, err)
	require.NotNil(t, globalProfile)

	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profile-enforcement", map[string]any{
		"draft": map[string]any{
			"id": globalProfile.ID, "name": globalProfile.Name,
			"filesystem": []any{}, "environment": []any{},
			"network": map[string]any{
				"baseline": "deny", "engine": "proxy",
				"allow": []any{map[string]any{"domain": "example.com", "ports": []any{443}}},
			},
			"unix_sockets": map[string]any{},
		},
		"targets": []any{map[string]any{
			"implementation": "tclaude-layer", "harness": "claude", "platform": "linux",
		}},
	})
	require.Equalf(t, http.StatusOK, rec.Code,
		"one conflicted assignment context must not fail the whole prediction; body=%s",
		rec.Body.String())

	var got struct {
		Targets []struct {
			Predicted       bool                          `json:"predicted"`
			Axes            harness.PredictedAccessAxes   `json:"axes"`
			Refusal         *refusalJSON                  `json:"refusal"`
			ContextAxes     []harness.PredictedAccessAxes `json:"context_axes"`
			ContextRefusals []*refusalJSON                `json:"context_refusals"`
		} `json:"targets"`
		Contexts []struct {
			Context map[string]string `json:"context"`
		} `json:"contexts"`
	}
	testharness.DecodeJSON(t, rec, &got)
	require.Len(t, got.Targets, 1)
	target := got.Targets[0]
	assert.True(t, target.Predicted,
		"a target with at least one surviving context still predicts")
	assert.Nil(t, target.Refusal,
		"a single conflicted context must not refuse the whole target")
	require.Len(t, got.Contexts, 2)
	require.Len(t, target.ContextAxes, 2,
		"a refused context still occupies its own index")
	require.Len(t, target.ContextRefusals, 2,
		"the refusal slice must stay index-aligned with the axes slice")

	conflicted := -1
	for i, context := range got.Contexts {
		if context.Context["group_name"] == "crew-conflicted" {
			conflicted = i
		}
	}
	require.NotEqual(t, -1, conflicted, "the conflicted assignment must be listed")
	clean := 1 - conflicted

	require.NotNil(t, target.ContextRefusals[conflicted],
		"the context where the two scopes compose into a conflict must refuse")
	assert.Equal(t, harness.SandboxCapabilityNetworkAllowlist,
		target.ContextRefusals[conflicted].Kind)
	assert.Contains(t, target.ContextRefusals[conflicted].Message,
		"proxy_engine_name_authority")
	assert.Empty(t, target.ContextAxes[conflicted].Network.Outcome,
		"the refused index carries the zero axes, never a fabricated verdict")

	assert.Nil(t, target.ContextRefusals[clean],
		"the context without the grant must keep its ordinary rows")
	assert.NotEmpty(t, target.ContextAxes[clean].Network.Outcome)
	assert.NotEmpty(t, target.ContextAxes[clean].Filesystem.Outcome)

	// The aggregate must summarize the SURVIVING context only, and this compares
	// the WHOLE axis rather than its outcome. Outcome alone is not discriminating
	// here: the refused context's zero axes carry outcome "", which ranks the same
	// as "enforced", so an aggregate that wrongly included them can still report
	// the surviving context's outcome by coincidence. Tier and Detail cannot
	// coincide — with two inputs aggregateSandboxFeature rewrites them to
	// "2 effective contexts" and "1 of 2 effective contexts have this worst
	// outcome: …", where a single survivor is returned verbatim.
	assert.Equal(t, target.ContextAxes[clean].Filesystem, target.Axes.Filesystem,
		"the aggregate must reflect only the context that actually predicted")
	assert.Equal(t, target.ContextAxes[clean].Network, target.Axes.Network)
	assert.Equal(t, target.ContextAxes[clean].UnixSockets, target.Axes.UnixSockets)
	assert.NotContains(t, target.Axes.Filesystem.Tier, "effective contexts",
		"a refused context must not be counted as an input to the aggregate")
}

// TestSandboxProfileDraftEnforcementRefusesTargetWhenEveryContextRefuses is the
// other half of the same shape. With nothing left to aggregate, a list of
// per-context refusals with no surviving verdict between them would be a worse
// answer than one target-level refusal, so the target collapses to refused.
//
// Falsifiability: delete the onlyContextRefusals collapse in
// handleSandboxProfileDraftEnforcement. The target then reports Predicted=true
// with a refusal-free target object whose aggregate axes are the ZERO value —
// pre-change Predicted is false with a non-nil Refusal, post-change it is true
// with a nil Refusal, so the two requires below fail and the values DIFFER.
func TestSandboxProfileDraftEnforcementRefusesTargetWhenEveryContextRefuses(t *testing.T) {
	f := newFlow(t)
	for _, name := range []string{"crew-a", "crew-b"} {
		_, err := db.CreateAgentGroup(name, "")
		require.NoError(t, err)
	}
	// The conflict must form ONLY in composition, or this test would not reach
	// the collapse at all: a draft that conflicts on its own already refuses at
	// the flattened-axes call site, before any context is evaluated. So the
	// engine lives in the draft (global) and the resolver-reaching grant lives in
	// the group profile that BOTH groups are assigned — the draft alone is clean,
	// and every effective assignment context conflicts.
	for _, body := range []map[string]any{{
		"name": "global-proxy-only", "filesystem": []any{}, "environment": []any{},
		"network": map[string]any{
			"baseline": "deny", "engine": "proxy",
			"allow": []any{map[string]any{"domain": "example.com", "ports": []any{443}}},
		},
	}, {
		"name": "group-resolver-everywhere", "environment": []any{},
		"filesystem": []any{
			map[string]any{"path": "/run/systemd/resolve", "access": "read"},
		},
	}} {
		rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", body)
		require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	}
	require.NoError(t, db.SetGlobalSandboxProfile("global-proxy-only"))
	for _, name := range []string{"crew-a", "crew-b"} {
		_, err := db.SetAgentGroupSandboxProfile(name, "group-resolver-everywhere")
		require.NoError(t, err)
	}
	globalProfile, err := db.GetSandboxProfile("global-proxy-only")
	require.NoError(t, err)
	require.NotNil(t, globalProfile)

	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profile-enforcement", map[string]any{
		"draft": map[string]any{
			"id": globalProfile.ID, "name": globalProfile.Name,
			"filesystem": []any{}, "environment": []any{},
			"network": map[string]any{
				"baseline": "deny", "engine": "proxy",
				"allow": []any{map[string]any{"domain": "example.com", "ports": []any{443}}},
			},
			"unix_sockets": map[string]any{},
		},
		"targets": []any{map[string]any{
			"implementation": "tclaude-layer", "harness": "claude", "platform": "linux",
		}},
	})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var got struct {
		Targets []struct {
			Predicted       bool                          `json:"predicted"`
			Axes            harness.PredictedAccessAxes   `json:"axes"`
			Refusal         *refusalJSON                  `json:"refusal"`
			ContextAxes     []harness.PredictedAccessAxes `json:"context_axes"`
			ContextRefusals []*refusalJSON                `json:"context_refusals"`
		} `json:"targets"`
		Contexts []struct {
			Context map[string]string `json:"context"`
		} `json:"contexts"`
	}
	testharness.DecodeJSON(t, rec, &got)
	require.Len(t, got.Targets, 1)
	require.Len(t, got.Contexts, 2,
		"both groups must produce an assignment context, so this is a real all-refused case")
	target := got.Targets[0]
	assert.False(t, target.Predicted)
	require.NotNil(t, target.Refusal,
		"with no surviving context the target itself is refused")
	assert.Equal(t, harness.SandboxCapabilityNetworkAllowlist, target.Refusal.Kind)
	assert.Contains(t, target.Refusal.Message, "proxy_engine_name_authority")
	assert.Empty(t, target.Axes.Filesystem.Outcome,
		"an all-refused target must not fabricate an aggregate verdict")
	assert.Empty(t, target.ContextAxes,
		"a target-level refusal replaces the per-context lists rather than duplicating them")
	assert.Empty(t, target.ContextRefusals)
}

// TestSandboxProfileDraftEnforcementCarriesRefusalsPastTheDisplayCap closes a
// gap a cold review found. The per-context lists are capped at 10 for display,
// and the aggregate deliberately summarizes SURVIVING contexts only — so before
// this fix a refusal in context 11 was truncated off the wire AND absent from
// the aggregate, i.e. invisible, while the editor still tells the operator the
// omitted assignments "are still included in the overall safety check".
//
// Falsifiability: delete the omittedRefusals collection in
// handleSandboxProfileDraftEnforcement (return an empty slice). OmittedRefusals
// is then empty where it is now length 2, and the require below fails. The
// pre-change value is 0 and the post-change value is 2; they DIFFER.
func TestSandboxProfileDraftEnforcementCarriesRefusalsPastTheDisplayCap(t *testing.T) {
	f := newFlow(t)
	// 12 groups: more than the 10-context display cap. Two of them get the
	// resolver grant, so their contexts conflict and the other ten do not.
	for i := range 12 {
		_, err := db.CreateAgentGroup(fmt.Sprintf("crew-%02d", i), "")
		require.NoError(t, err)
	}
	for _, body := range []map[string]any{{
		"name": "global-proxy-cap", "filesystem": []any{}, "environment": []any{},
		"network": map[string]any{
			"baseline": "deny", "engine": "proxy",
			"allow": []any{map[string]any{"domain": "example.com", "ports": []any{443}}},
		},
	}, {
		"name": "group-resolver-cap", "environment": []any{},
		"filesystem": []any{
			map[string]any{"path": "/run/systemd/resolve", "access": "read"},
		},
	}} {
		rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", body)
		require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())
	}
	require.NoError(t, db.SetGlobalSandboxProfile("global-proxy-cap"))
	// The LAST two groups, so their contexts land beyond the display cap.
	for _, name := range []string{"crew-10", "crew-11"} {
		_, err := db.SetAgentGroupSandboxProfile(name, "group-resolver-cap")
		require.NoError(t, err)
	}
	globalProfile, err := db.GetSandboxProfile("global-proxy-cap")
	require.NoError(t, err)
	require.NotNil(t, globalProfile)

	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profile-enforcement", map[string]any{
		"draft": map[string]any{
			"id": globalProfile.ID, "name": globalProfile.Name,
			"filesystem": []any{}, "environment": []any{},
			"network": map[string]any{
				"baseline": "deny", "engine": "proxy",
				"allow": []any{map[string]any{"domain": "example.com", "ports": []any{443}}},
			},
			"unix_sockets": map[string]any{},
		},
		"targets": []any{map[string]any{
			"implementation": "tclaude-layer", "harness": "claude", "platform": "linux",
		}},
	})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var got struct {
		Targets []struct {
			Predicted       bool                          `json:"predicted"`
			ContextAxes     []harness.PredictedAccessAxes `json:"context_axes"`
			ContextRefusals []*refusalJSON                `json:"context_refusals"`
			OmittedRefusals []*refusalJSON                `json:"omitted_refusals"`
		} `json:"targets"`
		Contexts          []struct{} `json:"contexts"`
		RemainingContexts int        `json:"remaining_contexts"`
	}
	testharness.DecodeJSON(t, rec, &got)
	require.Len(t, got.Targets, 1)
	target := got.Targets[0]

	require.Len(t, got.Contexts, 10, "the display cap must still apply")
	assert.Equal(t, 2, got.RemainingContexts)
	assert.Len(t, target.ContextAxes, 10)
	assert.Len(t, target.ContextRefusals, 10,
		"the displayed slices stay index-aligned and capped")

	// The refusals that would otherwise have been truncated into silence.
	require.Len(t, target.OmittedRefusals, 2,
		"a refusal past the display cap must still reach the client")
	for _, refusal := range target.OmittedRefusals {
		require.NotNil(t, refusal)
		assert.Equal(t, harness.SandboxCapabilityNetworkAllowlist, refusal.Kind)
		assert.Contains(t, refusal.Message, "proxy_engine_name_authority")
	}
	// The ten displayed contexts are genuinely clean, so this is a real
	// "invisible past the cap" case rather than a target-wide refusal.
	assert.True(t, target.Predicted)
	for i, refusal := range target.ContextRefusals {
		assert.Nilf(t, refusal, "displayed context %d must be clean", i)
	}
}

// TestSandboxProfileDraftEnforcementDoesNotRefuseOnDraftOnlyConflict closes the
// second gap the cold review found, in the opposite direction: OVER-claiming.
//
// The draft-only axes are the draft WITHOUT its includes, and they feed nothing
// but the authoring-level network rows. No launch uses that policy. When an
// include contributes a deny that removes the offending grant, the COMPOSED
// policy — the one a launch carries — is clean, so refusing the target here
// would claim a launch refusal the launch path would not make.
//
// Falsifiability: restore the refusal branch on the draftAxes prediction (make
// it append a refused target and continue). Predicted then flips true -> false
// and Refusal nil -> non-nil, so the two requires below fail; the values DIFFER.
// This case also proves the branch is REACHED, which the earlier draft test did
// not do — there the conflict was in the draft itself, so the composed-policy
// branch fired first and this code never ran.
func TestSandboxProfileDraftEnforcementDoesNotRefuseOnDraftOnlyConflict(t *testing.T) {
	f := newFlow(t)
	// The include denies the resolver socket at a MORE SPECIFIC path than the
	// draft's own read grant. The conflict detector resolves per guest position
	// by "most specific covering grant wins", so the socket is denied in the
	// COMPOSED policy while the draft on its own still carries the conflict. An
	// include cannot override the draft at the SAME path — the draft's own
	// entries are applied last — which is why this uses a narrower one.
	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{
		"name": "denies-resolver", "environment": []any{},
		"filesystem": []any{
			map[string]any{"path": "/run/nscd/socket", "access": "deny"},
		},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	// A resolver socket whose directory is not present on this host, so both the
	// broad grant and the narrower deny are authorable as ordinary rows.
	draft := resolverConflictProfile("draft-only-conflict")
	draft["filesystem"] = []any{
		map[string]any{"path": "/run/nscd", "access": "read"},
	}
	draft["includes"] = []any{"denies-resolver"}
	draft["unix_sockets"] = map[string]any{}
	rec = profileReq(t, f, http.MethodPost, "/v1/sandbox-profile-enforcement", map[string]any{
		"draft": draft,
		"targets": []any{map[string]any{
			"implementation": "tclaude-layer", "harness": "claude", "platform": "linux",
		}},
	})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var got struct {
		Targets []struct {
			Predicted      bool                            `json:"predicted"`
			Axes           harness.PredictedAccessAxes     `json:"axes"`
			Refusal        *refusalJSON                    `json:"refusal"`
			NetworkEntries []harness.PredictedNetworkEntry `json:"network_entries"`
		} `json:"targets"`
	}
	testharness.DecodeJSON(t, rec, &got)
	require.Len(t, got.Targets, 1)
	target := got.Targets[0]

	assert.True(t, target.Predicted,
		"the composed policy is clean, so the target must not be refused")
	require.Nil(t, target.Refusal,
		"PREVIEW/LAUNCH PARITY: refusing here would claim a refusal the launch would not make")
	assert.NotEmpty(t, target.Axes.Network.Outcome,
		"the target keeps the verdicts its composed policy earned")
	// The authoring-level rows are the one thing the draft-only evaluation feeds,
	// and they are omitted rather than rendered from a refused evaluation.
	assert.Empty(t, target.NetworkEntries,
		"rows from a refused evaluation would show verdicts that were never computed")
}

// TestSandboxProfileEnforcementRefusalKeepsThePredictionCaveat closes a third
// cold-review gap. A refusal is a prediction like any other and inherits the
// same qualification; before this fix the refused row skipped the caveat, so on
// a host that ran no capability probes it read MORE confidently than the
// enforced rows beside it.
//
// Falsifiability: drop the `Caveat:` field from the refused row in
// handleSandboxProfileEnforcement. The refused target's Caveat is then "" where
// it is now the non-host-platform sentence, and the require below fails — while
// the clean target's caveat is unchanged, so the two are compared against each
// other rather than against a constant.
func TestSandboxProfileEnforcementRefusalKeepsThePredictionCaveat(t *testing.T) {
	f := newFlow(t)
	rec := profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles",
		resolverConflictProfile("resolver-conflict-caveat"))
	require.Equalf(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	// Predict for a platform that is not this host, so the caveat applies. The
	// conflict is Linux-only, so the refused row must be the linux one; the
	// darwin row is the control that shows the caveat is platform-derived.
	rec = profileReq(t, f, http.MethodGet,
		"/v1/sandbox-profiles/resolver-conflict-caveat/enforcement?"+
			"for=tclaude-layer%2Fclaude%2Flinux&"+
			"for=tclaude-layer%2Fclaude%2Fdarwin", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var got struct {
		Targets []struct {
			Platform string       `json:"platform"`
			Caveat   string       `json:"caveat"`
			Refusal  *refusalJSON `json:"refusal"`
		} `json:"targets"`
	}
	testharness.DecodeJSON(t, rec, &got)
	require.Len(t, got.Targets, 2)
	refused, other := got.Targets[0], got.Targets[1]
	require.NotNil(t, refused.Refusal, "the linux target is the refused one")
	require.Nil(t, other.Refusal, "darwin deploys no proxy engine here")

	if runtime.GOOS != "linux" {
		require.NotEmpty(t, refused.Caveat,
			"a refused prediction must carry the same qualification as an enforced one")
		assert.Contains(t, refused.Caveat, "non-host platform")
	}
	if runtime.GOOS != "darwin" {
		assert.Contains(t, other.Caveat, "non-host platform",
			"control: the caveat is derived from the platform, not from the refusal")
	}
	// On a Linux host the refused target IS the host platform, so the caveat is
	// whatever the tooling probe says — the point is that it is computed by the
	// same helper for both rows rather than skipped for the refused one.
	assert.Equal(t, sandboxProfileCaveatForPlatform(refused.Platform), refused.Caveat != "",
		"the refused row's caveat must follow the same rule as every other row")
}

// sandboxProfileCaveatForPlatform mirrors only the platform half of the daemon's
// caveat rule, which is the half this test can predict from outside.
func sandboxProfileCaveatForPlatform(platform string) bool {
	return platform != runtime.GOOS
}
