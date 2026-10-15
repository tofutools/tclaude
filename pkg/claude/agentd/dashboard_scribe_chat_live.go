package agentd

import (
	"encoding/json"
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
// the tail of the scribe's own harness transcript and returns what the scribe
// has said and done so far in this turn: its text, plus a one-line summary of
// each tool call. The dock re-reads it on every poll and replaces it wholesale;
// the recorded reply supersedes it once the turn ends.
//
// It is served only through the scribe chat read, so it inherits that read's
// scribe-only gate and never becomes a general transcript surface. A harness
// whose history cannot be decoded degrades to the old behavior, with the
// reason in live.unavailable.

const (
	scribeLiveMaxItems     = 40
	scribeLiveMaxTextBytes = 4 << 10
	scribeLiveMaxToolBytes = 160
	scribeLiveMaxBytes     = 24 << 10
	// A transcript that could not be found is not looked for again for this
	// long: finding it can mean scanning a harness's whole store.
	scribeLiveMissTTL = 10 * time.Second
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

var (
	scribeLiveMissMu sync.Mutex
	scribeLiveMisses = map[string]scribeLiveMiss{}
)

type scribeLiveMiss struct {
	reason string
	at     time.Time
}

// scribeLiveTail returns what agentID's current conversation has produced
// since the scribe's last recorded reply (since, zero when none).
func scribeLiveTail(agentID string, since time.Time) *scribeLive {
	convID, err := db.CurrentConvForAgent(agentID)
	if err != nil || convID == "" {
		return &scribeLive{Items: []scribeLiveItem{}, Unavailable: "the scribe has no conversation yet"}
	}
	h := harnessForConv(convID)
	if !h.SupportsTranscript() {
		return &scribeLive{Items: []scribeLiveItem{}, Unavailable: "the " + h.Name + " harness cannot show a turn before it ends"}
	}
	if reason, ok := scribeLiveRecentMiss(convID); ok {
		return &scribeLive{Items: []scribeLiveItem{}, Unavailable: reason}
	}
	cwd := ""
	if rows, err := db.FindSessionsByConvID(convID); err == nil {
		for _, s := range rows {
			if s.Cwd != "" {
				cwd = s.Cwd
				break
			}
		}
	}
	t, err := harness.DecodeHistory(h, convID, cwd)
	if err != nil {
		reason := "the scribe's transcript cannot be read yet"
		scribeLiveRememberMiss(convID, reason)
		return &scribeLive{Items: []scribeLiveItem{}, Unavailable: reason}
	}
	return scribeLiveFromTranscript(t, since)
}

func scribeLiveRecentMiss(convID string) (string, bool) {
	scribeLiveMissMu.Lock()
	defer scribeLiveMissMu.Unlock()
	miss, ok := scribeLiveMisses[convID]
	if !ok || time.Since(miss.at) > scribeLiveMissTTL {
		delete(scribeLiveMisses, convID)
		return "", false
	}
	return miss.reason, true
}

func scribeLiveRememberMiss(convID, reason string) {
	scribeLiveMissMu.Lock()
	defer scribeLiveMissMu.Unlock()
	for id, miss := range scribeLiveMisses {
		if time.Since(miss.at) > scribeLiveMissTTL {
			delete(scribeLiveMisses, id)
		}
	}
	scribeLiveMisses[convID] = scribeLiveMiss{reason: reason, at: time.Now()}
}

// scribeLiveFromTranscript keeps the agent's text and tool calls of the open
// turn. An entry is part of it when it is newer than the last recorded reply;
// an entry without a timestamp counts when it follows the last operator
// message in the transcript, which is the best a format without times allows.
// The newest items win when the tail is over its bounds.
func scribeLiveFromTranscript(t *transcript.Transcript, since time.Time) *scribeLive {
	out := &scribeLive{Items: []scribeLiveItem{}}
	if t == nil {
		return out
	}
	lastHuman := -1
	for i, e := range t.Entries {
		if e.Role == transcript.RoleUser && e.Origin == transcript.OriginHuman {
			lastHuman = i
		}
	}
	for i, e := range t.Entries {
		if e.Role != transcript.RoleAssistant {
			continue
		}
		if e.Time.IsZero() {
			if i < lastHuman {
				continue
			}
		} else if !since.IsZero() && !e.Time.After(since) {
			continue
		}
		for _, b := range e.Blocks {
			switch b.Kind {
			case transcript.BlockText:
				if text := strings.TrimSpace(b.Text); text != "" {
					out.Items = append(out.Items, scribeLiveItem{Kind: "text", Text: clipUTF8(text, scribeLiveMaxTextBytes), At: e.Time})
				}
			case transcript.BlockToolCall:
				out.Items = append(out.Items, scribeLiveItem{Kind: "tool", Text: scribeToolSummary(b), At: e.Time})
			}
		}
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
