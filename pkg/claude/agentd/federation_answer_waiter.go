package agentd

// fedAnswerWaiter is one pending stream open: a route, terminal, model or
// peer-view open sent to a peer, waiting for that peer's answer.
//
// The stream ID keys the waiter, but it does not authenticate the answer. An
// answer is accepted only when it comes from the instance the open was sent to
// AND replies to that exact open envelope. Anything else is dropped, and the
// open keeps waiting for the real answer, so a stray or replayed answer can
// neither complete an open nor abort it.
//
// Each open path used to carry its own copy of this check. They drifted: the
// route copy never bound the answer to its request and aborted the open on an
// answer from the wrong instance. New open paths use this type.
type fedAnswerWaiter[T any] struct {
	peer    string // instance ID the open was sent to
	request string // envelope ID of the open
	answer  chan T
}

func newFedAnswerWaiter[T any](peer, request string) fedAnswerWaiter[T] {
	return fedAnswerWaiter[T]{peer: peer, request: request, answer: make(chan T, 1)}
}

// offer hands an answer to the waiter when it comes from the expected peer in
// reply to the expected envelope, and reports whether it did. It never blocks:
// a waiter holds one answer, and a duplicate of a delivered answer is dropped.
func (w fedAnswerWaiter[T]) offer(from, inReplyTo string, a T) bool {
	if w.answer == nil || from != w.peer || inReplyTo != w.request {
		return false
	}
	select {
	case w.answer <- a:
		return true
	default:
		return false
	}
}
