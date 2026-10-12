package agentd

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
	"github.com/tofutools/tclaude/pkg/paf"
)

// Landing rungs an imported conversation can take. Rung 4, reading the
// archive without any harness, is `tclaude paf render`, not an import.
const (
	landingNative     = "native"
	landingTranslated = "translated"
	landingNone       = "none"
)

// historyLanding says how an imported agent's conversation lands.
type historyLanding struct {
	SourceHarness string `json:"source_harness"`
	SourceModel   string `json:"source_model,omitempty"`
	Harness       string `json:"harness"`
	Model         string `json:"model,omitempty"`
	// Rung is native (the history installs as it is), translated (the
	// conversation is rewritten for the landing harness and model, within
	// its context budget) or none (configuration only).
	Rung   string                 `json:"rung"`
	Losses *transcript.LossReport `json:"losses,omitempty"`

	history *bundleHistoryLaunch
	cleanup func()
	// full is the whole conversation when the landing cut some of it, kept
	// so it can be saved beside the landed agent.
	full *transcript.Transcript
}

// landingOptions are a landing's choices beyond harness and model.
type landingOptions struct {
	// Skip lands configuration only.
	Skip bool
	// BudgetNative checks a native history against the landing model's
	// context budget too, and translates and trims it when it does not
	// fit. Imports set it; a teleport or move keeps the history as it is.
	BudgetNative bool
	// Summary is the agent-written handoff the PAF carries. It stands in
	// for the earlier history when the landing has to cut it.
	Summary string
}

// cutPercent is how much of the conversation a landing leaves out.
func (l *historyLanding) cutPercent() int {
	if l == nil || l.Losses == nil {
		return 0
	}
	return l.Losses.DroppedPercent
}

func (l *historyLanding) close() {
	if l != nil && l.cleanup != nil {
		l.cleanup()
	}
}

// planHistoryLanding picks the most faithful rung for b's conversation on
// dst. A native history in dst's own format installs unchanged. Otherwise
// the conversation, from the PAF's neutral context or decoded from the
// native history, is translated into dst's format under the same budget a
// morph uses. When neither works the agent imports as configuration, and
// the returned warnings say why. cwd is the landing working directory.
func planHistoryLanding(b *agentbundle.Bundle, dst *harness.Harness, model, cwd string, neutral func(paf.ReadOptions) (*transcript.Transcript, error), o landingOptions) (*historyLanding, []string, error) {
	l := &historyLanding{SourceHarness: b.Manifest.Agent.Harness, Harness: dst.Name, Model: model, Rung: landingNone}
	if o := b.Manifest.Agent.Origin; o != nil {
		l.SourceModel = o.Model
	}
	native := b.Manifest.History
	if o.Skip || (native == nil && neutral == nil) {
		return l, nil, nil
	}
	var warnings []string
	// fitsNative is a native history the budget turned away; it still lands
	// as it is when translating it cannot produce anything that fits.
	var fitsNative *bundleHistoryLaunch
	if native != nil && dst.SupportsHistoryTransfer() && native.Format == dst.History.Format() {
		if err := validateBundleHistory(dst, b); err != nil {
			return nil, nil, err
		}
		over := ""
		if o.BudgetNative {
			over = nativeOverBudget(b, dst, model, neutral)
		}
		if over == "" {
			l.Rung = landingNative
			l.history = &bundleHistoryLaunch{Raw: b.Transcript, Path: b.TranscriptPath, SourceID: native.SourceConvID, Format: native.Format}
			return l, nil, nil
		}
		warnings = append(warnings, over)
		fitsNative = &bundleHistoryLaunch{Raw: b.Transcript, Path: b.TranscriptPath, SourceID: native.SourceConvID, Format: native.Format}
	}
	asNative := func(why string) (*historyLanding, []string, error) {
		l.Rung, l.history, l.Losses = landingNative, fitsNative, nil
		return l, append(slices.DeleteFunc(warnings, func(w string) bool { return strings.HasPrefix(w, "the native history (about") }), "the native history exceeds the "+landingModelLabel(dst, model)+" context budget, but "+why+", so it lands as it is and the harness compacts it"), nil
	}
	if !dst.SupportsTranscript() {
		return l, append(warnings, dst.DisplayName+" cannot resume imported history; importing config only"), nil
	}
	t, err := importedTranscript(b, neutral)
	if err != nil {
		return l, append(warnings, "the conversation could not be read ("+err.Error()+"); importing config only"), nil
	}
	// With no model at all the budget would be a blind guess; a named model
	// whose window is unknown is budgeted conservatively, as morph does.
	if _, source := harness.MorphContextWindow(dst, model); model == "" && strings.HasPrefix(source, "unknown model") {
		return l, append(warnings, fmt.Sprintf("no landing model is known for %s, so the conversation cannot be budgeted; name one with --model, or it imports as config only", dst.DisplayName)), nil
	}
	opts := harness.MorphTranslateOptions()
	opts.Policy.TargetModel = model
	plan, err := harness.PlanTranscript(t, dst, opts)
	if err == nil && plan.Report.DroppedPercent > 0 && strings.TrimSpace(o.Summary) != "" {
		// The agent's own summary of its earlier work stands in for the
		// part that is cut, as a morph's source-written summary does.
		sopts := opts
		sopts.Policy.Summary = o.Summary
		if splan, serr := harness.PlanTranscript(t, dst, sopts); serr == nil && splan.Report.CarriedEstimatedTokens <= splan.Report.BudgetTokens {
			plan, opts = splan, sopts
		} else {
			warnings = append(warnings, "the agent's summary is too large to carry inside the context budget, so it is left out; it stays in the file as context/handoff.md")
		}
	}
	if err != nil {
		if fitsNative != nil {
			return asNative("it could not be planned for translation (" + err.Error() + ")")
		}
		return l, append(warnings, err.Error()+"; importing config only"), nil
	}
	l.Losses = plan.Report
	if plan.Report.CarriedEstimatedTokens > plan.Report.BudgetTokens {
		if fitsNative != nil {
			return asNative("even a trimmed translation does not fit")
		}
		return l, append(warnings, fmt.Sprintf("the conversation does not fit the %s context budget even after trimming; importing config only. Read it with `tclaude paf render`.", dst.DisplayName)), nil
	}
	if cwd == "" {
		cwd = os.TempDir()
	}
	f, err := os.CreateTemp("", ".agent-import-translated-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = f.Close(); _ = os.Remove(f.Name()) }
	sourceID, err := dst.Transcript.Encode(plan, cwd, f)
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err == nil {
		if streaming, ok := dst.History.(harness.StreamingHistoryTransfer); ok {
			err = streaming.ValidateReader(boundedHistoryReader{f, agentRecordLimit()}, sourceID)
		} else {
			var raw []byte
			if raw, err = io.ReadAll(f); err == nil {
				err = dst.History.Validate(raw, sourceID)
			}
		}
	}
	if err != nil {
		cleanup()
		if fitsNative != nil {
			return asNative("it could not be translated (" + err.Error() + ")")
		}
		return l, append(warnings, "the conversation could not be translated for "+dst.DisplayName+" ("+err.Error()+"); importing config only"), nil
	}
	plan.Report.Fallback = "native"
	l.Rung, l.cleanup = landingTranslated, cleanup
	l.history = &bundleHistoryLaunch{Path: f.Name(), SourceID: sourceID, Format: dst.History.Format()}
	if pct := plan.Report.DroppedPercent; pct > 0 {
		l.full = t
		if pct >= 10 {
			kept := "the pinned start, compaction summaries and the newest turns are kept"
			if opts.Policy.Summary != "" {
				kept += ", with the agent's own summary standing in for the rest"
			}
			warnings = append(warnings, fmt.Sprintf("landing on %s cuts about %d%% of the conversation to fit its context budget: %s. The full history stays in the PAF and is saved beside the agent.", landingModelLabel(dst, model), pct, kept))
		}
	}
	return l, warnings, nil
}

// importedTranscript is the conversation in neutral form: the PAF's own
// neutral context when it has one, else the native history decoded by its
// harness's codec, which needs no installed harness.
func importedTranscript(b *agentbundle.Bundle, neutral func(paf.ReadOptions) (*transcript.Transcript, error)) (*transcript.Transcript, error) {
	if neutral != nil {
		return neutral(paf.ReadOptions{})
	}
	native := b.Manifest.History
	src, err := harness.Resolve(b.Manifest.Agent.Harness)
	if err != nil {
		return nil, err
	}
	if src.Transcript == nil || !src.SupportsHistoryTransfer() || src.History.Format() != native.Format {
		return nil, fmt.Errorf("tclaude cannot read %s history", native.Format)
	}
	r, err := b.OpenHistory()
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return src.Transcript.Decode(boundedHistoryReader{r, agentRecordLimit()}, native.SourceConvID)
}

// nativeOverBudget explains why a native history does not fit the landing
// model's context budget, or is empty when it fits or cannot be measured:
// an unknown model, or a history tclaude cannot decode, keeps the native
// rung as before.
func nativeOverBudget(b *agentbundle.Bundle, dst *harness.Harness, model string, neutral func(paf.ReadOptions) (*transcript.Transcript, error)) string {
	if model == "" || !dst.SupportsTranscript() {
		return ""
	}
	if _, source := harness.MorphContextWindow(dst, model); strings.HasPrefix(source, "unknown model") {
		return ""
	}
	t, err := importedTranscript(b, neutral)
	if err != nil {
		return ""
	}
	opts := harness.MorphTranslateOptions()
	opts.Policy.TargetModel = model
	plan, err := harness.PlanTranscript(t, dst, opts)
	if err != nil || plan.Report.SourceEstimatedTokens <= plan.Report.BudgetTokens {
		return ""
	}
	r := plan.Report
	return fmt.Sprintf("the native history (about %dk tokens) exceeds the %s context budget (%dk of a %dk-token window), so it lands translated and trimmed instead of as it is", r.SourceEstimatedTokens/1000, landingModelLabel(dst, model), r.BudgetTokens/1000, r.ContextWindowTokens/1000)
}

// landsOnSmallerWindow says whether a native history should be budgeted:
// it lands on a context window smaller than the one it ran in, for example
// opus[1m] onto opus at 200k. Equal windows keep native fidelity, as does
// the same model when a window is unknown. When the source window is
// unknown (an older bundle names no model), a different landing model is
// budgeted by size: nativeOverBudget estimates the history's tokens, and
// only a history over the landing budget is trimmed; one that fits still
// lands natively.
func landsOnSmallerWindow(dst *harness.Harness, origin *agentbundle.Origin, model string) bool {
	source := ""
	if origin != nil {
		source = origin.Model
	}
	landing, lsrc := harness.MorphContextWindow(dst, model)
	from, fsrc := harness.MorphContextWindow(dst, source)
	if source == "" || strings.HasPrefix(fsrc, "unknown model") {
		return source != model
	}
	if strings.HasPrefix(lsrc, "unknown model") {
		return false
	}
	return landing < from
}

func landingModelLabel(dst *harness.Harness, model string) string {
	if model == "" {
		return dst.DisplayName
	}
	return dst.DisplayName + " " + model
}
