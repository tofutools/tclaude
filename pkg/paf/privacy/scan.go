// Package privacy scans PAF content for suspected credentials. Findings are
// kinds, counts and locations; matched text never leaves the scanner.
package privacy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/common/configbundle"
	"github.com/tofutools/tclaude/pkg/paf"
)

// Scanner names this scanner in privacy.json.
const Scanner = "tclaude-privacy/1"

// Finding kinds beyond the shared credential floor.
const (
	// KindAWSAccessKey is an AWS access key id.
	KindAWSAccessKey = "aws-access-key"
	// KindHighEntropy is a long random-looking token. It is advisory: it
	// is reported but does not block an export on its own.
	KindHighEntropy = "high-entropy-string"
)

// maxLocations bounds the locations kept per kind.
const maxLocations = 3

// maxToken is the longest run the entropy detector judges as a token.
const maxToken = 256

var (
	awsKey = regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)
	// run is a run of base64 / base64url characters. A run longer than
	// maxToken is a blob (encoded media or files), not a token; a shorter
	// one is judged segment by segment between '/', so a path is not one
	// candidate.
	run = regexp.MustCompile(`[A-Za-z0-9_+/=-]{24,}`)
	// structuralID is an identifier with a lowercase type prefix, as
	// harnesses and providers mint them (toolu_…, msg_…, call_…, req_…).
	structuralID = regexp.MustCompile(`^[a-z]{2,12}[_-]`)
)

// Detect counts suspected credentials in text by kind: the credential floor
// tclaude bundles share, AWS access key ids, and high-entropy tokens.
func Detect(text string) map[string]int {
	kinds := configbundle.CredentialKinds(text)
	kinds = mergeCounts(kinds, len(awsKey.FindAllStringIndex(text, -1)), KindAWSAccessKey)
	if n := countHighEntropy(text); n > 0 {
		kinds[KindHighEntropy] += n
	}
	return kinds
}

func mergeCounts(kinds map[string]int, n int, kind string) map[string]int {
	if n > 0 {
		kinds[kind] += n
	}
	return kinds
}

// countHighEntropy counts tokens that look like generated secrets: 24 to 256
// base64 characters mixing upper case, lower case and digits, with high
// character entropy. Hex digests, UUIDs, structural ids and words fall
// outside it; text the credential floor already matched is not counted
// twice.
func countHighEntropy(text string) int {
	n := 0
	for _, r := range run.FindAllString(text, -1) {
		if len(r) > maxToken {
			continue
		}
		for _, tok := range strings.Split(r, "/") {
			if len(tok) >= 24 && randomToken(tok) {
				n++
			}
		}
	}
	return n
}

func randomToken(tok string) bool {
	if structuralID.MatchString(tok) {
		return false
	}
	if len(configbundle.CredentialKinds(tok)) > 0 || awsKey.MatchString(tok) {
		return false
	}
	var upper, lower, digit, switches int
	last := byte(0)
	for i := 0; i < len(tok); i++ {
		class := byte('o')
		switch r := tok[i]; {
		case r >= 'A' && r <= 'Z':
			upper++
			class = 'A'
		case r >= 'a' && r <= 'z':
			lower++
			class = 'a'
		case r >= '0' && r <= '9':
			digit++
			class = '9'
		}
		if i > 0 && class != last {
			switches++
		}
		last = class
	}
	if upper < 2 || lower < 2 || digit < max(3, len(tok)/12) {
		return false
	}
	// Generated tokens switch between upper case, lower case and digits on
	// most characters (about 62% for base62); identifiers switch only at
	// word boundaries.
	if float64(switches)/float64(len(tok)-1) < 0.45 {
		return false
	}
	return entropy(tok) >= 4.0
}

// entropy is the Shannon entropy of s in bits per character.
func entropy(s string) float64 {
	counts := map[rune]int{}
	for _, r := range s {
		counts[r]++
	}
	h := 0.0
	for _, c := range counts {
		p := float64(c) / float64(len(s))
		h -= p * math.Log2(p)
	}
	return h
}

// Blocking reports whether findings include a credential kind, which an
// export refuses unless a human allows it. High-entropy findings alone are
// advisory.
func Blocking(findings []paf.Finding) bool {
	for _, f := range findings {
		if f.Kind != KindHighEntropy && f.Count > 0 {
			return true
		}
	}
	return false
}

// structuralKeys hold identifiers and opaque provider blobs, not prose. The
// credential floor still applies to them; the entropy detector does not.
var structuralKeys = map[string]bool{
	"id": true, "uuid": true, "parentUuid": true, "tool_use_id": true, "call_id": true,
	"sessionId": true, "session_id": true, "requestId": true, "request_id": true,
	"signature": true, "encrypted_content": true, "data": true, "sha256": true,
	"leafUuid": true, "messageId": true, "turn_id": true, "item_id": true,
}

// Collector accumulates findings across a PAF's content.
type Collector struct {
	byKind map[string]*paf.Finding
}

// NewCollector returns an empty collector.
func NewCollector() *Collector { return &Collector{byKind: map[string]*paf.Finding{}} }

// Text scans prose at location.
func (c *Collector) Text(text, location string) {
	c.add(Detect(text), location)
}

func (c *Collector) add(kinds map[string]int, location string) {
	for kind, count := range kinds {
		f := c.byKind[kind]
		if f == nil {
			f = &paf.Finding{Kind: kind}
			c.byKind[kind] = f
		}
		f.Count += count
		if len(f.Locations) < maxLocations && !slices.Contains(f.Locations, location) {
			f.Locations = append(f.Locations, location)
		}
	}
}

// Value walks a decoded JSON value, scanning every string at its path.
func (c *Collector) Value(v any, location string) {
	c.walk(v, location, "")
}

func (c *Collector) walk(v any, location, key string) {
	switch v := v.(type) {
	case string:
		kinds := Detect(v)
		if structuralKeys[key] {
			delete(kinds, KindHighEntropy)
		}
		c.add(kinds, location)
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			c.walk(v[k], location+"."+k, k)
		}
	case []any:
		for i, child := range v {
			c.walk(child, fmt.Sprintf("%s[%d]", location, i), key)
		}
	}
}

// JSON scans a JSON document, or its text when it is not valid JSON.
func (c *Collector) JSON(raw []byte, location string) {
	var v any
	if json.Unmarshal(raw, &v) == nil {
		c.Value(v, location)
		return
	}
	c.Text(string(raw), location)
}

// JSONLines scans a JSON Lines stream line by line, locating findings as
// "<name> line N". recordLimit bounds one line.
func (c *Collector) JSONLines(r io.Reader, name string, recordLimit int) error {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, min(64<<10, recordLimit)), recordLimit)
	line := 0
	for s.Scan() {
		line++
		c.JSON(s.Bytes(), fmt.Sprintf("%s line %d", name, line))
	}
	if err := s.Err(); err != nil {
		return fmt.Errorf("scan %s within %d byte records: %w", name, recordLimit, err)
	}
	return nil
}

// Findings returns the findings sorted by kind, never nil.
func (c *Collector) Findings() []paf.Finding {
	kinds := make([]string, 0, len(c.byKind))
	for k := range c.byKind {
		kinds = append(kinds, k)
	}
	slices.Sort(kinds)
	out := []paf.Finding{}
	for _, k := range kinds {
		out = append(out, *c.byKind[k])
	}
	return out
}

// ScanArchive scans everything a PAF carries that can hold text: group,
// agent and environment metadata, context files, handoffs, native blobs,
// text attachments and extension entries. It skips the manifest,
// signatures and binary attachments. Of privacy.json, which a producer
// writes about the content, it scans the free text a file can carry: the
// redaction details and extensions.
func ScanArchive(a *paf.Archive, recordLimit int) ([]paf.Finding, error) {
	c := NewCollector()
	if len(a.Manifest.Extensions) > 0 {
		raw, err := json.Marshal(a.Manifest.Extensions)
		if err != nil {
			return nil, err
		}
		c.JSON(raw, "manifest.json extensions")
	}
	if p := a.Privacy(); p != nil && (len(p.Redactions) > 0 || len(p.Extensions) > 0) {
		raw, err := json.Marshal(map[string]any{"redactions": p.Redactions, "extensions": p.Extensions})
		if err != nil {
			return nil, err
		}
		c.JSON(raw, paf.PrivacyPath)
	}
	for _, e := range a.Manifest.Entries {
		if e.Path == paf.PrivacyPath || strings.HasPrefix(e.Path, paf.SignaturesDir) {
			continue
		}
		if strings.HasPrefix(e.Path, paf.AttachmentsDir) && !textual(e.MediaType) {
			// The declared media type is the producer's claim; scan any
			// attachment whose bytes are text.
			if err := c.sniffedAttachment(a, e.Path); err != nil {
				return nil, err
			}
			continue
		}
		if err := c.entry(a, e, recordLimit); err != nil {
			return nil, err
		}
	}
	return c.Findings(), nil
}

// sniffLen is how much of an attachment decides whether it is text.
const sniffLen = 8 << 10

func (c *Collector) sniffedAttachment(a *paf.Archive, path string) error {
	rc, err := a.OpenEntry(path)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	br := bufio.NewReaderSize(rc, sniffLen)
	head, err := br.Peek(sniffLen)
	if err != nil && err != io.EOF && err != bufio.ErrBufferFull {
		return err
	}
	if !looksText(head) {
		return nil
	}
	return c.chunks(br, path)
}

// looksText reports bytes that read as UTF-8 text: no NUL bytes and valid
// UTF-8, allowing a rune cut off at the end of the sample.
func looksText(b []byte) bool {
	if bytes.IndexByte(b, 0) >= 0 {
		return false
	}
	for i := 0; i < 3 && len(b) > 0 && !utf8.Valid(b); i++ {
		b = b[:len(b)-1]
	}
	return utf8.Valid(b)
}

func (c *Collector) entry(a *paf.Archive, e paf.Entry, recordLimit int) error {
	rc, err := a.OpenEntry(e.Path)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	switch {
	case strings.HasSuffix(e.Path, ".jsonl") || e.MediaType == "application/jsonl" || e.MediaType == "application/x-ndjson":
		return c.JSONLines(rc, e.Path, recordLimit)
	case strings.HasSuffix(e.Path, ".json") || e.MediaType == "application/json":
		raw, err := io.ReadAll(rc)
		if err != nil {
			return err
		}
		c.JSON(raw, e.Path)
		return nil
	default:
		return c.chunks(rc, e.Path)
	}
}

// chunkLen bounds one plain-text scan window; chunkOverlap carries the tail
// of each window into the next, so a token across the boundary is seen.
const (
	chunkLen     = 1 << 20
	chunkOverlap = 512
)

// chunks scans plain text in bounded windows, so no line length can fail
// a scan. Locations name the line a window starts on.
func (c *Collector) chunks(r io.Reader, name string) error {
	buf := make([]byte, chunkLen+chunkOverlap)
	carry, line := 0, 1
	for {
		n, err := io.ReadFull(r, buf[carry:])
		if n > 0 || carry > 0 {
			window := buf[:carry+n]
			end := len(window)
			if err == nil {
				// Cut at the last newline so a window holds whole lines
				// where it can.
				if i := bytes.LastIndexByte(window[carry:], '\n'); i >= 0 {
					end = carry + i + 1
				}
			}
			c.Text(string(window[:end]), fmt.Sprintf("%s line %d", name, line))
			line += bytes.Count(window[:end], []byte{'\n'})
			rest := window[end:]
			if len(rest) == 0 && end == len(window) && err == nil {
				// No newline in the window: keep an overlap so a token
				// across the cut is still seen. It may be counted twice.
				rest = window[max(0, end-chunkOverlap):]
			}
			carry = copy(buf, rest)
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("scan %s: %w", name, err)
		}
	}
}

func textual(mediaType string) bool {
	mt, _, _ := strings.Cut(mediaType, ";")
	mt = strings.TrimSpace(strings.ToLower(mt))
	return strings.HasPrefix(mt, "text/") || mt == "application/json" || mt == "application/jsonl" || mt == "application/x-ndjson" || mt == "application/xml" || mt == "application/yaml" || strings.HasSuffix(mt, "+json") || strings.HasSuffix(mt, "+xml")
}
