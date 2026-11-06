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
	// Groups is the footprint the sending node checked the agent's grants
	// over: the receiver's groups, as its catalog names them. The receiver
	// refuses a request that touches a group outside it, so a stale or
	// partial catalog on the sender cannot widen what the agent reaches.
	Groups []string `json:"groups,omitempty"`
}

// MaxPeerViewAgentGroups bounds an agent principal's footprint.
const MaxPeerViewAgentGroups = 256

// Valid reports an agent principal whose ID is an agent ref and whose name
// and footprint groups are already in SafeName's charset, so they can be
// audited and compared as sent.
func (a *PeerViewAgent) Valid() bool {
	if a == nil || !ValidAgentRef(a.ID) || a.Name == "" || SafeName(a.Name, false) != a.Name || len(a.Groups) > MaxPeerViewAgentGroups {
		return false
	}
	for _, g := range a.Groups {
		if g == "" || SafeName(g, false) != g {
			return false
		}
	}
	return true
}

type PeerViewAnswerPayload struct {
	Stream string `json:"stream"`
	OK     bool   `json:"ok"`
	Key    []byte `json:"key,omitempty"`
	Reason string `json:"reason,omitempty"`
	// AgentPrincipal confirms the receiver serves the stream under the open's
	// agent principal. A sender that sent one aborts without it: an older
	// receiver ignores the principal and would serve the operator's view.
	AgentPrincipal bool `json:"agent_principal,omitempty"`
}
