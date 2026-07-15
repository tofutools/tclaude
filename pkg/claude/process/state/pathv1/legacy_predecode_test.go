package pathv1

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	legacy "github.com/tofutools/tclaude/pkg/claude/process/state"
)

func TestLegacyTimestampInventoryCoversStateSchema(t *testing.T) {
	want := declaredTimePaths(reflect.TypeFor[legacy.State](), "")
	got := make([]string, 0, len(legacyTimestampPaths))
	for _, path := range legacyTimestampPaths {
		var value string
		for _, segment := range path {
			switch segment.kind {
			case 'f':
				if value != "" {
					value += "."
				}
				value += segment.field
			case 'm':
				value += ".*"
			case 'a':
				value += "[]"
			}
		}
		got = append(got, value)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("legacy timestamp inventory differs from state schema\n got: %v\nwant: %v", got, want)
	}
}

func TestPredecodeLegacyStateRejectsMalformedDeclaredTimestamps(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
	}{
		{"wrong type", `42`},
		{"malformed", `"not-a-time"`},
		{"invalid calendar", `"2026-02-30T12:00:00Z"`},
		{"leap second", `"2026-07-15T12:00:60Z"`},
		{"finer than nanoseconds", `"2026-07-15T12:00:00.1234567890Z"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PredecodeLegacyState(legacyStateWithPauseUntil(tc.raw))
			if !errors.Is(err, ErrLegacyTimestampMalformed) {
				t.Fatalf("error = %v, want %v", err, ErrLegacyTimestampMalformed)
			}
			var malformed *LegacyTimestampMalformedError
			if !errors.As(err, &malformed) || malformed.Path != "state.pause.until" {
				t.Fatalf("typed timestamp error = %#v", malformed)
			}
		})
	}
}

func TestPredecodeLegacyStateCanonicalizesOnlyDeclaredTimestamps(t *testing.T) {
	input := legacyStateWithPauseUntil(`"2026-07-15T14:34:56.123400000+02:00"`)
	input = []byte(strings.Replace(string(input), `"reason":"rate"`, `"reason":"2026-07-15T14:34:56.123400000+02:00"`, 1))
	decoded, err := PredecodeLegacyState(input)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 7, 15, 12, 34, 56, 123400000, time.UTC)
	if decoded.State.Pause == nil || !decoded.State.Pause.Until.Equal(want) || decoded.State.Pause.Until.Location() != time.UTC {
		t.Fatalf("canonical pause timestamp = %#v", decoded.State.Pause)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(decoded.CanonicalJSON, &raw); err != nil {
		t.Fatal(err)
	}
	var pause struct {
		Reason string `json:"reason"`
		Until  string `json:"until"`
	}
	if err := json.Unmarshal(raw["pause"], &pause); err != nil {
		t.Fatal(err)
	}
	if pause.Until != "2026-07-15T12:34:56.1234Z" {
		t.Fatalf("canonical timestamp = %q", pause.Until)
	}
	if pause.Reason != "2026-07-15T14:34:56.123400000+02:00" {
		t.Fatalf("arbitrary string was normalized: %q", pause.Reason)
	}
}

func TestPredecodeLegacyStateNearLimitPreservesSiblingLexemes(t *testing.T) {
	const sourceTimestamp = "2026-07-15T14:34:56.123400000+02:00"
	const canonicalTimestamp = "2026-07-15T12:34:56.1234Z"
	prefix := []byte(`{"stateSchemaVersion":6,"runId":"run","status":"paused","pause":{"kind":"rate_limited","reason":"duplicate","reason":"`)
	timestampPrefix := []byte(`","until":"`)
	suffix := []byte(`"},"originalTemplateRef":"demo@sha256:x","currentTemplateRef":"demo@sha256:x","nodes":{},"lastLogSeq":0,"logChecksum":""}`)
	payloadSize := MaxCheckpointBytes - len(prefix) - len(timestampPrefix) - len(sourceTimestamp) - len(suffix)
	if payloadSize <= 0 {
		t.Fatal("test checkpoint framing exceeds byte budget")
	}
	payload := strings.Repeat("<>&", (payloadSize+2)/3)[:payloadSize]
	input := make([]byte, 0, MaxCheckpointBytes)
	input = append(input, prefix...)
	input = append(input, payload...)
	input = append(input, timestampPrefix...)
	input = append(input, sourceTimestamp...)
	input = append(input, suffix...)
	if len(input) != MaxCheckpointBytes {
		t.Fatalf("input size = %d, want %d", len(input), MaxCheckpointBytes)
	}

	decoded, err := PredecodeLegacyState(input)
	if err != nil {
		t.Fatal(err)
	}
	want := make([]byte, 0, MaxCheckpointBytes)
	want = append(want, prefix...)
	want = append(want, payload...)
	want = append(want, timestampPrefix...)
	want = append(want, canonicalTimestamp...)
	want = append(want, suffix...)
	if !bytes.Equal(decoded.CanonicalJSON, want) {
		t.Fatal("canonical checkpoint changed bytes outside the declared timestamp")
	}
	if len(decoded.CanonicalJSON) >= len(input) || len(decoded.CanonicalJSON) > MaxCheckpointBytes {
		t.Fatalf("canonical checkpoint size = %d from input %d", len(decoded.CanonicalJSON), len(input))
	}
	if bytes.Contains(decoded.CanonicalJSON, []byte(`\u003c`)) || bytes.Contains(decoded.CanonicalJSON, []byte(`\u003e`)) || bytes.Contains(decoded.CanonicalJSON, []byte(`\u0026`)) {
		t.Fatal("sibling string was HTML-escaped")
	}
	if bytes.Count(decoded.CanonicalJSON, []byte(`"reason"`)) != 2 {
		t.Fatal("duplicate sibling key was not preserved")
	}

	_, err = PredecodeLegacyState(append(input, ' '))
	var overBudget *OverBudgetError
	if !errors.As(err, &overBudget) || overBudget.Limit != "checkpoint_bytes" || overBudget.Value != MaxCheckpointBytes+1 || overBudget.Maximum != MaxCheckpointBytes {
		t.Fatalf("over-budget error = %#v (%v)", overBudget, err)
	}
}

func TestPredecodeLegacyStateDerivesDeterministicAdminProvenance(t *testing.T) {
	data := []byte(`{
  "stateSchemaVersion": 6,
  "runId": "run-legacy",
  "status": "pending",
  "originalTemplateRef": "demo@sha256:x",
  "currentTemplateRef": "demo@sha256:x",
  "nodes": {},
  "adminRecords": [
    {"type":"branch_skip","actor":"human:johan","reason":"waived","evidenceRef":"ticket-1","timestamp":"2026-07-15T14:00:00+02:00","resolution":{"nodeId":"work","blockedAttempt":2,"decision":"skip","actor":"human:johan","reason":"waived","evidenceRef":"ticket-1","timestamp":"2026-07-15T14:00:00+02:00"}},
    {"type":"branch_skip","actor":"human:johan","reason":"waived","evidenceRef":"ticket-1","timestamp":"2026-07-15T14:00:00+02:00"}
  ],
  "lastLogSeq": 0,
  "logChecksum": ""
}`)
	first, err := PredecodeLegacyState(data)
	if err != nil {
		t.Fatal(err)
	}
	second, err := PredecodeLegacyState(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.AdminRecords, second.AdminRecords) || !reflect.DeepEqual(first.AdminResolutions, second.AdminResolutions) {
		t.Fatal("repeated legacy provenance derivation differed")
	}
	if len(first.AdminRecords) != 2 || len(first.AdminResolutions) != 1 {
		t.Fatalf("provenance sizes = records %d resolutions %d", len(first.AdminRecords), len(first.AdminResolutions))
	}
	ids := make([]string, 0, 2)
	for id, record := range first.AdminRecords {
		ids = append(ids, id)
		if record.Timestamp != "2026-07-15T12:00:00Z" {
			t.Fatalf("record timestamp = %q", record.Timestamp)
		}
	}
	if ids[0] == ids[1] {
		t.Fatal("original array index did not distinguish legacy identities")
	}
}

func legacyStateWithPauseUntil(raw string) []byte {
	return []byte(`{"stateSchemaVersion":6,"runId":"run","status":"paused","pause":{"kind":"rate_limited","reason":"rate","until":` + raw + `},"originalTemplateRef":"demo@sha256:x","currentTemplateRef":"demo@sha256:x","nodes":{},"lastLogSeq":0,"logChecksum":""}`)
}

func declaredTimePaths(typ reflect.Type, prefix string) []string {
	timeType := reflect.TypeFor[time.Time]()
	if typ == timeType {
		return []string{prefix}
	}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Map:
		return declaredTimePaths(typ.Elem(), prefix+".*")
	case reflect.Slice, reflect.Array:
		if typ.Elem().Kind() == reflect.Uint8 {
			return nil
		}
		return declaredTimePaths(typ.Elem(), prefix+"[]")
	case reflect.Struct:
		var paths []string
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if name == "" || name == "-" {
				continue
			}
			child := name
			if prefix != "" {
				child = prefix + "." + name
			}
			paths = append(paths, declaredTimePaths(field.Type, child)...)
		}
		slices.Sort(paths)
		return paths
	default:
		return nil
	}
}
