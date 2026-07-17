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
		{"malformed unicode", `"\ud800"`},
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

func TestPredecodeLegacyStateRejectsDuplicateKeysDuringTimestampNormalization(t *testing.T) {
	const offsetTimestamp = `"2026-07-15T14:34:56.123400000+02:00"`
	tests := []struct {
		name string
		raw  string
	}{
		{
			name: "top-level unrelated key before rewritten field",
			raw:  `{"stateSchemaVersion":6,"runId":"first","runId":"run","status":"paused","pause":{"kind":"rate_limited","reason":"rate","until":` + offsetTimestamp + `},"originalTemplateRef":"demo@sha256:x","currentTemplateRef":"demo@sha256:x","nodes":{},"lastLogSeq":0,"logChecksum":""}`,
		},
		{
			name: "top-level unrelated key after rewritten field",
			raw:  `{"stateSchemaVersion":6,"runId":"run","status":"paused","pause":{"kind":"rate_limited","reason":"rate","until":` + offsetTimestamp + `},"originalTemplateRef":"demo@sha256:x","currentTemplateRef":"demo@sha256:x","nodes":{},"lastLogSeq":0,"lastLogSeq":1,"logChecksum":""}`,
		},
		{
			name: "top-level timestamp-bearing key",
			raw:  `{"stateSchemaVersion":6,"runId":"run","status":"paused","pause":{"kind":"rate_limited","reason":"first","until":` + offsetTimestamp + `},"pause":{"kind":"rate_limited","reason":"second","until":` + offsetTimestamp + `},"originalTemplateRef":"demo@sha256:x","currentTemplateRef":"demo@sha256:x","nodes":{},"lastLogSeq":0,"logChecksum":""}`,
		},
		{
			name: "nested unrelated key before rewritten field",
			raw:  `{"stateSchemaVersion":6,"runId":"run","status":"paused","pause":{"kind":"rate_limited","reason":"first","reason":"second","until":` + offsetTimestamp + `},"originalTemplateRef":"demo@sha256:x","currentTemplateRef":"demo@sha256:x","nodes":{},"lastLogSeq":0,"logChecksum":""}`,
		},
		{
			name: "nested unrelated key after rewritten field",
			raw:  `{"stateSchemaVersion":6,"runId":"run","status":"paused","pause":{"kind":"rate_limited","until":` + offsetTimestamp + `,"reason":"first","reason":"second"},"originalTemplateRef":"demo@sha256:x","currentTemplateRef":"demo@sha256:x","nodes":{},"lastLogSeq":0,"logChecksum":""}`,
		},
		{
			name: "nested timestamp key",
			raw:  `{"stateSchemaVersion":6,"runId":"run","status":"paused","pause":{"kind":"rate_limited","reason":"rate","until":` + offsetTimestamp + `,"until":"2026-07-15T12:34:56.1234Z"},"originalTemplateRef":"demo@sha256:x","currentTemplateRef":"demo@sha256:x","nodes":{},"lastLogSeq":0,"logChecksum":""}`,
		},
		{
			name: "nested map key",
			raw:  `{"stateSchemaVersion":6,"runId":"run","status":"paused","pause":{"kind":"rate_limited","reason":"rate","until":` + offsetTimestamp + `},"originalTemplateRef":"demo@sha256:x","currentTemplateRef":"demo@sha256:x","nodes":{"work":{},"work":{}},"lastLogSeq":0,"logChecksum":""}`,
		},
		{
			name: "nested array object key",
			raw:  `{"stateSchemaVersion":6,"runId":"run","status":"pending","originalTemplateRef":"demo@sha256:x","currentTemplateRef":"demo@sha256:x","nodes":{},"adminRecords":[{"type":"repair_recorded","actor":"human:operator","reason":"first","reason":"second","timestamp":` + offsetTimestamp + `}],"lastLogSeq":0,"logChecksum":""}`,
		},
		{
			name: "nested raw payload array object key",
			raw:  `{"stateSchemaVersion":6,"runId":"run","status":"paused","pause":{"kind":"rate_limited","reason":"rate","until":` + offsetTimestamp + `},"originalTemplateRef":"demo@sha256:x","currentTemplateRef":"demo@sha256:x","nodes":{},"outstandingCommands":{"cmd":{"id":"cmd","payload":{"outer":[{"x":1,"x":2}]},"nodeId":"work","kind":"start_attempt","status":"issued"}},"lastLogSeq":0,"logChecksum":""}`,
		},
		{
			name: "escaped equivalent key",
			raw:  `{"stateSchemaVersion":6,"runId":"run","status":"paused","pause":{"kind":"rate_limited","reason":"first","rea\u0073on":"second","until":` + offsetTimestamp + `},"originalTemplateRef":"demo@sha256:x","currentTemplateRef":"demo@sha256:x","nodes":{},"lastLogSeq":0,"logChecksum":""}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			decoded, err := PredecodeLegacyState([]byte(tc.raw))
			if err == nil {
				t.Fatalf("duplicate-key checkpoint accepted: %#v", decoded.State)
			}
			if !strings.Contains(err.Error(), "duplicate object key") {
				t.Fatalf("error = %v, want duplicate-key rejection", err)
			}
		})
	}
}

func TestValidateLegacyDuplicateKeysUsesDecodedPerObjectNames(t *testing.T) {
	tests := []struct {
		name          string
		raw           string
		wantDuplicate bool
	}{
		{name: "escaped ASCII equivalent", raw: `{"name":1,"\u006eame":2}`, wantDuplicate: true},
		{name: "escaped surrogate-pair equivalent", raw: `{"😀":1,"\ud83d\ude00":2}`, wantDuplicate: true},
		{name: "case-sensitive", raw: `{"Name":1,"name":2}`},
		{name: "same name in separate objects", raw: `{"left":{"id":1},"right":{"id":2}}`},
		{name: "same name in separate array objects", raw: `[{"id":1},{"id":2}]`},
		{name: "duplicate nested through array", raw: `{"payload":[{"outer":{"id":1,"id":2}}]}`, wantDuplicate: true},
		{name: "malformed JSON remains legacy decoder concern", raw: `{"id":`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateLegacyDuplicateKeys([]byte(tc.raw))
			var duplicate *duplicateObjectKeyError
			if errors.As(err, &duplicate) != tc.wantDuplicate {
				t.Fatalf("error = %v, want duplicate = %t", err, tc.wantDuplicate)
			}
		})
	}

	key := strings.Repeat("x", 4<<10)
	err := validateLegacyDuplicateKeys([]byte(`{"` + key + `":1,"` + key + `":2}`))
	if err == nil || !strings.Contains(err.Error(), "name truncated") || len(err.Error()) > 1<<10 {
		t.Fatalf("duplicate diagnostic is not bounded: length=%d error=%v", len(err.Error()), err)
	}
}

func TestPredecodeLegacyStatePreservesExistingErrorPrecedence(t *testing.T) {
	duplicateRunID := func(data []byte) []byte {
		return bytes.Replace(data, []byte(`"runId":"run"`), []byte(`"runId":"first","runId":"run"`), 1)
	}

	_, err := PredecodeLegacyState(duplicateRunID(legacyStateWithPauseUntil(`"not-a-time"`)))
	if !errors.Is(err, ErrLegacyTimestampMalformed) {
		t.Fatalf("invalid timestamp plus duplicate error = %v, want %v", err, ErrLegacyTimestampMalformed)
	}

	_, err = PredecodeLegacyState([]byte(`{"stateSchemaVersion":6,"runId":"first","runId":"run",`))
	if err == nil || strings.Contains(err.Error(), "duplicate object key") {
		t.Fatalf("malformed JSON precedence changed: %v", err)
	}

	trailing := append(duplicateRunID(legacyStateWithPauseUntil(`"2026-07-15T14:34:56+02:00"`)), []byte(` {}`)...)
	_, err = PredecodeLegacyState(trailing)
	if err == nil || !strings.Contains(err.Error(), "multiple JSON values") || strings.Contains(err.Error(), "duplicate object key") {
		t.Fatalf("trailing JSON precedence changed: %v", err)
	}
}

func TestPredecodeLegacyStateNearLimitPreservesSiblingLexemes(t *testing.T) {
	const sourceTimestamp = "2026-07-15T14:34:56.123400000+02:00"
	const canonicalTimestamp = "2026-07-15T12:34:56.1234Z"
	prefix := []byte(`{"stateSchemaVersion":6,"runId":"run","status":"paused","pause":{"kind":"rate_limited","reason":"`)
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

func TestPredecodeLegacyStateBindsProducerValidTimestampLessAdminRecords(t *testing.T) {
	for _, adminType := range []legacy.EventType{
		legacy.EventAdminRepairRecorded,
		legacy.EventAdminProgramsAllowed,
	} {
		t.Run(string(adminType), func(t *testing.T) {
			input := legacyStateWithAdminRecords(`[{"type":"` + string(adminType) + `","actor":"human:operator","reason":"historical audit","evidenceRef":"ticket:TCL-523"}]`)
			first, err := PredecodeLegacyState(input)
			if err != nil {
				t.Fatal(err)
			}
			second, err := PredecodeLegacyState(input)
			if err != nil {
				t.Fatal(err)
			}
			if len(first.AdminRecords) != 1 || len(first.AdminResolutions) != 0 {
				t.Fatalf("provenance = records %d resolutions %d", len(first.AdminRecords), len(first.AdminResolutions))
			}
			for id, record := range first.AdminRecords {
				if record.Timestamp != "" || record.AdminType != string(adminType) || record.ID != id {
					t.Fatalf("timestamp-less record = %#v", record)
				}
				if second.AdminRecords[id] != record {
					t.Fatal("timestamp-less identity changed across predecode")
				}
			}
		})
	}
}

func TestPredecodeLegacyStateClassifiesUnsupportedTimestampLessAdminRecords(t *testing.T) {
	for _, tc := range []struct {
		name, record                     string
		adminType                        string
		hasResolution                    bool
		recordMissing, resolutionMissing bool
	}{
		{
			name:      "block resolution",
			record:    `{"type":"block_resolution_recorded","actor":"human:operator","reason":"waived","evidenceRef":"ticket:TCL-523","resolution":{"nodeId":"work","blockedAttempt":1,"decision":"skip","actor":"human:operator","reason":"waived","evidenceRef":"ticket:TCL-523"}}`,
			adminType: string(legacy.EventBlockResolutionRecorded), hasResolution: true, recordMissing: true, resolutionMissing: true,
		},
		{
			name:      "present record timestamp missing resolution timestamp",
			record:    `{"type":"block_resolution_recorded","actor":"human:operator","reason":"waived","evidenceRef":"ticket:TCL-523","timestamp":"2026-07-16T00:00:00Z","resolution":{"nodeId":"work","blockedAttempt":1,"decision":"skip","actor":"human:operator","reason":"waived","evidenceRef":"ticket:TCL-523"}}`,
			adminType: string(legacy.EventBlockResolutionRecorded), hasResolution: true, resolutionMissing: true,
		},
		{
			name:      "unknown type",
			record:    `{"type":"forged_admin","actor":"human:operator","reason":"forged"}`,
			adminType: "forged_admin", recordMissing: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PredecodeLegacyState(legacyStateWithAdminRecords("[" + tc.record + "]"))
			if !errors.Is(err, ErrLegacyAdminTimestampMissing) {
				t.Fatalf("error = %v, want %v", err, ErrLegacyAdminTimestampMissing)
			}
			var missing *LegacyAdminTimestampMissingError
			if !errors.As(err, &missing) || missing.OriginalArrayIndex != 0 || missing.AdminType != tc.adminType || missing.HasResolution != tc.hasResolution ||
				missing.RecordTimestampMissing != tc.recordMissing || missing.ResolutionTimestampMissing != tc.resolutionMissing {
				t.Fatalf("typed timestamp error = %#v", missing)
			}
			if !strings.Contains(err.Error(), "restore it before migration") {
				t.Fatalf("error is not actionable: %v", err)
			}
		})
	}
}

func legacyStateWithPauseUntil(raw string) []byte {
	return []byte(`{"stateSchemaVersion":6,"runId":"run","status":"paused","pause":{"kind":"rate_limited","reason":"rate","until":` + raw + `},"originalTemplateRef":"demo@sha256:x","currentTemplateRef":"demo@sha256:x","nodes":{},"lastLogSeq":0,"logChecksum":""}`)
}

func legacyStateWithAdminRecords(records string) []byte {
	return []byte(`{"stateSchemaVersion":6,"runId":"run","status":"pending","originalTemplateRef":"demo@sha256:x","currentTemplateRef":"demo@sha256:x","nodes":{},"adminRecords":` + records + `,"lastLogSeq":0,"logChecksum":""}`)
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
