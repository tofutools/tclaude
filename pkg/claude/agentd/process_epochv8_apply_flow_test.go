package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state/epochv8"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestEpochV8ApplyPermissionFirstDomainReplayAndProvenance(t *testing.T) {
	f, root := processEngineFlow(t)
	fs, err := store.NewFS(root)
	require.NoError(t, err)

	source := func(prompt string) []byte {
		tmpl := &model.Template{
			APIVersion: model.APIVersion,
			Kind:       model.Kind,
			ID:         "apply-flow",
			Start:      "work",
			Nodes: map[string]model.Node{
				"work": {
					Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: prompt},
					Next: model.Next{"pass": "done"},
				},
				"done": {Type: model.NodeTypeEnd, Result: "completed"},
			},
		}
		encoded, encodeErr := model.CanonicalYAML(tmpl)
		require.NoError(t, encodeErr)
		return encoded
	}
	initialSource, candidateSource := source("initial-private-source"), source("candidate-private-source")
	parsed, err := model.ParseExactSource(initialSource)
	require.NoError(t, err)
	record, err := fs.PutTemplate(t.Context(), parsed.Template)
	require.NoError(t, err)
	initialized, err := fs.InitializeEpochV8Run(t.Context(), store.RunRecord{
		ID: "apply-flow-run", TemplateRef: record.Ref,
	}, initialSource)
	require.NoError(t, err)
	base := map[string]any{
		"revision": initialized.Checkpoint.Binding().Revision,
		"digest":   initialized.Checkpoint.Binding().Digest,
	}

	preview := func(handoffs []map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost,
			"/v1/process/runs/apply-flow-run/unlock/preview", map[string]any{
				"baseBinding": base, "candidateSource": string(candidateSource),
				"reason": "restricted-reason-sentinel", "handoffs": handoffs,
			})))
		return rec
	}

	blocked := preview([]map[string]any{})
	require.Equal(t, http.StatusUnprocessableEntity, blocked.Code, blocked.Body.String())
	var blockedBody struct {
		Blockers []struct {
			Token string `json:"token"`
		} `json:"blockers"`
	}
	testharness.DecodeJSON(t, blocked, &blockedBody)
	require.Len(t, blockedBody.Blockers, 1)
	handoff := map[string]any{"token": blockedBody.Blockers[0].Token, "action": epochv8.HandoffRetain}
	valid := preview([]map[string]any{handoff})
	require.Equal(t, http.StatusOK, valid.Code, valid.Body.String())
	var previewBody struct {
		ApplyToken string `json:"applyToken"`
	}
	testharness.DecodeJSON(t, valid, &previewBody)
	require.Len(t, previewBody.ApplyToken, 64)

	applyBody := map[string]any{
		"baseBinding": base, "applyToken": previewBody.ApplyToken,
		"candidateSource": string(candidateSource), "reason": "restricted-reason-sentinel",
		"handoffs": []map[string]any{handoff},
	}
	path := "/v1/process/runs/apply-flow-run/unlock/apply"
	missingPath := "/v1/process/runs/missing-run/unlock/apply"
	engineLease, err := fs.AcquireEngineLease(t.Context(), "apply-flow-run", "test-engine", time.Minute)
	require.NoError(t, err)
	busy := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, path, applyBody)))
	require.Equal(t, http.StatusConflict, busy.Code, busy.Body.String())
	assert.Contains(t, busy.Body.String(), `"code":"process_unlock_busy"`)
	require.NoError(t, fs.ReleaseEngineLease(t.Context(), engineLease))
	afterBusy, err := fs.LoadEpochV8RunView(t.Context(), "apply-flow-run")
	require.NoError(t, err)
	assert.Equal(t, initialized.Checkpoint.Binding(), afterBusy.Checkpoint.Binding())

	const caller = "apply-flow-caller-111111111111"
	f.HaveConvWithTitle(caller, "apply-flow-caller")
	agentID, _, err := db.EnsureAgentForConv(caller, "test")
	require.NoError(t, err)

	// Both identified denials happen before body decoding and run lookup.
	deniedExisting := agentReq(t, f, caller, http.MethodPost, path, map[string]any{"secret": "must-not-be-read"})
	deniedMissing := agentReq(t, f, caller, http.MethodPost, missingPath, map[string]any{"different": "body"})
	require.Equal(t, http.StatusForbidden, deniedExisting.Code, deniedExisting.Body.String())
	assert.Equal(t, deniedExisting.Body.String(), deniedMissing.Body.String())
	requireProcessNoStoreHeaders(t, deniedExisting)
	requireProcessNoStoreHeaders(t, deniedMissing)
	// A peer with no resolved identity remains the requirePermission 401 case.
	unidentified := testharness.Serve(f.Mux, testharness.JSONRequest(t, http.MethodPost, path, map[string]any{"secret": "unread"}))
	require.Equal(t, http.StatusUnauthorized, unidentified.Code, unidentified.Body.String())
	assert.Contains(t, unidentified.Body.String(), `"code":"auth"`)

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	request := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, path, applyBody), caller)
	// These are deliberately invalid generic headers. The sensitive route ignores
	// them before validation and relies exclusively on checkpoint-bound replay.
	request.Header.Set(agent.IdempotencyKeyHeader, "not-a-uuid")
	request.Header.Set(agent.RequestDigestHeader, "not-a-digest")
	request.Header.Set("X-Tclaude-Ask-Human", "5s")
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- testharness.Serve(f.Mux, request) }()
	dashboard := agentd.BuildDashboardHandlerForTest()
	pendingID := ""
	require.Eventually(t, func() bool {
		snapshot := fetchAccessReqSnapshot(t, dashboard)
		for _, pending := range snapshot.AccessRequests {
			if pending.Status != db.AccessRequestStatusPending || pending.Perm != agentd.PermProcessRunsUnlock {
				continue
			}
			pendingID = pending.ID
			assert.Equal(t, "/v1/process/runs/{id}/unlock/apply", pending.Path)
			assert.Equal(t, `{"unlockApply":"[redacted]"}`, pending.Body)
			assert.NotContains(t, pending.Body, "restricted-reason-sentinel")
			assert.NotContains(t, pending.Body, "candidate-private-source")
			assert.False(t, pending.AutoGrantable, "unlock apply must never offer persistent Always access")
			return true
		}
		return false
	}, 10*time.Second, 10*time.Millisecond)
	// Even a hand-crafted dashboard decision cannot persist this permission.
	always := testharness.Serve(dashboard, testharness.JSONRequest(t, http.MethodPost,
		"/api/access-requests/"+pendingID+"/decision", map[string]any{"decision": "always"}))
	require.Equal(t, http.StatusForbidden, always.Code, always.Body.String())
	approve := testharness.Serve(dashboard, testharness.JSONRequest(t, http.MethodPost,
		"/api/access-requests/"+pendingID+"/decision", map[string]any{"decision": "approve"}))
	require.Equal(t, http.StatusOK, approve.Code, approve.Body.String())
	applied := <-result
	require.Equal(t, http.StatusOK, applied.Code, applied.Body.String())
	var appliedBody struct {
		Status      string `json:"status"`
		Disposition string `json:"disposition"`
		ApplyToken  string `json:"applyToken"`
		EpochID     string `json:"epochId"`
		ReasonCode  string `json:"reasonCode"`
		Actor       string `json:"actor"`
		AppliedAt   string `json:"appliedAt"`
	}
	testharness.DecodeJSON(t, applied, &appliedBody)
	assert.Equal(t, "applied", appliedBody.Status)
	assert.Equal(t, string(epochv8.DispositionApplied), appliedBody.Disposition)
	// Deliberate S6 disposition: the S5 apply response keeps echoing the merged
	// applyToken. Its exposure is protected by the no-store/nosniff contract
	// and memory-only client handling, not by removal from the response.
	assert.Equal(t, previewBody.ApplyToken, appliedBody.ApplyToken)
	requireProcessNoStoreHeaders(t, applied)
	assert.Equal(t, epochv8.ApplyReasonUnlock, appliedBody.ReasonCode)
	assert.Equal(t, "agent:"+agentID, appliedBody.Actor)
	assert.NotEmpty(t, appliedBody.AppliedAt)

	// The adapted run's ordinary envelope reflects the second epoch without
	// exposing restricted material.
	adaptedView := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/process/runs/apply-flow-run/view", nil)))
	require.Equal(t, http.StatusOK, adaptedView.Code, adaptedView.Body.String())
	requireProcessNoStoreHeaders(t, adaptedView)
	assert.Contains(t, adaptedView.Body.String(), `"adapted":true`)
	assert.Contains(t, adaptedView.Body.String(), `"totalEpochs":2`)
	assert.NotContains(t, adaptedView.Body.String(), "candidate-private-source")
	assert.NotContains(t, adaptedView.Body.String(), "restricted-reason-sentinel")
	audits, err := db.ListAuditLog(db.AuditLogFilter{Verb: "process.unlock.apply", Outcome: "success"})
	require.NoError(t, err)
	require.Len(t, audits, 1)
	assert.Contains(t, audits[0].Detail, "reason_code=unlock_apply")
	assert.NotContains(t, audits[0].Detail, "restricted-reason-sentinel")
	assert.Equal(t, "/v1/process/runs/{id}/unlock/apply", audits[0].Path)

	// The one-shot approval is gone. Both generic and domain replay remain behind
	// a fresh permission check, so a valid generic key cannot replay the response.
	retry := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, path, applyBody), caller)
	retry.Header.Set(agent.IdempotencyKeyHeader, "11111111-1111-4111-8111-111111111111")
	retry.Header.Set(agent.RequestDigestHeader, strings.Repeat("a", 64))
	deniedReplay := testharness.Serve(f.Mux, retry)
	require.Equal(t, http.StatusForbidden, deniedReplay.Code, deniedReplay.Body.String())
	assert.Empty(t, deniedReplay.Header().Get("X-Tclaude-Idempotent-Replay"))
	_, idempotencyErr := db.GetAgentdRequest("11111111-1111-4111-8111-111111111111")
	require.Error(t, idempotencyErr, "unlock apply must not create generic idempotency state")

	// Runtime genesis may attach after the epoch commit but before a lost-ack
	// retry. Historical replay must verify, not re-run the now-inapplicable
	// unattached constructor.
	genesisLease, err := fs.AcquireEngineLease(t.Context(), "apply-flow-run", "test-genesis", time.Minute)
	require.NoError(t, err)
	_, err = fs.EnsureEpochV8Runtime(t.Context(), genesisLease)
	require.NoError(t, err)
	require.NoError(t, fs.ReleaseEngineLease(t.Context(), genesisLease))
	beforeReplay, err := fs.LoadEpochV8RunView(t.Context(), "apply-flow-run")
	require.NoError(t, err)
	require.NotNil(t, beforeReplay.Runtime)

	// A separately authorized caller gets domain replay and the original actor,
	// time, reason code, epoch, and disposition rather than newly minted values.
	replayed := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, path, applyBody)))
	require.Equal(t, http.StatusOK, replayed.Code, replayed.Body.String())
	var replayedBody struct {
		Status         string `json:"status"`
		Disposition    string `json:"disposition"`
		EpochID        string `json:"epochId"`
		ReasonCode     string `json:"reasonCode"`
		Actor          string `json:"actor"`
		AppliedAt      string `json:"appliedAt"`
		CurrentBinding struct {
			Revision uint64 `json:"revision"`
			Digest   string `json:"digest"`
		} `json:"currentBinding"`
	}
	testharness.DecodeJSON(t, replayed, &replayedBody)
	assert.Equal(t, "already_applied", replayedBody.Status)
	assert.Equal(t, string(epochv8.DispositionReplayed), replayedBody.Disposition)
	assert.Equal(t, appliedBody.EpochID, replayedBody.EpochID)
	assert.Equal(t, appliedBody.ReasonCode, replayedBody.ReasonCode)
	assert.Equal(t, appliedBody.Actor, replayedBody.Actor)
	assert.Equal(t, appliedBody.AppliedAt, replayedBody.AppliedAt)
	assert.Equal(t, beforeReplay.Checkpoint.Binding().Revision, replayedBody.CurrentBinding.Revision)
	assert.Equal(t, beforeReplay.Checkpoint.Binding().Digest, replayedBody.CurrentBinding.Digest)
	afterReplay, err := fs.LoadEpochV8RunView(t.Context(), "apply-flow-run")
	require.NoError(t, err)
	assert.Equal(t, beforeReplay.CheckpointJSON, afterReplay.CheckpointJSON, "read-only replay mutated checkpoint")
	assert.Equal(t, beforeReplay.RuntimeJSON, afterReplay.RuntimeJSON, "read-only replay mutated runtime")

	// Exact submitted handoff DTO identity is part of domain replay. A changed
	// action/target cannot borrow the old commit after the outer binding moved.
	tampered := cloneAnyMap(applyBody)
	tampered["handoffs"] = []map[string]any{{
		"token": blockedBody.Blockers[0].Token, "action": epochv8.HandoffTransfer,
		"target": map[string]any{"localId": "other", "reservationId": "other", "nodeId": "work"},
	}}
	stale := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, path, tampered)))
	require.Equal(t, http.StatusConflict, stale.Code, stale.Body.String())
	for _, mutation := range []struct {
		name  string
		field string
		value any
	}{
		{name: "source", field: "candidateSource", value: string(source("different-private-source"))},
		{name: "reason_presence", field: "reason", value: nil},
		{name: "canonical_token", field: "applyToken", value: strings.Repeat("0", 64)},
	} {
		t.Run("replay_mismatch_"+mutation.name, func(t *testing.T) {
			changed := cloneAnyMap(applyBody)
			changed[mutation.field] = mutation.value
			response := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, path, changed)))
			require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
		})
	}

	// Provenance fields are server-owned and therefore rejected by strict JSON.
	require.NoError(t, db.GrantAgentPermission(caller, agentd.PermProcessRunsUnlock, "test"))
	withClientProvenance := cloneAnyMap(applyBody)
	withClientProvenance["reasonCode"] = "unlock_apply"
	strict := agentReq(t, f, caller, http.MethodPost, path, withClientProvenance)
	require.Equal(t, http.StatusBadRequest, strict.Code, strict.Body.String())
	malformed := cloneAnyMap(applyBody)
	malformed["applyToken"] = "short"
	invalidToken := agentReq(t, f, caller, http.MethodPost, path, malformed)
	require.Equal(t, http.StatusUnprocessableEntity, invalidToken.Code, invalidToken.Body.String())
	overBudget := cloneAnyMap(applyBody)
	overBudget["candidateSource"] = strings.Repeat("x", model.MaxProcessTemplateSourceBytes+1)
	oversized := agentReq(t, f, caller, http.MethodPost, path, overBudget)
	require.Equal(t, http.StatusRequestEntityTooLarge, oversized.Code, oversized.Body.String())

	artifacts, err := fs.ReadEpochV8AppliedArtifacts(t.Context(), "apply-flow-run", epochv8.EpochID(appliedBody.EpochID))
	require.NoError(t, err)
	loaded, err := fs.LoadEpochV8RunView(t.Context(), "apply-flow-run")
	require.NoError(t, err)
	assert.Equal(t, candidateSource, loaded.EpochSources[epochv8.EpochID(appliedBody.EpochID)])
	assert.True(t, artifacts.HasReason)
	assert.Equal(t, []byte("restricted-reason-sentinel"), artifacts.Reason)
}

func TestEpochV8ApplyAskHumanDenyAndTimeoutDoNotReadOrRetainDraft(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout string
		stub    bool
	}{
		{name: "deny", timeout: "5s", stub: true},
		{name: "timeout", timeout: "1ms"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := processEngineFlow(t)
			t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
			if tc.stub {
				t.Cleanup(agentd.StubApprovalForTest(false))
			}
			const caller = "apply-denied-caller-111111111111"
			f.HaveConvWithTitle(caller, "apply-denied-caller")
			_, _, err := db.EnsureAgentForConv(caller, "test")
			require.NoError(t, err)

			reader := &countingErrorBody{}
			req := httptest.NewRequest(http.MethodPost, "/v1/process/runs/never-looked-up/unlock/apply", reader)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Tclaude-Ask-Human", tc.timeout)
			req = agentd.AsAgentPeer(req, caller)
			response := testharness.Serve(f.Mux, req)
			require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
			assert.Zero(t, reader.reads.Load(), "permission denial must not read sensitive draft bytes")
			assert.NotContains(t, response.Body.String(), "sensitive-draft")
			if !tc.stub {
				rows, listErr := db.ListRecentHandledAccessRequests(10)
				require.NoError(t, listErr)
				require.NotEmpty(t, rows)
				assert.Equal(t, `{"unlockApply":"[redacted]"}`, rows[0].BodyPreview)
				assert.NotContains(t, rows[0].BodyPreview, "sensitive-draft")
				assert.Equal(t, "timed out", rows[0].Status)
			}
		})
	}
}

func TestEpochV8ApplyRejectsTransferBeforeRuntimeGenesis(t *testing.T) {
	f, root := processEngineFlow(t)
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	makeSource := func(prompt string) []byte {
		tmpl := &model.Template{APIVersion: model.APIVersion, Kind: model.Kind, ID: "pregen-transfer", Start: "work", Nodes: map[string]model.Node{
			"work": {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: prompt}, Next: model.Next{"pass": "done"}},
			"done": {Type: model.NodeTypeEnd, Result: "completed"},
		}}
		encoded, encodeErr := model.CanonicalYAML(tmpl)
		require.NoError(t, encodeErr)
		return encoded
	}
	initialSource, candidateSource := makeSource("initial"), makeSource("candidate")
	parsed, err := model.ParseExactSource(initialSource)
	require.NoError(t, err)
	record, err := fs.PutTemplate(t.Context(), parsed.Template)
	require.NoError(t, err)
	initialized, err := fs.InitializeEpochV8Run(t.Context(), store.RunRecord{ID: "pregen-transfer-run", TemplateRef: record.Ref}, initialSource)
	require.NoError(t, err)
	owner := initialized.Checkpoint.View().ProtectedAuthorities[0]
	token, err := epochv8.HandoffToken(initialized.Checkpoint, owner.Identity)
	require.NoError(t, err)
	classification, err := epochv8.ClassifyTemplateSource(candidateSource)
	require.NoError(t, err)
	plan, err := epochv8.PreviewApply(initialized.Checkpoint, epochv8.ApplyDraft{
		BaseBinding: initialized.Checkpoint.Binding(), Candidate: classification.Candidate(),
		Handoffs: []epochv8.HandoffDirective{{
			Source: owner.Identity, Action: epochv8.HandoffTransfer,
			TargetLocalID: "next-frontier", TargetReservationID: "next-reservation", TargetNodeID: "work",
		}},
	})
	require.NoError(t, err)
	require.NotNil(t, plan.Plan)

	base := map[string]any{"revision": initialized.Checkpoint.Binding().Revision, "digest": initialized.Checkpoint.Binding().Digest}
	handoffs := []map[string]any{{
		"token": token, "action": epochv8.HandoffTransfer,
		"target": map[string]any{"localId": "next-frontier", "reservationId": "next-reservation", "nodeId": "work"},
	}}
	previewBody := map[string]any{"baseBinding": base, "candidateSource": string(candidateSource), "handoffs": handoffs}
	preview := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/process/runs/pregen-transfer-run/unlock/preview", previewBody)))
	require.Equal(t, http.StatusUnprocessableEntity, preview.Code, preview.Body.String())
	assert.NotContains(t, preview.Body.String(), "rescues_now")

	applyBody := map[string]any{
		"baseBinding": base, "applyToken": plan.Plan.ProposalDigest(),
		"candidateSource": string(candidateSource), "handoffs": handoffs,
	}
	applied := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/process/runs/pregen-transfer-run/unlock/apply", applyBody)))
	require.Equal(t, http.StatusUnprocessableEntity, applied.Code, applied.Body.String())
	loaded, err := fs.LoadEpochV8RunView(t.Context(), "pregen-transfer-run")
	require.NoError(t, err)
	assert.Equal(t, initialized.Checkpoint.Binding(), loaded.Checkpoint.Binding())
	_, err = fs.ReadEpochV8AppliedArtifacts(t.Context(), "pregen-transfer-run", plan.Plan.CandidateEpoch().ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	lease, err := fs.AcquireEngineLease(t.Context(), "pregen-transfer-run", "test-genesis", time.Minute)
	require.NoError(t, err)
	attached, err := fs.EnsureEpochV8Runtime(t.Context(), lease)
	require.NoError(t, err, "refused transfer must leave runtime genesis attachable")
	assert.Equal(t, epochv8.RuntimeAttachGenesis, attached.Checkpoint.View().History[0].Runtime.Kind)
	require.NoError(t, fs.ReleaseEngineLease(t.Context(), lease))
}

type countingErrorBody struct {
	reads atomic.Int32
}

func (body *countingErrorBody) Read([]byte) (int, error) {
	body.reads.Add(1)
	return 0, assert.AnError
}

func cloneAnyMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
