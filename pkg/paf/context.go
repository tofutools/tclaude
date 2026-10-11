package paf

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// Context JSON Lines line types.
const (
	LineHeader   = "header"
	LineEntry    = "entry"
	LineBoundary = "boundary"
)

// Block kinds the spec names. The set is open.
const (
	BlockText       = "text"
	BlockThinking   = "thinking"
	BlockToolCall   = "tool_call"
	BlockToolResult = "tool_result"
	BlockMedia      = "media"
	BlockSummary    = "summary"
)

// Line is one line of a context JSON Lines file. Type selects which fields
// are meaningful; a line of an unknown type keeps its raw JSON in Raw.
type Line struct {
	Type string `json:"type"`

	// header
	Scope  string  `json:"scope,omitempty"`
	Source *Source `json:"source,omitempty"`

	// entry
	ID     string    `json:"id,omitempty"`
	Role   string    `json:"role,omitempty"`
	Origin string    `json:"origin,omitempty"`
	Model  string    `json:"model,omitempty"`
	Author string    `json:"author,omitempty"`
	Blocks []Block   `json:"blocks,omitempty"`
	Time   time.Time `json:"time,omitzero"`

	// boundary
	Kind string `json:"kind,omitempty"`

	Extensions Extensions `json:"extensions,omitempty"`

	// Raw is the original line, kept for lines of an unknown type.
	Raw json.RawMessage `json:"-"`
}

// Source is a context header's source description.
type Source struct {
	Harness    string     `json:"harness"`
	Format     string     `json:"format,omitempty"`
	ConvID     string     `json:"conv_id,omitempty"`
	Model      string     `json:"model,omitempty"`
	Cwd        string     `json:"cwd,omitempty"`
	Title      string     `json:"title,omitempty"`
	Extensions Extensions `json:"extensions,omitempty"`
}

// Block is one piece of an entry.
type Block struct {
	Kind       string          `json:"kind"`
	Text       string          `json:"text,omitempty"`
	Opaque     *Opaque         `json:"opaque,omitempty"`
	CallID     string          `json:"call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
	Input      json.RawMessage `json:"input,omitempty"`
	Freeform   bool            `json:"freeform,omitempty"`
	IsError    bool            `json:"is_error,omitempty"`
	Media      []Media         `json:"media,omitempty"`
	Extensions Extensions      `json:"extensions,omitempty"`
}

// Known reports whether the block kind is one this package understands.
// Readers render other kinds as unsupported content, never drop them.
func (b Block) Known() bool {
	switch b.Kind {
	case BlockText, BlockThinking, BlockToolCall, BlockToolResult, BlockMedia, BlockSummary:
		return true
	}
	return false
}

// Opaque is provider-bound reasoning state.
type Opaque struct {
	Vendor string `json:"vendor"`
	Data   string `json:"data"`
}

// Media is an image or document. Exactly one of Attachment, URL or Path is set.
type Media struct {
	MediaType  string     `json:"media_type,omitempty"`
	Attachment string     `json:"attachment,omitempty"`
	URL        string     `json:"url,omitempty"`
	Path       string     `json:"path,omitempty"`
	Name       string     `json:"name,omitempty"`
	Extensions Extensions `json:"extensions,omitempty"`
}

// AttachmentPrefix prefixes Media.Attachment references.
const AttachmentPrefix = "sha256:"

// AttachmentRef returns the media reference for an attachment hash.
func AttachmentRef(sha256Hex string) string { return AttachmentPrefix + sha256Hex }

// AttachmentPath returns the archive entry of an attachment reference.
func AttachmentPath(ref string) (string, bool) {
	h, ok := cutPrefix(ref, AttachmentPrefix)
	if !ok || !validSHA256(h) {
		return "", false
	}
	return AttachmentsDir + h, true
}

func cutPrefix(s, p string) (string, bool) {
	if len(s) >= len(p) && s[:len(p)] == p {
		return s[len(p):], true
	}
	return "", false
}

// UnmarshalJSON keeps the raw line so an unknown type survives a round trip.
func (l *Line) UnmarshalJSON(b []byte) error {
	type plain Line
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*l = Line(p)
	l.Raw = append(json.RawMessage(nil), b...)
	return nil
}

// MarshalJSON writes an unknown line type back exactly as it was read.
func (l Line) MarshalJSON() ([]byte, error) {
	if l.Raw != nil && !l.KnownType() {
		return l.Raw, nil
	}
	type plain Line
	return json.Marshal(plain(l))
}

// KnownType reports whether the line type is one this package understands.
func (l Line) KnownType() bool {
	switch l.Type {
	case LineHeader, LineEntry, LineBoundary:
		return true
	}
	return false
}

// ContextWriter writes a context JSON Lines file.
type ContextWriter struct {
	enc *json.Encoder
}

// NewContextWriter writes the header line and returns a writer for entries.
func NewContextWriter(w io.Writer, scope string, src Source) (*ContextWriter, error) {
	cw := &ContextWriter{enc: json.NewEncoder(w)}
	cw.enc.SetEscapeHTML(false)
	if err := cw.enc.Encode(Line{Type: LineHeader, Scope: scope, Source: &src}); err != nil {
		return nil, err
	}
	return cw, nil
}

// Write writes one entry or boundary line.
func (cw *ContextWriter) Write(l Line) error {
	if l.Type == LineHeader {
		return errors.New("paf: context header written twice")
	}
	return cw.enc.Encode(l)
}

// ContextReader reads a context JSON Lines file.
type ContextReader struct {
	sc     *bufio.Scanner
	line   int
	Header Line
}

// NewContextReader reads and checks the header line. maxRecord bounds a
// single line.
func NewContextReader(r io.Reader, maxRecord int) (*ContextReader, error) {
	if maxRecord <= 0 {
		maxRecord = DefaultLimits().MaxRecordBytes
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, min(64<<10, maxRecord)), maxRecord)
	cr := &ContextReader{sc: sc}
	h, err := cr.next()
	if err == io.EOF {
		return nil, errors.New("paf: context file is empty")
	}
	if err != nil {
		return nil, err
	}
	if h.Type != LineHeader || h.Source == nil {
		return nil, errors.New("paf: context file does not start with a header line")
	}
	cr.Header = h
	return cr, nil
}

// Next returns the next line, or io.EOF.
func (cr *ContextReader) Next() (Line, error) {
	l, err := cr.next()
	if err == nil && l.Type == LineHeader {
		return Line{}, fmt.Errorf("paf: context line %d: second header", cr.line)
	}
	return l, err
}

func (cr *ContextReader) next() (Line, error) {
	for cr.sc.Scan() {
		cr.line++
		b := bytes.TrimSpace(cr.sc.Bytes())
		if len(b) == 0 {
			continue
		}
		if err := checkNoDuplicateKeys(b); err != nil {
			return Line{}, fmt.Errorf("paf: context line %d: %w", cr.line, err)
		}
		var l Line
		if err := json.Unmarshal(b, &l); err != nil {
			return Line{}, fmt.Errorf("paf: context line %d: %w", cr.line, err)
		}
		if err := l.check(); err != nil {
			return Line{}, fmt.Errorf("paf: context line %d: %w", cr.line, err)
		}
		return l, nil
	}
	if err := cr.sc.Err(); err != nil {
		return Line{}, fmt.Errorf("paf: context line %d: %w", cr.line+1, err)
	}
	return Line{}, io.EOF
}

// check enforces the closed parts of the line schema. Unknown line types
// and block kinds pass; they are open sets.
func (l Line) check() error {
	switch l.Type {
	case "":
		return errors.New("line has no type")
	case LineHeader:
		if l.Scope != ScopeEffective && l.Scope != ScopeFull {
			return fmt.Errorf("unknown scope %q", l.Scope)
		}
		if l.Source == nil || l.Source.Harness == "" {
			return errors.New("header has no source harness")
		}
	case LineEntry:
		if l.Role != "user" && l.Role != "assistant" {
			return fmt.Errorf("unknown role %q", l.Role)
		}
		if l.Origin != "human" && l.Origin != "agent" && l.Origin != "tool" {
			return fmt.Errorf("unknown origin %q", l.Origin)
		}
		if len(l.Blocks) == 0 {
			return errors.New("entry has no blocks")
		}
		for i, b := range l.Blocks {
			if err := b.check(); err != nil {
				return fmt.Errorf("block %d: %w", i, err)
			}
		}
	case LineBoundary:
		if l.Kind == "" {
			return errors.New("boundary has no kind")
		}
	}
	return nil
}

func (b Block) check() error {
	switch b.Kind {
	case "":
		return errors.New("block has no kind")
	case BlockToolCall:
		if b.CallID == "" || b.Name == "" {
			return errors.New("tool_call needs call_id and name")
		}
	case BlockToolResult:
		if b.CallID == "" {
			return errors.New("tool_result needs call_id")
		}
	case BlockMedia:
		if len(b.Media) == 0 {
			return errors.New("media block has no media")
		}
	}
	for i, m := range b.Media {
		n := 0
		for _, s := range []string{m.Attachment, m.URL, m.Path} {
			if s != "" {
				n++
			}
		}
		if n != 1 {
			return fmt.Errorf("media %d needs exactly one of attachment, url, path", i)
		}
		if m.Attachment != "" {
			if _, ok := AttachmentPath(m.Attachment); !ok {
				return fmt.Errorf("media %d: invalid attachment reference", i)
			}
		}
	}
	return nil
}
