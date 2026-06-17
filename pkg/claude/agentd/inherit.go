package agentd

import (
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
// launch-time reasoning effort. For Claude Code only, a `[1m]` suffix is
// appended when the context-window snapshot says the predecessor ran the
// 1M-token variant — the ID itself doesn't carry the window selection.
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

	model = snap.ModelID
	if h.Name == harness.DefaultName && model != "" && snap.ContextWindowSize == oneMillionContextWindow {
		model += "[1m]"
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
