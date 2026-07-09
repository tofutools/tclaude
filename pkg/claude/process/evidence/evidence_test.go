package evidence

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/state"
)

var testTime = time.Date(2026, 7, 9, 14, 0, 0, 0, time.UTC)

func TestAppendReadRoundTrip(t *testing.T) {
	entry := sampleLogEntry(1, "implement")
	var logBuf bytes.Buffer
	if err := AppendLogEntry(&logBuf, entry); err != nil {
		t.Fatal(err)
	}
	entries, err := ReadNodeLog("implement", &logBuf)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Seq != 1 || entries[0].Event.Type != state.EventNodeAttemptStarted {
		t.Fatalf("entries = %#v", entries)
	}

	manifestEntry, err := ManifestEntryForLog(entry, "")
	if err != nil {
		t.Fatal(err)
	}
	var manifestBuf bytes.Buffer
	if err := AppendManifestEntry(&manifestBuf, manifestEntry); err != nil {
		t.Fatal(err)
	}
	manifest, err := ReadManifest(&manifestBuf)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest) != 1 || manifest[0].EventRef != EventRefForLogEntry(entry) {
		t.Fatalf("manifest = %#v", manifest)
	}
}

func TestReadErrorsDistinguishTornTailAndMidFileCorruption(t *testing.T) {
	first, err := EncodeLogEntry(sampleLogEntry(1, "implement"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := EncodeLogEntry(sampleLogEntry(2, "implement"))
	if err != nil {
		t.Fatal(err)
	}

	_, err = ReadNodeLog("implement", bytes.NewReader(append(first, bytes.TrimSuffix(second, []byte{'\n'})...)))
	assertReadError(t, err, ReadErrorTornTail, 2)

	_, err = ReadNodeLog("implement", strings.NewReader(`{"schemaVersion":1,"seq":1`+"\n"+string(second)))
	assertReadError(t, err, ReadErrorMalformed, 1)
}

func TestStrictDecodeAndVersionPreflight(t *testing.T) {
	entry := sampleLogEntry(1, "implement")
	data, err := EncodeLogEntry(entry)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(data))
	withUnknown := strings.TrimSuffix(line, "}") + `,"extra":true}` + "\n"
	_, err = ReadNodeLog("implement", strings.NewReader(withUnknown))
	assertReadError(t, err, ReadErrorMalformed, 1)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected unknown field error, got %v", err)
	}

	futureVersion := strings.Replace(withUnknown, `"schemaVersion":1`, `"schemaVersion":99`, 1)
	_, err = ReadNodeLog("implement", strings.NewReader(futureVersion))
	assertReadError(t, err, ReadErrorMalformed, 1)
	if err == nil || !strings.Contains(err.Error(), "unsupported log entry schema version 99") {
		t.Fatalf("expected version error before unknown-field error, got %v", err)
	}
}

func TestVerifySequenceClean(t *testing.T) {
	logs, manifest := sampleEvidence(t, 3)
	if diagnostics := VerifySequence(manifest, logs); diagnostics.HasErrors() {
		t.Fatalf("unexpected diagnostics: %#v", diagnostics)
	}
}

func TestVerifySequenceDetectsSeqProblems(t *testing.T) {
	tests := []struct {
		name     string
		manifest []ManifestEntry
		code     string
	}{
		{
			name:     "gap",
			manifest: recomputeManifest(t, []ManifestEntry{manifestStub(1), manifestStub(3)}),
			code:     "seq_gap",
		},
		{
			name:     "duplicate",
			manifest: recomputeManifest(t, []ManifestEntry{manifestStub(1), manifestStub(1)}),
			code:     "seq_duplicate",
		},
		{
			name:     "regression",
			manifest: recomputeManifest(t, []ManifestEntry{manifestStub(2), manifestStub(1)}),
			code:     "seq_regression",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			diagnostics := VerifySequence(tt.manifest, nil)
			if !hasDiagnostic(diagnostics, tt.code) {
				t.Fatalf("expected %q, got %#v", tt.code, diagnostics)
			}
		})
	}
}

func TestVerifySequenceDetectsChecksumMismatch(t *testing.T) {
	logs, manifest := sampleEvidence(t, 2)
	manifest[1].Checksum = "sha256:bad"
	diagnostics := VerifySequence(manifest, logs)
	if !hasDiagnostic(diagnostics, "checksum_mismatch") {
		t.Fatalf("expected checksum mismatch, got %#v", diagnostics)
	}
}

func TestVerifySequenceDetectsCrossReferenceProblems(t *testing.T) {
	logs, manifest := sampleEvidence(t, 2)

	t.Run("log ahead of manifest", func(t *testing.T) {
		diagnostics := VerifySequence(manifest[:1], logs)
		if !hasDiagnostic(diagnostics, "log_ahead_of_manifest") {
			t.Fatalf("expected log_ahead_of_manifest, got %#v", diagnostics)
		}
	})

	t.Run("manifest ahead of log", func(t *testing.T) {
		diagnostics := VerifySequence(manifest, []NodeLog{{NodeID: "implement", Entries: logs[0].Entries[:1]}})
		if !hasDiagnostic(diagnostics, "manifest_ahead_of_log") {
			t.Fatalf("expected manifest_ahead_of_log, got %#v", diagnostics)
		}
	})

	t.Run("event ref mismatch", func(t *testing.T) {
		bad := append([]ManifestEntry(nil), manifest...)
		bad[0].EventRef = "nodes/other/log.jsonl#1"
		bad = recomputeManifest(t, bad)
		diagnostics := VerifySequence(bad, logs)
		if !hasDiagnostic(diagnostics, "event_ref_mismatch") {
			t.Fatalf("expected event_ref_mismatch, got %#v", diagnostics)
		}
	})
}

func TestVerifyStateAnchors(t *testing.T) {
	logs, manifest := sampleEvidence(t, 2)
	if diagnostics := VerifySequence(manifest, logs); diagnostics.HasErrors() {
		t.Fatalf("bad fixture: %#v", diagnostics)
	}
	st := &state.State{LastLogSeq: 2, LogChecksum: manifest[1].Checksum}
	if diagnostics := VerifyStateAnchors(st, manifest); diagnostics.HasErrors() {
		t.Fatalf("unexpected anchor diagnostics: %#v", diagnostics)
	}

	st.LastLogSeq = 1
	diagnostics := VerifyStateAnchors(st, manifest)
	if !hasDiagnostic(diagnostics, "state_log_seq_mismatch") {
		t.Fatalf("expected seq mismatch, got %#v", diagnostics)
	}
	st.LastLogSeq = 2
	st.LogChecksum = "sha256:bad"
	diagnostics = VerifyStateAnchors(st, manifest)
	if !hasDiagnostic(diagnostics, "state_checksum_mismatch") {
		t.Fatalf("expected checksum mismatch, got %#v", diagnostics)
	}
}

func TestDualWriteCrashWindowsAreDetectable(t *testing.T) {
	logs, manifest := sampleEvidence(t, 1)

	t.Run("after log before manifest", func(t *testing.T) {
		diagnostics := VerifySequence(nil, logs)
		if !hasDiagnostic(diagnostics, "log_ahead_of_manifest") {
			t.Fatalf("expected log ahead, got %#v", diagnostics)
		}
	})

	t.Run("after manifest before state", func(t *testing.T) {
		st := &state.State{LastLogSeq: 0, LogChecksum: ""}
		diagnostics := VerifyStateAnchors(st, manifest)
		if !hasDiagnostic(diagnostics, "state_log_seq_mismatch") || !hasDiagnostic(diagnostics, "state_checksum_mismatch") {
			t.Fatalf("expected stale state anchors, got %#v", diagnostics)
		}
	})
}

func sampleEvidence(t *testing.T, count int) ([]NodeLog, []ManifestEntry) {
	t.Helper()
	entries := make([]LogEntry, count)
	manifest := make([]ManifestEntry, count)
	previous := ""
	for i := 0; i < count; i++ {
		entry := sampleLogEntry(int64(i+1), "implement")
		entries[i] = entry
		manifestEntry, err := ManifestEntryForLog(entry, previous)
		if err != nil {
			t.Fatal(err)
		}
		manifest[i] = manifestEntry
		previous = manifestEntry.Checksum
	}
	return []NodeLog{{NodeID: "implement", Entries: entries}}, manifest
}

func sampleLogEntry(seq int64, nodeID string) LogEntry {
	return LogEntry{
		SchemaVersion: LogEntrySchemaVersion,
		Seq:           seq,
		At:            testTime.Add(time.Duration(seq) * time.Minute),
		Scope:         Scope{Kind: ScopeNode, ID: nodeID},
		Kind:          EntryKindAttempt,
		Event: &state.Event{
			Type:   state.EventNodeAttemptStarted,
			Seq:    seq,
			At:     testTime.Add(time.Duration(seq) * time.Minute),
			NodeID: nodeID,
			Actor:  "agent:agt_dev123",
		},
	}
}

func manifestStub(seq int64) ManifestEntry {
	entry := sampleLogEntry(seq, "implement")
	return ManifestEntry{
		SchemaVersion: ManifestEntrySchemaVersion,
		Seq:           entry.Seq,
		Timestamp:     entry.At,
		Scope:         entry.Scope,
		EventRef:      EventRefForLogEntry(entry),
	}
}

func recomputeManifest(t *testing.T, entries []ManifestEntry) []ManifestEntry {
	t.Helper()
	out, err := ComputeManifestChecksums(entries)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertReadError(t *testing.T, err error, kind ReadErrorKind, line int) {
	t.Helper()
	var readErr *ReadError
	if !errors.As(err, &readErr) {
		t.Fatalf("expected ReadError, got %v", err)
	}
	if readErr.Kind != kind || readErr.Line != line {
		t.Fatalf("read error = %#v, want kind %q line %d", readErr, kind, line)
	}
}

func hasDiagnostic(diagnostics Diagnostics, code string) bool {
	for _, diag := range diagnostics {
		if diag.Code == code {
			return true
		}
	}
	return false
}
