package agentd

import (
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
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
// Source of truth is the predecessor's session row, which the
// statusline hook keeps current on every render: model_id is the full
// Claude model ID (model.id — unlike the display name it round-trips
// into `claude --model`), effort_level the live reasoning effort. A
// `[1m]` suffix is appended when the context-window snapshot says the
// predecessor ran the 1M-token variant — the ID itself doesn't carry
// the window selection. This deliberately tracks the LIVE model, not
// the launch-time flag: a mid-life /model switch is part of the state
// the successor inherits.
//
// Fail-open by design: a missing row, a never-ticked statusbar (older
// Claude Code without model.id included), or a value ValidateModel /
// ValidateEffort won't vouch for all collapse to "" — the spawn then
// omits the flag and claude resolves its own default, exactly the
// pre-inheritance behaviour. Inheritance must never make a spawn fail
// that would have succeeded without it.
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
	model = snap.ModelID
	if model != "" && snap.ContextWindowSize == oneMillionContextWindow {
		model += "[1m]"
	}
	if v, err := clcommon.ValidateModel(model); err == nil {
		model = v
	} else {
		model = ""
	}
	if v, err := clcommon.ValidateEffort(snap.EffortLevel); err == nil {
		effort = v
	}
	return effort, model
}
