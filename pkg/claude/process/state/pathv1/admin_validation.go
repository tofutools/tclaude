package pathv1

import "fmt"

func ValidateBlockResolution(resolution BlockResolution) (string, error) {
	if resolution.NodeID == "" || resolution.Actor == "" {
		return "", fmt.Errorf("block resolution lacks node/actor")
	}
	if resolution.Decision != "retry" && resolution.Decision != "skip" && resolution.Decision != "cancel" {
		return "", fmt.Errorf("invalid block resolution decision %q", resolution.Decision)
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
	if legacy && record.Timestamp == "" {
		return fmt.Errorf("legacy admin record lacks timestamp")
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
