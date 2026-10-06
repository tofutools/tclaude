package agentd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// Remote --cc recipients. Each remote cc is its own sealed mail to that
// member, authorized exactly like a direct remote send, and all of them are
// authorized before anything is written: a refused cc aborts the send.
// Recipients on other instances do not see each other's addresses.

// splitFederatedCC separates member@peer cc entries from local ones.
func splitFederatedCC(cc []string) (local, remote []string) {
	for _, c := range cc {
		if isFederatedAddress(strings.TrimSpace(c)) {
			remote = append(remote, strings.TrimSpace(c))
		} else {
			local = append(local, c)
		}
	}
	return local, remote
}

type fedCC struct {
	target *fedTarget
	via    string
}

// resolveFederatedCC resolves remote cc addresses, dropping duplicates and
// any already in seen (peer/agent keys, e.g. the primary).
func resolveFederatedCC(fromConv string, addrs []string, seen map[string]bool) ([]*fedTarget, error) {
	var out []*fedTarget
	for _, a := range addrs {
		t, err := resolveFederatedTarget(fromConv, a)
		if err != nil {
			return nil, err
		}
		key := t.peer.InstanceID + "/" + t.agentID
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, t)
	}
	return out, nil
}

// authorizeFederatedTargets authorizes a send to several remote targets.
// Each must be covered by the sender's own message.direct grants; at
// most one may fall through to the full gate, because a one-shot
// --ask-human approval is scoped to the single target it showed the human
// and must not carry over to other peers or groups.
func authorizeFederatedTargets(w http.ResponseWriter, r *http.Request, fromConv string, targets []*fedTarget) ([]fedCC, bool) {
	out := make([]fedCC, len(targets))
	var uncovered []int
	for i, t := range targets {
		out[i].target = t
		for _, g := range t.remoteGroup {
			if ok, _, err := permissionAllowsAction(r, fromConv, PermMessageDirect, ActionContext{RemoteGroup: g, RemotePeer: t.peer.InstanceID}); err == nil && ok {
				out[i].via = g
				break
			}
		}
		if out[i].via == "" {
			uncovered = append(uncovered, i)
		}
	}
	switch len(uncovered) {
	case 0:
		return out, true
	case 1:
		via, ok := authorizeFederatedTarget(w, r, fromConv, targets[uncovered[0]])
		if !ok {
			return nil, false
		}
		out[uncovered[0]].via = via
		return out, true
	default:
		var labels []string
		for _, i := range uncovered {
			labels = append(labels, targets[i].label())
		}
		writeError(w, http.StatusForbidden, "permission",
			fmt.Sprintf("%q does not cover %s; an approval covers one remote recipient, so get a grant scoped to these peers or send separately",
				PermMessageDirect, strings.Join(labels, ", ")))
		return nil, false
	}
}

// validateFederatedCopies checks everything queueFederatedMail would refuse,
// for every target, before anything is written.
func validateFederatedCopies(targets []*fedTarget, subject, body string, atts []proto.AttachmentPayload) error {
	if strings.TrimSpace(body) == "" {
		return newFedErr(http.StatusBadRequest, "invalid_arg", "body is empty")
	}
	if len(body) > proto.MaxMailBody {
		return newFedErr(http.StatusRequestEntityTooLarge, "too_large", "remote messages are limited to %d bytes", proto.MaxMailBody)
	}
	if len(subject) > fedMaxSubject {
		return newFedErr(http.StatusBadRequest, "invalid_arg", "remote message subjects are limited to %d bytes", fedMaxSubject)
	}
	if len(atts) > 0 {
		if err := validateFedAttachments(atts); err != nil {
			return newFedErr(http.StatusBadRequest, "invalid_arg", "%v", err)
		}
		for _, t := range targets {
			if !t.attachments {
				return newFedErr(http.StatusForbidden, "not_exported", "%s does not accept attachments from this instance", t.label())
			}
		}
	}
	return nil
}

// captureWriter records a handler's response so it can be extended.
type captureWriter struct {
	h    http.Header
	code int
	buf  bytes.Buffer
}

func (c *captureWriter) Header() http.Header         { return c.h }
func (c *captureWriter) Write(p []byte) (int, error) { return c.buf.Write(p) }
func (c *captureWriter) WriteHeader(code int)        { c.code = code }

// handleSendWithRemoteCC sends to a local target (and local ccs) through the
// ordinary path, then queues a remote copy per remote cc, reporting all of
// them as one multi-recipient result. Everything that can refuse the remote
// copies is checked before the local send writes anything.
func handleSendWithRemoteCC(w http.ResponseWriter, r *http.Request, fromID string, req *sendReq, localCC, remoteCC []string) {
	if strings.HasPrefix(strings.TrimSpace(req.To), multicastPrefix) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "cc is not valid with a 'group:' target")
		return
	}
	if strings.TrimSpace(req.Gen) != "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "gen is only valid on a direct (non-group, non-cc) send")
		return
	}
	if len(req.Attachments) > 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "attachments are only supported when every recipient is remote")
		return
	}
	// Cheap failures first, so nobody is asked to approve a send that
	// cannot happen.
	primary, _, err := agent.ResolveSelector(req.To)
	if err != nil || primary == nil || strings.TrimSpace(req.Body) == "" {
		req.Cc = localCC
		dispatchSend(w, fromID, req) // writes the ordinary error
		return
	}
	targets, err := resolveFederatedCC(fromID, remoteCC, map[string]bool{})
	if err == nil {
		err = validateFederatedCopies(targets, req.Subject, req.Body, nil)
	}
	if err != nil {
		writeFedErr(w, err)
		return
	}
	ccs, ok := authorizeFederatedTargets(w, r, fromID, targets)
	if !ok {
		return
	}
	req.Cc = localCC
	cw := &captureWriter{h: http.Header{}, code: http.StatusOK}
	dispatchSend(cw, fromID, req)
	if cw.code != http.StatusOK {
		for k, v := range cw.h {
			w.Header()[k] = v
		}
		w.WriteHeader(cw.code)
		_, _ = w.Write(cw.buf.Bytes())
		return
	}
	var resp sendResp
	if err := json.Unmarshal(cw.buf.Bytes(), &resp); err != nil {
		writeError(w, http.StatusInternalServerError, "io", "could not read the local send result")
		return
	}
	if len(resp.Recipients) == 0 {
		// A direct send answers in the single-recipient shape; restate it
		// as the first recipient of the multi-recipient result.
		conv := primary.ConvID
		resp.Recipients = []recipient{{
			ConvID: conv, AgentID: peerAgentID(conv), Title: agent.TitleFor(conv),
			MessageID: resp.ID, Queued: resp.Queued, Pending: resp.Pending, Held: resp.Held,
			RedirectedFrom: resp.RedirectedFrom,
		}}
		resp.ID, resp.Queued, resp.Pending, resp.Held, resp.RedirectedFrom = 0, false, 0, false, ""
	}
	for _, c := range ccs {
		rc := recipient{AgentID: c.target.agentID + "@" + c.target.peer.InstanceID, Title: c.target.label()}
		if row, err := queueFederatedMail(fromID, c.target, req.Subject, req.Body, "", nil); err != nil {
			rc.Error = "failed to queue remote message: " + err.Error()
		} else {
			rc.EnvelopeID, rc.Queued = row.EnvelopeID, true
		}
		resp.Recipients = append(resp.Recipients, rc)
	}
	writeJSON(w, http.StatusOK, resp)
}
