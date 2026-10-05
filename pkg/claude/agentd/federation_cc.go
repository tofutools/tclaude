package agentd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
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

// authorizeFederatedCC resolves and authorizes every remote cc, or writes
// the refusal and returns false.
func authorizeFederatedCC(w http.ResponseWriter, r *http.Request, fromConv string, addrs []string) ([]fedCC, bool) {
	var out []fedCC
	seen := map[string]bool{}
	for _, a := range addrs {
		t, err := resolveFederatedTarget(fromConv, a)
		if err != nil {
			writeFedErr(w, err)
			return nil, false
		}
		key := t.peer.InstanceID + "/" + t.agentID
		if seen[key] {
			continue
		}
		seen[key] = true
		via, ok := authorizeFederatedTarget(w, r, fromConv, t)
		if !ok {
			return nil, false
		}
		out = append(out, fedCC{target: t, via: via})
	}
	return out, true
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
// them as one multi-recipient result.
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
	ccs, ok := authorizeFederatedCC(w, r, fromID, remoteCC)
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
		conv := ""
		if target, _, err := agent.ResolveSelector(req.To); err == nil && target != nil {
			conv = target.ConvID
		}
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
