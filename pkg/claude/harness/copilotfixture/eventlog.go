package copilotfixture

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Sanitizing a REAL session event log for commit.
//
// The scenario goldens next door record a run's SHAPE — event types in order,
// request structure, digests. That is the right form for proving the CLI still
// behaves as tclaude expects, and the wrong form for testing a follower: an
// incremental reader has to be fed bytes that are byte-for-byte a plausible
// events.jsonl, complete with the field names and nesting Copilot writes.
//
// So this produces the other artifact: the log itself, minus everything that
// must not be committed.
//
//   - Host paths become placeholders (the run's temp directories are already
//     known to the Sanitizer; anything else absolute is redacted wholesale).
//   - The ~26 kB system prompt and the wrapped/transformed user content are
//     replaced by a digest plus a byte count. They are the bulk that TCL-970
//     forbids committing, and no follower field reads them.
//   - UUIDs and timestamps are remapped DETERMINISTICALLY — first-appearance
//     order into a fixed synthetic series — rather than flattened to a single
//     `<uuid>`/`<timestamp>` token. Flattening would destroy the distinctness a
//     parser sees and make the file unparsable as timestamps; a stable series
//     keeps re-records diff-clean while remaining faithful in shape.
//
// Everything else — every event type, every usage/context/cost field — is kept
// verbatim, because that is precisely what the follower is being tested on.

// eventLogTimeBase is the instant the synthetic timestamp series starts from.
// Fixed so a re-record of an unchanged run produces an identical file.
var eventLogTimeBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// eventLogHostPathRE is the catch-all for absolute paths the run's own
// directory replacements did not cover — a Copilot-probed system path, a git
// root outside the sandbox, a path inside an error message. It requires two
// segments so a bare "/v1" style API fragment survives.
var eventLogHostPathRE = regexp.MustCompile(`/[\w.@+-]+(?:/[\w.@+-]+)+`)

// eventLogDirTokens rewrite the Sanitizer's slash-bearing directory
// placeholders into slash-free ones. Without this the catch-all above would
// swallow the tail of "<tmp>/work/nested" and erase the very structure the
// replacement was there to preserve.
var eventLogDirTokens = []replacement{
	{pathPlaceholder + "/home", "<tmp-home>"},
	{pathPlaceholder + "/cache", "<tmp-cache>"},
	{pathPlaceholder + "/work", "<tmp-work>"},
}

// EventLogSanitizer rewrites one session's events.jsonl into a committable
// fixture. It is single-use: the identifier maps it builds are what make the
// output stable, and sharing one across sessions would interleave them.
type EventLogSanitizer struct {
	text *Sanitizer

	uuids      map[string]string
	timestamps map[string]string
}

// NewEventLogSanitizer builds a sanitizer bound to one run's directories.
func NewEventLogSanitizer(text *Sanitizer) *EventLogSanitizer {
	return &EventLogSanitizer{
		text:       text,
		uuids:      map[string]string{},
		timestamps: map[string]string{},
	}
}

// bulkFields are the per-event `data` keys replaced by a digest. They carry
// model input (system prompt, tool schemas, wrapped user content) rather than
// anything tclaude projects.
var bulkFields = map[string][]string{
	"system.message":          {"content"},
	"user.message":            {"transformedContent", "attachments", "supportedNativeDocumentMimeTypes"},
	"assistant.message":       {"content", "toolRequests"},
	"assistant.reasoning":     {"content"},
	"tool.execution_complete": {"content", "result"},
	"tool.execution_start":    {"arguments"},
}

// SanitizeLog rewrites a whole events.jsonl. A line that is not valid JSON is
// PASSED THROUGH unchanged: the only such lines a real run produces are a
// partially flushed tail, and preserving one in a fixture is useful rather
// than harmful — it is a case the follower must survive.
func (s *EventLogSanitizer) SanitizeLog(raw []byte) ([]byte, error) {
	var out bytes.Buffer
	reader := bufio.NewReaderSize(bytes.NewReader(raw), 1<<20)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		sanitized, err := s.sanitizeLine(line)
		if err != nil {
			return nil, err
		}
		out.Write(sanitized)
		out.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("copilotfixture: reading event log: %w", err)
	}
	return out.Bytes(), nil
}

func (s *EventLogSanitizer) sanitizeLine(line []byte) ([]byte, error) {
	var event map[string]any
	if err := json.Unmarshal(line, &event); err != nil {
		// An unparsable line is kept — a half-flushed tail is a case the
		// follower must survive — but it is still REDACTED. The only lines a
		// real run fails to parse are truncated tails of the largest records,
		// which are exactly `system.message` and `tool.execution_complete`:
		// raw prompt and raw tool output, the two things bulkFields exists to
		// keep out of testdata. Passing such a fragment through verbatim would
		// bypass every replacement in this file.
		return []byte(s.rewriteString(string(line))), nil
	}
	eventType, _ := event["type"].(string)
	if data, ok := event["data"].(map[string]any); ok {
		for _, field := range bulkFields[eventType] {
			if value, present := data[field]; present {
				data[field] = digestOf(value)
			}
		}
	}
	rewritten := s.rewrite(event)
	encoded, err := json.Marshal(rewritten)
	if err != nil {
		return nil, fmt.Errorf("copilotfixture: re-encoding event: %w", err)
	}
	return encoded, nil
}

// rewrite walks the decoded event and normalizes every string it finds.
func (s *EventLogSanitizer) rewrite(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = s.rewrite(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = s.rewrite(item)
		}
		return out
	case string:
		return s.rewriteString(typed)
	default:
		return value
	}
}

// rewriteString applies, in order: the run's own temp directories, then the
// stable UUID and timestamp series, then a catch-all absolute-path redaction
// for anything the directory replacements did not cover (a Copilot-probed
// system path, a git root outside the sandbox).
func (s *EventLogSanitizer) rewriteString(in string) string {
	out := in
	for _, r := range s.text.replacements {
		if r.from != "" {
			out = strings.ReplaceAll(out, r.from, r.to)
		}
	}
	for _, token := range eventLogDirTokens {
		out = strings.ReplaceAll(out, token.from, token.to)
	}
	out = uuidRE.ReplaceAllStringFunc(out, s.stableUUID)
	out = timestampRE.ReplaceAllStringFunc(out, s.stableTimestamp)
	out = loopbackRE.ReplaceAllString(out, baseURLPlaceholder)
	out = eventLogHostPathRE.ReplaceAllString(out, "<redacted-path>")
	return out
}

func (s *EventLogSanitizer) stableUUID(in string) string {
	if mapped, ok := s.uuids[in]; ok {
		return mapped
	}
	// A v4-shaped id keeps the fixture parseable by anything that validates
	// the format, and the counter tail keeps distinct ids distinct.
	mapped := fmt.Sprintf("00000000-0000-4000-8000-%012d", len(s.uuids)+1)
	s.uuids[in] = mapped
	return mapped
}

func (s *EventLogSanitizer) stableTimestamp(in string) string {
	if mapped, ok := s.timestamps[in]; ok {
		return mapped
	}
	mapped := eventLogTimeBase.Add(time.Duration(len(s.timestamps)) * time.Second).
		Format("2006-01-02T15:04:05.000Z")
	s.timestamps[in] = mapped
	return mapped
}

// digestOf reduces a bulk value to a stable, non-reversible marker. The byte
// count is kept because a sudden change in it is the signal that Copilot's
// prompting changed, which is exactly the drift these fixtures exist to catch.
func digestOf(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "<redacted>"
	}
	sum := sha256.Sum256(encoded)
	return fmt.Sprintf("<redacted sha256:%s bytes:%d>", hex.EncodeToString(sum[:8]), len(encoded))
}

// WriteEventLogFixture sanitizes a session's log and writes it to dst.
func WriteEventLogFixture(sanitizer *Sanitizer, home, sessionID, dst string) error {
	src := filepath.Join(home, "session-state", sessionID, "events.jsonl")
	raw, err := os.ReadFile(src) //nolint:gosec // fixture-owned temp path
	if err != nil {
		return fmt.Errorf("copilotfixture: reading %s: %w", src, err)
	}
	encoded, err := NewEventLogSanitizer(sanitizer).SanitizeLog(raw)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, encoded, 0o644) //nolint:gosec // committed fixture
}

// EventTypesIn lists the `type` discriminators of a sanitized log in order,
// which is the compact assertion a smoke test compares.
func EventTypesIn(raw []byte) []string {
	var types []string
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for scanner.Scan() {
		var event struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		types = append(types, event.Type)
	}
	return types
}

// SortedUnique is a small helper for asserting on a type SET rather than the
// exact ordering, which varies with how many tool calls a turn made.
func SortedUnique(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, item := range in {
		if !seen[item] {
			seen[item] = true
			out = append(out, item)
		}
	}
	sort.Strings(out)
	return out
}
