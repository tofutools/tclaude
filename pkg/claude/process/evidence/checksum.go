package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
)

const checksumSeed = "tclaude-process-manifest-sha256-chain-v1"

func EventRefForLogEntry(entry LogEntry) string {
	switch entry.Scope.Kind {
	case ScopeNode:
		return "nodes/" + entry.Scope.ID + "/log.jsonl#" + itoa64(entry.Seq)
	case ScopeRun:
		return "run/log.jsonl#" + itoa64(entry.Seq)
	default:
		return "unknown#" + itoa64(entry.Seq)
	}
}

func ManifestEntryForLog(entry LogEntry, previousChecksum string) (ManifestEntry, error) {
	manifest := ManifestEntry{
		SchemaVersion: ManifestEntrySchemaVersion,
		Seq:           entry.Seq,
		Timestamp:     entry.At,
		Scope:         entry.Scope,
		EventRef:      EventRefForLogEntry(entry),
	}
	checksum, err := NextChecksum(previousChecksum, manifest)
	if err != nil {
		return ManifestEntry{}, err
	}
	manifest.Checksum = checksum
	return manifest, nil
}

func ComputeManifestChecksums(entries []ManifestEntry) ([]ManifestEntry, error) {
	out := make([]ManifestEntry, len(entries))
	previous := ""
	for i, entry := range entries {
		entry.Checksum = ""
		checksum, err := NextChecksum(previous, entry)
		if err != nil {
			return nil, err
		}
		entry.Checksum = checksum
		out[i] = entry
		previous = checksum
	}
	return out, nil
}

func ManifestChecksum(entries []ManifestEntry) (string, error) {
	withChecksums, err := ComputeManifestChecksums(entries)
	if err != nil {
		return "", err
	}
	if len(withChecksums) == 0 {
		return "", nil
	}
	return withChecksums[len(withChecksums)-1].Checksum, nil
}

func NextChecksum(previous string, entry ManifestEntry) (string, error) {
	if previous == "" {
		previous = checksumSeed
	}
	entry.Checksum = ""
	payload, err := canonicalManifestPayload(entry)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, _ = h.Write([]byte(previous))
	_, _ = h.Write([]byte{'\n'})
	_, _ = h.Write(payload)
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func VerifyManifestChecksums(entries []ManifestEntry) Diagnostics {
	var diagnostics Diagnostics
	previous := ""
	for i, entry := range entries {
		want, err := NextChecksum(previous, entry)
		if err != nil {
			diagnostics = append(diagnostics, diagError("checksum_error", "manifest["+itoa(i)+"]", err.Error()))
			continue
		}
		if entry.Checksum != want {
			diagnostics = append(diagnostics, diagError("checksum_mismatch", "manifest["+itoa(i)+"].checksum", fmt.Sprintf("manifest seq %d checksum %q does not match expected %q", entry.Seq, entry.Checksum, want)))
		}
		previous = entry.Checksum
	}
	return diagnostics
}

func canonicalManifestPayload(entry ManifestEntry) ([]byte, error) {
	payload := struct {
		SchemaVersion int    `json:"schemaVersion"`
		Seq           int64  `json:"seq"`
		Timestamp     string `json:"ts"`
		Scope         Scope  `json:"scope"`
		EventRef      string `json:"eventRef"`
	}{
		SchemaVersion: entry.SchemaVersion,
		Seq:           entry.Seq,
		Timestamp:     entry.Timestamp.UTC().Format("2006-01-02T15:04:05.000000000Z07:00"),
		Scope:         entry.Scope,
		EventRef:      entry.EventRef,
	}
	return json.Marshal(payload)
}

func itoa64(value int64) string {
	return strconv.FormatInt(value, 10)
}
