package pathv1

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	legacy "github.com/tofutools/tclaude/pkg/claude/process/state"
)

const (
	LegacyMaxSchemaVersion    = 6
	MaxActiveLegacyIDs        = MaxRoutingList
	MaxLegacyAdminRecordCount = MaxRoutingList
)

var ErrUpgradeNeededOverBudget = errors.New("upgrade_needed_over_budget")

var legacyTemplateIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

type UpgradeNeededOverBudgetError struct {
	Limit          string
	Value, Maximum int
}

func (e *UpgradeNeededOverBudgetError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%v: %s is %d, maximum %d", ErrUpgradeNeededOverBudget, e.Limit, e.Value, e.Maximum)
}

func (e *UpgradeNeededOverBudgetError) Unwrap() error { return ErrUpgradeNeededOverBudget }

type UpgradeNeededReason string

const (
	UpgradeLegacyDrainRequired UpgradeNeededReason = "legacy_drain_required"
	UpgradeMigrationRequired   UpgradeNeededReason = "migration_required"
)

func (r UpgradeNeededReason) Valid() bool {
	return r == UpgradeLegacyDrainRequired || r == UpgradeMigrationRequired
}

type LegacyActiveKind string

const (
	LegacyActiveCommand         LegacyActiveKind = "command"
	LegacyActiveAttempt         LegacyActiveKind = "attempt"
	LegacyActiveWait            LegacyActiveKind = "wait"
	LegacyActiveTimer           LegacyActiveKind = "timer"
	LegacyActiveContact         LegacyActiveKind = "contact"
	LegacyActiveObligation      LegacyActiveKind = "obligation"
	LegacyActiveBlockedNode     LegacyActiveKind = "blocked_node"
	LegacyActiveBlockResolution LegacyActiveKind = "block_resolution"
	LegacyActiveSideEffect      LegacyActiveKind = "side_effect"
)

func (k LegacyActiveKind) Valid() bool {
	switch k {
	case LegacyActiveCommand, LegacyActiveAttempt, LegacyActiveWait, LegacyActiveTimer,
		LegacyActiveContact, LegacyActiveObligation, LegacyActiveBlockedNode,
		LegacyActiveBlockResolution, LegacyActiveSideEffect:
		return true
	}
	return false
}

type LegacyActiveID struct {
	Kind LegacyActiveKind `json:"kind"`
	ID   string           `json:"id"`
}

type CheckpointLegacyAdminRecord struct {
	ID         string            `json:"id"`
	LegacyID   string            `json:"legacyId"`
	Record     PathV1AdminRecord `json:"record"`
	Resolution *BlockResolution  `json:"resolution,omitempty"`
}

// UpgradeNeeded is the detached scheduler-facing migration-readiness
// authority. It is constructed from one coherent checkpoint/template/source
// view; evidence may verify that view but never supplies membership or IDs.
type UpgradeNeeded struct {
	Reason                 UpgradeNeededReason           `json:"reason"`
	RunID                  string                        `json:"runId"`
	LegacyStateSchema      int                           `json:"legacyStateSchema"`
	Checkpoint             CheckpointBinding             `json:"checkpoint"`
	TemplateRef            string                        `json:"templateRef"`
	TemplateSourceHash     string                        `json:"templateSourceHash"`
	ActiveLegacyIDs        []LegacyActiveID              `json:"activeLegacyIds"`
	CheckpointAdminRecords []CheckpointLegacyAdminRecord `json:"checkpointAdminRecords,omitempty"`
}

// AssessUpgradeNeeded is pure and must be called only from the coherent
// append-lock-held execution-view callback. All returned slices and records
// are copied, bounded, and deterministically sorted before the callback ends.
func AssessUpgradeNeeded(
	ctx context.Context,
	checkpointJSON []byte,
	st *legacy.State,
	templateRef, templateSourceHash string,
	adminRecords map[string]PathV1AdminRecord,
	adminResolutions map[string]BlockResolution,
) (UpgradeNeeded, error) {
	if err := ctx.Err(); err != nil {
		return UpgradeNeeded{}, err
	}
	if st == nil || st.RunID == "" || st.StateSchemaVersion <= 0 || st.StateSchemaVersion > LegacyMaxSchemaVersion {
		return UpgradeNeeded{}, fmt.Errorf("legacy migration readiness requires state schema 1-%d", LegacyMaxSchemaVersion)
	}
	if len(checkpointJSON) == 0 {
		return UpgradeNeeded{}, fmt.Errorf("legacy migration readiness checkpoint is empty")
	}
	if templateRef == "" || st.OriginalTemplateRef != templateRef || st.CurrentTemplateRef != templateRef {
		return UpgradeNeeded{}, fmt.Errorf("legacy migration readiness template binding mismatch")
	}
	if !canonicalDigest(templateSourceHash) {
		return UpgradeNeeded{}, fmt.Errorf("legacy migration readiness source hash is not canonical")
	}
	if st.LastLogSeq < 0 {
		return UpgradeNeeded{}, fmt.Errorf("legacy migration readiness has negative checkpoint sequence")
	}
	checkpointDigest, err := CheckpointIdentity(string(st.Status), uint64(st.LastLogSeq), st.LogChecksum, checkpointJSON)
	if err != nil {
		return UpgradeNeeded{}, err
	}
	checkpoint := CheckpointBinding{Generation: uint64(st.LastLogSeq), Digest: checkpointDigest}
	admins, err := checkpointAdminRecords(ctx, st.RunID, checkpoint, adminRecords, adminResolutions)
	if err != nil {
		return UpgradeNeeded{}, err
	}
	active, err := activeLegacyIDs(ctx, st, admins, adminResolutions)
	if err != nil {
		return UpgradeNeeded{}, err
	}
	reason := UpgradeMigrationRequired
	if len(active) > 0 {
		reason = UpgradeLegacyDrainRequired
	}
	result := UpgradeNeeded{
		Reason: reason, RunID: st.RunID, LegacyStateSchema: st.StateSchemaVersion,
		Checkpoint: checkpoint, TemplateRef: templateRef, TemplateSourceHash: templateSourceHash,
		ActiveLegacyIDs: active, CheckpointAdminRecords: admins,
	}
	if err := ValidateUpgradeNeeded(result); err != nil {
		return UpgradeNeeded{}, err
	}
	return result, nil
}

// ValidateUpgradeNeeded rejects partial or forged scheduler authority before
// a pre-planning decision can be made.
func ValidateUpgradeNeeded(needed UpgradeNeeded) error {
	if strings.TrimSpace(needed.RunID) == "" {
		return fmt.Errorf("upgrade-needed run id is required")
	}
	if needed.LegacyStateSchema <= 0 || needed.LegacyStateSchema > LegacyMaxSchemaVersion {
		return fmt.Errorf("upgrade-needed legacy schema is outside 1-%d", LegacyMaxSchemaVersion)
	}
	if !needed.Reason.Valid() {
		return fmt.Errorf("upgrade-needed reason %q is invalid", needed.Reason)
	}
	if !canonicalDigest(needed.Checkpoint.Digest) {
		return fmt.Errorf("upgrade-needed checkpoint digest is not canonical")
	}
	if !canonicalTemplateRef(needed.TemplateRef) {
		return fmt.Errorf("upgrade-needed template ref is not canonical")
	}
	if !canonicalDigest(needed.TemplateSourceHash) {
		return fmt.Errorf("upgrade-needed template source hash is not canonical")
	}
	if len(needed.ActiveLegacyIDs) > MaxActiveLegacyIDs {
		return &UpgradeNeededOverBudgetError{Limit: "active_legacy_ids", Value: len(needed.ActiveLegacyIDs), Maximum: MaxActiveLegacyIDs}
	}
	for i, active := range needed.ActiveLegacyIDs {
		if !active.Kind.Valid() || strings.TrimSpace(active.ID) == "" || active.ID != strings.TrimSpace(active.ID) {
			return fmt.Errorf("upgrade-needed active legacy id %d is invalid", i)
		}
		if i > 0 {
			previous := needed.ActiveLegacyIDs[i-1]
			if strings.Compare(string(previous.Kind), string(active.Kind)) > 0 ||
				(previous.Kind == active.Kind && strings.Compare(previous.ID, active.ID) >= 0) {
				return fmt.Errorf("upgrade-needed active legacy ids are not strictly sorted")
			}
		}
	}
	if needed.Reason == UpgradeLegacyDrainRequired && len(needed.ActiveLegacyIDs) == 0 ||
		needed.Reason == UpgradeMigrationRequired && len(needed.ActiveLegacyIDs) != 0 {
		return fmt.Errorf("upgrade-needed reason does not match active legacy ids")
	}
	if len(needed.CheckpointAdminRecords) > MaxLegacyAdminRecordCount {
		return &UpgradeNeededOverBudgetError{Limit: "legacy_admin_records", Value: len(needed.CheckpointAdminRecords), Maximum: MaxLegacyAdminRecordCount}
	}
	if needed.Reason == UpgradeMigrationRequired && len(needed.CheckpointAdminRecords) != 0 {
		return fmt.Errorf("upgrade-needed migration classification has checkpoint admin provenance")
	}
	for i, admin := range needed.CheckpointAdminRecords {
		if err := validateCheckpointAdminRecord(needed.RunID, needed.Checkpoint, admin); err != nil {
			return fmt.Errorf("upgrade-needed checkpoint admin record %d: %w", i, err)
		}
		if admin.Resolution != nil && !hasActiveLegacyID(needed.ActiveLegacyIDs, LegacyActiveID{Kind: LegacyActiveBlockResolution, ID: admin.ID}) {
			return fmt.Errorf("upgrade-needed checkpoint admin record %d resolution is absent from active legacy ids", i)
		}
		if i > 0 && strings.Compare(needed.CheckpointAdminRecords[i-1].ID, admin.ID) >= 0 {
			return fmt.Errorf("upgrade-needed checkpoint admin records are not strictly sorted")
		}
	}
	return nil
}

func canonicalTemplateRef(ref string) bool {
	if strings.Count(ref, "@sha256:") != 1 {
		return false
	}
	id, digest, ok := strings.Cut(ref, "@sha256:")
	return ok && legacyTemplateIDPattern.MatchString(id) && canonicalDigest(digest)
}

func checkpointAdminRecords(
	ctx context.Context,
	runID string,
	checkpoint CheckpointBinding,
	records map[string]PathV1AdminRecord,
	resolutions map[string]BlockResolution,
) ([]CheckpointLegacyAdminRecord, error) {
	if len(records) > MaxLegacyAdminRecordCount {
		return nil, &UpgradeNeededOverBudgetError{Limit: "legacy_admin_records", Value: len(records), Maximum: MaxLegacyAdminRecordCount}
	}
	result := make([]CheckpointLegacyAdminRecord, 0, len(records))
	for legacyID, source := range records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var resolution *BlockResolution
		if value, ok := resolutions[legacyID]; ok {
			copy := value
			resolution = &copy
		}
		id, err := CheckpointLegacyAdminRecordIdentity(checkpoint, source)
		if err != nil {
			return nil, err
		}
		admin := CheckpointLegacyAdminRecord{ID: id, LegacyID: legacyID, Record: source, Resolution: resolution}
		if err := validateCheckpointAdminRecord(runID, checkpoint, admin); err != nil {
			return nil, fmt.Errorf("legacy admin record %q: %w", legacyID, err)
		}
		result = append(result, admin)
	}
	for legacyID := range resolutions {
		if _, ok := records[legacyID]; !ok {
			return nil, fmt.Errorf("legacy admin resolution %q has no admin record", legacyID)
		}
	}
	slices.SortFunc(result, func(a, b CheckpointLegacyAdminRecord) int { return strings.Compare(a.ID, b.ID) })
	return result, nil
}

func validateCheckpointAdminRecord(runID string, checkpoint CheckpointBinding, admin CheckpointLegacyAdminRecord) error {
	if admin.Record.RunID != runID {
		return fmt.Errorf("record belongs to run %q, want %q", admin.Record.RunID, runID)
	}
	if !canonicalDigest(admin.ID) || !canonicalDigest(admin.LegacyID) || admin.Record.ID != admin.LegacyID {
		return fmt.Errorf("invalid identity")
	}
	if err := ValidateAdminRecord(admin.Record, true, admin.Resolution); err != nil {
		return err
	}
	if err := validateCheckpointAdminSemantics(admin); err != nil {
		return err
	}
	legacyID, err := LegacyAdminRecordIdentity(admin.Record)
	if err != nil || legacyID != admin.LegacyID {
		return fmt.Errorf("legacy identity mismatch")
	}
	checkpointID, err := CheckpointLegacyAdminRecordIdentity(checkpoint, admin.Record)
	if err != nil || checkpointID != admin.ID {
		return fmt.Errorf("checkpoint identity mismatch")
	}
	return nil
}

func validateCheckpointAdminSemantics(admin CheckpointLegacyAdminRecord) error {
	record := admin.Record
	switch record.AdminType {
	case string(legacy.EventBlockResolutionRecorded):
		if admin.Resolution == nil {
			return fmt.Errorf("block-resolution admin record lacks resolution")
		}
	case string(legacy.EventAdminRepairRecorded), string(legacy.EventAdminProgramsAllowed):
		if admin.Resolution != nil {
			return fmt.Errorf("admin type %q cannot carry block resolution", record.AdminType)
		}
		return nil
	default:
		return fmt.Errorf("legacy admin type %q is invalid", record.AdminType)
	}

	resolution := *admin.Resolution
	if resolution.BlockedAttempt == 0 {
		return fmt.Errorf("block resolution lacks blocked attempt")
	}
	if resolution.NodeID == "" || resolution.NodeID != strings.TrimSpace(resolution.NodeID) {
		return fmt.Errorf("block resolution node id is empty or noncanonical")
	}
	actor := legacy.ActorRef(resolution.Actor)
	if !legacy.ValidateActorRef(actor) || legacy.IsEngineActor(actor) {
		return fmt.Errorf("block resolution requires a valid non-engine actor")
	}
	if resolution.Reason == "" || resolution.Reason != strings.TrimSpace(resolution.Reason) {
		return fmt.Errorf("block resolution reason is empty or noncanonical")
	}
	if resolution.EvidenceRef == "" || resolution.EvidenceRef != strings.TrimSpace(resolution.EvidenceRef) {
		return fmt.Errorf("block resolution evidence ref is empty or noncanonical")
	}
	if resolution.Timestamp == "" {
		return fmt.Errorf("block resolution lacks timestamp")
	}
	if record.Actor != resolution.Actor || record.ReasonCode != resolution.Reason ||
		record.EvidenceRef != resolution.EvidenceRef || record.Timestamp != resolution.Timestamp {
		return fmt.Errorf("block-resolution admin authority does not match resolution payload")
	}
	return nil
}

func hasActiveLegacyID(active []LegacyActiveID, target LegacyActiveID) bool {
	_, found := slices.BinarySearchFunc(active, target, func(a, b LegacyActiveID) int {
		if n := strings.Compare(string(a.Kind), string(b.Kind)); n != 0 {
			return n
		}
		return strings.Compare(a.ID, b.ID)
	})
	return found
}

func activeLegacyIDs(ctx context.Context, st *legacy.State, admins []CheckpointLegacyAdminRecord, adminResolutions map[string]BlockResolution) ([]LegacyActiveID, error) {
	result := make([]LegacyActiveID, 0)
	add := func(kind LegacyActiveKind, id string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("active legacy %s has empty identity", kind)
		}
		result = append(result, LegacyActiveID{Kind: kind, ID: id})
		if len(result) > MaxActiveLegacyIDs {
			return &UpgradeNeededOverBudgetError{Limit: "active_legacy_ids", Value: len(result), Maximum: MaxActiveLegacyIDs}
		}
		return nil
	}

	for id, command := range st.OutstandingCommands {
		if !legacyCommandActive(command.Status) {
			continue
		}
		if err := add(LegacyActiveCommand, id); err != nil {
			return nil, err
		}
		if command.ExternalRef != "" {
			if err := add(LegacyActiveSideEffect, id); err != nil {
				return nil, err
			}
		}
	}
	for nodeID, node := range st.Nodes {
		if node.ActiveAttempt != nil && node.ActiveAttempt.SettledAt.IsZero() {
			id := node.ActiveAttempt.CommandID
			if id == "" {
				id = nodeID + "/attempt-" + strconv.Itoa(node.ActiveAttempt.Attempt)
			}
			if err := add(LegacyActiveAttempt, id); err != nil {
				return nil, err
			}
		}
		if node.Status == legacy.NodeStatusBlocked {
			if err := add(LegacyActiveBlockedNode, nodeID); err != nil {
				return nil, err
			}
		}
		if node.BlockResolution != nil {
			resolution, err := legacyResolution(*node.BlockResolution)
			if err != nil {
				return nil, fmt.Errorf("node %q block resolution: %w", nodeID, err)
			}
			digest, err := BlockResolutionIdentity(resolution)
			if err != nil {
				return nil, err
			}
			if err := add(LegacyActiveBlockResolution, nodeID+"/"+digest); err != nil {
				return nil, err
			}
		}
	}
	for id, wait := range st.Waits {
		if wait.Status == legacy.WaitStatusPending {
			if err := add(LegacyActiveWait, id); err != nil {
				return nil, err
			}
		}
	}
	for id, timer := range st.Timers {
		if timer.Status == legacy.WaitStatusPending {
			if err := add(LegacyActiveTimer, id); err != nil {
				return nil, err
			}
		}
	}
	for id, contact := range st.Contacts {
		command, ok := st.OutstandingCommands[id]
		inactive := contact.Paused && contact.NextContactAt.IsZero() && (!ok || !legacyCommandActive(command.Status))
		if !inactive {
			if err := add(LegacyActiveContact, id); err != nil {
				return nil, err
			}
		}
	}
	for id, obligation := range st.Obligations {
		if obligation.Status == legacy.WaitStatusPending {
			if err := add(LegacyActiveObligation, id); err != nil {
				return nil, err
			}
		}
	}
	adminByLegacyID := make(map[string]string, len(admins))
	for _, admin := range admins {
		adminByLegacyID[admin.LegacyID] = admin.ID
	}
	for legacyID := range adminResolutions {
		id, ok := adminByLegacyID[legacyID]
		if !ok {
			return nil, fmt.Errorf("legacy admin resolution %q has no admin record", legacyID)
		}
		if err := add(LegacyActiveBlockResolution, id); err != nil {
			return nil, err
		}
	}

	slices.SortFunc(result, func(a, b LegacyActiveID) int {
		if n := strings.Compare(string(a.Kind), string(b.Kind)); n != 0 {
			return n
		}
		return strings.Compare(a.ID, b.ID)
	})
	return slices.Clip(result), nil
}

func legacyCommandActive(status legacy.CommandStatus) bool {
	return status == legacy.CommandStatusIssued || status == legacy.CommandStatusObserved
}

func legacyResolution(source legacy.BlockResolution) (BlockResolution, error) {
	if source.BlockedAttempt < 0 {
		return BlockResolution{}, fmt.Errorf("negative blocked attempt")
	}
	result := BlockResolution{
		NodeID: source.NodeID, BlockedAttempt: uint64(source.BlockedAttempt), Decision: string(source.Decision),
		Actor: string(source.Actor), Reason: source.Reason, EvidenceRef: source.EvidenceRef,
		Timestamp: CanonicalTimestamp(source.Timestamp),
	}
	if _, err := ValidateBlockResolution(result); err != nil {
		return BlockResolution{}, err
	}
	return result, nil
}
