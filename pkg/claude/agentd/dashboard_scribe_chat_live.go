package agentd

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
)

// The scribe chat's live tail (tcl-fbxfwa). The Stop hook records a scribe's
// final message only when its turn ends, so a long turn used to show nothing
// but "scribe is working…". While a turn is open, the chat read also decodes
// the scribe's own harness transcript and returns what the scribe has said and
// done so far in this turn: its text, plus a one-line summary of each tool
// call. The dock re-reads it on every poll and replaces it wholesale; the
// recorded reply supersedes it once the turn ends.
//
// It is served only through the scribe chat read, so it inherits that read's
// scribe-only gate and never becomes a general transcript surface. A harness
// whose history cannot be decoded degrades to the old behavior, with the
// reason in live.unavailable.
//
// The dock polls about once a second while there is progress to show, so a
// decode is reused until the transcript changes: a JSONL transcript the conv
// index locates is keyed by its size and mtime, so an idle poll costs a stat;
// any other is decoded at most every scribeLiveReuse.

const (
	scribeLiveMaxItems     = 40
	scribeLiveMaxTextBytes = 4 << 10
	scribeLiveMaxToolBytes = 160
	scribeLiveMaxBytes     = 24 << 10
	// scribeLiveKeep bounds the candidates kept per decode: more than one
	// turn's worth, so the since-filter still has the whole open turn.
	scribeLiveKeep = 4 * scribeLiveMaxItems
	// scribeLiveReuse is how long a decode without a file identity, or one
	// that failed, is reused. Finding a transcript can mean scanning a
	// harness's whole store.
	scribeLiveReuse = 5 * time.Second
	// scribeLiveEvict drops cache entries no dock has read for this long.
	scribeLiveEvict = time.Minute
)

type scribeLiveItem struct {
	Kind string    `json:"kind"` // "text" | "tool"
	Text string    `json:"text"`
	At   time.Time `json:"at,omitzero"`
}

type scribeLive struct {
	Items       []scribeLiveItem `json:"items"`
	Truncated   bool             `json:"truncated,omitempty"`
	Unavailable string           `json:"unavailable,omitempty"`
}

// scribeLiveCandidate is an item of the decoded transcript, before the
// open-turn filter. afterHuman marks an item that follows the transcript's
// last operator message; it decides for entries without a timestamp.
type scribeLiveCandidate struct {
	item       scribeLiveItem
	afterHuman bool
}

type scribeLiveDecode struct {
	mu       sync.Mutex
	key      string // file identity; "" when the transcript has none
	at       time.Time
	read     time.Time
	cands    []scribeLiveCandidate
	dropped  bool
	reason   string
	resolved bool
}

var (
	scribeLiveMu    sync.Mutex
	scribeLiveCache = map[string]*scribeLiveDecode{}
)

func unavailableScribeLive(reason string) *scribeLive {
	return &scribeLive{Items: []scribeLiveItem{}, Unavailable: reason}
}

// scribeLiveTail returns what agentID's current conversation has produced
// since the scribe's last recorded reply (since, zero when none).
func scribeLiveTail(agentID string, since time.Time) *scribeLive {
	convID, err := db.CurrentConvForAgent(agentID)
	if err != nil || convID == "" {
		return unavailableScribeLive("the scribe has no conversation yet")
	}
	h := harnessForConv(convID)
	if !h.SupportsTranscript() {
		return unavailableScribeLive("the " + h.Name + " harness cannot show a turn before it ends")
	}
	if h.Name == harness.OpenCodeName {
		// OpenCode's history is read through an `opencode export`
		// subprocess, far too heavy to run on every poll.
		return unavailableScribeLive("OpenCode's progress shows when the turn ends")
	}
	d := scribeLiveEntry(convID)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.read = time.Now()
	path, key := scribeLiveFileKey(convID, h)
	fresh := d.resolved && ((key != "" && key == d.key) || (key == "" && d.key == "" && time.Since(d.at) < scribeLiveReuse))
	if d.reason != "" && key != "" && key == d.key {
		fresh = time.Since(d.at) < scribeLiveReuse // a failed decode is retried, but not on every poll
	}
	if !fresh {
		d.key, d.at, d.resolved = key, time.Now(), true
		d.cands, d.dropped, d.reason = nil, false, ""
		t, err := decodeScribeLive(h, convID, path)
		if err != nil {
			d.reason = "the scribe's transcript cannot be read yet"
		} else {
			d.cands, d.dropped = scribeLiveCandidates(t)
		}
	}
	if d.reason != "" {
		return unavailableScribeLive(d.reason)
	}
	return scribeLiveSince(d.cands, d.dropped, since)
}

func scribeLiveEntry(convID string) *scribeLiveDecode {
	scribeLiveMu.Lock()
	defer scribeLiveMu.Unlock()
	for id, d := range scribeLiveCache {
		if d.mu.TryLock() {
			stale := time.Since(d.read) > scribeLiveEvict
			d.mu.Unlock()
			if stale {
				delete(scribeLiveCache, id)
			}
		}
	}
	d, ok := scribeLiveCache[convID]
	if !ok {
		d = &scribeLiveDecode{}
		scribeLiveCache[convID] = d
	}
	return d
}

// scribeLiveFileKey identifies a JSONL transcript the conv index locates by
// path, size and mtime. Other transcripts have no key.
func scribeLiveFileKey(convID string, h *harness.Harness) (string, string) {
	if h.Name != harness.DefaultName && h.Name != harness.CodexName {
		return "", ""
	}
	row, err := db.GetConvIndex(convID)
	if err != nil || row == nil || row.FullPath == "" || (row.Harness != "" && row.Harness != h.Name) {
		return "", ""
	}
	info, err := os.Stat(row.FullPath)
	if err != nil {
		return "", ""
	}
	return row.FullPath, row.FullPath + "|" + strconv.FormatInt(info.Size(), 10) + "|" + strconv.FormatInt(info.ModTime().UnixNano(), 10)
}

// decodeScribeLive decodes the scribe's history. A JSONL transcript is read
// up to its last complete line: the harness may be part-way through
// appending a record, and a torn line must not fail the whole read.
func decodeScribeLive(h *harness.Harness, convID, path string) (*transcript.Transcript, error) {
	if path == "" {
		cwd := ""
		if rows, err := db.FindSessionsByConvID(convID); err == nil {
			for _, s := range rows {
				if s.Cwd != "" {
					cwd = s.Cwd
					break
				}
			}
		}
		return harness.DecodeHistory(h, convID, cwd)
	}
	streaming, ok := h.History.(harness.StreamingHistoryTransfer)
	if !ok {
		return nil, errors.New("history cannot be streamed")
	}
	r, err := streaming.Open(convID, "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return h.Transcript.Decode(&completeLinesReader{r: bufio.NewReader(r)}, convID)
}

// scribeLiveCandidates lists the agent's text and tool calls in t, newest
// scribeLiveKeep of them, and whether older ones were left out.
func scribeLiveCandidates(t *transcript.Transcript) ([]scribeLiveCandidate, bool) {
	if t == nil {
		return nil, false
	}
	lastHuman := -1
	for i, e := range t.Entries {
		if e.Role == transcript.RoleUser && e.Origin == transcript.OriginHuman {
			lastHuman = i
		}
	}
	var out []scribeLiveCandidate
	for i, e := range t.Entries {
		if e.Role != transcript.RoleAssistant {
			continue
		}
		after := lastHuman >= 0 && i > lastHuman
		for _, b := range e.Blocks {
			switch b.Kind {
			case transcript.BlockText:
				if text := strings.TrimSpace(b.Text); text != "" {
					out = append(out, scribeLiveCandidate{scribeLiveItem{Kind: "text", Text: clipUTF8(text, scribeLiveMaxTextBytes), At: e.Time}, after})
				}
			case transcript.BlockToolCall:
				out = append(out, scribeLiveCandidate{scribeLiveItem{Kind: "tool", Text: scribeToolSummary(b), At: e.Time}, after})
			}
		}
	}
	if n := len(out); n > scribeLiveKeep {
		return out[n-scribeLiveKeep:], true
	}
	return out, false
}

// scribeLiveSince keeps the open turn's items. An item is part of it when it
// is newer than the last recorded reply; one without a timestamp counts when
// it follows the last operator message in the transcript, which is the best a
// format without times allows. The newest items win when the tail is over its
// bounds.
func scribeLiveSince(cands []scribeLiveCandidate, dropped bool, since time.Time) *scribeLive {
	out := &scribeLive{Items: []scribeLiveItem{}}
	first := true
	for i, c := range cands {
		if c.item.At.IsZero() {
			if !c.afterHuman {
				continue
			}
		} else if !since.IsZero() && !c.item.At.After(since) {
			continue
		}
		if first {
			// Older candidates the decode left out may have belonged to
			// this turn too only when the first kept one does.
			out.Truncated, first = dropped && i == 0, false
		}
		out.Items = append(out.Items, c.item)
	}
	if n := len(out.Items); n > scribeLiveMaxItems {
		out.Items, out.Truncated = out.Items[n-scribeLiveMaxItems:], true
	}
	total := 0
	for i := len(out.Items) - 1; i >= 0; i-- {
		total += len(out.Items[i].Text)
		if total > scribeLiveMaxBytes {
			out.Items, out.Truncated = out.Items[i+1:], true
			break
		}
	}
	return out
}

// scribeToolSummaryKeys are the arguments that say what a tool call is about,
// most telling first. Anything else is left out: the summary is a progress
// hint, not the call.
var scribeToolSummaryKeys = []string{"description", "command", "file_path", "path", "pattern", "query", "url", "prompt"}

func scribeToolSummary(b transcript.Block) string {
	name := strings.TrimSpace(b.Name)
	if name == "" {
		name = "tool"
	}
	detail := ""
	if b.Freeform {
		detail = b.Text
	} else if len(b.Input) > 0 {
		var args map[string]any
		if json.Unmarshal(b.Input, &args) == nil {
			for _, key := range scribeToolSummaryKeys {
				if s, ok := args[key].(string); ok && strings.TrimSpace(s) != "" {
					detail = s
					break
				}
			}
		}
	}
	detail = strings.Join(strings.Fields(detail), " ")
	if detail == "" {
		return clipUTF8(name, scribeLiveMaxToolBytes)
	}
	return clipUTF8(name+": "+detail, scribeLiveMaxToolBytes)
}

func clipUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const ellipsis = "…"
	cut := max - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}
