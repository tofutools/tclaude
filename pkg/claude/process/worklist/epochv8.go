package worklist

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/state/epochv8"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

// EpochV8Projection is the shared schema-8 projection core: the worklist items
// and the safe report are derived together so their obligation/signal/contact
// state cannot disagree. Any coherence failure fails the whole projection —
// a schema-8 run never yields partial items.
type EpochV8Projection struct {
	Items  []Item
	Report EpochV8Report
}

// EpochV8Report is the bounded safe schema-8 report: open-work entries that
// mirror the worklist projector one-to-one, plus a bounded history timeline.
// Fields are whitelisted to state/kind/time/count and safe provenance; it
// never carries sources, prompts, params, reason text, identities, or tokens.
type EpochV8Report struct {
	Entries []EpochV8ReportEntry `json:"entries"`
	// EntriesTotal/EntriesTruncated bound the ordinary viewer rows
	// independently of the complete worklist projection: the worklist route
	// serves every item, while the envelope report keeps a deterministic cap.
	EntriesTotal     int                    `json:"entriesTotal"`
	EntriesTruncated bool                   `json:"entriesTruncated,omitempty"`
	Timeline         []EpochV8TimelineEvent `json:"timeline"`
	TimelineTotal    int                    `json:"timelineTotal"`
	Truncated        bool                   `json:"truncated,omitempty"`
}

// EpochV8ReportEntry mirrors one worklist item under the same collision-free
// primary key (owner epoch ordinal, kind, node, attempt, presentation id).
type EpochV8ReportEntry struct {
	ID                string           `json:"id"`
	OwnerEpochOrdinal uint64           `json:"ownerEpochOrdinal"`
	Kind              Kind             `json:"kind"`
	NodeID            string           `json:"nodeId"`
	Attempt           int              `json:"attempt"`
	Status            state.WaitStatus `json:"status"`
}

// EpochV8TimelineEvent projects one checkpoint history event. ReasonCode is
// the fixed engine enum (for example unlock_apply), never reason text;
// ActorClass is the actor kind prefix only, never an identity.
type EpochV8TimelineEvent struct {
	Revision     uint64 `json:"revision"`
	Kind         string `json:"kind"`
	EpochOrdinal uint64 `json:"epochOrdinal"`
	AppliedAt    string `json:"appliedAt,omitempty"`
	ReasonCode   string `json:"reasonCode,omitempty"`
	ActorClass   string `json:"actorClass,omitempty"`
}

// maxEpochV8TimelineEvents bounds the projected timeline; newer events win.
const maxEpochV8TimelineEvents = 64

// maxEpochV8ReportEntries bounds the envelope report's open-work rows. Items
// are already in deterministic presentation-id order, so the cap keeps a
// stable prefix; the worklist route remains the complete projection.
const maxEpochV8ReportEntries = 64

// epochV8Record pairs one user-visible runtime record with the authority
// agreement fields the cross-check requires. The localID is never exposed.
// alsoCovers lists paired authorities the record accounts for without
// emitting a second item (a wait item covers its perform command too).
type epochV8Record struct {
	localID       string
	kind          epochv8.AuthorityKind
	state         epochv8.AuthorityState
	reservationID string
	nodeID        string
	item          Item
	alsoCovers    []epochV8Expectation
}

type epochV8Expectation struct {
	localID       string
	kind          epochv8.AuthorityKind
	state         epochv8.AuthorityState
	reservationID string
	nodeID        string
}

// DeriveEpochV8 projects one coherent schema-8 snapshot into worklist items
// and the safe report. Items derive only from typed verified runtime records
// that produce user-visible work (perform-attempt commands and their wait/
// obligation/block side effects, with paired contact data); nothing is emitted
// merely for being an active authority. Every derived item must agree with
// exactly one checkpoint authority on kind, local id, node, reservation,
// state, and epoch, and every active work-bearing authority must be covered;
// any zero/multiple/ambiguous match fails the whole projection.
func DeriveEpochV8(ctx context.Context, snapshot store.EpochV8RunSnapshot) (EpochV8Projection, error) {
	if snapshot.Checkpoint == nil {
		return EpochV8Projection{}, fmt.Errorf("schema-8 worklist checkpoint is absent")
	}
	view := snapshot.Checkpoint.View()
	projection := EpochV8Projection{Items: []Item{}, Report: EpochV8Report{Entries: []EpochV8ReportEntry{}, Timeline: []EpochV8TimelineEvent{}}}
	projection.Report.Timeline, projection.Report.TimelineTotal, projection.Report.Truncated = epochV8Timeline(view)
	if snapshot.Runtime == nil {
		// A run whose runtime has not attached yet truthfully has zero items.
		return projection, nil
	}
	owner, err := epochV8OwnerEpoch(view, snapshot.Runtime.EpochID)
	if err != nil {
		return EpochV8Projection{}, err
	}
	ownerSource, ok := snapshot.EpochSources[snapshot.Runtime.EpochID]
	if !ok {
		return EpochV8Projection{}, fmt.Errorf("schema-8 worklist owner epoch source is absent")
	}
	parsed, err := model.ParseExactSource(ownerSource)
	if err != nil || parsed == nil || parsed.Template == nil || parsed.Diagnostics.HasErrors() {
		return EpochV8Projection{}, fmt.Errorf("schema-8 worklist owner template is invalid")
	}
	checkpoint, err := pathv1.DecodeCheckpointV7(snapshot.Runtime.Checkpoint)
	if err != nil {
		return EpochV8Projection{}, fmt.Errorf("schema-8 worklist runtime checkpoint: %w", err)
	}
	aggregate, err := pathv1.CurrentAggregateCheckpoint(checkpoint)
	if err != nil {
		return EpochV8Projection{}, err
	}
	records, err := epochV8Records(snapshot.Run.ID, snapshot.Runtime.InternalRunID, parsed.Template, aggregate)
	if err != nil {
		return EpochV8Projection{}, err
	}
	if err := crossCheckEpochV8(records, view.Authorities, snapshot.Runtime.EpochID); err != nil {
		return EpochV8Projection{}, err
	}
	items := make([]Item, 0, len(records))
	for _, record := range records {
		item := record.item
		item.OwnerEpoch = &OwnerEpochRef{Ordinal: owner.Ordinal, TemplateRef: owner.TemplateRef}
		items = append(items, item)
	}
	assignEpochV8PresentationIDs(snapshot.Run.ID, owner.Ordinal, items)
	slices.SortFunc(items, func(a, b Item) int { return strings.Compare(a.ID, b.ID) })
	projection.Items = items
	projection.Report.Entries, projection.Report.EntriesTotal, projection.Report.EntriesTruncated = epochV8ReportEntries(items, owner.Ordinal)
	return projection, nil
}

// epochV8ReportEntries mirrors the items into report rows under the shared
// primary key, capped deterministically at maxEpochV8ReportEntries.
func epochV8ReportEntries(items []Item, ownerOrdinal uint64) ([]EpochV8ReportEntry, int, bool) {
	entries := make([]EpochV8ReportEntry, 0, min(len(items), maxEpochV8ReportEntries))
	for _, item := range items {
		if len(entries) == maxEpochV8ReportEntries {
			return entries, len(items), true
		}
		entries = append(entries, EpochV8ReportEntry{
			ID: item.ID, OwnerEpochOrdinal: ownerOrdinal, Kind: item.Kind,
			NodeID: item.Node, Attempt: item.Attempt, Status: item.Status,
		})
	}
	return entries, len(items), false
}

// epochV8Records derives the user-visible work records from the typed runtime
// aggregate. Summaries use bounded allowlisted labels only: node type,
// performer kind, wait kind, and signal name. Prompt/ask text, params,
// assignees, and available actions never enter schema-8 items.
func epochV8Records(runID, internalRunID string, tmpl *model.Template, aggregate pathv1.AggregateCheckpoint) ([]epochV8Record, error) {
	records := make([]epochV8Record, 0)
	coveredEffects := make(map[string]struct{})
	commandIDs := make([]string, 0, len(aggregate.Commands))
	for id := range aggregate.Commands {
		commandIDs = append(commandIDs, id)
	}
	slices.Sort(commandIDs)
	for _, id := range commandIDs {
		command := aggregate.Commands[id]
		if command.Identity.Kind != pathv1.CommandPerformAttempt {
			continue
		}
		status := state.WaitStatusPending
		if command.State == pathv1.CommandObserved || command.State == pathv1.CommandReconciled {
			status = state.WaitStatusSatisfied
		} else if !command.State.Active() {
			continue
		}
		work, contextErr := pathV1WorkContext(&aggregate.Routing, tmpl, command.Identity.SourceActivationID)
		if contextErr != nil {
			return nil, contextErr
		}
		if work.node.Type == model.NodeTypeWait {
			// Wait identities were minted under the runtime's internal run id;
			// the projected item still carries the public run id.
			item, effectID, buildErr := buildPathV1WaitItem(internalRunID, aggregate, work, command, status)
			if buildErr != nil {
				return nil, buildErr
			}
			item.Run = runID
			item.Links.RunID = runID
			coveredEffects[effectID] = struct{}{}
			effect := aggregate.SideEffects[effectID]
			records = append(records, epochV8Record{
				localID: "effect." + effectID, kind: epochv8.AuthorityWait,
				state: epochV8EffectAuthorityState(effect), reservationID: work.reservation.ID,
				nodeID: work.reservation.NodeID, item: item,
				alsoCovers: []epochV8Expectation{{
					localID: "command." + command.ID, kind: epochv8.AuthorityCommand,
					state: epochV8CommandAuthorityState(command.State), reservationID: work.reservation.ID,
					nodeID: work.reservation.NodeID,
				}},
			})
			continue
		}
		if work.node.Performer == nil || (work.node.Type != model.NodeTypeTask && work.node.Type != model.NodeTypeDecision) {
			continue
		}
		item := epochV8CommandItem(runID, work, command, status)
		if nudge := pathV1Nudge(aggregate, command.ID); nudge != nil {
			// Contact schedule state is allowlisted; the escalation target is
			// performer contact config and stays out of schema-8 items.
			nudge.EscalationTarget = ""
			item.Nudge = nudge
			if item.DueAt.IsZero() {
				item.DueAt = nudge.NextContactAt
			}
		}
		records = append(records, epochV8Record{
			localID: "command." + command.ID, kind: epochv8.AuthorityCommand,
			state: epochV8CommandAuthorityState(command.State), reservationID: work.reservation.ID,
			nodeID: work.reservation.NodeID, item: item,
		})
	}
	effectIDs := make([]string, 0, len(aggregate.SideEffects))
	for id := range aggregate.SideEffects {
		effectIDs = append(effectIDs, id)
	}
	slices.Sort(effectIDs)
	for _, id := range effectIDs {
		effect := aggregate.SideEffects[id]
		if _, covered := coveredEffects[effect.ID]; covered {
			continue
		}
		if effect.Kind != pathv1.SideEffectObligation && effect.Kind != pathv1.SideEffectBlock {
			continue
		}
		work, contextErr := pathV1WorkContext(&aggregate.Routing, tmpl, effect.ActivationID)
		if contextErr != nil {
			return nil, contextErr
		}
		item := buildPathV1SideEffectItem(runID, work, effect)
		item.Assignee = ""
		records = append(records, epochV8Record{
			localID: "effect." + effect.ID, kind: epochv8.AuthorityObligation,
			state: epochV8EffectAuthorityState(effect), reservationID: work.reservation.ID,
			nodeID: work.reservation.NodeID, item: item,
		})
	}
	return records, nil
}

func epochV8CommandItem(runID string, work pathV1WorkContextRecord, command pathv1.CommandRecord, status state.WaitStatus) Item {
	kind := KindAgentObligation
	summary := "Task awaiting agent performer"
	if work.node.Performer.Kind == model.PerformerHuman {
		kind = KindHumanWait
		summary = "Task awaiting human performer"
		if work.node.Type == model.NodeTypeDecision {
			kind = KindDecisionNeeded
			summary = "Decision awaiting human choice"
		}
	}
	attempt := int(command.Identity.Attempt)
	return Item{
		Run: runID, Node: work.reservation.NodeID, Attempt: attempt, Kind: kind,
		Status: status, Summary: summary,
		Detached: work.detachmentCount > 0, DetachmentCount: work.detachmentCount,
		Links: Links{RunID: runID, NodeID: work.reservation.NodeID},
	}
}

func epochV8CommandAuthorityState(commandState pathv1.CommandState) epochv8.AuthorityState {
	if commandState.Active() {
		return epochv8.AuthorityActive
	}
	if commandState == pathv1.CommandCanceled {
		return epochv8.AuthorityCanceled
	}
	return epochv8.AuthorityCompleted
}

func epochV8EffectAuthorityState(effect pathv1.SideEffectIdentity) epochv8.AuthorityState {
	if pathv1.ActiveSideEffect(effect) {
		return epochv8.AuthorityActive
	}
	if strings.Contains(effect.State, "cancel") {
		return epochv8.AuthorityCanceled
	}
	return epochv8.AuthorityCompleted
}

// crossCheckEpochV8 requires exact one-to-one agreement between the derived
// runtime records and the checkpoint authorities: each record must match
// exactly one authority on local id, kind, state, reservation, node, and
// epoch, and every active work-bearing authority on the runtime epoch must be
// covered by a record. Either direction failing is a whole-run coherence
// failure, never a dropped or invented item.
func crossCheckEpochV8(records []epochV8Record, authorities []epochv8.AuthorityRecord, ownerEpoch epochv8.EpochID) error {
	byLocal := make(map[string][]epochv8.AuthorityRecord, len(authorities))
	for _, authority := range authorities {
		byLocal[authority.LocalID] = append(byLocal[authority.LocalID], authority)
	}
	covered := make(map[string]struct{}, len(records))
	match := func(expected epochV8Expectation, primary bool) error {
		matches := byLocal[expected.localID]
		if len(matches) != 1 {
			return fmt.Errorf("schema-8 worklist authority match is not one-to-one")
		}
		authority := matches[0]
		if authority.EpochID != ownerEpoch || authority.Kind != expected.kind || authority.State != expected.state ||
			authority.ReservationID != expected.reservationID || authority.NodeID != expected.nodeID {
			return fmt.Errorf("schema-8 worklist authority disagrees with its runtime record")
		}
		if _, duplicate := covered[expected.localID]; duplicate && primary {
			return fmt.Errorf("schema-8 worklist runtime records are ambiguous")
		}
		covered[expected.localID] = struct{}{}
		return nil
	}
	for _, record := range records {
		if err := match(epochV8Expectation{localID: record.localID, kind: record.kind, state: record.state, reservationID: record.reservationID, nodeID: record.nodeID}, true); err != nil {
			return err
		}
		for _, paired := range record.alsoCovers {
			if err := match(paired, false); err != nil {
				return err
			}
		}
	}
	for _, authority := range authorities {
		if !epochV8WorkBearing(authority.Kind) || authority.EpochID != ownerEpoch {
			continue
		}
		if authority.State != epochv8.AuthorityClaimed && authority.State != epochv8.AuthorityActive {
			continue
		}
		if _, ok := covered[authority.LocalID]; !ok {
			return fmt.Errorf("schema-8 worklist active authority has no runtime record")
		}
	}
	return nil
}

// epochV8WorkBearing reports whether an authority kind represents
// user-visible work. Frontier/outcome/join/propagation and other routing
// authorities never become worklist items.
func epochV8WorkBearing(kind epochv8.AuthorityKind) bool {
	switch kind {
	case epochv8.AuthorityCommand, epochv8.AuthorityWait, epochv8.AuthorityObligation:
		return true
	default:
		return false
	}
}

// assignEpochV8PresentationIDs derives each item's collision-free
// presentation identity from safe semantic fields only: run, owner epoch
// ordinal, entry kind, node, attempt, and — when those still collide (for
// example repeated contacts on one node) — a response-local ordinal assigned
// in deterministic projection order. Raw command, receipt, and effect IDs
// never feed the identity.
func assignEpochV8PresentationIDs(runID string, ownerOrdinal uint64, items []Item) {
	seen := make(map[string]int, len(items))
	for index := range items {
		key := strings.Join([]string{
			runID, strconv.FormatUint(ownerOrdinal, 10), string(items[index].Kind),
			items[index].Node, strconv.Itoa(items[index].Attempt),
		}, "\x00")
		ordinal := seen[key]
		seen[key] = ordinal + 1
		sum := sha256.Sum256([]byte("worklist-epochv8/v1\x00" + key + "\x00" + strconv.Itoa(ordinal)))
		items[index].ID = "wi8_" + hex.EncodeToString(sum[:12])
	}
}

func epochV8OwnerEpoch(view epochv8.CheckpointView, epochID epochv8.EpochID) (epochv8.TemplateEpoch, error) {
	for _, epoch := range view.Epochs {
		if epoch.ID == epochID {
			return epoch, nil
		}
	}
	return epochv8.TemplateEpoch{}, fmt.Errorf("schema-8 worklist owner epoch is absent from the checkpoint")
}

func epochV8Timeline(view epochv8.CheckpointView) ([]EpochV8TimelineEvent, int, bool) {
	ordinals := make(map[epochv8.EpochID]uint64, len(view.Epochs))
	for _, epoch := range view.Epochs {
		ordinals[epoch.ID] = epoch.Ordinal
	}
	events := make([]EpochV8TimelineEvent, 0, len(view.History))
	for _, event := range view.History {
		projected := EpochV8TimelineEvent{Revision: event.Revision, Kind: string(event.Kind)}
		switch {
		case event.Apply != nil:
			projected.EpochOrdinal = event.Apply.CandidateEpoch.Ordinal
			projected.AppliedAt = event.Apply.AppliedAt
			projected.ReasonCode = event.Apply.ReasonCode
			projected.ActorClass = epochV8ActorClass(event.Apply.Actor)
		case event.Finish != nil:
			projected.EpochOrdinal = ordinals[event.Finish.OwnerEpochID]
		case event.Runtime != nil:
			projected.EpochOrdinal = ordinals[event.Runtime.EpochID]
		}
		events = append(events, projected)
	}
	total := len(events)
	slices.Reverse(events)
	if len(events) > maxEpochV8TimelineEvents {
		return events[:maxEpochV8TimelineEvents], total, true
	}
	return events, total, false
}

// epochV8ActorClass keeps only the actor kind prefix (human, agent), never
// the identity behind it.
func epochV8ActorClass(actor string) string {
	prefix, _, found := strings.Cut(actor, ":")
	if !found {
		return ""
	}
	return prefix
}
