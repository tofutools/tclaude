package pathv1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	legacy "github.com/tofutools/tclaude/pkg/claude/process/state"
)

var (
	ErrLegacyTimestampMalformed = errors.New("inconsistent:legacy_timestamp_malformed")
	legacyTimestampPattern      = regexp.MustCompile(`^([0-9]{4})-([0-9]{2})-([0-9]{2})T(?:[01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](?:\.([0-9]+))?(?:Z|[+-](?:[01][0-9]|2[0-3]):[0-5][0-9])$`)
)

// LegacyTimestampMalformedError is the stable fail-closed classification for
// a declared legacy checkpoint timestamp that cannot be represented exactly
// by Go's nanosecond time.Time. The raw pass reports this before strict state
// decoding can collapse wrong types and precision loss into generic JSON
// errors.
type LegacyTimestampMalformedError struct {
	Path   string
	Reason string
}

func (e *LegacyTimestampMalformedError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%v at %s: %s", ErrLegacyTimestampMalformed, e.Path, e.Reason)
}

func (e *LegacyTimestampMalformedError) Unwrap() error { return ErrLegacyTimestampMalformed }

// LegacyStatePredecode is the bounded, deterministic bridge from the current
// legacy checkpoint schema to dormant path-v1 inputs. CanonicalJSON differs
// only when a schema-declared timestamp needed UTC/RFC3339Nano normalization.
type LegacyStatePredecode struct {
	CanonicalJSON    []byte
	State            *legacy.State
	AdminRecords     map[string]PathV1AdminRecord
	AdminResolutions map[string]BlockResolution
}

type legacyTimestampSegment struct {
	field string
	kind  byte // f = named field, m = every map value, a = every array value
}

// This is an explicit inventory of every time.Time field reachable from
// state.State. It intentionally does not inspect arbitrary string values.
var legacyTimestampPaths = [][]legacyTimestampSegment{
	{{field: "pause", kind: 'f'}, {field: "until", kind: 'f'}},
	{{field: "templateDivergence", kind: 'f'}, {field: "at", kind: 'f'}},
	{{field: "nodes", kind: 'f'}, {kind: 'm'}, {field: "pendingFeedback", kind: 'f'}, {field: "at", kind: 'f'}},
	{{field: "nodes", kind: 'f'}, {kind: 'm'}, {field: "activeAttempt", kind: 'f'}, {field: "startedAt", kind: 'f'}},
	{{field: "nodes", kind: 'f'}, {kind: 'm'}, {field: "activeAttempt", kind: 'f'}, {field: "settledAt", kind: 'f'}},
	{{field: "nodes", kind: 'f'}, {kind: 'm'}, {field: "decisions", kind: 'f'}, {kind: 'a'}, {field: "timestamp", kind: 'f'}},
	{{field: "nodes", kind: 'f'}, {kind: 'm'}, {field: "blockedAt", kind: 'f'}},
	{{field: "nodes", kind: 'f'}, {kind: 'm'}, {field: "blockResolution", kind: 'f'}, {field: "timestamp", kind: 'f'}},
	{{field: "outstandingCommands", kind: 'f'}, {kind: 'm'}, {field: "createdAt", kind: 'f'}},
	{{field: "outstandingCommands", kind: 'f'}, {kind: 'm'}, {field: "reconcileAfter", kind: 'f'}},
	{{field: "waits", kind: 'f'}, {kind: 'm'}, {field: "createdAt", kind: 'f'}},
	{{field: "waits", kind: 'f'}, {kind: 'm'}, {field: "dueAt", kind: 'f'}},
	{{field: "waits", kind: 'f'}, {kind: 'm'}, {field: "satisfiedAt", kind: 'f'}},
	{{field: "timers", kind: 'f'}, {kind: 'm'}, {field: "createdAt", kind: 'f'}},
	{{field: "timers", kind: 'f'}, {kind: 'm'}, {field: "dueAt", kind: 'f'}},
	{{field: "timers", kind: 'f'}, {kind: 'm'}, {field: "satisfiedAt", kind: 'f'}},
	{{field: "obligations", kind: 'f'}, {kind: 'm'}, {field: "dueAt", kind: 'f'}},
	{{field: "obligations", kind: 'f'}, {kind: 'm'}, {field: "createdAt", kind: 'f'}},
	{{field: "obligations", kind: 'f'}, {kind: 'm'}, {field: "resolvedAt", kind: 'f'}},
	{{field: "contacts", kind: 'f'}, {kind: 'm'}, {field: "lastContactedAt", kind: 'f'}},
	{{field: "contacts", kind: 'f'}, {kind: 'm'}, {field: "nextContactAt", kind: 'f'}},
	{{field: "contacts", kind: 'f'}, {kind: 'm'}, {field: "lastRecoveredAt", kind: 'f'}},
	{{field: "contacts", kind: 'f'}, {kind: 'm'}, {field: "escalatedAt", kind: 'f'}},
	{{field: "contacts", kind: 'f'}, {kind: 'm'}, {field: "humanInteractedAt", kind: 'f'}},
	{{field: "adminRecords", kind: 'f'}, {kind: 'a'}, {field: "timestamp", kind: 'f'}},
	{{field: "adminRecords", kind: 'f'}, {kind: 'a'}, {field: "resolution", kind: 'f'}, {field: "timestamp", kind: 'f'}},
}

var legacyTimestampSchema = buildLegacyTimestampSchema()

type legacyTimestampSchemaNode struct {
	fields     map[string]*legacyTimestampSchemaNode
	mapValue   *legacyTimestampSchemaNode
	arrayValue *legacyTimestampSchemaNode
	terminal   bool
}

type legacyTimestampReplacement struct {
	start, end int
	value      []byte
}

// PredecodeLegacyState performs the raw declared-timestamp pass, the ordinary
// legacy decode, raw duplicate-name rejection, then derives index-bound admin
// provenance.
func PredecodeLegacyState(data []byte) (LegacyStatePredecode, error) {
	return PredecodeLegacyStateContext(context.Background(), data)
}

func PredecodeLegacyStateContext(ctx context.Context, data []byte) (LegacyStatePredecode, error) {
	if len(data) > MaxCheckpointBytes {
		return LegacyStatePredecode{}, &OverBudgetError{Limit: "checkpoint_bytes", Value: len(data), Maximum: MaxCheckpointBytes}
	}
	canonical, err := rewriteLegacyTimestamps(ctx, data)
	if err != nil {
		return LegacyStatePredecode{}, err
	}
	st, err := legacy.DecodeContext(ctx, canonical)
	if err != nil {
		return LegacyStatePredecode{}, err
	}
	// Decode first so the existing timestamp, syntax, Unicode, and
	// single-document errors retain their established precedence. No decoded
	// state can escape until the untouched raw names pass duplicate validation.
	if err := validateLegacyDuplicateKeys(data); err != nil {
		return LegacyStatePredecode{}, err
	}
	if err := ctx.Err(); err != nil {
		return LegacyStatePredecode{}, err
	}
	records, resolutions, err := deriveLegacyAdminProvenance(st)
	if err != nil {
		return LegacyStatePredecode{}, fmt.Errorf("derive legacy admin provenance: %w", err)
	}
	return LegacyStatePredecode{CanonicalJSON: canonical, State: st, AdminRecords: records, AdminResolutions: resolutions}, nil
}

// validateLegacyDuplicateKeys uses the bounded raw-token parser and discards
// its semantic tree. The checkpoint byte limit bounds retained keys and values;
// the original byte slice remains the sole rewrite source.
func validateLegacyDuplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	_, err := parseJCSValue(dec)
	var duplicate *duplicateObjectKeyError
	if errors.As(err, &duplicate) {
		return duplicate
	}
	return nil
}

func buildLegacyTimestampSchema() *legacyTimestampSchemaNode {
	root := &legacyTimestampSchemaNode{}
	for _, path := range legacyTimestampPaths {
		node := root
		for _, segment := range path {
			switch segment.kind {
			case 'f':
				if node.fields == nil {
					node.fields = make(map[string]*legacyTimestampSchemaNode)
				}
				if node.fields[segment.field] == nil {
					node.fields[segment.field] = &legacyTimestampSchemaNode{}
				}
				node = node.fields[segment.field]
			case 'm':
				if node.mapValue == nil {
					node.mapValue = &legacyTimestampSchemaNode{}
				}
				node = node.mapValue
			case 'a':
				if node.arrayValue == nil {
					node.arrayValue = &legacyTimestampSchemaNode{}
				}
				node = node.arrayValue
			default:
				panic(fmt.Sprintf("unknown legacy timestamp path segment %q", segment.kind))
			}
		}
		node.terminal = true
	}
	return root
}

// rewriteLegacyTimestamps walks the raw checkpoint once and replaces only the
// byte ranges of declared timestamp string values. It deliberately does not
// round-trip containers through encoding/json: sibling lexemes and object
// order remain byte-for-byte intact, and the output is bounded by
// MaxCheckpointBytes.
func rewriteLegacyTimestamps(ctx context.Context, raw []byte) ([]byte, error) {
	scanner := legacyTimestampScanner{ctx: ctx, raw: raw, nextContextCheck: 32 << 10}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := scanner.scanValue(0, legacyTimestampSchema, "state", 0); err != nil {
		var syntaxErr *legacyTimestampScanSyntaxError
		if errors.As(err, &syntaxErr) {
			// The strict legacy decoder owns generic JSON/container errors. No
			// partial rewrite is applied when the raw scan cannot finish.
			return append([]byte(nil), raw...), nil
		}
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	size := len(raw)
	for _, replacement := range scanner.replacements {
		size += len(replacement.value) - (replacement.end - replacement.start)
	}
	if size > MaxCheckpointBytes {
		return nil, &OverBudgetError{Limit: "checkpoint_bytes", Value: size, Maximum: MaxCheckpointBytes}
	}
	if len(scanner.replacements) == 0 {
		return append([]byte(nil), raw...), nil
	}

	canonical := make([]byte, 0, size)
	previous := 0
	for _, replacement := range scanner.replacements {
		canonical = append(canonical, raw[previous:replacement.start]...)
		canonical = append(canonical, replacement.value...)
		previous = replacement.end
	}
	canonical = append(canonical, raw[previous:]...)
	return canonical, nil
}

const maxLegacyTimestampScanDepth = 256
const maxLegacyTimestampMapKeyPathBytes = 1 << 10

type legacyTimestampScanSyntaxError struct{ offset int }

func (e *legacyTimestampScanSyntaxError) Error() string {
	return fmt.Sprintf("invalid JSON near byte %d", e.offset)
}

type legacyTimestampScanner struct {
	ctx              context.Context
	raw              []byte
	replacements     []legacyTimestampReplacement
	nextContextCheck int
}

func (s *legacyTimestampScanner) scanValue(offset int, schema *legacyTimestampSchemaNode, location string, depth int) (int, error) {
	offset, err := s.skipSpace(offset)
	if err != nil {
		return 0, err
	}
	if offset >= len(s.raw) {
		return 0, &legacyTimestampScanSyntaxError{offset: offset}
	}
	if schema != nil && schema.terminal {
		return s.scanTimestamp(offset, location)
	}
	if depth >= maxLegacyTimestampScanDepth {
		return 0, &legacyTimestampScanSyntaxError{offset: offset}
	}
	switch s.raw[offset] {
	case '{':
		if schema == nil || (len(schema.fields) == 0 && schema.mapValue == nil) {
			return s.skipObject(offset, depth)
		}
		return s.scanObject(offset, schema, location, depth)
	case '[':
		if schema == nil || schema.arrayValue == nil {
			return s.skipArray(offset, depth)
		}
		return s.scanArray(offset, schema.arrayValue, location, depth)
	default:
		return s.skipScalar(offset)
	}
}

func (s *legacyTimestampScanner) scanTimestamp(offset int, location string) (int, error) {
	if s.raw[offset] != '"' {
		return 0, &LegacyTimestampMalformedError{Path: location, Reason: "timestamp must be a JSON string"}
	}
	end, err := s.scanString(offset)
	if err != nil {
		return 0, err
	}
	var value string
	if err := json.Unmarshal(s.raw[offset:end], &value); err != nil {
		return 0, &LegacyTimestampMalformedError{Path: location, Reason: "timestamp must be a JSON string"}
	}
	canonical, err := canonicalLegacyTimestamp(value)
	if err != nil {
		return 0, &LegacyTimestampMalformedError{Path: location, Reason: err.Error()}
	}
	if canonical != value {
		s.replacements = append(s.replacements, legacyTimestampReplacement{
			start: offset,
			end:   end,
			value: []byte(strconv.Quote(canonical)),
		})
	}
	return end, nil
}

func (s *legacyTimestampScanner) scanObject(offset int, schema *legacyTimestampSchemaNode, location string, depth int) (int, error) {
	offset++
	for {
		var err error
		offset, err = s.skipSpace(offset)
		if err != nil {
			return 0, err
		}
		if offset >= len(s.raw) {
			return 0, &legacyTimestampScanSyntaxError{offset: offset}
		}
		if s.raw[offset] == '}' {
			return offset + 1, nil
		}
		if s.raw[offset] != '"' {
			return 0, &legacyTimestampScanSyntaxError{offset: offset}
		}
		keyEnd, err := s.scanString(offset)
		if err != nil {
			return 0, err
		}
		key, keyDecoded, err := decodeLegacyTimestampSchemaKey(s.raw[offset:keyEnd], schema)
		if err != nil {
			return 0, &legacyTimestampScanSyntaxError{offset: offset}
		}
		offset, err = s.skipSpace(keyEnd)
		if err != nil {
			return 0, err
		}
		if offset >= len(s.raw) || s.raw[offset] != ':' {
			return 0, &legacyTimestampScanSyntaxError{offset: offset}
		}
		offset++

		var child *legacyTimestampSchemaNode
		childLocation := location + ".*"
		if keyDecoded {
			child = schema.fields[key]
			childLocation = location + "." + key
		}
		if child == nil && schema.mapValue != nil {
			child = schema.mapValue
		}
		offset, err = s.scanValue(offset, child, childLocation, depth+1)
		if err != nil {
			return 0, err
		}
		offset, err = s.skipSpace(offset)
		if err != nil {
			return 0, err
		}
		if offset >= len(s.raw) {
			return 0, &legacyTimestampScanSyntaxError{offset: offset}
		}
		switch s.raw[offset] {
		case ',':
			offset++
		case '}':
			return offset + 1, nil
		default:
			return 0, &legacyTimestampScanSyntaxError{offset: offset}
		}
	}
}

func decodeLegacyTimestampSchemaKey(raw []byte, schema *legacyTimestampSchemaNode) (string, bool, error) {
	maxBytes := 0
	for field := range schema.fields {
		// Every byte in these ASCII schema names can occupy at most six bytes
		// as a JSON Unicode escape, plus the surrounding quotes.
		if encodedBytes := 6*len(field) + 2; encodedBytes > maxBytes {
			maxBytes = encodedBytes
		}
	}
	if schema.mapValue != nil && maxBytes < maxLegacyTimestampMapKeyPathBytes {
		maxBytes = maxLegacyTimestampMapKeyPathBytes
	}
	if len(raw) > maxBytes {
		return "", false, nil
	}
	var key string
	if err := json.Unmarshal(raw, &key); err != nil {
		return "", false, err
	}
	return key, true, nil
}

func (s *legacyTimestampScanner) scanArray(offset int, element *legacyTimestampSchemaNode, location string, depth int) (int, error) {
	offset++
	for index := 0; ; index++ {
		var err error
		offset, err = s.skipSpace(offset)
		if err != nil {
			return 0, err
		}
		if offset >= len(s.raw) {
			return 0, &legacyTimestampScanSyntaxError{offset: offset}
		}
		if s.raw[offset] == ']' {
			return offset + 1, nil
		}
		offset, err = s.scanValue(offset, element, location+"["+strconv.Itoa(index)+"]", depth+1)
		if err != nil {
			return 0, err
		}
		offset, err = s.skipSpace(offset)
		if err != nil {
			return 0, err
		}
		if offset >= len(s.raw) {
			return 0, &legacyTimestampScanSyntaxError{offset: offset}
		}
		switch s.raw[offset] {
		case ',':
			offset++
		case ']':
			return offset + 1, nil
		default:
			return 0, &legacyTimestampScanSyntaxError{offset: offset}
		}
	}
}

func (s *legacyTimestampScanner) skipValue(offset, depth int) (int, error) {
	offset, err := s.skipSpace(offset)
	if err != nil {
		return 0, err
	}
	if offset >= len(s.raw) || depth >= maxLegacyTimestampScanDepth {
		return 0, &legacyTimestampScanSyntaxError{offset: offset}
	}
	switch s.raw[offset] {
	case '{':
		return s.skipObject(offset, depth)
	case '[':
		return s.skipArray(offset, depth)
	default:
		return s.skipScalar(offset)
	}
}

func (s *legacyTimestampScanner) skipObject(offset, depth int) (int, error) {
	offset++
	for {
		var err error
		offset, err = s.skipSpace(offset)
		if err != nil {
			return 0, err
		}
		if offset >= len(s.raw) {
			return 0, &legacyTimestampScanSyntaxError{offset: offset}
		}
		if s.raw[offset] == '}' {
			return offset + 1, nil
		}
		if s.raw[offset] != '"' {
			return 0, &legacyTimestampScanSyntaxError{offset: offset}
		}
		offset, err = s.scanString(offset)
		if err != nil {
			return 0, err
		}
		offset, err = s.skipSpace(offset)
		if err != nil {
			return 0, err
		}
		if offset >= len(s.raw) || s.raw[offset] != ':' {
			return 0, &legacyTimestampScanSyntaxError{offset: offset}
		}
		offset, err = s.skipValue(offset+1, depth+1)
		if err != nil {
			return 0, err
		}
		offset, err = s.skipSpace(offset)
		if err != nil {
			return 0, err
		}
		if offset >= len(s.raw) {
			return 0, &legacyTimestampScanSyntaxError{offset: offset}
		}
		switch s.raw[offset] {
		case ',':
			offset++
		case '}':
			return offset + 1, nil
		default:
			return 0, &legacyTimestampScanSyntaxError{offset: offset}
		}
	}
}

func (s *legacyTimestampScanner) skipArray(offset, depth int) (int, error) {
	offset++
	for {
		var err error
		offset, err = s.skipSpace(offset)
		if err != nil {
			return 0, err
		}
		if offset >= len(s.raw) {
			return 0, &legacyTimestampScanSyntaxError{offset: offset}
		}
		if s.raw[offset] == ']' {
			return offset + 1, nil
		}
		offset, err = s.skipValue(offset, depth+1)
		if err != nil {
			return 0, err
		}
		offset, err = s.skipSpace(offset)
		if err != nil {
			return 0, err
		}
		if offset >= len(s.raw) {
			return 0, &legacyTimestampScanSyntaxError{offset: offset}
		}
		switch s.raw[offset] {
		case ',':
			offset++
		case ']':
			return offset + 1, nil
		default:
			return 0, &legacyTimestampScanSyntaxError{offset: offset}
		}
	}
}

func (s *legacyTimestampScanner) skipScalar(offset int) (int, error) {
	if s.raw[offset] == '"' {
		return s.scanString(offset)
	}
	start := offset
	for offset < len(s.raw) {
		if err := s.checkContext(offset); err != nil {
			return 0, err
		}
		switch s.raw[offset] {
		case ' ', '\t', '\r', '\n', ',', '}', ']':
			if offset == start {
				return 0, &legacyTimestampScanSyntaxError{offset: offset}
			}
			return offset, nil
		default:
			offset++
		}
	}
	if offset == start {
		return 0, &legacyTimestampScanSyntaxError{offset: offset}
	}
	return offset, nil
}

func (s *legacyTimestampScanner) scanString(offset int) (int, error) {
	if offset >= len(s.raw) || s.raw[offset] != '"' {
		return 0, &legacyTimestampScanSyntaxError{offset: offset}
	}
	for offset++; offset < len(s.raw); offset++ {
		if err := s.checkContext(offset); err != nil {
			return 0, err
		}
		switch s.raw[offset] {
		case '"':
			return offset + 1, nil
		case '\\':
			offset++
			if offset >= len(s.raw) {
				return 0, &legacyTimestampScanSyntaxError{offset: offset}
			}
		}
	}
	return 0, &legacyTimestampScanSyntaxError{offset: offset}
}

func (s *legacyTimestampScanner) skipSpace(offset int) (int, error) {
	for offset < len(s.raw) {
		if err := s.checkContext(offset); err != nil {
			return 0, err
		}
		switch s.raw[offset] {
		case ' ', '\t', '\r', '\n':
			offset++
		default:
			return offset, nil
		}
	}
	return offset, nil
}

func (s *legacyTimestampScanner) checkContext(offset int) error {
	if offset < s.nextContextCheck {
		return nil
	}
	if err := s.ctx.Err(); err != nil {
		return err
	}
	s.nextContextCheck = offset + (32 << 10)
	return nil
}

func canonicalLegacyTimestamp(value string) (string, error) {
	match := legacyTimestampPattern.FindStringSubmatch(value)
	if match == nil {
		return "", fmt.Errorf("timestamp must use strict RFC3339 syntax without leap seconds")
	}
	if len(match[4]) > 9 {
		return "", fmt.Errorf("timestamp fractional precision exceeds nanoseconds")
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "", fmt.Errorf("timestamp has an invalid calendar value: %w", err)
	}
	return CanonicalTimestamp(parsed), nil
}

func deriveLegacyAdminProvenance(st *legacy.State) (map[string]PathV1AdminRecord, map[string]BlockResolution, error) {
	records := make(map[string]PathV1AdminRecord, len(st.AdminRecords))
	resolutions := make(map[string]BlockResolution)
	for index, source := range st.AdminRecords {
		record := PathV1AdminRecord{
			RunID:              st.RunID,
			OriginalArrayIndex: uint64(index),
			AdminType:          string(source.Type),
			Actor:              string(source.Actor),
			ReasonCode:         source.Reason,
			EvidenceRef:        source.EvidenceRef,
			Timestamp:          CanonicalTimestamp(source.Timestamp),
		}
		var resolution *BlockResolution
		if source.Resolution != nil {
			if source.Resolution.BlockedAttempt < 0 {
				return nil, nil, fmt.Errorf("adminRecords[%d] has negative blocked attempt", index)
			}
			value := BlockResolution{
				NodeID:         source.Resolution.NodeID,
				BlockedAttempt: uint64(source.Resolution.BlockedAttempt),
				Decision:       string(source.Resolution.Decision),
				Actor:          string(source.Resolution.Actor),
				Reason:         source.Resolution.Reason,
				EvidenceRef:    source.Resolution.EvidenceRef,
				Timestamp:      CanonicalTimestamp(source.Resolution.Timestamp),
			}
			resolution = &value
		}
		// Classify absent outer or attached-resolution timestamps before strict
		// resolution validation so migration callers always receive the stable,
		// actionable compatibility error.
		if err := validateLegacyAdminTimestamps(record, resolution); err != nil {
			return nil, nil, err
		}
		if resolution != nil {
			digest, err := ValidateBlockResolution(*resolution)
			if err != nil {
				return nil, nil, fmt.Errorf("adminRecords[%d] resolution: %w", index, err)
			}
			record.ResolutionDigest = digest
		}
		id, err := LegacyAdminRecordIdentity(record)
		if err != nil {
			return nil, nil, fmt.Errorf("adminRecords[%d] identity: %w", index, err)
		}
		record.ID = id
		if err := ValidateAdminRecord(record, true, resolution); err != nil {
			return nil, nil, fmt.Errorf("adminRecords[%d]: %w", index, err)
		}
		records[id] = record
		if resolution != nil {
			resolutions[id] = *resolution
		}
	}
	return records, resolutions, nil
}
