package proto

// Peer views open a one-request encrypted stream. HTTP credentials and caller
// identities are never part of the control envelope.
const (
	KindPeerViewOpen   = "peer_view_open"
	KindPeerViewAnswer = "peer_view_answer"
)

type PeerViewOpenPayload struct {
	Stream string `json:"stream"`
	Key    []byte `json:"key"`
	// Agent names the sending node's agent the view acts for. Without it the
	// view is the sending node's operator's. A sender sets it only toward a
	// peer whose catalog advertises PeerViewAgents: an older receiver ignores
	// the field and would serve the request as the operator's.
	Agent *PeerViewAgent `json:"agent,omitempty"`
}

// PeerViewAgent is a peer view's agent principal.
type PeerViewAgent struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Valid reports an agent principal whose ID is an agent ref and whose name
// is already in SafeName's charset, so it can be audited as sent.
func (a *PeerViewAgent) Valid() bool {
	return a != nil && ValidAgentRef(a.ID) && a.Name != "" && SafeName(a.Name, false) == a.Name
}

type PeerViewAnswerPayload struct {
	Stream string `json:"stream"`
	OK     bool   `json:"ok"`
	Key    []byte `json:"key,omitempty"`
	Reason string `json:"reason,omitempty"`
}
