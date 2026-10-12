package harness

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// pi's session file, version 3, read from pi-coding-agent's
// dist/core/session-manager at PiMinimumVersion and observed on real files.
//
// The first parsed line is the header:
//
//	{"type":"session","version":3,"id":"<id>","timestamp":"<ISO>","cwd":"<abs>","parentSession":"<path>"?}
//
// Every later line is an entry {"type","id","parentId","timestamp",…}. The
// entries form a tree through parentId: a /tree navigation, an aborted retry
// or an edit leaves an abandoned branch in the file, and the session's current
// branch is the path from the LAST entry in the file back to the root
// (SessionManager._buildIndex sets the leaf to the last entry read). Malformed
// lines are skipped, as pi skips them.
//
// What the model sees is that path projected by buildContextEntries: when the
// path holds a compaction, only the newest one counts, and context is its
// summary, then the path from its firstKeptEntryId up to it (system messages
// excepted), then everything after it. A context_edit entry replaces (or,
// with a null replacement, hides) an earlier entry's content.

const (
	piSessionHeaderType = "session"
	piSessionVersion    = 3
)

// piEntry is one session-file line. Fields are the union of the entry types
// tclaude reads; each type uses its own subset.
type piEntry struct {
	Type      string  `json:"type"`
	ID        string  `json:"id"`
	ParentID  *string `json:"parentId"`
	Timestamp string  `json:"timestamp"`

	// session header
	Version       int    `json:"version"`
	Cwd           string `json:"cwd"`
	ParentSession string `json:"parentSession,omitempty"`

	// message
	Message *piMessage `json:"message"`

	// session_info. A pointer: an explicit clear is a session_info without
	// a name.
	Name *string `json:"name"`

	// compaction and branch_summary
	Summary          string `json:"summary"`
	FirstKeptEntryID string `json:"firstKeptEntryId"`
	TokensBefore     int64  `json:"tokensBefore"`

	// model_change
	Provider string `json:"provider"`
	ModelID  string `json:"modelId"`

	// usage entries, and the summarizing call of a compaction or branch
	// summary
	Model string   `json:"model"`
	Usage *piUsage `json:"usage"`

	// custom_message
	CustomType string          `json:"customType"`
	Content    json.RawMessage `json:"content"`

	// context_edit
	TargetID    string          `json:"targetId"`
	Replacement json.RawMessage `json:"replacement"`
}

func (e *piEntry) parent() string {
	if e.ParentID == nil {
		return ""
	}
	return *e.ParentID
}

// piMessage is an AgentMessage: pi-ai's system, user, assistant and
// toolResult messages, plus pi-coding-agent's bashExecution, custom,
// branchSummary and compactionSummary roles.
type piMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`

	// assistant
	Provider     string   `json:"provider"`
	Model        string   `json:"model"`
	Usage        *piUsage `json:"usage"`
	StopReason   string   `json:"stopReason"`
	ErrorMessage string   `json:"errorMessage"`

	// toolResult
	ToolCallID string `json:"toolCallId"`
	ToolName   string `json:"toolName"`
	IsError    bool   `json:"isError"`

	// bashExecution (a user's `!command`)
	Command            string `json:"command"`
	Output             string `json:"output"`
	ExitCode           *int   `json:"exitCode"`
	Cancelled          bool   `json:"cancelled"`
	Truncated          bool   `json:"truncated"`
	FullOutputPath     string `json:"fullOutputPath"`
	ExcludeFromContext bool   `json:"excludeFromContext"`

	// custom
	CustomType string `json:"customType"`

	Timestamp int64 `json:"timestamp"`
}

// piBlock is one content block: text, thinking, toolCall or image.
type piBlock struct {
	Type              string          `json:"type"`
	Text              string          `json:"text"`
	Thinking          string          `json:"thinking"`
	ThinkingSignature string          `json:"thinkingSignature"`
	Redacted          bool            `json:"redacted"`
	Data              string          `json:"data"`
	MimeType          string          `json:"mimeType"`
	ID                string          `json:"id"`
	Name              string          `json:"name"`
	Arguments         json.RawMessage `json:"arguments"`
	ThoughtSignature  string          `json:"thoughtSignature"`
}

// piUsage is pi-ai's Usage. Cost is pi's own USD figure from its catalog.
type piUsage struct {
	Input       int64 `json:"input"`
	Output      int64 `json:"output"`
	CacheRead   int64 `json:"cacheRead"`
	CacheWrite  int64 `json:"cacheWrite"`
	TotalTokens int64 `json:"totalTokens"`
	Cost        struct {
		Total float64 `json:"total"`
	} `json:"cost"`
}

// piBlocks decodes message content: a plain string is one text block.
func piBlocks(raw json.RawMessage) []piBlock {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil
		}
		return []piBlock{{Type: "text", Text: s}}
	}
	var blocks []piBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	return blocks
}

// piContentText joins a content's text blocks, as pi's extractTextContent.
func piContentText(raw json.RawMessage) string {
	var parts []string
	for _, b := range piBlocks(raw) {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, " ")
}

// piSession is a parsed session file.
type piSession struct {
	Header  piEntry
	Entries []*piEntry
	byID    map[string]*piEntry
}

var errPiNoHeader = errors.New("pi session has no session header")

// add folds one line. The first parsed line must be the header; a malformed
// line is skipped, as pi skips it.
func (s *piSession) add(line []byte, found *bool) error {
	var e piEntry
	if json.Unmarshal(line, &e) != nil {
		return nil
	}
	if !*found {
		if e.Type != piSessionHeaderType || e.ID == "" {
			return errPiNoHeader
		}
		s.Header, *found = e, true
		return nil
	}
	if e.Type != piSessionHeaderType && e.ID != "" {
		s.Entries = append(s.Entries, &e)
		s.byID[e.ID] = &e
	}
	return nil
}

// readPiSession parses a session file for a listing: a line over the
// Copilot reader's bound is dropped rather than failing the scan, and a final
// partial line from a live writer is tolerated.
func readPiSession(r io.Reader) (*piSession, error) {
	s := &piSession{byID: map[string]*piEntry{}}
	found := false
	reader := bufio.NewReaderSize(r, 64<<10)
	for {
		line, readErr := readCopilotEventLine(reader)
		if len(line) > 0 {
			if err := s.add(line, &found); err != nil {
				return nil, err
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return nil, readErr
			}
			break
		}
	}
	if !found {
		return nil, errPiNoHeader
	}
	return s, nil
}

// decodePiSession parses a whole history for a transfer or a translation,
// where nothing may be silently dropped: a record over the history record
// bound is an error.
func decodePiSession(r io.Reader) (*piSession, error) {
	s := &piSession{byID: map[string]*piEntry{}}
	found := false
	if err := scanHistoryRecords(r, func(_ int, line []byte) error {
		return s.add(line, &found)
	}); err != nil {
		return nil, err
	}
	if !found {
		return nil, errPiNoHeader
	}
	return s, nil
}

// Branch returns the current branch, root first: the path from the last
// entry back to the root.
func (s *piSession) Branch() []*piEntry {
	if len(s.Entries) == 0 {
		return nil
	}
	var path []*piEntry
	seen := map[string]bool{}
	for e := s.Entries[len(s.Entries)-1]; e != nil && !seen[e.ID]; e = s.byID[e.parent()] {
		seen[e.ID] = true
		path = append(path, e)
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path
}

// Context returns the entries that make up the model's context, as pi's
// buildContextEntries: with a compaction on the branch, the newest one first,
// then the kept entries from its firstKeptEntryId (system messages
// excepted), then everything after it. compacted counts the conversation
// messages the compaction replaced.
func (s *piSession) Context() (entries []*piEntry, compacted int) {
	path := s.Branch()
	at := -1
	for i, e := range path {
		if e.Type == "compaction" {
			at = i
		}
	}
	if at < 0 {
		return path, 0
	}
	compaction := path[at]
	entries = []*piEntry{compaction}
	kept := false
	for _, e := range path[:at] {
		if e.ID == compaction.FirstKeptEntryID {
			kept = true
		}
		if kept {
			if e.Type != "message" || e.Message == nil || e.Message.Role != "system" {
				entries = append(entries, e)
			}
		} else if e.Type == "message" && e.Message != nil && e.Message.Role != "system" {
			compacted++
		}
	}
	return append(entries, path[at+1:]...), compacted
}

// Name returns the session's display name: the newest session_info entry's,
// where an explicit clear counts.
func (s *piSession) Name() string {
	name := ""
	for _, e := range s.Entries {
		if e.Type == "session_info" {
			name = ""
			if e.Name != nil {
				name = strings.TrimSpace(*e.Name)
			}
		}
	}
	return name
}
