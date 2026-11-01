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

// These bound what waiting requests hold for a route that must read its whole
// body to describe it, such as a .paf on process import. The first
// approvalBodyFreeBytes of a body are not charged: the slots bound those, so a
// few large files cannot stop ordinary requests from reaching the human. Other
// routes' bodies are bounded by the slots alone, whatever cap each route has.
var (
	maxApprovalHeldBytes          int64 = 256 << 20
	maxApprovalHeldBytesPerCaller int64 = 128 << 20
	approvalBodyFreeBytes         int64 = maxApprovalRestoreBody
)

var (
	// errApprovalBodyTooLarge can never succeed, however long the caller waits.
	errApprovalBodyTooLarge = errors.New("approval body over the per-caller limit")
	// errApprovalBodyBusy clears as waiting requests are answered.
	errApprovalBodyBusy = errors.New("no room to hold the approval body now")
)

// approvalTicket is one admitted request. A nil ticket holds nothing.
type approvalTicket struct {
	caller string
	held   int64
}

// approvalAdmission counts the admitted requests, under approvals.mu.
type approvalAdmission struct {
	byCaller   map[string]int
	total      int
	held       int64
	heldByCall map[string]int64
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
	if s.held > 0 {
		a.admission.held -= s.held
		if a.admission.heldByCall[s.caller] -= s.held; a.admission.heldByCall[s.caller] <= 0 {
			delete(a.admission.heldByCall, s.caller)
		}
		s.held = 0
	}
	if a.admission.byCaller[s.caller]--; a.admission.byCaller[s.caller] <= 0 {
		delete(a.admission.byCaller, s.caller)
	}
	a.admission.total--
}

func (a *approvalRegistry) hold(s *approvalTicket, n int64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s.held+n > maxApprovalHeldBytesPerCaller {
		return errApprovalBodyTooLarge
	}
	if a.admission.held+n > maxApprovalHeldBytes || a.admission.heldByCall[s.caller]+n > maxApprovalHeldBytesPerCaller {
		return errApprovalBodyBusy
	}
	if a.admission.heldByCall == nil {
		a.admission.heldByCall = map[string]int64{}
	}
	a.admission.held += n
	a.admission.heldByCall[s.caller] += n
	s.held += n
	return nil
}

// readBody reads body into memory, charging the slot as it goes, so a body with
// no declared length is stopped at the limit too. A read error comes back with
// what was read, as a plain read would; errApprovalBodyTooLarge and
// errApprovalBodyBusy come back alone.
func (a *approvalRegistry) readBody(s *approvalTicket, body io.Reader) ([]byte, error) {
	var out bytes.Buffer
	chunk := make([]byte, 64<<10)
	for {
		n, err := body.Read(chunk)
		if n > 0 {
			free := min(max(approvalBodyFreeBytes-int64(out.Len()), 0), int64(n))
			if err := a.hold(s, int64(n)-free); err != nil {
				return nil, err
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

func refuseApprovalBody(w http.ResponseWriter, err error) {
	var overLimit *http.MaxBytesError
	if errors.Is(err, errApprovalBodyTooLarge) || errors.As(err, &overLimit) {
		writeError(w, http.StatusRequestEntityTooLarge, "approval_body_too_large",
			"this request is too large to hold while the human decides; get the permission granted first, or have the operator do it")
		return
	}
	if !errors.Is(err, errApprovalBodyBusy) {
		writeError(w, http.StatusBadRequest, "body_read", "the request body could not be read: "+err.Error())
		return
	}
	w.Header().Set("Retry-After", "30")
	writeError(w, http.StatusTooManyRequests, "approval_queue_full",
		"large requests already waiting for the human leave no room for this one; ask again once some are answered")
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
