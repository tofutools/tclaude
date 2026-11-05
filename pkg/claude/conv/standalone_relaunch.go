package conv

import (
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// A standalone relaunch (`tclaude conv resume`, the conversation watch TUI)
// starts its pane itself and renders the sandbox snapshot stored for the
// agent. Before it does, it asks agentd to prepare that sandbox as an ordinary
// resume does (the profiles re-read, the group's environment, the relaunch
// policy), and after the launch it tells agentd whether the pane started. The
// daemon routes are /v1/relaunch-sandbox/prepare and /settle.
//
// conv cannot import the daemon client (the agent package imports conv), so
// the agent package installs the two calls. With no daemon, the launch goes
// ahead under the stored policy, with a warning: a human is never kept out of
// a pane because agentd is down.

// StandaloneRelaunchPrepared is what agentd prepared for a standalone
// relaunch: what it changes and the warnings to show, and the relaunch
// decisions the pane launches with.
type StandaloneRelaunchPrepared struct {
	Changes, Warnings []string
	// RemoteControl is agentd's Remote Access decision (relaunchRemoteControl,
	// as on resume); nil leaves the launch on the recorded posture.
	RemoteControl *bool
}

// StandaloneRelaunchPrepare asks agentd to prepare convID's relaunch sandbox,
// and returns what agentd prepared. It returns a
// *StandaloneRelaunchUnavailable when agentd could not do it (not reachable,
// not confirming the caller as the operator, too old for the route): the
// launch then goes ahead under the stored policy. Any other error is agentd
// refusing the relaunch.
var StandaloneRelaunchPrepare func(convID string) (*StandaloneRelaunchPrepared, error)

// StandaloneRelaunchSettle reports to agentd whether the prepared launch
// started, and returns the warnings to show.
var StandaloneRelaunchSettle func(convID string, launched bool) []string

// StandaloneRelaunchUnavailable says agentd could not prepare the relaunch,
// and what to tell the human.
type StandaloneRelaunchUnavailable struct{ Warning string }

func (e *StandaloneRelaunchUnavailable) Error() string { return e.Warning }

// Warnings for a launch under the stored policy, one per reason.
const (
	AgentdNotRunning  = "agentd is not running, so the sandbox profiles were not re-read and the stored policy is used. Start agentd (tclaude agentd), then resume again to apply profile changes."
	AgentdNotOperator = "agentd did not confirm this terminal as the operator, so the sandbox profiles were not re-read and the stored policy is used. Export TCLAUDE_HUMAN_TOKEN from agentd, then resume again to apply profile changes."
	AgentdTooOld      = "the running agentd predates sandbox re-reading for conv resume, so the stored policy is used. Restart agentd, then resume again to apply profile changes."
)

// standaloneRelaunch is a prepared relaunch to settle once the pane has, or
// has not, started.
type standaloneRelaunch struct {
	convID  string
	pending bool
	out     io.Writer
	// remoteControlDecision is agentd's Remote Access decision, or nil.
	remoteControlDecision *bool
	// nativeGeneration is the resume's generated Codex profile in the native
	// permission registry, or "" when none was registered.
	nativeGeneration string
	// nativeLaunched is set once the pane has started with that profile.
	nativeLaunched bool
}

// prepareStandaloneRelaunch prepares convID's relaunch sandbox when it belongs
// to an agent; a plain conversation has no agent sandbox to prepare. A daemon
// refusal is returned, as an ordinary resume would refuse.
func prepareStandaloneRelaunch(convID string, out io.Writer) (*standaloneRelaunch, error) {
	r := &standaloneRelaunch{convID: convID, out: out}
	if agentID, err := db.AgentIDForConv(convID); err != nil || agentID == "" {
		return r, err
	}
	if StandaloneRelaunchPrepare == nil {
		r.warn(AgentdNotRunning)
		return r, nil
	}
	prepared, err := StandaloneRelaunchPrepare(convID)
	var unavailable *StandaloneRelaunchUnavailable
	if errors.As(err, &unavailable) {
		r.warn(unavailable.Warning)
		return r, nil
	}
	if err != nil {
		return nil, fmt.Errorf("prepare the agent's sandbox: %w", err)
	}
	r.pending = true
	if prepared == nil {
		return r, nil
	}
	r.remoteControlDecision = prepared.RemoteControl
	for _, line := range prepared.Changes {
		if line != "" && out != nil {
			_, _ = fmt.Fprintln(out, line)
		}
	}
	r.warn(prepared.Warnings...)
	return r, nil
}

// remoteControl is whether the pane arms Remote Access: agentd's decision
// when it made one, else the recorded posture (resumeRemoteControl), as
// before agentd prepared standalone relaunches. Either way it goes through
// the harness's own resolution, so a harness without Remote Access never
// gets it.
func (r *standaloneRelaunch) remoteControl(h *harness.Harness, convID string) (bool, error) {
	if r == nil || r.remoteControlDecision == nil {
		return resumeRemoteControl(h, convID)
	}
	remoteControl, err := harness.ResolveRemoteControl(h, *r.remoteControlDecision)
	if err != nil {
		slog.Warn("agentd's remote-access decision does not apply to this harness; resuming without it",
			"conv_id", convID, "harness", h.Name, "error", err)
		return false, nil
	}
	return remoteControl, nil
}

// settle reports the launch's outcome to agentd, once. A resume whose pane
// never started also leaves the native Codex registry: nothing runs with its
// profile.
func (r *standaloneRelaunch) settle(launched bool) {
	if r == nil {
		return
	}
	if launched {
		r.nativeLaunched = true
	} else if r.nativeGeneration != "" && !r.nativeLaunched {
		if err := session.UnregisterCodexNativePermissionProfile(r.nativeGeneration); err != nil {
			slog.Warn("could not remove the resume's generated Codex profile from the native registry",
				"conv_id", r.convID, "generation", r.nativeGeneration, "error", err)
		}
		r.nativeGeneration = ""
	}
	if !r.pending || StandaloneRelaunchSettle == nil {
		return
	}
	r.pending = false
	r.warn(StandaloneRelaunchSettle(r.convID, launched)...)
}

// activateNativeProfile marks the resume's registered Codex profile ready
// once its session row is saved, as `session new` does at its launch
// boundary: the row is what keeps it live, and readiness prunes the
// conversation's superseded profiles. The pane is already running, so a
// failure is a warning; the registration stays and later reconciliation
// decides it.
func (r *standaloneRelaunch) activateNativeProfile() {
	if r == nil || r.nativeGeneration == "" {
		return
	}
	generation := r.nativeGeneration
	r.nativeGeneration = ""
	if err := session.ActivateCodexNativePermissionProfile(generation); err != nil {
		r.warn(fmt.Sprintf("could not activate the resumed Codex permission profile: %v", err))
	}
}

func (r *standaloneRelaunch) warn(lines ...string) {
	for _, line := range lines {
		if line != "" && r.out != nil {
			_, _ = fmt.Fprintln(r.out, "Warning: "+line)
		}
	}
}
