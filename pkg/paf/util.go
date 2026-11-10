package paf

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Limits bounds what a reader accepts and a writer produces.
type Limits struct {
	// MaxArchiveBytes bounds the whole archive and the sum of uncompressed
	// entries.
	MaxArchiveBytes int64
	// MaxMetadataBytes bounds each JSON metadata entry.
	MaxMetadataBytes int64
	// MaxRecordBytes bounds one JSON Lines record.
	MaxRecordBytes int
	// MaxEntries bounds the number of archive entries.
	MaxEntries int
}

// DefaultLimits are tclaude's defaults, matching agent bundle transfer.
func DefaultLimits() Limits {
	return Limits{
		MaxArchiveBytes:  2 << 30,
		MaxMetadataBytes: 1 << 20,
		MaxRecordBytes:   256 << 20,
		MaxEntries:       100_000,
	}
}

func (l Limits) orDefault() Limits {
	d := DefaultLimits()
	if l.MaxArchiveBytes <= 0 {
		l.MaxArchiveBytes = d.MaxArchiveBytes
	}
	if l.MaxMetadataBytes <= 0 {
		l.MaxMetadataBytes = d.MaxMetadataBytes
	}
	if l.MaxRecordBytes <= 0 {
		l.MaxRecordBytes = d.MaxRecordBytes
	}
	if l.MaxEntries <= 0 {
		l.MaxEntries = d.MaxEntries
	}
	return l
}

var (
	segmentRE     = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	agentKeyRE    = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
	extensionIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*(\.[a-z0-9][a-z0-9-]*)*$`)
	versionRE     = regexp.MustCompile(`^([0-9]+)\.([0-9]+)$`)
	sha256RE      = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ValidEntryPath reports whether name is an acceptable archive entry name.
func ValidEntryPath(name string) bool {
	if name == "" || len(name) > 255 {
		return false
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "." || seg == ".." || !segmentRE.MatchString(seg) {
			return false
		}
	}
	return true
}

// ValidAgentKey reports whether k is an acceptable agent key.
func ValidAgentKey(k string) bool { return agentKeyRE.MatchString(k) }

// documentedExtensions are the single-label extension ids the spec reserves
// and documents. Writers mint no other single-label id.
var documentedExtensions = map[string]bool{ExtTclaude: true}

// ValidExtensionID reports whether a writer may use id: a reverse-DNS id,
// or a single-label id documented in the spec.
func ValidExtensionID(id string) bool {
	return wellFormedExtensionID(id) && (strings.Contains(id, ".") || documentedExtensions[id])
}

// wellFormedExtensionID is what a reader requires of an extension id. An
// undocumented single-label id, say one a later spec documents, is an
// unknown extension like any other: ignored, and refused only when the
// manifest lists it in extensions_required.
func wellFormedExtensionID(id string) bool {
	return len(id) <= 128 && extensionIDRE.MatchString(id)
}

func validSHA256(s string) bool { return sha256RE.MatchString(s) }

// ParseVersion splits a MAJOR.MINOR version.
func ParseVersion(v string) (major, minor int, err error) {
	m := versionRE.FindStringSubmatch(v)
	if m == nil {
		return 0, 0, fmt.Errorf("invalid version %q", v)
	}
	major, err = strconv.Atoi(m[1])
	if err == nil {
		minor, err = strconv.Atoi(m[2])
	}
	return major, minor, err
}

func hexSHA256(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// ContentDigest is the digest consent records bind to: SHA-256 over
// "<sha256>  <path>\n" for every entry except privacy.json, sorted by path in
// byte order.
func ContentDigest(entries []Entry) string {
	rows := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if e.Path != PrivacyPath {
			rows = append(rows, e)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Path < rows[j].Path })
	var b bytes.Buffer
	for _, e := range rows {
		fmt.Fprintf(&b, "%s  %s\n", e.SHA256, e.Path)
	}
	return hexSHA256(b.Bytes())
}

// SectionDigest is ContentDigest over the entries under one prefix: what a
// workflow item or a link names. It covers what the section holds, not the
// attachments an agent's context points at, which the manifest pins anyway.
func SectionDigest(entries []Entry, prefix string) string {
	rows := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if strings.HasPrefix(e.Path, prefix) {
			rows = append(rows, e)
		}
	}
	return ContentDigest(rows)
}

// WorkflowPath returns the entry path of a file in a workflow's directory.
func WorkflowPath(key, rel string) string { return WorkflowsDir + key + "/" + rel }

// checkNoDuplicateKeys refuses JSON with a repeated object key at any depth,
// so every reader parses the same signed content.
func checkNoDuplicateKeys(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var walk func() error
	walk = func() error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch tok {
		case json.Delim('{'):
			seen := map[string]bool{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return err
				}
				k, _ := kt.(string)
				if seen[k] {
					return fmt.Errorf("duplicate key %q", k)
				}
				seen[k] = true
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		case json.Delim('['):
			for dec.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		}
		return nil
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data after JSON value")
	}
	return nil
}

// DecodeStrict decodes one JSON document after refusing duplicate object
// keys at any depth, so every reader parses the same signed content.
func DecodeStrict(b []byte, v any) error { return decodeStrict(b, v) }

// decodeStrict decodes one JSON document after the duplicate-key check.
func decodeStrict(b []byte, v any) error {
	if err := checkNoDuplicateKeys(b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// marshalJSON is the canonical-enough encoding used for written metadata.
func marshalJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
