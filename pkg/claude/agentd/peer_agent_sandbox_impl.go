package agentd

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// An agent's sandbox implementation under peer grants: agent.sandbox-impl held
// in every group of the agent reads it and, with groups.members.spawn.sandbox
// there too (a spawn's sandbox choice takes that), changes it. A restricted
// peer only ever adds confinement: off to resource-only, either of those to an
// implementation that confines access. It never moves an agent onto one with
// less, nor between the two that confine (neither bounds the other), never
// pins a harness-builtin mode (which can be a full-access one), and never
// accepts a move-out's consequences for your operator. A restart of a live
// agent also takes groups.members.stop and groups.members.resume there.
// Unrestricted trust assigns as this node's operator does.

func servePeerAgentSandboxImpl(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	if v.unrestricted() {
		target, err := db.GetAgent(r.PathValue("id"))
		if err != nil || target == nil || target.CurrentConvID == "" {
			writeError(w, http.StatusNotFound, "not_found", "object not found")
			return
		}
		servePeerAgentAsLocal(w, r, v, target)
		return
	}
	target, ok := v.visibleAgent(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "object not found")
		return
	}
	conv := target.CurrentConvID
	peer := v.peer.InstanceID
	if !peerAgentActionAllowed(peer, conv, PermAgentSandboxImplementation) {
		writeError(w, http.StatusForbidden, "permission", fmt.Sprintf("requires %s for every group of the agent", PermAgentSandboxImplementation))
		return
	}
	if r.Method == http.MethodPost {
		if !peerAgentActionAllowed(peer, conv, PermGroupsMembersSpawnSandbox) && !peerAgentActionAllowed(peer, conv, PermGroupsMembersSpawnAll) {
			writeError(w, http.StatusForbidden, "permission", fmt.Sprintf("requires %s for every group of the agent", PermGroupsMembersSpawnSandbox))
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", "read body: "+err.Error())
			return
		}
		var body struct {
			Implementation string `json:"implementation"`
			Restart        bool   `json:"restart"`
		}
		if fail := peerStructureBody(raw, &body, "implementation", "restart"); fail != nil {
			writeError(w, fail.status, fail.code, fail.msg)
			return
		}
		if body.Restart {
			for _, slug := range []string{PermGroupsMembersStop, PermGroupsMembersResume} {
				if !peerAgentActionAllowed(peer, conv, slug) {
					writeError(w, http.StatusForbidden, "permission", fmt.Sprintf("restarting the agent requires %s for every group of the agent", slug))
					return
				}
			}
		}
		current, err := durableRelaunchConfigForConv(conv)
		if err != nil {
			writeError(w, http.StatusConflict, "relaunch_profile", err.Error())
			return
		}
		if msg := peerSandboxImplMoveFailure(current.SandboxImplementation, body.Implementation); msg != "" {
			writeError(w, http.StatusForbidden, "permission", msg)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		r.ContentLength = int64(len(raw))
	}
	servePeerAgentAsLocal(w, r, v, target)
}

// peerSandboxImplMoveFailure says why a restricted peer may not move an agent
// from implementation from to to, or "" when the move only adds confinement.
// A value that does not parse is left to the handler's own 400.
func peerSandboxImplMoveFailure(from, to string) string {
	next, err := sandboxpolicy.NormalizeImplementation(strings.TrimSpace(to))
	if err != nil {
		return ""
	}
	cur, err := sandboxpolicy.NormalizeImplementation(strings.TrimSpace(from))
	if err != nil {
		return "the agent's current sandbox implementation cannot be read; only this node's operator may change it"
	}
	if next == cur {
		return ""
	}
	switch {
	case cur == sandboxpolicy.ImplementationOff && next == sandboxpolicy.ImplementationResourceOnly:
		return ""
	case cur.OmitsOSConfinement() && !next.OmitsOSConfinement():
		return ""
	}
	return fmt.Sprintf("moving the agent from %s to %s does not only add confinement; that takes unrestricted trust", cur, next)
}
