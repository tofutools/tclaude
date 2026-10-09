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
}

type PeerViewAnswerPayload struct {
	Stream string `json:"stream"`
	OK     bool   `json:"ok"`
	Key    []byte `json:"key,omitempty"`
	Reason string `json:"reason,omitempty"`
}
