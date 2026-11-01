package agentd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// An ask-human request holds its body in memory until the human answers, and the
// caller needs no grant to raise one. So raising one takes a slot first, before
// anything is read. Requests past these limits are refused; a request that holds
// a slot keeps it until it is answered or times out.
const (
	maxPendingApprovalsPerCaller = 8
	maxPendingApprovals          = 64
)

// maxApprovalHeldBytes bounds what the requests waiting on a human hold between
// them for a route that must read its whole body to describe it, such as a .paf
// on process import. Every other route's body is capped per request at
// maxApprovalRestoreBody, so the slots above bound those.
var maxApprovalHeldBytes int64 = 256 << 20

var errApprovalBodyTooLarge = errors.New("approval body over the held-bytes limit")

// approvalTicket is one admitted request. A nil slot holds nothing.
type approvalTicket struct {
	caller string
	held   int64
}

// approvalAdmission counts the admitted requests, under approvals.mu.
type approvalAdmission struct {
	byCaller map[string]int
	total    int
	held     int64
}

// admit takes a slot for caller. It says why not when one is refused.
func (a *approvalRegistry) admit(caller string) (*approvalTicket, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.admission.byCaller[caller] >= maxPendingApprovalsPerCaller {
		return nil, fmt.Sprintf("%d of your requests are already waiting for the human; ask again once one is answered", maxPendingApprovalsPerCaller)
	}
	if a.admission.total >= maxPendingApprovals {
		return nil, fmt.Sprintf("%d requests are already waiting for the human; ask again once some are answered", maxPendingApprovals)
	}
	if a.admission.byCaller == nil {
		a.admission.byCaller = map[string]int{}
	}
	a.admission.byCaller[caller]++
	a.admission.total++
	return &approvalTicket{caller: caller}, ""
}

// release gives the slot back, with the bytes it held.
func (a *approvalRegistry) release(s *approvalTicket) {
	if s == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.admission.held -= s.held
	s.held = 0
	if a.admission.byCaller[s.caller]--; a.admission.byCaller[s.caller] <= 0 {
		delete(a.admission.byCaller, s.caller)
	}
	a.admission.total--
}

func (a *approvalRegistry) hold(s *approvalTicket, n int64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.admission.held+n > maxApprovalHeldBytes {
		return false
	}
	a.admission.held += n
	s.held += n
	return true
}

// readBody reads body into memory, charging the slot as it goes, so a body with
// no declared length is stopped at the limit too. A read error comes back with
// what was read, as a plain read would. errApprovalBodyTooLarge means the limit.
func (a *approvalRegistry) readBody(s *approvalTicket, body io.Reader) ([]byte, error) {
	var out bytes.Buffer
	chunk := make([]byte, 64<<10)
	for {
		n, err := body.Read(chunk)
		if n > 0 {
			if !a.hold(s, int64(n)) {
				return nil, errApprovalBodyTooLarge
			}
			out.Write(chunk[:n])
		}
		if errors.Is(err, io.EOF) {
			return out.Bytes(), nil
		}
		if err != nil {
			return out.Bytes(), err
		}
	}
}

func refuseApprovalBody(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "30")
	writeError(w, http.StatusTooManyRequests, "approval_body_too_large",
		"this request is too large to hold while the human decides; ask for the permission first, or have the operator do it")
}

// admitApproval takes a slot for an ask-human request, or refuses it. The caller
// releases the slot once the request is answered.
func admitApproval(w http.ResponseWriter, convID string) (*approvalTicket, bool) {
	caller := peerAgentID(convID)
	if caller == "" {
		caller = convID
	}
	slot, refused := approvals.admit(caller)
	if slot == nil {
		w.Header().Set("Retry-After", "30")
		writeError(w, http.StatusTooManyRequests, "approval_queue_full", refused)
		return nil, false
	}
	return slot, true
}
