package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
)

// TranslateOptions configures a cross-harness history translation.
type TranslateOptions struct {
	// Policy is the target-side policy. Target and InlineMedia are filled
	// from the destination harness.
	Policy transcript.Options
	// ArtifactDir receives transcript.md (the full neutral transcript),
	// loss-report.json and media/ for externalized attachments. Empty skips
	// the files; markers then name no transcript path.
	ArtifactDir string
}

// MorphTranslateOptions returns the operator-configured policy defaults
// (agent.morph.history_budget_percent / tool_result_clip_bytes).
func MorphTranslateOptions() TranslateOptions {
	var o TranslateOptions
	if c, err := config.Load(); err == nil && c.Agent != nil && c.Agent.Morph != nil {
		o.Policy.BudgetPercent = c.Agent.Morph.HistoryBudgetPercent
		o.Policy.ToolResultClipBytes = c.Agent.Morph.ToolResultClipBytes
	}
	return o
}

// TranslateResult is an installed translation.
type TranslateResult struct {
	ConvID  string
	Cleanup func()
	Report  *transcript.LossReport
	Plan    *transcript.Plan
}

func translationPair(src, dst *Harness) error {
	switch {
	case !src.SupportsTranscript():
		return fmt.Errorf("harness %s cannot export a translatable history", src.Name)
	case !dst.SupportsTranscript():
		return fmt.Errorf("harness %s cannot install a translated history", dst.Name)
	}
	if _, ok := src.History.(StreamingHistoryTransfer); !ok {
		return fmt.Errorf("harness %s cannot stream its history", src.Name)
	}
	return nil
}

// DecodeHistory reads a conversation's native history into the neutral form.
func DecodeHistory(src *Harness, convID, cwd string) (*transcript.Transcript, error) {
	if !src.SupportsTranscript() {
		return nil, fmt.Errorf("harness %s cannot export a translatable history", src.Name)
	}
	streaming, ok := src.History.(StreamingHistoryTransfer)
	if !ok {
		return nil, fmt.Errorf("harness %s cannot stream its history", src.Name)
	}
	r, err := streaming.Open(convID, cwd)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	t, err := src.Transcript.Decode(r, convID)
	if err != nil {
		return nil, fmt.Errorf("decode %s history: %w", src.Name, err)
	}
	if t.Source.Cwd == "" {
		t.Source.Cwd = cwd
	}
	return t, nil
}

// PlanTranslation decodes src's history and applies dst's policy.
func PlanTranslation(src, dst *Harness, convID, cwd string, opts TranslateOptions) (*transcript.Transcript, *transcript.Plan, error) {
	if err := translationPair(src, dst); err != nil {
		return nil, nil, err
	}
	t, err := DecodeHistory(src, convID, cwd)
	if err != nil {
		return nil, nil, err
	}
	policy := opts.Policy
	policy.Target = dst.Name
	policy.InlineMedia = dst.Transcript.InlineMedia()
	if opts.ArtifactDir != "" {
		policy.MediaDir = filepath.Join(opts.ArtifactDir, "media")
		policy.TranscriptPath = filepath.Join(opts.ArtifactDir, "transcript.md")
	}
	return t, transcript.NewPlan(t, policy), nil
}

// PreviewTranslation is a dry run: decode, plan, encode and validate the
// result against dst's own importer, without installing or writing files.
// A failed encode/validate reports the context-file rung instead of native.
func PreviewTranslation(src, dst *Harness, convID, cwd string, opts TranslateOptions) (*transcript.LossReport, error) {
	_, plan, err := PlanTranslation(src, dst, convID, cwd, opts)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	plan.Report.Fallback = "native"
	sourceID, err := dst.Transcript.Encode(plan, cwd, &buf)
	if err == nil {
		err = dst.History.Validate(buf.Bytes(), sourceID)
	}
	if err != nil {
		plan.Report.Fallback = "context_file"
	}
	return plan.Report, nil
}

// TranslateHistory installs src's conversation as a new native dst
// conversation in cwd. The source history is only read. On error nothing is
// left installed, and the returned result still carries the loss report
// (Fallback "context_file") so the caller can take the next rung; the
// artifact files (transcript, report) are kept for it.
func TranslateHistory(src, dst *Harness, convID, cwd string, opts TranslateOptions) (*TranslateResult, error) {
	t, plan, err := PlanTranslation(src, dst, convID, cwd, opts)
	if err != nil {
		return nil, err
	}
	res := &TranslateResult{Report: plan.Report, Plan: plan}
	fail := func(err error) (*TranslateResult, error) {
		plan.Report.Fallback = "context_file"
		if opts.ArtifactDir != "" {
			_ = writeLossReport(plan.Report, opts.ArtifactDir)
		}
		return res, err
	}
	if opts.ArtifactDir != "" {
		if err := writeTranslationArtifacts(t, plan, opts.ArtifactDir); err != nil {
			return fail(err)
		}
	}
	encoded, err := os.CreateTemp("", "tclaude-morph-history-*")
	if err != nil {
		return fail(err)
	}
	defer func() { _ = encoded.Close(); _ = os.Remove(encoded.Name()) }()
	sourceID, err := dst.Transcript.Encode(plan, cwd, encoded)
	if err != nil {
		return fail(fmt.Errorf("encode %s history: %w", dst.Name, err))
	}
	if _, err := encoded.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	var id string
	var cleanup func()
	if streaming, ok := dst.History.(StreamingHistoryTransfer); ok {
		id, cleanup, err = streaming.ImportReader(encoded, sourceID, cwd)
	} else {
		raw, readErr := io.ReadAll(encoded)
		if readErr != nil {
			return fail(readErr)
		}
		id, cleanup, err = dst.History.Import(raw, sourceID, cwd)
	}
	if err != nil {
		return fail(fmt.Errorf("install translated %s history: %w", dst.Name, err))
	}
	plan.Report.Fallback = "native"
	if opts.ArtifactDir != "" {
		if err := writeLossReport(plan.Report, opts.ArtifactDir); err != nil {
			cleanup()
			return fail(err)
		}
	}
	res.ConvID, res.Cleanup = id, cleanup
	return res, nil
}

// InstallContextFile is the context-file rung: when the translated history
// cannot be installed natively, dst starts a minimal native conversation
// whose one turn points at briefing.md (the budgeted history as plain text)
// and transcript.md (everything) in artifactDir. plan is the failed
// translation's plan, so the briefing carries exactly what native would have.
func InstallContextFile(dst *Harness, plan *transcript.Plan, cwd, artifactDir string) (*TranslateResult, error) {
	if !dst.SupportsTranscript() {
		return nil, fmt.Errorf("harness %s cannot install a translated history", dst.Name)
	}
	if plan == nil || artifactDir == "" {
		return nil, errors.New("the context-file fallback needs a plan and an artifact directory")
	}
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		return nil, err
	}
	from := describeSource(plan.Source)
	briefing := filepath.Join(artifactDir, "briefing.md")
	head := "# Conversation carried over from " + from + " by tclaude morph\n\n" +
		"⟦tool call⟧ and ⟦tool result⟧ records are history, not tools you have.\n\n"
	if err := os.WriteFile(briefing, []byte(head+plan.Briefing()), 0o600); err != nil {
		return nil, err
	}
	report := *plan.Report
	report.Fallback = "context_file"
	notice := "⟦tclaude morph⟧ This conversation continues one from " + from + ". Its history could not be " +
		"installed natively, so it was saved to files instead. Before you continue, read " + briefing +
		" (the recent history)"
	if report.TranscriptPath != "" {
		notice += "; " + report.TranscriptPath + " has the full transcript if you need more"
	}
	notice += ". The tool records in them are history, not tools you have; use your own tools to act."
	stub := &transcript.Plan{
		Source: plan.Source, Target: dst.Name, TargetModel: plan.TargetModel, Title: plan.Title, Report: &report,
		Entries: []transcript.Entry{{Role: transcript.RoleUser, Origin: transcript.OriginHuman, Blocks: []transcript.Block{transcript.Text(notice)}}},
	}
	var buf bytes.Buffer
	sourceID, err := dst.Transcript.Encode(stub, cwd, &buf)
	if err != nil {
		return nil, fmt.Errorf("encode %s context-file history: %w", dst.Name, err)
	}
	var id string
	var cleanup func()
	if streaming, ok := dst.History.(StreamingHistoryTransfer); ok {
		id, cleanup, err = streaming.ImportReader(&buf, sourceID, cwd)
	} else {
		id, cleanup, err = dst.History.Import(buf.Bytes(), sourceID, cwd)
	}
	if err != nil {
		return nil, fmt.Errorf("install %s context-file history: %w", dst.Name, err)
	}
	if err := writeLossReport(&report, artifactDir); err != nil {
		cleanup()
		return nil, err
	}
	plan.Report.Fallback = report.Fallback
	return &TranslateResult{ConvID: id, Cleanup: cleanup, Report: &report, Plan: plan}, nil
}

func describeSource(src transcript.Source) string {
	if src.Model != "" {
		return src.Harness + " (" + src.Model + ")"
	}
	return src.Harness
}

func writeTranslationArtifacts(t *transcript.Transcript, plan *transcript.Plan, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "transcript.md"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := transcript.WriteMarkdown(t, f); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return plan.WriteMedia()
}

func writeLossReport(r *transcript.LossReport, dir string) error {
	if r == nil {
		return errors.New("no loss report")
	}
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "loss-report.json"), append(raw, '\n'), 0o600)
}
