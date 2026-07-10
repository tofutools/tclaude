// Package worklist derives explicit process obligations from durable run
// snapshots. It does not persist state or perform transitions.
package worklist

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

type Kind string

const (
	KindHumanWait       Kind = "human-wait"
	KindDecisionNeeded  Kind = "decision-needed"
	KindReviewNeeded    Kind = "review-needed"
	KindBlocked         Kind = "blocked"
	KindAgentObligation Kind = "agent-obligation"
)

type Nudge struct {
	LastContactAt    time.Time `json:"lastContactAt,omitzero"`
	NextContactAt    time.Time `json:"nextContactAt,omitzero"`
	BudgetUsed       int       `json:"budgetUsed"`
	BudgetMax        int       `json:"budgetMax"`
	EscalationTarget string    `json:"escalationTarget"`
	Paused           bool      `json:"paused"`
}

type Links struct {
	RunID        string   `json:"runId"`
	NodeID       string   `json:"nodeId"`
	EvidenceRefs []string `json:"evidenceRefs,omitempty"`
}

type ActionTarget struct {
	CommandID string `json:"-"`
	Blocked   bool   `json:"-"`
}

type Item struct {
	ID               string           `json:"id"`
	Run              string           `json:"run"`
	Node             string           `json:"node"`
	Attempt          int              `json:"attempt"`
	Kind             Kind             `json:"kind"`
	Assignee         string           `json:"assignee"`
	Status           state.WaitStatus `json:"status"`
	CreatedAt        time.Time        `json:"createdAt,omitzero"`
	DueAt            time.Time        `json:"dueAt,omitzero"`
	Nudge            *Nudge           `json:"nudge,omitempty"`
	Summary          string           `json:"summary"`
	AvailableActions []string         `json:"availableActions,omitempty"`
	Links            Links            `json:"links"`
	Target           ActionTarget     `json:"-"`
}

type Filter struct {
	Assignee string
	Kind     Kind
	Run      string
	Status   state.WaitStatus
}

// Derive projects snapshots into a stable, deterministically ordered worklist.
func Derive(snapshots []store.Snapshot) []Item {
	items := make([]Item, 0)
	for _, snapshot := range snapshots {
		if snapshot.State == nil {
			continue
		}
		items = append(items, obligationItems(snapshot)...)
		items = append(items, blockedItems(snapshot)...)
	}
	slices.SortFunc(items, func(a, b Item) int {
		return strings.Compare(a.ID, b.ID)
	})
	return items
}

// ApplyFilter applies exact-match REST/CLI filters without changing order.
func ApplyFilter(items []Item, filter Filter) []Item {
	filtered := make([]Item, 0, len(items))
	for _, item := range items {
		if filter.Assignee != "" && item.Assignee != filter.Assignee ||
			filter.Kind != "" && item.Kind != filter.Kind ||
			filter.Run != "" && item.Run != filter.Run ||
			filter.Status != "" && item.Status != filter.Status {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func Find(items []Item, id string) (Item, bool) {
	for _, item := range items {
		if item.ID == id {
			return item, true
		}
	}
	return Item{}, false
}

func obligationItems(snapshot store.Snapshot) []Item {
	ids := make([]string, 0, len(snapshot.State.Obligations))
	for id := range snapshot.State.Obligations {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	items := make([]Item, 0, len(ids))
	for _, id := range ids {
		obligation := snapshot.State.Obligations[id]
		node := snapshot.State.Nodes[obligation.NodeID]
		kind := obligationKind(obligation, node)
		if kind == "" {
			continue
		}
		item := Item{
			ID:  stableID(snapshot.Run.ID, obligation.NodeID, id, obligation.Attempt),
			Run: snapshot.Run.ID, Node: obligation.NodeID, Attempt: obligation.Attempt,
			Kind: kind, Assignee: obligation.Assignee, Status: obligation.Status,
			CreatedAt: obligation.CreatedAt, DueAt: obligation.DueAt,
			Summary: obligation.Summary, AvailableActions: append([]string(nil), obligation.AvailableActions...),
			Links:  Links{RunID: snapshot.Run.ID, NodeID: obligation.NodeID, EvidenceRefs: evidenceRefs(obligation.EvidenceRef)},
			Target: ActionTarget{CommandID: obligation.CommandID},
		}
		if contact, ok := snapshot.State.Contacts[obligation.CommandID]; ok {
			item.Nudge = &Nudge{
				LastContactAt: contact.LastContactedAt, NextContactAt: contact.NextContactAt,
				BudgetUsed: contact.Used, BudgetMax: contact.Budget,
				EscalationTarget: contact.EscalationTarget, Paused: contact.Paused,
			}
		}
		items = append(items, item)
	}
	return items
}

func obligationKind(obligation state.ObligationRecord, node state.NodeState) Kind {
	switch obligation.Kind {
	case state.WaitKindAgent:
		return KindAgentObligation
	case state.WaitKindHuman:
		if node.Type == model.NodeTypeDecision {
			return KindDecisionNeeded
		}
		if node.Stage.IsGateStage() {
			return KindReviewNeeded
		}
		return KindHumanWait
	default:
		return ""
	}
}

func blockedItems(snapshot store.Snapshot) []Item {
	nodeIDs := make([]string, 0, len(snapshot.State.Nodes))
	for nodeID := range snapshot.State.Nodes {
		nodeIDs = append(nodeIDs, nodeID)
	}
	slices.Sort(nodeIDs)
	var items []Item
	for _, nodeID := range nodeIDs {
		node := snapshot.State.Nodes[nodeID]
		// Expanded parents mirror their blocked child. The child is the
		// generation-bound action target and therefore the canonical item.
		if node.Parent == "" && len(node.Children) > 0 {
			continue
		}
		if node.Status != state.NodeStatusBlocked && node.BlockResolution == nil {
			continue
		}
		attempt := node.BlockedAttempt
		if attempt <= 0 {
			attempt = node.Attempt
		}
		status := state.WaitStatusPending
		assignee, summary := node.BlockedOwner, node.BlockedReason
		refs := nodeEvidenceRefs(node)
		if node.BlockResolution != nil && node.Status != state.NodeStatusBlocked {
			status = state.WaitStatusSatisfied
			assignee = string(node.BlockResolution.Actor)
			summary = node.BlockResolution.Reason
			refs = appendEvidenceRef(refs, node.BlockResolution.EvidenceRef)
		}
		items = append(items, Item{
			ID:  stableID(snapshot.Run.ID, nodeID, "blocked", attempt),
			Run: snapshot.Run.ID, Node: nodeID, Attempt: attempt,
			Kind: KindBlocked, Assignee: assignee, Status: status, Summary: summary,
			AvailableActions: []string{string(state.BlockDecisionRetry), string(state.BlockDecisionSkip), string(state.BlockDecisionCancel)},
			Links:            Links{RunID: snapshot.Run.ID, NodeID: nodeID, EvidenceRefs: refs},
			Target:           ActionTarget{Blocked: true},
		})
	}
	return items
}

func stableID(runID, nodeID, slot string, attempt int) string {
	sum := sha256.Sum256([]byte(runID + "\x00" + nodeID + "\x00" + slot + "\x00" + strconv.Itoa(attempt)))
	return "wi_" + hex.EncodeToString(sum[:12])
}

func evidenceRefs(ref string) []string {
	if strings.TrimSpace(ref) == "" {
		return nil
	}
	return []string{ref}
}

func nodeEvidenceRefs(node state.NodeState) []string {
	var refs []string
	if node.ActiveAttempt != nil {
		refs = appendEvidenceRef(refs, node.ActiveAttempt.EvidenceRef)
	}
	for _, decision := range node.Decisions {
		refs = appendEvidenceRef(refs, decision.EvidenceRef)
	}
	if node.BlockResolution != nil {
		refs = appendEvidenceRef(refs, node.BlockResolution.EvidenceRef)
	}
	return refs
}

func appendEvidenceRef(refs []string, ref string) []string {
	ref = strings.TrimSpace(ref)
	if ref == "" || slices.Contains(refs, ref) {
		return refs
	}
	return append(refs, ref)
}
