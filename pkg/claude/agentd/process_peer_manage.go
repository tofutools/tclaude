package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/executor"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/claude/process/strictjson"
)

// Process run manage verbs over the peer view (S6b, tcl-ltltmc). A restricted
// peer, and every agent principal, manages runs under process.runs.manage
// held unscoped, within one rule: a peer never causes host program execution
// here. So a verb other than pause applies only to a run with no program
// authorizations; pause only holds work, so it applies to any run. Also:
//   - decide records a human gate's verdict (every awaited decision is one),
//     so an agent principal never decides; the peer's operator does;
//   - assign hands an attempt only to an agent the peer could start an
//     agent-bound run for, inside the run's own scope, never in an unscoped run;
//   - free text the peer writes (a verdict's evidence, a resolution's note)
//     can reach a later performer's prompt, so it is stored under the
//     untrusted-content banner naming the peer;
//   - the actor recorded is the peer (processPeerOwner), never this node's
//     operator.
//
// Rescope stays this node's human's (it is human-only locally too), and
// reissue and record-outcome reconcile host program commands, which a
// program-free run has none of: no peer route reaches them.
// Unrestricted trust runs every verb as this node's operator does.

// processActorKey carries the actor a peer's manage verb records.
type processActorKey struct{}

// processRequestActor is the actor a run mutation records: the peer when a
// peer's manage verb runs it, else the authenticated caller.
func processRequestActor(r *http.Request, caller string) (store.ActorRef, error) {
	if actor, _ := r.Context().Value(processActorKey{}).(string); actor != "" {
		return store.ActorRef(actor), nil
	}
	return processTemplateAuthor(caller)
}

// processPeerManageTextFields are the body fields each verb takes free text
// in, which the peer's banner is put in front of.
var processPeerManageTextFields = map[string]string{"decide": "evidence", "resolve-blocked": "note"}

// processPeerManageTextLimits are those fields' limits in the executor,
// which the banner counts toward.
var processPeerManageTextLimits = map[string]int{"evidence": executor.MaxDecisionEvidenceBytes, "note": executor.MaxResolutionNoteBytes}

func servePeerProcessRunManage(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	if v.unrestricted() {
		servePeerAsLocalOperator(w, r)
		return
	}
	if !v.allows(rule, 0) {
		writeError(w, http.StatusForbidden, "permission", "requires "+rule.requires)
		return
	}
	runID := strings.TrimSpace(r.PathValue("id"))
	verb := path.Base(r.URL.Path)
	if r.PathValue("node") != "" {
		verb = "assign"
	}
	v.auditDetail = "run: " + stripControl(runID)
	run, err := db.GetProcessRun(runID)
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	if verb != "pause" {
		var authorizations []string
		if err := run.DecodeProgramAuthorizations(&authorizations); err != nil || len(authorizations) > 0 {
			writeError(w, http.StatusConflict, "process_run_programs", "this run executes host programs; only this node's operator manages it")
			return
		}
		// A run owned by this node whose agent steps may fall back to a
		// spawn would spawn under this node's authority at the peer's word.
		// A peer-owned run never spawns (planProcessPerformerSpawn).
		if !processOwnerIsPeer(run.Scope.Owner) && processRunCanSpawn(run) {
			writeError(w, http.StatusConflict, "process_run_spawns", "this run may spawn performers on this node's authority; only this node's operator manages it")
			return
		}
	}
	// Every decision this route records is a human-performer gate: the
	// engine opens an awaited-decision obligation only for a decision node
	// with no agent performer (awaitsHuman), and an agent performer's verdict
	// is its own report, not this route. So refusing an agent principal the
	// route is exactly refusing it human gates (steward's decision). Should a
	// non-human gate kind ever be decided here, narrow this to human ones.
	if verb == "decide" && v.agent != nil {
		writeError(w, http.StatusForbidden, "process_run_human_gate", "a decision is a human gate: only the linked node's operator decides it, not its agents")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxProcessRunRequestBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "process_run_request", err.Error())
		return
	}
	if field, ok := processPeerManageTextFields[verb]; ok {
		if body, err = peerManageBanner(body, field, peerFollowUpAuthor(v, v.peer.InstanceID)); err != nil {
			writeError(w, http.StatusBadRequest, "process_run_request", err.Error())
			return
		}
	}
	if verb == "assign" {
		if run.Scope.Kind != db.ProcessRunScopeGroup && run.Scope.Kind != db.ProcessRunScopeAgent {
			writeError(w, http.StatusConflict, "process_run_scope", "a linked node assigns only within a run scoped to a group or an agent")
			return
		}
		if body, err = peerManageAssignee(v, body); err != nil {
			switch {
			case errors.Is(err, errPeerNotFound):
				writeError(w, http.StatusNotFound, "not_found", err.Error())
			case errors.Is(err, errPeerAssignGrant):
				writeError(w, http.StatusForbidden, "permission", err.Error())
			default:
				writeError(w, http.StatusBadRequest, "process_assign_request", err.Error())
			}
			return
		}
	}
	ctx := context.WithValue(context.WithValue(r.Context(), remoteAuthedCtxKey{}, true), processActorKey{}, processPeerOwner(v.peer.InstanceID, v.agent))
	req := r.Clone(ctx)
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()
	peerDashboardMux().ServeHTTP(rec, req)
	writeRedacted(w, rec, func(doc map[string]any) {
		if view, ok := doc["run"].(map[string]any); ok {
			redactPeerRunView(view)
		}
	})
}

// peerManageBanner puts the peer's untrusted-content banner in front of a
// non-empty free-text field of a JSON object body.
func peerManageBanner(body []byte, field, author string) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("expected a JSON object")
	}
	raw, ok, err := takeFoldField(doc, field)
	if err != nil {
		return nil, err
	}
	if !ok {
		return body, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return nil, fmt.Errorf("%s must be a string", field)
	}
	if strings.TrimSpace(text) != "" {
		banner := peerFollowUpBanner(author)
		if limit := processPeerManageTextLimits[field]; limit > 0 && len(banner)+len(text) > limit {
			return nil, fmt.Errorf("%s takes at most %d bytes from a linked node: the untrusted-content banner takes the rest", field, limit-len(banner))
		}
		text = banner + text
	}
	doc[field], _ = json.Marshal(text)
	return json.Marshal(doc)
}

// takeFoldField removes every key of doc that names field, which the
// handler's decode matches case-insensitively, and returns its value. More
// than one is refused: which one the handler would take is not this code's
// to guess.
func takeFoldField(doc map[string]json.RawMessage, field string) (json.RawMessage, bool, error) {
	var raw json.RawMessage
	found := 0
	for key, value := range doc {
		if strings.EqualFold(key, field) {
			raw = value
			found++
			delete(doc, key)
		}
	}
	if found > 1 {
		return nil, false, fmt.Errorf("%s is given more than once", field)
	}
	return raw, found == 1, nil
}

var (
	errPeerNotFound    = errors.New("object not found")
	errPeerAssignGrant = fmt.Errorf("requires %s for every group the agent is in", PermGroupsMembersProcess)
)

// peerManageAssignee resolves an assign's agent to one the peer sees and
// could start an agent-bound run for (groups.members.process in every group
// it is in), and names it by its exact agent id.
func peerManageAssignee(v *peerView, body []byte) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("expected a JSON object")
	}
	var ref string
	raw, ok, err := takeFoldField(doc, "agent")
	if err != nil {
		return nil, err
	}
	if !ok || json.Unmarshal(raw, &ref) != nil {
		return nil, fmt.Errorf("agent must be an agent id")
	}
	target, ok := v.visibleAgent(strings.TrimSpace(ref))
	if !ok {
		return nil, errPeerNotFound
	}
	if !peerAgentActionAllowed(v.peer.InstanceID, target.CurrentConvID, PermGroupsMembersProcess) {
		return nil, errPeerAssignGrant
	}
	v.targetConv = target.CurrentConvID
	doc["agent"], _ = json.Marshal(target.AgentID)
	return json.Marshal(doc)
}

// processRunCanSpawn reports whether any agent step of the run's template
// (subprocess bodies and compound stages included) may fall back to spawning
// a performer: its own fallback, the template's default one, or the legacy
// spawn: allowed. A snapshot that does not decode counts as one that can.
func processRunCanSpawn(run *db.ProcessRun) bool {
	var tmpl model.Template
	if err := strictjson.Decode(run.TemplateSnapshotJSON, &tmpl); err != nil {
		return true
	}
	spawns := false
	templateDefault := tmpl.Fallback != nil && tmpl.Fallback.Action == model.FallbackSpawn
	check := func(p *model.Performer) {
		if p == nil || p.Kind != model.PerformerAgent {
			return
		}
		switch {
		case p.Fallback != nil:
			spawns = spawns || p.Fallback.Action == model.FallbackSpawn
		case p.Spawn == model.SpawnAllowed || templateDefault:
			spawns = true
		}
	}
	model.WalkNodes(&tmpl, func(_ string, node model.Node) {
		check(node.Performer)
		if node.Plan != nil {
			check(&node.Plan.Performer)
		}
		for i := range node.Checks {
			check(&node.Checks[i].Performer)
		}
		if node.Review != nil {
			check(&node.Review.Performer)
		}
	})
	return spawns
}
