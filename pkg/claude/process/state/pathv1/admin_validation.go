package pathv1

import (
	"errors"
	"fmt"

	legacy "github.com/tofutools/tclaude/pkg/claude/process/state"
)

var ErrLegacyAdminTimestampMissing = errors.New("inconsistent:legacy_admin_timestamp_missing")

// LegacyAdminTimestampMissingError classifies a timestamp-less legacy admin
// shape that cannot be bound under the narrow historical compatibility rule.
// Callers can direct an operator to restore authoritative provenance instead
// of inventing a timestamp during migration.
type LegacyAdminTimestampMissingError struct {
	AdminType          string
	OriginalArrayIndex uint64
	HasResolution      bool
}

func (e *LegacyAdminTimestampMissingError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf(
		"%v: adminRecords[%d] type %q is not a producer-valid timestamp-less non-resolution record; restore its authoritative timestamp before migration",
		ErrLegacyAdminTimestampMissing, e.OriginalArrayIndex, e.AdminType,
	)
}

func (e *LegacyAdminTimestampMissingError) Unwrap() error { return ErrLegacyAdminTimestampMissing }

func ValidateBlockResolution(resolution BlockResolution) (string, error) {
	if resolution.NodeID == "" || resolution.Actor == "" {
		return "", fmt.Errorf("block resolution lacks node/actor")
	}
	if resolution.Decision != "retry" && resolution.Decision != "skip" && resolution.Decision != "cancel" {
		return "", fmt.Errorf("invalid block resolution decision %q", resolution.Decision)
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
		if err := validateLegacyAdminTimestamp(record, resolution != nil); err != nil {
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

func validateLegacyAdminTimestamp(record PathV1AdminRecord, hasResolution bool) error {
	if record.Timestamp != "" || timestampLessLegacyAdminCompatible(record.AdminType, hasResolution) {
		return nil
	}
	return &LegacyAdminTimestampMissingError{
		AdminType: record.AdminType, OriginalArrayIndex: record.OriginalArrayIndex, HasResolution: hasResolution,
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
