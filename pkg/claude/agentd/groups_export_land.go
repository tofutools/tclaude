package agentd

import (
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"sort"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/groupexport"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/paf/legacy"
)

// groups_export_land.go decides where each member of an imported group
// lands. A Claude Code conversation landing on Claude Code takes the group
// import's own file path (path rewriting, retitling). Every other member
// lands through the agent import rungs (planHistoryLanding): its native
// history when the landing harness reads that format, else the
// conversation translated for the landing harness and model, else
// configuration only.

// groupLandingOptions are the importer's landing choices.
type groupLandingOptions struct {
	// Harness and Model apply to every member without its own choice.
	// Empty Harness keeps each member on its source harness.
	Harness, Model string
	// MemberHarness and MemberModel are keyed by member: its title, its
	// agent key in a group PAF, or its conversation id.
	MemberHarness, MemberModel map[string]string
	// ConfirmHistoryLoss accepts members' large history cuts.
	ConfirmHistoryLoss bool
}

// parseGroupLandingOptions reads harness, model, and the repeatable
// member_harness / member_model (member=value) query parameters.
func parseGroupLandingOptions(q url.Values) (groupLandingOptions, error) {
	o := groupLandingOptions{
		Harness: strings.TrimSpace(q.Get("harness")), Model: strings.TrimSpace(q.Get("model")),
		MemberHarness: map[string]string{}, MemberModel: map[string]string{},
		ConfirmHistoryLoss: q.Get("confirm_history_loss") == "true",
	}
	for param, into := range map[string]map[string]string{"member_harness": o.MemberHarness, "member_model": o.MemberModel} {
		for _, binding := range q[param] {
			member, value, ok := strings.Cut(binding, "=")
			member, value = strings.TrimSpace(member), strings.TrimSpace(value)
			if !ok || member == "" || value == "" {
				return o, fmt.Errorf("%s needs member=value, got %q", param, binding)
			}
			into[member] = value
		}
	}
	return o, nil
}

// groupMemberLanding is one member's landing plan.
type groupMemberLanding struct {
	ConvID string `json:"conv_id"`
	Title  string `json:"title"`
	*historyLanding
	Warnings []string `json:"warnings,omitempty"`

	// fileLanding is the Claude Code file path of the group import.
	fileLanding bool
	dst         *harness.Harness
}

func closeGroupLandings(ls []*groupMemberLanding) {
	for _, l := range ls {
		l.close()
	}
}

// planGroupLandings plans every conversation of exp. a is the group PAF the
// export was read from (nil for a zip); cwd is the import target. The
// caller closes the plans.
func planGroupLandings(exp *groupexport.Export, a *paf.Archive, opts groupLandingOptions, cwd string) ([]*groupMemberLanding, error) {
	names := map[string]bool{}
	for _, c := range exp.Convs {
		names[c.ConvID], names[c.Title] = true, true
		if c.AgentKey != "" {
			names[c.AgentKey] = true
		}
	}
	for _, m := range []map[string]string{opts.MemberHarness, opts.MemberModel} {
		for member := range m {
			if !names[member] {
				return nil, fmt.Errorf("no member %q in the archive", member)
			}
		}
	}
	pick := func(m map[string]string, c groupexport.Conv, fallback string) (string, bool) {
		for _, k := range []string{c.ConvID, c.AgentKey, c.Title} {
			if v, ok := m[k]; ok && k != "" {
				return v, true
			}
		}
		return fallback, fallback != ""
	}

	var out []*groupMemberLanding
	fail := func(err error) ([]*groupMemberLanding, error) {
		closeGroupLandings(out)
		return nil, err
	}
	for i := range exp.Convs {
		c := &exp.Convs[i]
		src := c.Harness
		if src == "" {
			src = harness.DefaultName
		}
		format := c.HistoryFormat
		if format == "" && src == harness.DefaultName && c.Content != nil {
			format = legacy.ClaudeHistoryFormat
		}
		dstName, explicit := pick(opts.MemberHarness, *c, opts.Harness)
		if dstName == "" {
			dstName = src
		}
		model, _ := pick(opts.MemberModel, *c, opts.Model)
		l := &groupMemberLanding{ConvID: c.ConvID, Title: c.Title, historyLanding: &historyLanding{SourceHarness: src, Harness: dstName, Model: model, Rung: landingNone}}
		out = append(out, l)

		if c.Missing {
			// No conversation travelled: the member imports as
			// configuration, as a zip's missing conversation always has.
			// Off Claude Code it still records its harness, so it resumes
			// there.
			if dstName == harness.DefaultName {
				l.fileLanding = true
				continue
			}
			dst, err := harness.Resolve(dstName)
			if err != nil || dst.UsesCommandInput() {
				if explicit {
					return fail(fmt.Errorf("member %q: unsupported landing harness %q", c.Title, dstName))
				}
				l.fileLanding, l.Harness = true, harness.DefaultName
				l.Warnings = append(l.Warnings, fmt.Sprintf("harness %q is unknown here; imports as Claude Code configuration", src))
				continue
			}
			l.dst, l.Harness = dst, dst.Name
			continue
		}
		neutral := memberNeutralContext(a, c.AgentKey)
		if dstName == harness.DefaultName && (format == legacy.ClaudeHistoryFormat || src == harness.DefaultName && neutral == nil) {
			// The group import's own path: unchanged for every zip.
			l.fileLanding, l.Rung = true, landingNative
			continue
		}
		dst, err := harness.ResolveSpawnable(dstName)
		if err != nil || dst.UsesCommandInput() {
			if explicit {
				return fail(fmt.Errorf("member %q: unsupported landing harness %q", c.Title, dstName))
			}
			return fail(fmt.Errorf("member %q uses harness %q, which is not available here; choose a landing harness with --harness or --member-harness %s=<harness>", c.Title, src, c.Title))
		}
		l.dst, l.Harness = dst, dst.Name
		if dst.Name == harness.OpenCodeName {
			// OpenCode keeps history in per-agent state that only a spawn
			// prepares; installing into the shared store would strand it.
			l.Warnings = append(l.Warnings, "group import cannot install OpenCode history yet; importing config only")
			continue
		}
		b := &agentbundle.Bundle{}
		b.Manifest.Agent.Harness = src
		if c.Content != nil && format != "" {
			b.SetHistory(format, c.ConvID, c.Content)
		}
		landing, warnings, err := planHistoryLanding(b, dst, model, cwd, neutral, landingOptions{BudgetNative: true})
		if err != nil {
			return fail(fmt.Errorf("member %q: %w", c.Title, err))
		}
		l.historyLanding = landing
		l.Warnings = warnings
		if dst.Name != src {
			l.Warnings = append(l.Warnings, fmt.Sprintf("lands on %s instead of %s; receiver defaults apply at its next launch", dst.Name, src))
		}
		if l.needsCutConfirm() && !opts.ConfirmHistoryLoss {
			l.Warnings = append(l.Warnings, fmt.Sprintf("cutting %d%% of the conversation needs confirmation: --confirm-history-loss", l.cutPercent()))
		}
	}
	return out, nil
}

// needsCutConfirm says the member's landing cuts so much of its conversation
// that the importer must accept it, as an agent import must.
func (l *groupMemberLanding) needsCutConfirm() bool {
	return l.history != nil && l.Losses != nil && l.Losses.NeedsSummary
}

// unconfirmedCuts refuses the landings when any needs a confirmation that was
// not given. The refusal names each member and its cut.
func unconfirmedCuts(ls []*groupMemberLanding, confirmed bool) error {
	if confirmed {
		return nil
	}
	var cuts []string
	for _, l := range ls {
		if l.needsCutConfirm() {
			cuts = append(cuts, fmt.Sprintf("%s about %d%%", l.Title, l.cutPercent()))
		}
	}
	if len(cuts) == 0 {
		return nil
	}
	sort.Strings(cuts)
	return &groupHistoryCutError{cuts}
}

// groupHistoryCutError is the import's refusal of unconfirmed history cuts.
type groupHistoryCutError struct{ cuts []string }

func (e *groupHistoryCutError) Error() string {
	return "landing cuts the conversation of " + strings.Join(e.cuts, ", ") + ". Confirm with --confirm-history-loss (the full history stays in the file and is saved beside each agent), or choose a model with a larger context window."
}

// saveCutMemberHistories saves the whole conversation of every member whose
// landing cut it, beside the agent, as an agent import does.
func saveCutMemberHistories(ls []*groupMemberLanding, exp *groupexport.Export, convRemap map[string]string, group string) {
	g, err := db.GetAgentGroupByName(group)
	if err != nil {
		slog.Warn("import: save full history", "group", group, "error", err)
	}
	for i, l := range ls {
		if l.full != nil {
			saveLandedHistory(agent.SpawnResponse{ConvID: convRemap[exp.Convs[i].ConvID]}, l.historyLanding, g)
		}
	}
}

// memberNeutralContext reads a group PAF member's neutral context, or is
// nil when it has none.
func memberNeutralContext(a *paf.Archive, key string) func(paf.ReadOptions) (*transcript.Transcript, error) {
	if a == nil || key == "" || !a.HasContext(key, paf.ScopeEffective) {
		return nil
	}
	return func(o paf.ReadOptions) (*transcript.Transcript, error) {
		return paf.ReadTranscript(a, key, paf.ScopeEffective, o)
	}
}

// installGroupLanding installs a planned non-file landing through the
// landing harness's history import, returning the minted conversation id.
func installGroupLanding(l *groupMemberLanding, cwd string) (string, func(), error) {
	return importBundleHistory(l.dst, l.history, cwd)
}

// landingWarnings flattens the plans' warnings, member by member.
func landingWarnings(ls []*groupMemberLanding) []string {
	var out []string
	for _, l := range ls {
		for _, w := range l.Warnings {
			out = append(out, l.Title+": "+w)
		}
	}
	sort.Strings(out)
	return out
}

// exportMemberHistory reads a non-Claude-Code member's native history for a
// group PAF. ok is false when the member's harness cannot export history.
func exportMemberHistory(c *groupexport.Conv) (bool, error) {
	h := harnessForConv(c.ConvID)
	if rows, err := db.FindSessionsByConvID(c.ConvID); err == nil && len(rows) == 0 {
		if row, err := db.GetConvIndex(c.ConvID); err == nil && row != nil && row.Harness != "" {
			if resolved, err := harness.Resolve(row.Harness); err == nil {
				h = resolved
			}
		}
	}
	if h.Name == harness.DefaultName || !h.SupportsHistoryTransfer() {
		return false, nil
	}
	cwd := c.SourceCwd
	if cwd == "" {
		if row, err := db.GetConvIndex(c.ConvID); err == nil && row != nil {
			cwd = row.ProjectPath
		}
	}
	b := &agentbundle.Bundle{MaxBytes: maxImportArchiveBytes}
	if err := snapshotAgentHistory(b, h, c.ConvID, cwd); err != nil {
		return false, err
	}
	defer func() { _ = b.Close() }()
	r, err := b.OpenHistory()
	if err != nil {
		return false, err
	}
	defer func() { _ = r.Close() }()
	raw, err := io.ReadAll(r)
	if err != nil {
		return false, err
	}
	c.Content, c.Harness, c.HistoryFormat, c.SourceCwd = raw, h.Name, b.Manifest.History.Format, cwd
	return true, nil
}
