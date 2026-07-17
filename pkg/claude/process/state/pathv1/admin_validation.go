package pathv1

import (
	"errors"
	"fmt"
	"strings"

	legacy "github.com/tofutools/tclaude/pkg/claude/process/state"
)

var ErrLegacyAdminTimestampMissing = errors.New("inconsistent:legacy_admin_timestamp_missing")

// LegacyAdminTimestampMissingError classifies a timestamp-less legacy admin
// shape that cannot be bound under the narrow historical compatibility rule.
// Callers can direct an operator to restore authoritative provenance instead
// of inventing a timestamp during migration.
type LegacyAdminTimestampMissingError struct {
	AdminType                  string
	OriginalArrayIndex         uint64
	HasResolution              bool
	RecordTimestampMissing     bool
	ResolutionTimestampMissing bool
}

func (e *LegacyAdminTimestampMissingError) Error() string {
	if e == nil {
		return ""
	}
	missing := "record timestamp"
	if e.RecordTimestampMissing && e.ResolutionTimestampMissing {
		missing = "record and resolution timestamps"
	} else if e.ResolutionTimestampMissing {
		missing = "resolution timestamp"
	}
	return fmt.Sprintf("%v: adminRecords[%d] type %q lacks its authoritative %s; restore it before migration",
		ErrLegacyAdminTimestampMissing, e.OriginalArrayIndex, e.AdminType, missing)
}

func (e *LegacyAdminTimestampMissingError) Unwrap() error { return ErrLegacyAdminTimestampMissing }

func ValidateBlockResolution(resolution BlockResolution) (string, error) {
	if resolution.NodeID == "" || resolution.NodeID != strings.TrimSpace(resolution.NodeID) {
		return "", fmt.Errorf("block resolution node is empty or noncanonical")
	}
	actor := legacy.ActorRef(resolution.Actor)
	if !legacy.ValidateActorRef(actor) || legacy.IsEngineActor(actor) {
		return "", fmt.Errorf("block resolution requires valid non-engine actor authority")
	}
	if resolution.BlockedAttempt == 0 {
		return "", fmt.Errorf("block resolution lacks blocked attempt")
	}
	if resolution.Decision != "retry" && resolution.Decision != "skip" && resolution.Decision != "cancel" {
		return "", fmt.Errorf("invalid block resolution decision %q", resolution.Decision)
	}
	if resolution.Reason == "" || resolution.Reason != strings.TrimSpace(resolution.Reason) {
		return "", fmt.Errorf("block resolution reason is empty or noncanonical")
	}
	if resolution.EvidenceRef == "" || resolution.EvidenceRef != strings.TrimSpace(resolution.EvidenceRef) {
		return "", fmt.Errorf("block resolution evidence is empty or noncanonical")
	}
	if resolution.Timestamp == "" {
		return "", fmt.Errorf("block resolution lacks timestamp")
	}
	if _, err := ParseCanonicalTimestamp(resolution.Timestamp); err != nil {
		return "", err
	}
	return BlockResolutionIdentity(resolution)
}
func ValidateAdminRecord(record PathV1AdminRecord, legacy bool, resolution *BlockResolution) error {
	if record.RunID == "" || record.AdminType == "" || record.Actor == "" {
		return fmt.Errorf("admin record lacks required authority tuple")
	}
	if legacy {
		if err := validateLegacyAdminTimestamps(record, resolution); err != nil {
			return err
		}
	}
	if legacy && record.EventSeq != 0 {
		return fmt.Errorf("legacy admin record has nonzero event sequence")
	}
	if record.EventSeq < 0 {
		return fmt.Errorf("negative admin event sequence")
	}
	if _, err := ParseCanonicalTimestamp(record.Timestamp); err != nil {
		return err
	}
	if resolution == nil {
		if record.ResolutionDigest != "" {
			return fmt.Errorf("admin record has digest without resolution")
		}
	} else {
		digest, err := ValidateBlockResolution(*resolution)
		if err != nil {
			return err
		}
		if record.ResolutionDigest != digest {
			return fmt.Errorf("admin resolution digest mismatch")
		}
		if record.Actor != resolution.Actor || record.EvidenceRef != resolution.EvidenceRef || record.Timestamp != resolution.Timestamp {
			return fmt.Errorf("admin resolution authority mismatch")
		}
	}
	var want string
	var err error
	if legacy {
		want, err = LegacyAdminRecordIdentity(record)
	} else {
		if record.OriginalArrayIndex != 0 {
			return fmt.Errorf("nonlegacy admin record has original array index")
		}
		want, err = AdminRecordIdentity(record)
	}
	if err != nil {
		return err
	}
	if record.ID != want {
		return fmt.Errorf("admin record identity mismatch")
	}
	return nil
}

func validateLegacyAdminTimestamps(record PathV1AdminRecord, resolution *BlockResolution) error {
	recordMissing := record.Timestamp == ""
	resolutionMissing := resolution != nil && resolution.Timestamp == ""
	if !recordMissing && !resolutionMissing {
		return nil
	}
	if recordMissing && !resolutionMissing && timestampLessLegacyAdminCompatible(record.AdminType, resolution != nil) {
		return nil
	}
	return &LegacyAdminTimestampMissingError{
		AdminType: record.AdminType, OriginalArrayIndex: record.OriginalArrayIndex, HasResolution: resolution != nil,
		RecordTimestampMissing: recordMissing, ResolutionTimestampMissing: resolutionMissing,
	}
}

func timestampLessLegacyAdminCompatible(adminType string, hasResolution bool) bool {
	if hasResolution {
		return false
	}
	switch adminType {
	case string(legacy.EventAdminRepairRecorded), string(legacy.EventAdminProgramsAllowed):
		return true
	default:
		return false
	}
}
