package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
	"github.com/tofutools/tclaude/pkg/paf"
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
	// ContextFile skips the native rung and installs the context-file one
	// directly (it needs ArtifactDir).
	ContextFile bool
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
	plan, err := PlanTranscript(t, dst, opts)
	return t, plan, err
}

// PlanTranscript applies dst's policy to an already decoded transcript, for
// a history that does not live in an installed conversation (an imported
// PAF's neutral context, or a native blob decoded on import).
func PlanTranscript(t *transcript.Transcript, dst *Harness, opts TranslateOptions) (*transcript.Plan, error) {
	if !dst.SupportsTranscript() {
		return nil, fmt.Errorf("harness %s cannot install a translated history", dst.Name)
	}
	policy := opts.Policy
	policy.Target = dst.Name
	resolved, source := MorphContextWindow(dst, policy.TargetModel)
	if dst.Name == DefaultName && policy.NativeContextWindowTokens == 1_000_000 {
		resolved, source = 1_000_000, "Claude extended-context selection"
	}
	if policy.ContextWindowTokens <= 0 || (!strings.HasPrefix(source, "unknown model") && resolved < policy.ContextWindowTokens) {
		policy.ContextWindowTokens, policy.ContextWindowSource = resolved, source
	}
	policy.InlineMedia = dst.Transcript.InlineMedia()
	if opts.ArtifactDir != "" {
		policy.MediaDir = filepath.Join(opts.ArtifactDir, "media")
		policy.TranscriptPath = filepath.Join(opts.ArtifactDir, "transcript.md")
	}
	return transcript.NewPlan(t, policy), nil
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
	if plan.Report.CarriedEstimatedTokens > plan.Report.BudgetTokens {
		plan.Report.Fallback = "context_file"
		return plan.Report, nil
	}
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
	if plan.Report.CarriedEstimatedTokens > plan.Report.BudgetTokens {
		opts.ContextFile = true
	}
	if opts.ContextFile {
		res, err := InstallContextFile(dst, plan, cwd, opts.ArtifactDir)
		if err != nil {
			return fail(err)
		}
		return res, nil
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
	if plan == nil || artifactDir == "" {
		return nil, errors.New("the context-file fallback needs a plan and an artifact directory")
	}
	if err := prepareArtifactDir(artifactDir); err != nil {
		return nil, err
	}
	from := describeSource(plan.Source)
	briefing := filepath.Join(artifactDir, "briefing.md")
	head := "# Conversation carried over from " + from + " by tclaude morph\n\n" +
		"⟦tool call⟧ and ⟦tool result⟧ records are history, not tools you have.\n\n"
	if err := transcript.WriteFileExclusive(briefing, []byte(head+plan.Briefing())); err != nil {
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
	res, err := installNotice(dst, plan.Source, plan.Title, plan.TargetModel, &report, notice, cwd, artifactDir)
	if err != nil {
		return nil, fmt.Errorf("install %s context-file history: %w", dst.Name, err)
	}
	plan.Report.Fallback = report.Fallback
	res.Plan = plan
	return res, nil
}

// InstallHandoff is the handoff rung: dst starts a minimal native
// conversation whose one turn points at handoff.md, the handoff the source
// agent wrote for its successor. When the source history can still be
// decoded its full transcript is saved beside it for reference; when it
// cannot, the handoff is all that is carried and the report says so.
func InstallHandoff(src, dst *Harness, convID, cwd, handoff string, opts TranslateOptions) (*TranslateResult, error) {
	if !dst.SupportsTranscript() {
		return nil, fmt.Errorf("harness %s cannot install a translated history", dst.Name)
	}
	if strings.TrimSpace(handoff) == "" || opts.ArtifactDir == "" {
		return nil, errors.New("the handoff rung needs a handoff and an artifact directory")
	}
	if err := prepareArtifactDir(opts.ArtifactDir); err != nil {
		return nil, err
	}
	source := transcript.Source{Harness: src.Name, ConvID: convID, Cwd: cwd}
	report := &transcript.LossReport{Source: src.Name, Target: dst.Name}
	title, targetModel := "", opts.Policy.TargetModel
	t, plan, planErr := PlanTranslation(src, dst, convID, cwd, opts)
	if planErr == nil {
		if err := writeTranslationArtifacts(t, plan, opts.ArtifactDir); err != nil {
			return nil, err
		}
		source, title, targetModel, report = plan.Source, plan.Title, plan.TargetModel, plan.Report
	} else {
		report.Add(transcript.LossHistoryUndecodable, 1, "")
	}
	path := filepath.Join(opts.ArtifactDir, "handoff.md")
	if err := transcript.WriteFileExclusive(path, []byte(handoff)); err != nil {
		return nil, err
	}
	report.Fallback = "handoff"
	notice := "⟦tclaude morph⟧ This conversation continues one from " + describeSource(source) +
		". Before the switch, the agent you are continuing wrote a handoff for you. Read " + path +
		" before you continue"
	if report.TranscriptPath != "" {
		notice += "; " + report.TranscriptPath + " has the full earlier transcript if you need more, and its " +
			"tool records are history, not tools you have"
	}
	notice += ". Use your own tools to act."
	res, err := installNotice(dst, source, title, targetModel, report, notice, cwd, opts.ArtifactDir)
	if err != nil {
		return nil, fmt.Errorf("install %s handoff history: %w", dst.Name, err)
	}
	res.Plan = plan
	return res, nil
}

func prepareArtifactDir(dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	return transcript.EnsureRealDir(dir)
}

// installNotice installs a native dst conversation of one user turn, the
// notice, and writes the report beside it.
func installNotice(dst *Harness, source transcript.Source, title, targetModel string, report *transcript.LossReport,
	notice, cwd, artifactDir string) (*TranslateResult, error) {
	if !dst.SupportsTranscript() {
		return nil, fmt.Errorf("harness %s cannot install a translated history", dst.Name)
	}
	stub := &transcript.Plan{
		Source: source, Target: dst.Name, TargetModel: targetModel, Title: title, Report: report,
		Entries: []transcript.Entry{{Role: transcript.RoleUser, Origin: transcript.OriginHuman, Blocks: []transcript.Block{transcript.Text(notice)}}},
	}
	var buf bytes.Buffer
	sourceID, err := dst.Transcript.Encode(stub, cwd, &buf)
	if err != nil {
		return nil, err
	}
	var id string
	var cleanup func()
	if streaming, ok := dst.History.(StreamingHistoryTransfer); ok {
		id, cleanup, err = streaming.ImportReader(&buf, sourceID, cwd)
	} else {
		id, cleanup, err = dst.History.Import(buf.Bytes(), sourceID, cwd)
	}
	if err != nil {
		return nil, err
	}
	if err := writeLossReport(report, artifactDir); err != nil {
		cleanup()
		return nil, err
	}
	return &TranslateResult{ConvID: id, Cleanup: cleanup, Report: report}, nil
}

func describeSource(src transcript.Source) string {
	if src.Model != "" {
		return src.Harness + " (" + src.Model + ")"
	}
	return src.Harness
}

func writeTranslationArtifacts(t *transcript.Transcript, plan *transcript.Plan, dir string) error {
	if err := prepareArtifactDir(dir); err != nil {
		return err
	}
	var md bytes.Buffer
	if err := transcript.WriteMarkdown(t, &md); err != nil {
		return err
	}
	if err := transcript.WriteFileExclusive(filepath.Join(dir, "transcript.md"), md.Bytes()); err != nil {
		return err
	}
	if err := writeContextArtifact(t, dir); err != nil {
		return err
	}
	return plan.WriteMedia()
}

var contextMediaExt = map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp", "application/pdf": ".pdf"}

// writeContextArtifact writes the full source transcript as a PAF context
// file, transcript.jsonl, with inline media stored under context-media/ and
// referenced by path. The loss counts it adds stay on a copy, so the
// translation's own report is unchanged.
func writeContextArtifact(t *transcript.Transcript, dir string) error {
	cp := *t
	cp.Losses = &transcript.LossReport{}
	mediaDir := filepath.Join(dir, "context-media")
	n := 0
	var buf bytes.Buffer
	err := paf.WriteContextFile(&buf, paf.ScopeFull, &cp, func(m *transcript.Media) (string, error) {
		if n == 0 {
			if err := transcript.EnsureRealDir(mediaDir); err != nil {
				return "", err
			}
		}
		n++
		ext := contextMediaExt[m.MediaType]
		if ext == "" {
			ext = ".bin"
			if exts, _ := mime.ExtensionsByType(m.MediaType); len(exts) > 0 {
				ext = exts[0]
			}
		}
		path := filepath.Join(mediaDir, fmt.Sprintf("media-%03d%s", n, ext))
		return path, transcript.WriteFileExclusive(path, m.Data)
	})
	if err != nil {
		return err
	}
	return transcript.WriteFileExclusive(filepath.Join(dir, "transcript.jsonl"), buf.Bytes())
}

// writeLossReport replaces loss-report.json through a fresh temp file and a
// rename, which replaces a planted symlink rather than following it.
func writeLossReport(r *transcript.LossReport, dir string) error {
	if r == nil {
		return errors.New("no loss report")
	}
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".loss-report-*")
	if err != nil {
		return err
	}
	_, writeErr := tmp.Write(append(raw, '\n'))
	closeErr := tmp.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, "loss-report.json")); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}
