package proto

const (
	KindPeerProposal         = "peer_proposal"
	KindPeerProposalDecision = "peer_proposal_decision"
	KindGroupLinkRevoke      = "group_link_revoke"
	CapGroupVisibility       = "group_visibility"
	CapGroupLinkAccept       = "group_link_accept"
	ProposalShareVisibility  = "share_visibility"
	ProposalGroupLink        = "group_link"
)

// ProposalItem binds authority to exact host/group identities, never display
// names. Host authorizes sharing or ingress; Source is used only for a link.
type ProposalItem struct {
	ID              string `json:"id"`
	Kind            string `json:"kind"`
	Host            string `json:"host"`
	GroupID         string `json:"group_id"`
	GroupName       string `json:"group_name"`
	Description     string `json:"description,omitempty"`
	MemberCount     int    `json:"member_count"`
	IncludeMembers  bool   `json:"include_members,omitempty"`
	Source          string `json:"source,omitempty"`
	SourceGroupID   string `json:"source_group_id,omitempty"`
	SourceGroupName string `json:"source_group_name,omitempty"`
	Mode            string `json:"mode,omitempty"`
	Status          string `json:"status"`
}

type PeerProposalPayload struct {
	ID           string         `json:"id"`
	Origin       string         `json:"origin"`
	Author       string         `json:"author"`
	Revision     int            `json:"revision"`
	PreviousHash string         `json:"previous_hash,omitempty"`
	Deadline     string         `json:"deadline"`
	Note         string         `json:"note,omitempty"`
	Items        []ProposalItem `json:"items"`
}

type PeerProposalDecisionPayload struct {
	ID             string   `json:"id"`
	Revision       int      `json:"revision"`
	RevisionHash   string   `json:"revision_hash"`
	Decision       string   `json:"decision"`
	ItemIDs        []string `json:"item_ids"`
	WithoutMembers []string `json:"without_members,omitempty"`
	AppliedIDs     []string `json:"applied_ids"`
}

type GroupLinkRevokePayload struct {
	ProposalID string `json:"proposal_id"`
	ItemID     string `json:"item_id"`
}

// GroupLinkMailAuthority selects one exact edge. Sender membership is checked
// by the signed sending daemon; the receiver checks its own live ingress half.
type GroupLinkMailAuthority struct {
	ProposalID    string `json:"proposal_id"`
	ItemID        string `json:"item_id"`
	SourceGroupID string `json:"source_group_id"`
	GroupID       string `json:"group_id"`
}
