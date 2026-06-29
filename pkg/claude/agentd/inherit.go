package agentd

import (
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// oneMillionContextWindow is the context_window_size Claude Code
// reports for a session running a [1m] model variant. The statusline's
// model.id carries no window suffix, so the snapshot's window size is
// what distinguishes "fable" from "fable[1m]".
const oneMillionContextWindow = 1_000_000

// inheritedLaunchFlags resolves the --effort / --model a successor
// session should be launched with so reincarnate, clone and resume
// bring the agent back on the same LLM model (and reasoning effort)
// its predecessor was running, rather than claude's default.
//
// Source of truth is the predecessor's session row, which the statusline
// hook keeps current for Claude Code and the Codex hook/launch path keeps
// current for Codex: model_id is the machine-facing model token that
// round-trips into the harness's --model flag, effort_level the live or
// launch-time reasoning effort. For Claude Code only, the `[1m]`
// 1M-context window suffix is normalised to exactly-once from the
// context-window snapshot: current Claude Code builds report model.id
// already carrying the suffix, older builds reported the bare id and
// relied on the snapshot's window size to distinguish the variant —
// both forms are handled (see the suffix normalisation below).
// This deliberately tracks the LIVE model when the harness reports it,
// not just the launch-time flag: a mid-life /model switch is part of the
// state the successor inherits.
//
// Fail-open by design: a missing row, a never-ticked statusbar/hook, or a
// value the recorded harness's ModelCatalog won't vouch for all collapse
// to "" — the spawn then omits the flag and the harness resolves its own
// default, exactly the pre-inheritance behaviour. Inheritance must never
// make a spawn fail that would have succeeded without it.
//
// Known imprecision, accepted: a predecessor launched with
// `--model opusplan` reports whichever concrete model it is currently
// on, so the successor gets that model pinned rather than the
// plan/work split. The statusline doesn't expose the opusplan setting.
func inheritedLaunchFlags(sessionID string) (effort, model string) {
	snap, err := db.GetContextSnapshot(sessionID)
	if err != nil {
		return "", ""
	}

	h := harness.MustGet(harness.DefaultName)
	if row, err := db.LoadSession(sessionID); err == nil {
		if resolved, err := harness.Resolve(row.Harness); err == nil {
			h = resolved
		}
	}

	// Normalise the Claude Code `[1m]` window suffix to exactly-once.
	// Current Claude Code builds report model.id WITH the suffix
	// (e.g. "claude-opus-4-8[1m]"); older builds reported the bare id and
	// relied on the context-window snapshot to distinguish the variant.
	// Strip whatever suffix the id already carries, then re-append it from
	// the snapshot, so the successor's --model resolves to "<id>[1m]" once
	// regardless of which form was recorded. The original code blind-
	// appended, producing "<id>[1m][1m]" against a suffix-carrying id —
	// which fails ValidateModel below and collapsed the whole flag to "",
	// so a resumed/reincarnated/cloned agent silently lost the 1M window:
	// it kept the family Claude Code restores from the conversation, plus
	// the separately-validated effort, which is why ONLY [1m] vanished.
	// Scoped to Claude Code — other harnesses (Codex) take model.id
	// verbatim and never carry this suffix.
	model = snap.ModelID
	if h.Name == harness.DefaultName {
		model = strings.TrimSuffix(model, "[1m]")
		if model != "" && snap.ContextWindowSize == oneMillionContextWindow {
			model += "[1m]"
		}
	}
	if v, err := h.Models.ValidateModel(model); err == nil {
		model = v
	} else {
		model = ""
	}
	if v, err := h.Models.ValidateEffort(snap.EffortLevel); err == nil {
		effort = v
	}
	return effort, model
}
