package transcript

import (
	"fmt"
	"sort"
	"strings"
)

// Severity grades a loss. Warn means the target agent may notice: content it
// could reasonably need did not carry over in its original form.
type Severity string

const (
	SeverityInfo Severity = "info"
	SeverityWarn Severity = "warn"
)

// LossKind names one category of translation loss.
type LossKind string

const (
	LossReasoningDropped   LossKind = "reasoning_dropped"
	LossToolCallsFlattened LossKind = "tool_calls_flattened"
	LossToolOutputClipped  LossKind = "tool_output_clipped"
	LossInterruptedCall    LossKind = "interrupted_tool_call"
	LossMediaExternalized  LossKind = "media_externalized"
	LossMediaDropped       LossKind = "media_dropped"
	LossHarnessContext     LossKind = "harness_context_dropped"
	LossClearedHistory     LossKind = "cleared_history"
	LossPreCompaction      LossKind = "pre_compaction_history"
	LossEncryptedSummary   LossKind = "encrypted_compaction_summary"
	LossInactiveBranch     LossKind = "inactive_branch"
	LossSidechain          LossKind = "sidechain_dropped"
	LossTruncatedBudget    LossKind = "truncated_for_budget"
	LossUnknownRecord      LossKind = "unknown_records"
	LossNotCarried         LossKind = "not_carried"
	LossHistoryUndecodable LossKind = "history_undecodable"
)

var lossSeverity = map[LossKind]Severity{
	LossReasoningDropped:   SeverityInfo,
	LossToolCallsFlattened: SeverityInfo,
	LossToolOutputClipped:  SeverityWarn,
	LossInterruptedCall:    SeverityWarn,
	LossMediaExternalized:  SeverityWarn,
	LossMediaDropped:       SeverityWarn,
	LossHarnessContext:     SeverityInfo,
	LossClearedHistory:     SeverityInfo,
	LossPreCompaction:      SeverityInfo,
	LossEncryptedSummary:   SeverityWarn,
	LossInactiveBranch:     SeverityInfo,
	LossSidechain:          SeverityInfo,
	LossTruncatedBudget:    SeverityWarn,
	LossUnknownRecord:      SeverityWarn,
	LossNotCarried:         SeverityInfo,
	LossHistoryUndecodable: SeverityWarn,
}

var lossText = map[LossKind]string{
	LossReasoningDropped:   "reasoning/thinking blocks not carried (provider-bound)",
	LossToolCallsFlattened: "tool calls carried as text records",
	LossToolOutputClipped:  "tool outputs clipped (full text in the transcript file)",
	LossInterruptedCall:    "tool calls without a recorded result",
	LossMediaExternalized:  "images/documents saved to files and referenced by path",
	LossMediaDropped:       "images/documents dropped",
	LossHarnessContext:     "harness-injected context/system entries not carried",
	LossClearedHistory:     "entries removed by a conversation clear",
	LossPreCompaction:      "entries before the last compaction (summary carried instead)",
	LossEncryptedSummary:   "compaction summaries were encrypted and could not be carried",
	LossInactiveBranch:     "entries on abandoned branches (rewinds/edits)",
	LossSidechain:          "sub-agent entries not carried",
	LossTruncatedBudget:    "oldest entries omitted to fit the target context budget",
	LossUnknownRecord:      "unrecognised native records skipped",
	LossNotCarried:         "metadata not carried",
	LossHistoryUndecodable: "the conversation history could not be read; only the agent's handoff is carried",
}

// Loss is one aggregated loss line.
type Loss struct {
	Kind     LossKind `json:"kind"`
	Severity Severity `json:"severity"`
	Count    int      `json:"count"`
	// Detail refines the kind: a record type for unknown_records, a field
	// family for not_carried. Never transcript content.
	Detail string `json:"detail,omitempty"`
}

// LossReport is everything a translation did not carry 1:1. It holds counts
// and categories only, so it is safe to show in notes, previews and audit.
type LossReport struct {
	CarriedEstimatedTokens int64  `json:"carried_estimated_tokens"`
	ContextWindowTokens    int64  `json:"context_window_tokens"`
	ContextWindowSource    string `json:"context_window_source"`
	BudgetTokens           int64  `json:"budget_tokens"`
	Source                 string `json:"source"`
	Target                 string `json:"target,omitempty"`
	SourceEntries          int    `json:"source_entries"`
	Carried                int    `json:"carried_entries"`
	Items                  []Loss `json:"items"`
	// Fallback is the delivery rung: "native", "context_file" or "handoff".
	Fallback string `json:"fallback,omitempty"`
	// TranscriptPath is the full neutral transcript written beside the morph.
	TranscriptPath string `json:"transcript_path,omitempty"`
}

// Add records n losses of a kind (merging with an existing line).
func (r *LossReport) Add(kind LossKind, n int, detail string) {
	if n <= 0 {
		return
	}
	for i := range r.Items {
		if r.Items[i].Kind == kind && r.Items[i].Detail == detail {
			r.Items[i].Count += n
			return
		}
	}
	r.Items = append(r.Items, Loss{Kind: kind, Severity: lossSeverity[kind], Count: n, Detail: detail})
}

// Count returns the total count recorded for a kind.
func (r *LossReport) Count(kind LossKind) int {
	n := 0
	for _, l := range r.Items {
		if l.Kind == kind {
			n += l.Count
		}
	}
	return n
}

// Warnings reports whether any warn-severity loss was recorded.
func (r *LossReport) Warnings() bool {
	for _, l := range r.Items {
		if l.Severity == SeverityWarn {
			return true
		}
	}
	return false
}

func (r *LossReport) sort() {
	sort.SliceStable(r.Items, func(i, j int) bool {
		if r.Items[i].Severity != r.Items[j].Severity {
			return r.Items[i].Severity == SeverityWarn
		}
		if r.Items[i].Kind != r.Items[j].Kind {
			return r.Items[i].Kind < r.Items[j].Kind
		}
		return r.Items[i].Detail < r.Items[j].Detail
	})
}

// Lines renders one plain-text line per loss, warnings first.
func (r *LossReport) Lines() []string {
	r.sort()
	var out []string
	for _, l := range r.Items {
		line := fmt.Sprintf("%s: %d %s", l.Severity, l.Count, lossText[l.Kind])
		if l.Detail != "" {
			line += " (" + l.Detail + ")"
		}
		out = append(out, line)
	}
	return out
}

// Summary is a short plain-text paragraph for a morph note: counts and
// categories only, never transcript content.
func (r *LossReport) Summary() string {
	var b strings.Builder
	from, to := r.Source, r.Target
	if to == "" {
		to = "?"
	}
	fmt.Fprintf(&b, "History %s -> %s: carried %d of %d entries", from, to, r.Carried, r.SourceEntries)
	if r.Fallback != "" && r.Fallback != "native" {
		fmt.Fprintf(&b, " via %s", strings.ReplaceAll(r.Fallback, "_", " "))
	}
	b.WriteString(".")
	for _, line := range r.Lines() {
		b.WriteString("\n- " + line)
	}
	if r.TranscriptPath != "" {
		b.WriteString("\nFull transcript: " + r.TranscriptPath)
	}
	return b.String()
}
