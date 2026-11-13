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
	// Summary is source-written context requested before a large budget cut.
	SummaryUnavailable bool
	Summary            string
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

	// Operator carry-over overrides. All zero is the automatic policy.
	//
	// StatedWindowTokens is the destination window as the operator states
	// it. It replaces the detected window instead of being capped by it.
	StatedWindowTokens int64
	// DetectedWindowTokens and DetectedWindowSource are the window the
	// policy would have used without StatedWindowTokens, for the report.
	DetectedWindowTokens int64
	DetectedWindowSource string
	// BudgetTokens replaces the window-derived budget (window ×
	// BudgetPercent). CarryAll carries the whole history: no budget cut.
	BudgetTokens int64
	CarryAll     bool
	// Keep is the share of each category kept at full fidelity, newest
	// first; the older rest is shortened to stubs that point at the full
	// transcript. A category not named keeps the automatic rule.
	Keep map[Category]int
	// CutFirst is the category a budget cut shortens before it omits whole
	// turns: its keep share becomes the largest that fits the budget. ""
	// omits the oldest turns first.
	CutFirst Category
}

// operatorCarry reports whether any carry-over override is set.
func (o Options) operatorCarry() bool {
	return o.StatedWindowTokens > 0 || o.BudgetTokens > 0 || o.CarryAll || len(o.Keep) > 0 || o.CutFirst != ""
}

func (o Options) budgetTokens() int64 {
	if o.BudgetTokens > 0 {
		return o.BudgetTokens
	}
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
	// cats is the estimated cost of each category this entry carries.
	cats map[Category]int64
}

// NewPlan applies target policy to a transcript. It never mutates t.
func NewPlan(t *Transcript, opts Options) *Plan {
	costs := categoryCosts(t)
	keep := opts.Keep
	if opts.CarryAll {
		// Carrying everything means everything whole: a category keeps its
		// automatic stubs only when the operator names a share for it.
		keep = map[Category]int{}
		for _, c := range Categories {
			keep[c] = 100
		}
		for c, v := range opts.Keep {
			keep[c] = v
		}
	}
	cut := resolveCutoffs(t, costs, keep)
	if _, explicit := opts.Keep[opts.CutFirst]; !ValidCategory(opts.CutFirst) || explicit {
		return newPlan(t, opts, cut, nil)
	}
	// The cut-first category gives way first: keep the largest share of it
	// with which the rest fits the budget without omitting whole turns.
	// Each probe is a full plan, so the estimate is the one applied.
	build := func(pct int) *Plan {
		c := cutoffs{}
		for k, v := range cut {
			c[k] = v
		}
		c[opts.CutFirst] = keepCutoff(costs, opts.CutFirst, pct)
		return newPlan(t, opts, c, &pct)
	}
	best := build(100)
	if opts.CarryAll || best.Report.Count(LossTruncatedBudget) == 0 {
		return best
	}
	lo, hi := 0, 99
	if none := build(0); none.Report.Count(LossTruncatedBudget) > 0 {
		// Even shortening all of it does not fit: carry whichever omits
		// less, so cut-first is never worse than the oldest-first cut.
		if none.Report.Count(LossTruncatedBudget) < best.Report.Count(LossTruncatedBudget) {
			return none
		}
		return best
	} else {
		best = none
	}
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if p := build(mid); p.Report.Count(LossTruncatedBudget) == 0 {
			lo, best = mid, p
		} else {
			hi = mid - 1
		}
	}
	return best
}

func newPlan(t *Transcript, opts Options, cut cutoffs, cutFirstPct *int) *Plan {
	report := &LossReport{Source: t.Source.Harness, Target: opts.Target, SourceEntries: len(t.Entries), BudgetTokens: opts.budgetTokens()}
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
	report.CarryAll = opts.CarryAll
	if opts.CarryAll {
		report.BudgetTokens = 0
	}
	report.BudgetSource = "auto"
	switch {
	case opts.CarryAll:
		report.BudgetSource = "all"
	case opts.BudgetTokens > 0:
		report.BudgetSource = "stated"
	}
	report.WindowStated = opts.StatedWindowTokens > 0
	report.DetectedWindowTokens, report.DetectedWindowSource = opts.DetectedWindowTokens, opts.DetectedWindowSource
	report.CutFirst = opts.CutFirst
	p := &Plan{Source: t.Source, Target: opts.Target, TargetModel: opts.TargetModel, Title: t.Source.Title, Report: report}
	if opts.TranscriptPath != "" {
		report.TranscriptPath = opts.TranscriptPath
	}

	// The original task and the latest instructions are never shortened.
	firstTurn, lastTurn := -1, -1
	for i, e := range t.Entries {
		if inputTurn(e) && !daemonControlTurn(e) {
			if firstTurn < 0 {
				firstTurn = i
			}
			lastTurn = i
		}
	}
	usage := map[Category]*CategoryUsage{}
	for _, c := range Categories {
		usage[c] = &CategoryUsage{Category: c}
		if pct, ok := opts.Keep[c]; ok {
			usage[c].KeepPercent = &pct
		}
	}
	if cutFirstPct != nil {
		usage[opts.CutFirst].KeepPercent = cutFirstPct
	}
	for _, m := range categoryCosts(t) {
		for c, n := range m {
			usage[c].SourceTokens += n
		}
	}
	// Under a share (set by the operator or found by CutFirst) a record is
	// stubbed only when the stub is smaller, so a lower share never carries
	// more. The automatic rule keeps stubbing every older tool record: the
	// stub is the agent's pointer into the full transcript.
	shrinks := func(c Category, stub string, fullLen int) bool {
		return usage[c].KeepPercent == nil || len(stub) < fullLen
	}
	shorten := func(c Category, full, stub string) {
		usage[c].Shortened++
		usage[c].ShortenedTokens += max(0, textTokens(full)-textTokens(stub))
	}
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
		cats := map[Category]int64{}
		add := func(c Category, b Block) {
			out.Blocks = append(out.Blocks, b)
			cats[c] += textTokens(b.Text)
		}
		stubbed := func(c Category) bool { return index < cut[c] && opts.TranscriptPath != "" }
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
				// Text that is not the operator's is labelled so the landing
				// model does not take it as the operator's instructions,
				// unless it already carries tclaude's own header.
				// A tclaude notice or agent message names its sender in its own
				// header. An origin this reader does not know is labelled anyway:
				// its text may only look like one.
				if !strings.HasPrefix(strings.TrimSpace(text), "[system:") || !e.Origin.Known() {
					switch {
					case e.Origin == OriginPeer || (e.Author != "" && e.Origin.Known() && e.Origin != OriginSystem):
						from := e.Author
						if from == "" {
							from = "another agent"
						}
						text = fmt.Sprintf("%smessage from %s%s\n%s", markOpen, from, markClose, text)
					case !e.Origin.Known() && e.Origin != "":
						text = fmt.Sprintf("%suntrusted input (origin %s), not from the operator%s\n%s", markOpen, e.Origin, markClose, text)
					}
				}
				if stubbed(CategoryChat) && index != firstTurn && index != lastTurn {
					if stub := chatStub(out.Role, text, opts.TranscriptPath, index); len(stub) < len(text) {
						shorten(CategoryChat, text, stub)
						text = stub
					}
				}
				add(CategoryChat, Text(text))
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
				if stubbed(CategoryToolCalls) {
					result, found := results[b.CallID]
					status := "interrupted"
					if found {
						status = toolStatus(result)
					}
					// A stub of a small call can outgrow it; keep such a call whole.
					if stub := toolStub(b.Name, input, status, result.Text, opts.TranscriptPath, index); shrinks(CategoryToolCalls, stub, len(b.Name)+len(input)+32) {
						if !found {
							report.Add(LossInterruptedCall, 1, "")
						}
						shorten(CategoryToolCalls, b.Name+input, stub)
						add(CategoryToolCalls, Text(stub))
						report.Add(LossToolRecordsStubbed, 1, "")
						continue
					}
				}
				text := fmt.Sprintf("%stool call · %s%s", markOpen, b.Name, markClose)
				if input != "" {
					text += "\n" + p.clipText(input, opts, report)
				}
				if _, found := results[b.CallID]; b.CallID == "" || !found {
					report.Add(LossInterruptedCall, 1, "")
					text += "\n" + markOpen + "no result was recorded for this call (interrupted before the morph)" + markClose
				}
				add(CategoryToolCalls, Text(text))
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
				stub := ""
				if stubbed(CategoryToolOutput) {
					stub = toolStub(name, "", status, b.Text, opts.TranscriptPath, index)
				}
				if stub != "" && shrinks(CategoryToolOutput, stub, len(text)+len(b.Text)) {
					shorten(CategoryToolOutput, b.Text, stub)
					text = stub
					report.Add(LossToolRecordsStubbed, 1, "")
				} else if b.Text != "" {
					recentOpts := opts
					if opts.TranscriptPath != "" {
						recentOpts.ToolResultClipBytes = min(opts.clip(), 4<<10)
					}
					text += "\n" + p.clipText(b.Text, recentOpts, report)
				}
				add(CategoryToolOutput, Text(text))
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
		entries = append(entries, planEntry{entry: out, sources: 1, turn: inputTurn(e) && !daemonControlTurn(e), summary: summary, cats: cats})
	}

	entries = coalesce(entries)
	entries = p.fitBudget(entries, opts, report)

	notice := fmt.Sprintf("%stclaude morph%s History carried over from %s to %s. Tool records are history; use your own tools.", markOpen, markClose, describe(t.Source.Harness, t.Source.Model), describe(opts.Target, opts.TargetModel))
	if report.TranscriptPath != "" {
		notice += " Full tool output: " + report.TranscriptPath + "."
	}
	for _, c := range Categories {
		report.Categories = append(report.Categories, *usage[c])
	}
	notice += cutNote(report, opts.operatorCarry())
	if opts.SummaryUnavailable {
		report.Add(LossNotCarried, 1, "source could not write a history summary; explicitly continued with readable history and the full-transcript pointer")
		report.SelectionReason = "source could not write a summary; readable history only, full transcript saved"
		if report.Count(LossEncryptedSummary) > 0 {
			report.SelectionReason = "compaction summary unreadable; source could not write a handoff; readable history only, full transcript saved"
			notice += " Compaction summary unreadable; source could not write a handoff. Earlier context may be missing. Read the full transcript when needed."
		} else {
			notice += " Source could not write a summary; earlier context may be missing. Read the full transcript when needed."
		}
	}
	notice += " No need to re-read skills or re-verify the environment unless something looks wrong."
	entries = append(entries, planEntry{entry: Entry{Role: RoleUser, Origin: OriginSystem, Blocks: []Block{Text(notice)}}})
	entries = coalesce(entries)
	if entries[0].entry.Role != RoleUser {
		lead := planEntry{entry: Entry{Role: RoleUser, Origin: OriginSystem, Blocks: []Block{Text(markOpen + "the conversation continues from earlier history" + markClose)}}}
		entries = append([]planEntry{lead}, entries...)
	}

	for _, e := range entries {
		report.Carried += e.sources
		p.Entries = append(p.Entries, e.entry)
		report.CarriedEstimatedTokens += estimateTokens(e.entry)
		for i := range report.Categories {
			report.Categories[i].CarriedTokens += e.cats[report.Categories[i].Category]
		}
	}
	report.OverWindow = report.CarriedEstimatedTokens > report.ContextWindowTokens
	if pct := opts.BudgetPercent; report.BudgetSource != "auto" || report.WindowStated {
		if pct <= 0 || pct > 100 {
			pct = DefaultBudgetPercent
		}
		report.OverHeadroom = report.CarriedEstimatedTokens > report.ContextWindowTokens*int64(pct)/100
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

// MediaPath returns the file WriteMedia writes data to, when data is one of
// the plan's externalized attachments (the same bytes, not a copy), or "".
func (p *Plan) MediaPath(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	for _, m := range p.pendingMedia {
		if len(m.data) == len(data) && &m.data[0] == &data[0] {
			return m.path
		}
	}
	return ""
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
// The daemon's own morph request arrives as a native user turn. It must
// not replace task/latest pins. Human and peer inbox instructions still count.
func daemonControlTurn(e Entry) bool {
	if len(e.Blocks) == 0 {
		return false
	}
	for _, b := range e.Blocks {
		text := strings.TrimSpace(b.Text)
		header, _, found := strings.Cut(text, "]")
		if b.Kind != BlockText || !found || !strings.HasPrefix(header, "[system: new agent message") || strings.Contains(header, " from ") || !strings.HasSuffix(header, "; subject: Morph handoff requested") {
			return false
		}
	}
	return true
}

func coalesce(in []planEntry) []planEntry {
	var out []planEntry
	for _, e := range in {
		if n := len(out); n > 0 && out[n-1].entry.Role == e.entry.Role {
			last := &out[n-1]
			last.entry.Blocks = append(last.entry.Blocks, e.entry.Blocks...)
			last.sources += e.sources
			if len(e.cats) > 0 {
				if last.cats == nil {
					last.cats = map[Category]int64{}
				}
				for c, n := range e.cats {
					last.cats[c] += n
				}
			}
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

// fitBudget pins the task and latest instructions, then selects the newest
// complete human turns that fit beside a requested source-written summary.
func (p *Plan) fitBudget(entries []planEntry, opts Options, report *LossReport) []planEntry {
	// Reserve room for the notice and omission markers. Never intentionally
	// overflow the destination just because a single user turn is enormous.
	budget := opts.budgetTokens()
	reserve := min(int64(1024), budget/4)
	available := budget - reserve
	var total int64
	for _, e := range entries {
		total += estimateTokens(e.entry)
	}
	report.KeptPercent = 100
	if (total <= available || opts.CarryAll) && opts.Summary == "" {
		return entries
	}
	if opts.CarryAll {
		// Everything is carried; only the summary is added in front.
		available = max(available, total+estimateTokens(Entry{Blocks: []Block{Text(opts.Summary)}})+1024)
	}

	first, last := -1, -1
	for i, e := range entries {
		if e.turn && e.entry.Role == RoleUser {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	// Pin the original task and latest user instructions. If too large for
	// the inline budget, keep an explicit pointer to their full file record.
	pin := func(i int) planEntry {
		e := entries[i]
		if estimateTokens(e.entry) > max(int64(64), available/3) && opts.TranscriptPath != "" {
			label := "original task"
			if i == last {
				label = "latest user instructions"
			}
			e.entry.Blocks = []Block{Text("⟦Read the full " + label + " in " + opts.TranscriptPath + " before continuing; it is too large to inline safely.⟧")}
			e.cats = nil
			report.PinnedExternalized = true
		}
		return e
	}
	pinned := map[int]planEntry{}
	for _, i := range []int{first, last} {
		if i >= 0 {
			pinned[i] = pin(i)
		}
	}
	used := int64(0)
	for _, e := range pinned {
		used += estimateTokens(e.entry)
	}
	var summary *planEntry
	if opts.Summary != "" {
		e := planEntry{entry: Entry{Role: RoleUser, Origin: OriginSystem, Blocks: []Block{Text("⟦Source-written summary of earlier history⟧\n" + opts.Summary)}}}
		// The summary uses a reserved slot rather than displacing the tail
		// shown in the confirmed preview. A larger summary stays in its file.
		if estimateTokens(e.entry) > reserve/2 && opts.TranscriptPath != "" {
			e.entry.Blocks = []Block{Text("⟦Read the source-written summary in " + filepath.Join(filepath.Dir(opts.TranscriptPath), "summary.md") + " before continuing; it is too large to inline safely.⟧")}
		}
		summary = &e
		// Its tokens count against the budget: the tail is what fits
		// beside it, so a long summary never overflows the landing.
		used += estimateTokens(e.entry)
	}
	// Keep the newest complete human-turn groups that fit after the pins.
	keep := map[int]bool{}
	for i := range pinned {
		keep[i] = true
	}
	for end := len(entries); end > 0; {
		start := end - 1
		for start > 0 && !entries[start].turn {
			start--
		}
		cost := int64(0)
		for i := start; i < end; i++ {
			if !keep[i] {
				cost += estimateTokens(entries[i].entry)
			}
		}
		if used+cost > available {
			break
		}
		for i := start; i < end; i++ {
			keep[i] = true
		}
		used += cost
		end = start
	}
	omitted := 0
	for i, e := range entries {
		if !keep[i] {
			omitted += e.sources
		}
	}
	if omitted > 0 {
		report.Add(LossTruncatedBudget, omitted, "")
		report.DroppedPercent = int((int64(omitted)*100 + int64(report.SourceEntries) - 1) / max(int64(1), int64(report.SourceEntries)))
		report.KeptPercent = max(0, 100-report.DroppedPercent)
	}
	report.BudgetWarning = report.DroppedPercent >= 10 || report.PinnedExternalized
	report.NeedsSummary = report.DroppedPercent >= 25 || report.PinnedExternalized
	if report.NeedsSummary {
		report.SelectionReason = "large history cut: carry a source-written summary with the original task and latest instructions"
	} else if report.BudgetWarning {
		report.SelectionReason = "older history cut to fit the destination budget"
	}
	var out []planEntry
	if summary != nil {
		out = append(out, *summary)
	}
	if omitted > 0 {
		marker := fmt.Sprintf("⟦%d earlier entries were omitted to fit the context budget", omitted)
		if opts.TranscriptPath != "" {
			marker += "; read them in " + opts.TranscriptPath
		}
		out = append(out, planEntry{entry: Entry{Role: RoleUser, Origin: OriginSystem, Blocks: []Block{Text(marker + "⟧")}}})
	}
	for i, e := range entries {
		if keep[i] {
			if v, ok := pinned[i]; ok {
				e = v
			}
			out = append(out, e)
		}
	}
	return coalesce(out)
}

// Carried notices are plain native text on the next hop. Replace only our
// generated notice line, retaining adjacent user instructions and all history
// wrappers (tool records, summaries and file pointers remain useful context).
func withoutPriorMorphNotices(text string) string {
	lines := strings.Split(text, "\n")
	kept := lines[:0]
	var fence string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if fence != "" {
			kept = append(kept, line)
			if strings.HasPrefix(trimmed, fence) && strings.Trim(trimmed, fence[:1]+" \t") == "" {
				fence = ""
			}
			continue
		}
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			n := 3
			for n < len(trimmed) && trimmed[n] == trimmed[0] {
				n++
			}
			fence = trimmed[:n]
			kept = append(kept, line)
			continue
		}
		if strings.HasPrefix(line, "⟦tclaude morph⟧ History carried over from ") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}
