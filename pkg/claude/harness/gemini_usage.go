package harness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
// either on the message itself or by re-appending the whole record under the
// same id once the usage lands. Gemini's own context meter is the LAST call's
// promptTokenCount against a static per-model limit (core/tokenLimits.ts).
// That is exactly what this projection reports, so tclaude's context column
// agrees with the pane's.
//
// Output is summed over the messages that SURVIVE the file's fold (a
// `$rewindTo` or a `$set.messages` checkpoint drops records), so it is the
// conversation's output, not a billing total, and it can move down after a
// rewind or a compression. A compression's synthetic history carries no usage,
// so the context reading holds its pre-compression value until the next call.
// There is no cost: Gemini writes no price.

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
	// CostUSD is the WHAT-IF cost of every priced call the file recorded,
	// including calls a rewind or compression later dropped (gemini_cost.go).
	// 0 when no call used a priced model.
	CostUSD float64
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

// geminiUsageLine decodes only what the usage fold needs from one record.
// Everything else on the line (content, tool results, thoughts) is skipped by
// the decoder rather than materialized, which is what keeps a refresh of a
// tool-heavy session cheap.
type geminiUsageLine struct {
	ID        *string       `json:"id"`
	Model     string        `json:"model"`
	Timestamp string        `json:"timestamp"`
	Tokens    *geminiTokens `json:"tokens"`
	RewindTo  *string       `json:"$rewindTo"`
	Set       *struct {
		Messages *[]geminiUsageLine `json:"messages"`
	} `json:"$set"`
	// Messages is a legacy whole-file record's inline message list.
	Messages *[]geminiUsageLine `json:"messages"`
}

type geminiUsageRecord struct {
	id     string
	model  string
	tokens *geminiTokens
}

// geminiUsageFold is the usage-only counterpart of geminiSession: the same
// replace-by-id, rewind and checkpoint rules (loadConversationRecord), over
// just the fields usage needs.
//
// billed is kept apart from the fold: every call the file ever recorded, by
// message id, whether or not a rewind or a checkpoint later dropped it, since
// the WHAT-IF cost prices calls made rather than history kept.
type geminiUsageFold struct {
	records []geminiUsageRecord
	index   map[string]int

	billed      []geminiBilledCall
	billedIndex map[string]int
}

// reset clears the conversation fold. billed survives it, including a
// re-read of a rewritten or shrunk file: every record in it belongs to this
// conversation and is keyed by message id, so a call the rewrite restates
// replaces its entry and one it dropped stays billed. Only a new follower
// (a new session generation, or a daemon restart) starts it empty.
func (g *geminiUsageFold) reset() {
	g.records = nil
	g.index = map[string]int{}
}

// bill records a call's usage under its message id; a re-appended record (the
// usage landing after the message) or a checkpoint's restatement replaces it.
func (g *geminiUsageFold) bill(line geminiUsageLine) {
	if line.ID == nil || *line.ID == "" || line.Tokens == nil {
		return
	}
	call := geminiBilledCall{model: line.Model, tokens: *line.Tokens}
	if at, err := time.Parse(time.RFC3339Nano, line.Timestamp); err == nil {
		call.timestamp = at
	}
	if g.billedIndex == nil {
		g.billedIndex = map[string]int{}
	}
	if at, ok := g.billedIndex[*line.ID]; ok {
		g.billed[at] = call
		return
	}
	g.billedIndex[*line.ID] = len(g.billed)
	g.billed = append(g.billed, call)
}

func (g *geminiUsageFold) put(line geminiUsageLine) {
	if line.ID == nil || *line.ID == "" {
		return
	}
	g.bill(line)
	record := geminiUsageRecord{id: *line.ID, model: line.Model, tokens: line.Tokens}
	if at, ok := g.index[record.id]; ok {
		g.records[at] = record
		return
	}
	g.index[record.id] = len(g.records)
	g.records = append(g.records, record)
}

func (g *geminiUsageFold) replace(lines []geminiUsageLine) {
	g.reset()
	for _, line := range lines {
		g.put(line)
	}
}

// apply folds one line in loadConversationRecord's precedence: rewind,
// message, metadata update, metadata.
func (g *geminiUsageFold) apply(raw []byte) {
	var line geminiUsageLine
	if json.Unmarshal(raw, &line) != nil {
		return
	}
	switch {
	case line.RewindTo != nil:
		at, ok := g.index[*line.RewindTo]
		if !ok {
			at = 0
		}
		for _, record := range g.records[at:] {
			delete(g.index, record.id)
		}
		g.records = g.records[:at]
	case line.ID != nil:
		g.put(line)
	case line.Set != nil:
		if line.Set.Messages != nil {
			g.replace(*line.Set.Messages)
		}
	case line.Messages != nil:
		g.replace(*line.Messages)
	}
}

func (g *geminiUsageFold) usage() GeminiUsage {
	var usage GeminiUsage
	for _, record := range g.records {
		if record.tokens == nil {
			continue
		}
		usage.Calls++
		usage.OutputTokens += max(record.tokens.Output, 0) + max(record.tokens.Thoughts, 0)
		usage.ContextTokens = max(record.tokens.Input, 0)
		if record.model != "" {
			usage.Model = record.model
		}
	}
	if usage.Calls > 0 {
		usage.ContextWindow = GeminiContextWindow(usage.Model)
	}
	usage.CostUSD, _ = geminiCostHistory(g.billed, time.Now())
	return usage
}

// geminiUsageLineLimit bounds one decoded line. It is far above the
// convstore's listing cap because a `$set.messages` checkpoint carries the
// whole history, and dropping one would leave the fold describing a history
// Gemini already replaced.
const geminiUsageLineLimit = geminiLegacyFileLimit

// GeminiUsageFollower follows one conversation's session file. A JSONL file is
// only ever appended to by Gemini between atomic rewrites, so the follower
// reads just the bytes added since the last call, and re-folds from the start
// only when the file was replaced or shrank. A legacy `.json` record is re-read
// whole; Gemini never appends to one.
//
// Not safe for concurrent use; callers serialize per conversation.
type GeminiUsageFollower struct {
	path   string
	info   os.FileInfo
	offset int64
	fold   geminiUsageFold
}

// Path is the file the follower last read, "" before the first read.
func (f *GeminiUsageFollower) Path() string { return f.path }

// Read brings the fold up to date with path and returns the usage. found is
// false when the file is gone (Gemini migrates and rewrites these), in which
// case the follower forgets it.
func (f *GeminiUsageFollower) Read(path string) (usage GeminiUsage, found bool, err error) {
	file, err := os.Open(path)
	if err != nil {
		f.forget()
		if errors.Is(err, os.ErrNotExist) {
			return GeminiUsage{}, false, nil
		}
		return GeminiUsage{}, false, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		f.forget()
		return GeminiUsage{}, false, err
	}

	legacy := strings.HasSuffix(path, ".json")
	sameFile := f.path == path && f.info != nil && os.SameFile(f.info, info)
	switch {
	case sameFile && info.Size() == f.offset && info.ModTime().Equal(f.info.ModTime()):
		return f.fold.usage(), true, nil
	case !sameFile || legacy || info.Size() < f.offset:
		f.path, f.offset = path, 0
		f.fold.reset()
	}
	f.info = info

	if legacy {
		raw, err := io.ReadAll(io.LimitReader(file, geminiLegacyFileLimit))
		if err != nil {
			f.forget()
			return GeminiUsage{}, false, err
		}
		f.fold.apply(raw)
		f.offset = info.Size()
		return f.fold.usage(), true, nil
	}
	if _, err := file.Seek(f.offset, io.SeekStart); err != nil {
		f.forget()
		return GeminiUsage{}, false, err
	}
	consumed, err := f.foldLines(bufio.NewReaderSize(file, 64<<10))
	f.offset += consumed
	if err != nil {
		f.forget()
		return GeminiUsage{}, false, err
	}
	return f.fold.usage(), true, nil
}

// foldLines folds every COMPLETE line and reports the bytes consumed. A final
// line without its newline is a record Gemini is still writing; it is left for
// the next read. An oversized line is consumed but not decoded.
func (f *GeminiUsageFollower) foldLines(reader *bufio.Reader) (int64, error) {
	var consumed int64
	var line []byte
	lineBytes := int64(0)
	overLimit := false
	for {
		chunk, err := reader.ReadSlice('\n')
		lineBytes += int64(len(chunk))
		if !overLimit {
			if len(line)+len(chunk) > geminiUsageLineLimit {
				overLimit, line = true, nil
			} else {
				line = append(line, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			return consumed, nil
		}
		if err != nil {
			return consumed, err
		}
		if !overLimit {
			if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
				f.fold.apply(trimmed)
			}
		}
		consumed += lineBytes
		line, lineBytes, overLimit = line[:0], 0, false
	}
}

// CostHistory is the WHAT-IF cost as cumulative per-day rows, as of the last
// Read. now places calls that carry no timestamp.
func (f *GeminiUsageFollower) CostHistory(now time.Time) []GeminiCostDay {
	_, history := geminiCostHistory(f.fold.billed, now)
	return history
}

func (f *GeminiUsageFollower) forget() {
	f.path, f.info, f.offset = "", nil, 0
	f.fold.reset()
}

// LocateGeminiSessionFile finds the file that holds convID, without the full
// store scan: Gemini names a session file `session-<timestamp>-<id[:8]>`, so
// only same-prefix files are opened, under both runtime directories (the
// Seatbelt mode keeps chats in ~/.cache/.gemini). When a legacy `.json` and its migrated
// `.jsonl` both carry the id, the `.jsonl` wins — Gemini writes only to it
// after the migration, and the two share a lastUpdated until the first new
// message lands. found is false when no file holds the conversation yet.
func LocateGeminiSessionFile(convID string) (path string, found bool, err error) {
	convID = strings.TrimSpace(convID)
	if len(convID) < 8 || strings.ContainsAny(convID, `/\*?[`) {
		return "", false, nil
	}
	roots := geminiRuntimeDirs()
	if len(roots) == 0 {
		return "", false, errors.New("gemini: cannot determine the Gemini CLI home directory")
	}
	var matches []string
	for _, root := range roots {
		pattern := filepath.Join(root, geminiTmpDirName, "*", geminiChatsDirName,
			geminiSessionPrefix+"*-"+convID[:8]+".json*")
		found, err := filepath.Glob(pattern)
		if err != nil {
			return "", false, fmt.Errorf("gemini: locate %s: %w", convID, err)
		}
		matches = append(matches, found...)
	}
	bestUpdated := ""
	for _, candidate := range matches {
		if !geminiIsSessionFile(filepath.Base(candidate)) {
			continue
		}
		meta, ok := readGeminiSessionMeta(candidate)
		if !ok || meta.SessionID != convID {
			continue
		}
		better := !found || meta.LastUpdated > bestUpdated ||
			(meta.LastUpdated == bestUpdated && strings.HasSuffix(candidate, ".jsonl") &&
				!strings.HasSuffix(path, ".jsonl"))
		if better {
			path, bestUpdated, found = candidate, meta.LastUpdated, true
		}
	}
	return path, found, nil
}

// GeminiMigratedSessionFile returns the `.jsonl` a legacy `.json` session
// file is migrated to on resume, when that file exists; "" otherwise.
func GeminiMigratedSessionFile(path string) string {
	if !strings.HasSuffix(path, ".json") {
		return ""
	}
	if _, err := os.Stat(path + "l"); err != nil {
		return ""
	}
	return path + "l"
}

func readGeminiSessionMeta(path string) (geminiMetadata, bool) {
	file, err := os.Open(path)
	if err != nil {
		return geminiMetadata{}, false
	}
	defer func() { _ = file.Close() }()
	session, ok := foldGeminiSessionFile(file, path)
	if !ok {
		return geminiMetadata{}, false
	}
	return session.meta, true
}
