package session

import (
	"encoding/json"
	"fmt"
	"io"
)

// HookResponse is the harness-independent result of applying one hook event:
// what, if anything, tclaude wants the harness to do with the event beyond
// recording it.
//
// It exists because the hook stdout channel used to be reachable by exactly
// one caller. DispatchHookEvent branched PreCompact off before applyHook and
// handed only that branch the io.Writer, and applyHook returned nothing but an
// error — so no ordinary event (SessionStart, PreToolUse, SubagentStart) could
// produce model-visible output at all, however much it had to say.
//
// Making the answer a VALUE rather than a writer is what opens that channel to
// every event. The two edges that own real byte streams — the direct hook
// callback and the agentd broker's response buffer — do the serializing, once,
// through Write below; nothing in the middle of the hook path needs an
// io.Writer or needs to know a harness's JSON dialect.
type HookResponse struct {
	// AdditionalContext is text to place in front of the model on its next
	// request within the current turn. Empty for the overwhelming majority of
	// events.
	AdditionalContext string

	// Decision is a gate verdict. "" allows the event; "block" refuses it,
	// with Reason carrying the explanation the harness shows the model. Today
	// only the pre-compact guard sets it.
	Decision string

	// Reason accompanies a non-empty Decision.
	Reason string

	// commit is run once the response has been written successfully. It exists
	// so a producer can defer a durable side effect — recording that a standing
	// order was delivered — until the bytes are actually out, rather than
	// claiming a delivery a failed write never made.
	commit func()
}

// Commit runs the response's deferred side effect, if it has one. It is called
// by the edges after a successful Write and is a no-op otherwise.
func (r HookResponse) Commit() {
	if r.commit != nil {
		r.commit()
	}
}

// IsEmpty reports whether there is nothing to say. An empty response writes no
// bytes at all, which is what every hook did before this type existed and what
// every hook that has no opinion must keep doing: a harness reads an empty
// stdout as "no instruction", and emitting `{}` instead would be a behaviour
// change on every event.
func (r HookResponse) IsEmpty() bool {
	return r.AdditionalContext == "" && r.Decision == ""
}

// hookDecisionDocument is the JSON Claude Code and Codex read from a gating
// hook's stdout: {"decision":"block","reason":"..."}. See
// https://code.claude.com/docs/en/hooks.
type hookDecisionDocument struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

// hookContextDocument is the JSON both harnesses read to place text in front
// of the model for the next request in the current turn.
//
// hookEventName is echoed back because the schema is per-event; a document
// naming the wrong event is ignored rather than applied, so it is filled from
// the event actually being answered rather than hardcoded.
type hookContextDocument struct {
	HookSpecificOutput struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

// Write serializes a response for one event and writes it to w.
//
// A block decision wins over additional context when both are set. On a
// blocking exit the harness treats the decision document as the whole answer,
// so emitting both would mean writing two JSON documents to a stream that is
// parsed as one — and the context half would be the one silently lost. Callers
// that need to say something while blocking put it in Reason, which IS
// delivered to the model on that path.
func (r HookResponse) Write(w io.Writer, hookEventName string) error {
	if r.IsEmpty() {
		return nil
	}
	enc := json.NewEncoder(w)
	if r.Decision != "" {
		if err := enc.Encode(hookDecisionDocument{Decision: r.Decision, Reason: r.Reason}); err != nil {
			return fmt.Errorf("hook response: failed to write %s decision: %w", hookEventName, err)
		}
		return nil
	}
	var doc hookContextDocument
	doc.HookSpecificOutput.HookEventName = hookEventName
	doc.HookSpecificOutput.AdditionalContext = r.AdditionalContext
	if err := enc.Encode(doc); err != nil {
		return fmt.Errorf("hook response: failed to write %s context: %w", hookEventName, err)
	}
	return nil
}
