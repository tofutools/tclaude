package agentd

import (
	"fmt"
	"io"
	"os"
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
func planHistoryLanding(b *agentbundle.Bundle, dst *harness.Harness, model, cwd string, neutral func(paf.ReadOptions) (*transcript.Transcript, error), skip bool) (*historyLanding, []string, error) {
	l := &historyLanding{SourceHarness: b.Manifest.Agent.Harness, Harness: dst.Name, Model: model, Rung: landingNone}
	if o := b.Manifest.Agent.Origin; o != nil {
		l.SourceModel = o.Model
	}
	native := b.Manifest.History
	if skip || (native == nil && neutral == nil) {
		return l, nil, nil
	}
	if native != nil && dst.SupportsHistoryTransfer() && native.Format == dst.History.Format() {
		if err := validateBundleHistory(dst, b); err != nil {
			return nil, nil, err
		}
		l.Rung = landingNative
		l.history = &bundleHistoryLaunch{Raw: b.Transcript, Path: b.TranscriptPath, SourceID: native.SourceConvID, Format: native.Format}
		return l, nil, nil
	}
	if !dst.SupportsTranscript() {
		return l, []string{dst.DisplayName + " cannot resume imported history; importing config only"}, nil
	}
	t, err := importedTranscript(b, neutral)
	if err != nil {
		return l, []string{"the conversation could not be read (" + err.Error() + "); importing config only"}, nil
	}
	// With no model at all the budget would be a blind guess; a named model
	// whose window is unknown is budgeted conservatively, as morph does.
	if _, source := harness.MorphContextWindow(dst, model); model == "" && strings.HasPrefix(source, "unknown model") {
		return l, []string{fmt.Sprintf("no landing model is known for %s, so the conversation cannot be budgeted; name one with --model, or it imports as config only", dst.DisplayName)}, nil
	}
	opts := harness.MorphTranslateOptions()
	opts.Policy.TargetModel = model
	plan, err := harness.PlanTranscript(t, dst, opts)
	if err != nil {
		return l, []string{err.Error() + "; importing config only"}, nil
	}
	l.Losses = plan.Report
	if plan.Report.CarriedEstimatedTokens > plan.Report.BudgetTokens {
		return l, []string{fmt.Sprintf("the conversation does not fit the %s context budget even after trimming; importing config only. Read it with `tclaude paf render`.", dst.DisplayName)}, nil
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
		return l, []string{"the conversation could not be translated for " + dst.DisplayName + " (" + err.Error() + "); importing config only"}, nil
	}
	plan.Report.Fallback = "native"
	l.Rung, l.cleanup = landingTranslated, cleanup
	l.history = &bundleHistoryLaunch{Path: f.Name(), SourceID: sourceID, Format: dst.History.Format()}
	return l, nil, nil
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
