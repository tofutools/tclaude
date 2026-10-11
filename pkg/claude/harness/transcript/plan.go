package transcript

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Defaults for Options. The operator can override the budget and the clip
// size through agent.morph config.
const (
	DefaultBudgetPercent       = 60
	DefaultToolResultClipBytes = 16 << 10
	DefaultContextWindowTokens = 32_000
	// mediaTokenCost is a coarse per-image estimate for the context budget.
	mediaTokenCost = 1600
)

// Options is the target-side policy for one translation.
type Options struct {
	// Target is the destination harness name (for the report and notice).
	Target string
	// TargetModel is the destination model, used only in the notice.
	TargetModel string
	// ContextWindowTokens is the destination model's window; zero uses
	// DefaultContextWindowTokens.
	ContextWindowTokens int64
	// NativeContextWindowTokens is a launch-selected native limit (Claude 1M).
	// It is independent of an explicit carried-history budget override.
	NativeContextWindowTokens int64
	// ContextWindowSource describes the resolved limit, shown in the preview.
	ContextWindowSource string
	// BudgetPercent of the window the carried history may fill. The rest is
	// headroom so the target does not compact on its first turn.
	BudgetPercent int
	// ToolResultClipBytes bounds each flattened tool call input and result.
	ToolResultClipBytes int
	// InlineMedia says the target's native user turns accept inline images.
	InlineMedia bool
	// MediaDir receives media the target cannot inline. Empty drops them.
	MediaDir string
	// TranscriptPath is the full transcript file, named in omission and
	// clipping markers so the target agent can read what was cut.
	TranscriptPath string
}

func (o Options) budgetTokens() int64 {
	window := o.ContextWindowTokens
	if window <= 0 {
		window = DefaultContextWindowTokens
	}
	pct := o.BudgetPercent
	if pct <= 0 || pct > 100 {
		pct = DefaultBudgetPercent
	}
	return window * int64(pct) / 100
}

func (o Options) clip() int {
	if o.ToolResultClipBytes <= 0 {
		return DefaultToolResultClipBytes
	}
	return o.ToolResultClipBytes
}

// Plan is a Transcript after target policy: user/assistant entries holding
// only text and inline media blocks, starting with a user entry and strictly
// alternating, ready for any harness encoder.
type Plan struct {
	Source Source
	Target string
	// TargetModel labels carried messages where a format records a model.
	TargetModel string
	Title       string
	Entries     []Entry
	Report      *LossReport
	// pendingMedia are files to write under Options.MediaDir before the
	// encoded history is installed. Preview leaves them unwritten.
	pendingMedia []pendingMedia
}

type pendingMedia struct {
	path string
	data []byte
}

// Marker glyphs make carried records unmistakable as history, in any
// language and against any tool-call syntax.
const (
	markOpen  = "⟦"
	markClose = "⟧"
)

type planEntry struct {
	entry Entry
	// sources is how many transcript entries this plan entry carries.
	sources int
	// turn is true when the entry begins with a human turn: a safe place to
	// cut for the budget.
	turn    bool
	summary bool
}

// NewPlan applies target policy to a transcript. It never mutates t.
func NewPlan(t *Transcript, opts Options) *Plan {
	report := &LossReport{Source: t.Source.Harness, Target: opts.Target, SourceEntries: len(t.Entries)}
	if t.Losses != nil {
		report.Items = append(report.Items, t.Losses.Items...)
	}
	report.ContextWindowTokens = opts.ContextWindowTokens
	if report.ContextWindowTokens <= 0 {
		report.ContextWindowTokens = DefaultContextWindowTokens
	}
	report.ContextWindowSource = opts.ContextWindowSource
	if report.ContextWindowSource == "" {
		if opts.ContextWindowTokens > 0 {
			report.ContextWindowSource = "explicit history window"
		} else {
			report.ContextWindowSource = "unknown model, assumed 32k"
		}
	}
	report.BudgetTokens = opts.budgetTokens()
	p := &Plan{Source: t.Source, Target: opts.Target, TargetModel: opts.TargetModel, Title: t.Source.Title, Report: report}
	if opts.TranscriptPath != "" {
		report.TranscriptPath = opts.TranscriptPath
	}

	recentStart := recentToolStart(t.Entries)
	results := map[string]Block{}
	calls := map[string]string{}
	for _, e := range t.Entries {
		for _, b := range e.Blocks {
			if b.Kind == BlockToolResult && b.CallID != "" {
				results[b.CallID] = b
			}
			if b.Kind == BlockToolCall && b.CallID != "" {
				calls[b.CallID] = b.Name
			}
		}
	}

	var entries []planEntry
	for index, e := range t.Entries {
		out := Entry{Role: e.Role, Origin: e.Origin, Time: e.Time, Model: e.Model}
		if e.Origin == OriginTool {
			out.Role = RoleUser
		}
		summary := false
		for _, b := range e.Blocks {
			switch b.Kind {
			case BlockText:
				if strings.TrimSpace(b.Text) == "" {
					continue
				}
				text := withoutPriorMorphNotices(b.Text)
				if strings.TrimSpace(text) == "" {
					continue
				}
				if e.Author != "" {
					text = fmt.Sprintf("%smessage from %s%s\n%s", markOpen, e.Author, markClose, text)
				}
				out.Blocks = append(out.Blocks, Text(text))
			case BlockThinking:
				report.Add(LossReasoningDropped, 1, "")
			case BlockSummary:
				summary = true
				out.Blocks = append(out.Blocks, Text(fmt.Sprintf("%sSummary of the earlier conversation, written when %s compacted it%s\n%s", markOpen, t.Source.Harness, markClose, b.Text)))
			case BlockToolCall:
				report.Add(LossToolCallsFlattened, 1, "")
				input := b.Text
				if !b.Freeform {
					input = compactJSON(b.Input)
				}
				if index < recentStart && opts.TranscriptPath != "" {
					result, found := results[b.CallID]
					status := "interrupted"
					if found {
						status = toolStatus(result)
					} else {
						report.Add(LossInterruptedCall, 1, "")
					}
					out.Blocks = append(out.Blocks, Text(toolStub(b.Name, input, status, result.Text, opts.TranscriptPath, index)))
					report.Add(LossToolRecordsStubbed, 1, "")
					continue
				}
				text := fmt.Sprintf("%stool call · %s%s", markOpen, b.Name, markClose)
				if input != "" {
					text += "\n" + p.clipText(input, opts, report)
				}
				if _, found := results[b.CallID]; b.CallID == "" || !found {
					report.Add(LossInterruptedCall, 1, "")
					text += "\n" + markOpen + "no result was recorded for this call (interrupted before the morph)" + markClose
				}
				out.Blocks = append(out.Blocks, Text(text))
			case BlockToolResult:
				name := b.Name
				if name == "" {
					name = calls[b.CallID]
				}
				if name == "" {
					name = "tool"
				}
				status := toolStatus(b)
				text := fmt.Sprintf("%stool result · %s · %s%s", markOpen, name, status, markClose)
				if index < recentStart && opts.TranscriptPath != "" {
					text = toolStub(name, "", status, b.Text, opts.TranscriptPath, index)
					report.Add(LossToolRecordsStubbed, 1, "")
				} else if b.Text != "" {
					recentOpts := opts
					if opts.TranscriptPath != "" {
						recentOpts.ToolResultClipBytes = min(opts.clip(), 4<<10)
					}
					text += "\n" + p.clipText(b.Text, recentOpts, report)
				}
				out.Blocks = append(out.Blocks, Text(text))
				for _, m := range b.Media {
					out.Blocks = append(out.Blocks, p.media(m, out.Role, opts, report)...)
				}
			case BlockMedia:
				for _, m := range b.Media {
					out.Blocks = append(out.Blocks, p.media(m, out.Role, opts, report)...)
				}
			}
		}
		if len(out.Blocks) == 0 {
			continue
		}
		entries = append(entries, planEntry{entry: out, sources: 1, turn: e.Origin == OriginHuman, summary: summary})
	}

	entries = coalesce(entries)
	entries = p.fitBudget(entries, opts, report)

	notice := fmt.Sprintf("%stclaude morph%s History carried over from %s to %s. Tool records are history; use your own tools.", markOpen, markClose, describe(t.Source.Harness, t.Source.Model), describe(opts.Target, opts.TargetModel))
	if report.TranscriptPath != "" {
		notice += " Full tool output: " + report.TranscriptPath + "."
	}
	notice += " No need to re-read skills or re-verify the environment unless something looks wrong."
	entries = append(entries, planEntry{entry: Entry{Role: RoleUser, Origin: OriginHuman, Blocks: []Block{Text(notice)}}})
	entries = coalesce(entries)
	if entries[0].entry.Role != RoleUser {
		lead := planEntry{entry: Entry{Role: RoleUser, Origin: OriginHuman, Blocks: []Block{Text(markOpen + "the conversation continues from earlier history" + markClose)}}}
		entries = append([]planEntry{lead}, entries...)
	}

	for _, e := range entries {
		report.Carried += e.sources
		p.Entries = append(p.Entries, e.entry)
		report.CarriedEstimatedTokens += estimateTokens(e.entry)
	}
	for _, e := range t.Entries {
		report.SourceEstimatedTokens += estimateTokens(e)
		for _, b := range e.Blocks {
			report.SourceEstimatedTokens += (int64(len(b.Input))*3 + 3) / 4
		}
	}
	report.sort()
	return p
}

func describe(harness, model string) string {
	if model == "" {
		return harness
	}
	return harness + " (" + model + ")"
}

func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	s := buf.String()
	if s == "{}" || s == "null" {
		return ""
	}
	return s
}

// clipText keeps the head and tail of an over-long tool record.
func (p *Plan) clipText(s string, opts Options, report *LossReport) string {
	limit := opts.clip()
	if len(s) <= limit {
		return s
	}
	report.Add(LossToolOutputClipped, 1, "")
	head := limit * 3 / 5
	tail := limit - head
	h, tl := s[:head], s[len(s)-tail:]
	for len(h) > 0 && !utf8.ValidString(h) {
		h = h[:len(h)-1]
	}
	for len(tl) > 0 && !utf8.ValidString(tl) {
		tl = tl[1:]
	}
	marker := fmt.Sprintf("\n%s… %d bytes clipped", markOpen, len(s)-len(h)-len(tl))
	if report.TranscriptPath != "" {
		marker += "; full text in " + report.TranscriptPath
	}
	return h + marker + " …" + markClose + "\n" + tl
}

// inlineImageTypes are the image types every inline-capable target's model
// API accepts. Anything else would be rejected on every later turn.
var inlineImageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

func (p *Plan) media(m Media, role Role, opts Options, report *LossReport) []Block {
	if opts.InlineMedia && role == RoleUser && len(m.Data) > 0 && inlineImageTypes[m.MediaType] {
		return []Block{{Kind: BlockMedia, Media: []Media{m}}}
	}
	label := m.Name
	if label == "" {
		label = m.MediaType
	}
	switch {
	case m.Path != "":
		report.Add(LossMediaExternalized, 1, "")
		return []Block{Text(fmt.Sprintf("%sattachment %s: %s%s", markOpen, label, m.Path, markClose))}
	case len(m.Data) > 0 && opts.MediaDir != "":
		report.Add(LossMediaExternalized, 1, "")
		path := filepath.Join(opts.MediaDir, fmt.Sprintf("media-%03d%s", len(p.pendingMedia)+1, mediaExt(m.MediaType)))
		p.pendingMedia = append(p.pendingMedia, pendingMedia{path: path, data: m.Data})
		return []Block{Text(fmt.Sprintf("%sattachment %s saved to %s%s", markOpen, label, path, markClose))}
	case m.URL != "" && !strings.HasPrefix(m.URL, "data:"):
		report.Add(LossMediaExternalized, 1, "")
		return []Block{Text(fmt.Sprintf("%sattachment %s: %s%s", markOpen, label, m.URL, markClose))}
	default:
		report.Add(LossMediaDropped, 1, "")
		return []Block{Text(fmt.Sprintf("%sattachment %s not carried%s", markOpen, label, markClose))}
	}
}

func mediaExt(mediaType string) string {
	switch mediaType {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "application/pdf":
		return ".pdf"
	}
	if exts, _ := mime.ExtensionsByType(mediaType); len(exts) > 0 {
		return exts[0]
	}
	return ".bin"
}

// WriteMedia writes externalized media files. Call it only when the plan is
// applied, never for a preview. The artifact directory may be writable by
// the agent, so no existing path is followed or reused.
func (p *Plan) WriteMedia() error {
	for _, m := range p.pendingMedia {
		if err := EnsureRealDir(filepath.Dir(m.path)); err != nil {
			return err
		}
		if err := WriteFileExclusive(m.path, m.data); err != nil {
			return err
		}
	}
	return nil
}

// EnsureRealDir creates dir (its parent must exist) or accepts it only when
// it already is a real directory, never a symlink.
func EnsureRealDir(dir string) error {
	if err := os.Mkdir(dir, 0o700); err == nil || !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s is not a real directory", dir)
	}
	return nil
}

// WriteFileExclusive creates path and writes data. O_EXCL refuses an
// existing file and does not follow a symlink planted at path.
func WriteFileExclusive(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// coalesce merges adjacent same-role entries so every encoder can rely on
// strict user/assistant alternation.
func coalesce(in []planEntry) []planEntry {
	var out []planEntry
	for _, e := range in {
		if n := len(out); n > 0 && out[n-1].entry.Role == e.entry.Role {
			last := &out[n-1]
			last.entry.Blocks = append(last.entry.Blocks, e.entry.Blocks...)
			last.sources += e.sources
			last.summary = last.summary || e.summary
			last.turn = last.turn || e.turn
			if last.entry.Model == "" {
				last.entry.Model = e.entry.Model
			}
			if last.entry.Time.IsZero() {
				last.entry.Time = e.entry.Time
			}
			continue
		}
		out = append(out, e)
	}
	return out
}

func estimateTokens(e Entry) int64 {
	var n int64
	for _, b := range e.Blocks {
		n += (int64(len(b.Text))*3 + 3) / 4
		n += int64(len(b.Media)) * mediaTokenCost
	}
	return n + 32
}

// fitBudget drops the oldest entries, at a human-turn boundary, until the
// rest fits. A leading compaction summary is kept when it fits.
func (p *Plan) fitBudget(entries []planEntry, opts Options, report *LossReport) []planEntry {
	budget := opts.budgetTokens()
	var total int64
	for _, e := range entries {
		total += estimateTokens(e.entry)
	}
	if total <= budget || len(entries) == 0 {
		return entries
	}
	var pinned []planEntry
	rest := entries
	if entries[0].summary && estimateTokens(entries[0].entry) < budget/4 {
		pinned, rest = entries[:1], entries[1:]
		budget -= estimateTokens(entries[0].entry)
	}
	cut := -1
	var suffix int64
	for i := len(rest) - 1; i >= 0; i-- {
		suffix += estimateTokens(rest[i].entry)
		if suffix > budget {
			break
		}
		if rest[i].turn && rest[i].entry.Role == RoleUser {
			cut = i
		}
	}
	if cut < 0 {
		// Not even the newest turn fits: keep from the last human turn and
		// let the target compact.
		for i := len(rest) - 1; i >= 0; i-- {
			if rest[i].turn && rest[i].entry.Role == RoleUser {
				cut = i
				break
			}
		}
		if cut < 0 {
			cut = 0
		}
	}
	omitted := 0
	for _, e := range rest[:cut] {
		omitted += e.sources
	}
	if omitted == 0 {
		return entries
	}
	report.Add(LossTruncatedBudget, omitted, "")
	marker := fmt.Sprintf("%s%d earlier entries were omitted to fit the context budget", markOpen, omitted)
	if report.TranscriptPath != "" {
		marker += "; read them in " + report.TranscriptPath
	}
	marker += markClose
	lead := planEntry{entry: Entry{Role: RoleUser, Origin: OriginHuman, Blocks: []Block{Text(marker)}}}
	out := append(append(append([]planEntry{}, pinned...), lead), rest[cut:]...)
	return coalesce(out)
}

// Carried notices are plain native text on the next hop. Replace only our
// generated notice line, retaining adjacent user instructions and all history
// wrappers (tool records, summaries and file pointers remain useful context).
func withoutPriorMorphNotices(text string) string {
	lines := strings.Split(text, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if strings.HasPrefix(line, "⟦tclaude morph⟧ History carried over from ") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}
