package harness

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Gemini CLI token usage, read from the conversation's own session file.
//
// Read from the upstream source at GeminiPinnedVersion, not measured on a live
// pane: chatRecordingService.recordMessageTokens stamps every `gemini` message
// with the usage metadata of the model call that produced it,
//
//	"tokens": {"input": promptTokenCount, "output": candidatesTokenCount,
//	           "cached": cachedContentTokenCount, "thoughts": …, "tool": …,
//	           "total": totalTokenCount}
//
// and Gemini's own context meter is the LAST call's promptTokenCount against
// a static per-model limit (core/tokenLimits.ts). That is exactly what this
// projection reports, so tclaude's context column agrees with the pane's.
//
// Output is summed over the messages that SURVIVE the file's fold (a
// `$rewindTo` drops the rewound turns' records), so it is the conversation's
// output, not a billing total. There is no cost: Gemini writes no price, and
// tclaude carries no Gemini price table yet.

// geminiTokens is one message's TokensSummary.
type geminiTokens struct {
	Input    int64 `json:"input"`
	Output   int64 `json:"output"`
	Cached   int64 `json:"cached"`
	Thoughts int64 `json:"thoughts"`
	Tool     int64 `json:"tool"`
	Total    int64 `json:"total"`
}

// GeminiUsage is one conversation's projected usage.
type GeminiUsage struct {
	// Model is the model of the latest call that reported usage.
	Model string
	// ContextTokens is that call's prompt size: the context occupancy Gemini's
	// own footer shows.
	ContextTokens int64
	// OutputTokens sums output and thinking tokens over the conversation.
	OutputTokens int64
	// ContextWindow is Gemini's token limit for Model.
	ContextWindow int64
	// Calls counts the messages that carried usage.
	Calls int
}

// ContextPct is ContextTokens as a percentage of ContextWindow, or 0 when
// either is unknown.
func (u GeminiUsage) ContextPct() float64 {
	if u.ContextTokens <= 0 || u.ContextWindow <= 0 {
		return 0
	}
	return min(float64(u.ContextTokens)*100/float64(u.ContextWindow), 100)
}

// Gemini's static limits (core/tokenLimits.ts): Gemma 4 has a 256k window and
// every other model, known or not, is given 1,048,576.
const (
	geminiDefaultTokenLimit = 1_048_576
	geminiGemma4TokenLimit  = 256_000
)

// GeminiContextWindow mirrors Gemini CLI's tokenLimit(model).
func GeminiContextWindow(model string) int64 {
	switch strings.TrimSpace(model) {
	case "gemma-4-31b-it", "gemma-4-26b-a4b-it":
		return geminiGemma4TokenLimit
	default:
		return geminiDefaultTokenLimit
	}
}

// geminiUsageOf projects a folded session.
func geminiUsageOf(session *geminiSession) GeminiUsage {
	var usage GeminiUsage
	for _, msg := range session.messages {
		if msg.Tokens == nil {
			continue
		}
		usage.Calls++
		usage.OutputTokens += max(msg.Tokens.Output, 0) + max(msg.Tokens.Thoughts, 0)
		usage.ContextTokens = max(msg.Tokens.Input, 0)
		if msg.Model != "" {
			usage.Model = msg.Model
		}
	}
	if usage.Calls > 0 {
		usage.ContextWindow = GeminiContextWindow(usage.Model)
	}
	return usage
}

// GeminiSessionFileStat identifies the session file a conversation lives in,
// and its size and mtime, so a caller can skip re-reading an unchanged file.
type GeminiSessionFileStat struct {
	Path    string
	Size    int64
	ModTime time.Time
}

// LocateGeminiSessionFile finds the file that holds convID, without the full
// store scan: Gemini names a session file `session-<timestamp>-<id[:8]>`, so
// only same-prefix files are opened. When a legacy `.json` and its migrated
// `.jsonl` both carry the id, the later lastUpdated wins, as in the store.
// found is false when no file holds the conversation yet.
func LocateGeminiSessionFile(convID string) (GeminiSessionFileStat, bool, error) {
	convID = strings.TrimSpace(convID)
	if len(convID) < 8 || strings.ContainsAny(convID, `/\*?[`) {
		return GeminiSessionFileStat{}, false, nil
	}
	root := geminiDir()
	if root == "" {
		return GeminiSessionFileStat{}, false, errors.New("gemini: cannot determine the Gemini CLI home directory")
	}
	pattern := filepath.Join(root, geminiTmpDirName, "*", geminiChatsDirName,
		geminiSessionPrefix+"*-"+convID[:8]+".json*")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return GeminiSessionFileStat{}, false, fmt.Errorf("gemini: locate %s: %w", convID, err)
	}
	var best GeminiSessionFileStat
	bestUpdated := ""
	found := false
	for _, path := range matches {
		if !geminiIsSessionFile(filepath.Base(path)) {
			continue
		}
		session, info, ok := readGeminiSession(path)
		if !ok || session.meta.SessionID != convID {
			continue
		}
		if found && session.meta.LastUpdated <= bestUpdated {
			continue
		}
		best = GeminiSessionFileStat{Path: path, Size: info.Size(), ModTime: info.ModTime()}
		bestUpdated = session.meta.LastUpdated
		found = true
	}
	return best, found, nil
}

// GeminiUsageFromFile projects the usage recorded in one session file. A file
// that vanished is (zero, false, nil): Gemini migrates and rewrites these.
func GeminiUsageFromFile(path string) (GeminiUsage, GeminiSessionFileStat, bool) {
	session, info, ok := readGeminiSession(path)
	if !ok {
		return GeminiUsage{}, GeminiSessionFileStat{}, false
	}
	return geminiUsageOf(session),
		GeminiSessionFileStat{Path: path, Size: info.Size(), ModTime: info.ModTime()}, true
}

// StatGeminiSessionFile re-stats a located file. found is false once it is
// gone.
func StatGeminiSessionFile(path string) (GeminiSessionFileStat, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return GeminiSessionFileStat{}, false
	}
	return GeminiSessionFileStat{Path: path, Size: info.Size(), ModTime: info.ModTime()}, true
}

func readGeminiSession(path string) (*geminiSession, fs.FileInfo, bool) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, false
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, nil, false
	}
	session, ok := foldGeminiSessionFile(file, path)
	if !ok {
		return nil, nil, false
	}
	return session, info, true
}
