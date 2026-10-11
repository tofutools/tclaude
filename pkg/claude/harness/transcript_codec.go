package harness

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
)

// TranscriptCodec converts between a harness's HistoryTransfer wire format
// and the neutral transcript, for cross-harness morph. Decode reads what the
// harness's History.Open produces; Encode writes what its
// History.ImportReader accepts, under a fresh synthetic source identity the
// import then remints, validates and indexes exactly as for any transfer.
type TranscriptCodec interface {
	Decode(r io.Reader, sourceID string) (*transcript.Transcript, error)
	Encode(p *transcript.Plan, cwd string, w io.Writer) (sourceID string, err error)
	// InlineMedia reports whether native user turns carry inline images.
	InlineMedia() bool
}

// SupportsTranscript reports whether this harness can take part in a
// cross-harness history translation (both directions).
func (h *Harness) SupportsTranscript() bool {
	return h != nil && h.Transcript != nil && h.SupportsHistoryTransfer()
}

// scanHistoryRecords calls fn for each non-blank JSONL record.
func scanHistoryRecords(r io.Reader, fn func(line int, record []byte) error) error {
	limit := MaxHistoryRecordBytes
	if bounded, ok := r.(interface{ HistoryRecordLimit() int }); ok && bounded.HistoryRecordLimit() > 0 {
		limit = bounded.HistoryRecordLimit()
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, min(64<<10, limit)), limit)
	line := 0
	for scanner.Scan() {
		line++
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		if err := fn(line, scanner.Bytes()); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("history record exceeds %d bytes or cannot be read: %w", limit, err)
	}
	return nil
}

// jsonlWriter writes one JSON value per line.
type jsonlWriter struct {
	enc *json.Encoder
	err error
}

func newJSONLWriter(w io.Writer) *jsonlWriter {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return &jsonlWriter{enc: enc}
}

func (j *jsonlWriter) write(v any) {
	if j.err == nil {
		j.err = j.enc.Encode(v)
	}
}

// parseTime accepts RFC 3339 strings, falling back to the zero time.
func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// encodeTimes assigns each plan entry a strictly increasing timestamp,
// keeping source times where they are already ordered.
func encodeTimes(p *transcript.Plan) []time.Time {
	out := make([]time.Time, len(p.Entries))
	now := time.Now().UTC()
	prev := time.Time{}
	for i, e := range p.Entries {
		t := e.Time.UTC()
		if t.IsZero() || !t.After(prev) || t.After(now) {
			t = prev.Add(time.Millisecond)
			if prev.IsZero() {
				t = now.Add(-time.Duration(len(p.Entries)) * time.Millisecond)
			}
		}
		out[i] = t
		prev = t
	}
	return out
}

func rfc3339ms(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// dataURL parses "data:<type>;base64,<data>".
func dataURL(s string) (string, []byte, bool) {
	rest, ok := strings.CutPrefix(s, "data:")
	if !ok {
		return "", nil, false
	}
	meta, payload, ok := strings.Cut(rest, ",")
	if !ok || !strings.HasSuffix(meta, ";base64") {
		return "", nil, false
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", nil, false
	}
	return strings.TrimSuffix(meta, ";base64"), data, true
}

func toDataURL(m transcript.Media) string {
	return "data:" + m.MediaType + ";base64," + base64.StdEncoding.EncodeToString(m.Data)
}

// planText joins an entry's text blocks.
func planText(e transcript.Entry) string {
	var parts []string
	for _, b := range e.Blocks {
		if b.Kind == transcript.BlockText {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// rawJSONString decodes a JSON string, or returns the raw text of any other
// JSON value (tool outputs are sometimes structured).
func rawJSONString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// msTime converts epoch milliseconds, keeping zero as the zero time.
func msTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}
