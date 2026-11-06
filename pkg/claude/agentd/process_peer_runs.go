package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// Process runs over the peer view (S6, tcl-e6v1t9). A restricted peer reads
// this node's runs under the instance grant process.runs.read, and starts a
// run under process.runs.start in a group it sees, or under
// groups.members.process for one agent held in every group the agent is in.
// Unrestricted trust runs these routes as this node's operator does.
//
// A run a peer starts is owned by the peer ("peer:<instance>", or
// "peer:<instance>/agent:<id>" for one of its agents), never by this node's
// operator: the owner is an authority source. Such a run tasks only the
// members inside its own scope, spawns no performer, and runs no program,
// and every node prompt it delivers names the peer and marks its text as
// remote and untrusted.

// processOwnerPeerPrefix starts a peer run owner.
const processOwnerPeerPrefix = "peer:"

// processPeerOwner is the run owner for a run the peer's principal starts.
func processPeerOwner(instance string, agent *proto.PeerViewAgent) string {
	if agent != nil {
		return processOwnerPeerPrefix + instance + "/agent:" + agent.ID
	}
	return processOwnerPeerPrefix + instance
}

func processOwnerIsPeer(owner string) bool {
	return strings.HasPrefix(owner, processOwnerPeerPrefix)
}

// processPeerOwnerLabel names a peer run owner in text this node writes to
// one of its agents.
func processPeerOwnerLabel(owner string) string {
	rest, ok := strings.CutPrefix(owner, processOwnerPeerPrefix)
	if !ok {
		return "a linked node"
	}
	instance, agentID, isAgent := strings.Cut(rest, "/agent:")
	if !proto.ValidInstanceID(instance) {
		return "a linked node"
	}
	if isAgent {
		return "agent " + proto.SafeName(agentID, false) + " of " + peerRequesterLabel(instance)
	}
	return "the operator of " + peerRequesterLabel(instance)
}

// servePeerAsLocalOperator runs the request through this node's own
// dashboard handlers, as this node's operator: unrestricted trust.
func servePeerAsLocalOperator(w http.ResponseWriter, r *http.Request) {
	peerDashboardMux().ServeHTTP(w, r.Clone(context.WithValue(r.Context(), remoteAuthedCtxKey{}, true)))
}

// servePeerProcessRuns serves GET (the run list) and POST (a run start) of
// /v1/process/runs.
func servePeerProcessRuns(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	if v.unrestricted() {
		servePeerAsLocalOperator(w, r)
		return
	}
	if r.Method == http.MethodPost {
		servePeerProcessRunStart(w, r, v)
		return
	}
	if !v.allows(rule, 0) {
		writeError(w, http.StatusForbidden, "permission", "requires "+rule.requires)
		return
	}
	servePeerAsLocalOperator(w, r)
}

// servePeerProcessRunRead serves one run's view or its events, redacted.
func servePeerProcessRunRead(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	if v.unrestricted() {
		servePeerAsLocalOperator(w, r)
		return
	}
	if !v.allows(rule, 0) {
		writeError(w, http.StatusForbidden, "permission", "requires "+rule.requires)
		return
	}
	writeRedactedPeerRun(w, r, strings.HasSuffix(r.URL.Path, "/events"))
}

// writeRedactedPeerRun runs the local read and writes its answer with what
// only this node's operator sees removed.
func writeRedactedPeerRun(w http.ResponseWriter, r *http.Request, events bool) {
	rec := httptest.NewRecorder()
	servePeerAsLocalOperator(rec, r)
	redact := redactPeerRunView
	if events {
		redact = redactPeerRunEvents
	}
	writeRedacted(w, rec, redact)
}

// writeRedacted writes a recorded answer, a success redacted. A success that
// does not decode is not passed on unredacted.
func writeRedacted(w http.ResponseWriter, rec *httptest.ResponseRecorder, redact func(map[string]any)) {
	body := rec.Body.Bytes()
	if rec.Code >= 200 && rec.Code < 300 {
		var doc map[string]any
		if err := json.Unmarshal(body, &doc); err != nil {
			writeError(w, http.StatusInternalServerError, "io", "unreadable run view")
			return
		}
		redact(doc)
		body, _ = json.Marshal(doc)
	}
	for k, vals := range rec.Header() {
		if k != "Content-Length" {
			w.Header()[k] = vals
		}
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(body)
}

// redactPeerRunView removes from a run view what describes host program
// execution — argv, working directories and sandboxes, program
// authorizations — and the bound agent prompts of its outstanding commands,
// which carry the template's own text (templates are not peer-readable).
func redactPeerRunView(doc map[string]any) {
	delete(doc, "programBounds")
	delete(doc, "programAuthorizations")
	if commands, ok := doc["commands"].([]any); ok {
		for _, c := range commands {
			if m, ok := c.(map[string]any); ok {
				delete(m, "program")
			}
		}
	}
	if checkpoint, ok := doc["checkpoint"].(map[string]any); ok {
		if commands, ok := checkpoint["commands"].([]any); ok {
			for _, c := range commands {
				if m, ok := c.(map[string]any); ok {
					delete(m, "program")
					delete(m, "agent")
				}
			}
		}
	}
}

// redactPeerRunEvents drops every event's payload, which can hold program
// commands, output and bounds; the sequence, time, node, kind and actor stay.
func redactPeerRunEvents(doc map[string]any) {
	if events, ok := doc["events"].([]any); ok {
		for _, e := range events {
			if m, ok := e.(map[string]any); ok {
				delete(m, "payload")
			}
		}
	}
}

// servePeerProcessRunStart starts a group- or agent-scoped run that the
// peer owns.
func servePeerProcessRunStart(w http.ResponseWriter, r *http.Request, v *peerView) {
	var request processRunCreateRequest
	if err := decodeProcessRuntimeRequest(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "process_run_request", err.Error())
		return
	}
	switch {
	case len(request.AuthorizeProgramProfiles) > 0:
		writeError(w, http.StatusForbidden, "process_run_programs", "a linked node cannot authorize program profiles: a run it starts runs no program")
		return
	case strings.TrimSpace(request.TemplateRef) != "":
		writeError(w, http.StatusForbidden, "process_run_version", "only the operator can start an older version of a template")
		return
	case strings.TrimSpace(request.ID) != "":
		writeError(w, http.StatusBadRequest, "process_run_request", "a linked node cannot choose a run id")
		return
	}
	groupRef, agentRef := strings.TrimSpace(request.Group), strings.TrimSpace(request.Agent)
	peer := v.peer.InstanceID
	var scope db.ProcessRunScope
	switch {
	case groupRef != "" && agentRef != "":
		writeError(w, http.StatusBadRequest, "process_run_scope", "a run is scoped to a group or an agent, not both")
		return
	case groupRef != "":
		g, err := db.GetAgentGroupByName(groupRef)
		if err != nil || g == nil || g.IsArchived() || !fedPeerGroupVisible(peer, g.ID) {
			writeError(w, http.StatusNotFound, "not_found", "object not found")
			return
		}
		v.groupName = g.Name
		if !fedPeerAllows(peer, g.ID, PermProcessRunsStart) {
			writeError(w, http.StatusForbidden, "permission", fmt.Sprintf("requires %s for group:%s", PermProcessRunsStart, g.Name))
			return
		}
		scope = db.ProcessRunScope{Kind: db.ProcessRunScopeGroup, GroupID: g.ID}
	case agentRef != "":
		target, ok := v.visibleAgent(agentRef)
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", "object not found")
			return
		}
		if !peerAgentActionAllowed(peer, target.CurrentConvID, PermGroupsMembersProcess) {
			writeError(w, http.StatusForbidden, "permission", fmt.Sprintf("requires %s for every affected group", PermGroupsMembersProcess))
			return
		}
		v.targetConv = target.CurrentConvID
		request.Agent = target.AgentID
		scope = db.ProcessRunScope{Kind: db.ProcessRunScopeAgent, AgentID: target.AgentID}
	default:
		writeError(w, http.StatusForbidden, "process_run_scope", "a linked node starts a run scoped to a group or an agent")
		return
	}
	v.auditDetail = "template: " + stripControl(strings.TrimSpace(request.TemplateID))
	rec := httptest.NewRecorder()
	startProcessRunAs(rec, r, request, processPeerOwner(peer, v.agent), scope)
	writeRedacted(w, rec, redactPeerRunView)
}
