package conv

import (
	"errors"
	"fmt"
	"io"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
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

// StandaloneRelaunchPrepare asks agentd to prepare convID's relaunch sandbox,
// and returns what the relaunch changes and the warnings to show. It returns
// ErrAgentdUnreachable when no daemon answered.
var StandaloneRelaunchPrepare func(convID string) (changes, warnings []string, err error)

// StandaloneRelaunchSettle reports to agentd whether the prepared launch
// started, and returns the warnings to show.
var StandaloneRelaunchSettle func(convID string, launched bool) []string

// ErrAgentdUnreachable is StandaloneRelaunchPrepare's answer when no daemon
// is reachable.
var ErrAgentdUnreachable = errors.New("agentd is not reachable")

// profilesNotReread is the warning for a launch under the stored policy.
const profilesNotReread = "agentd is not running, so the sandbox profiles were not re-read and the stored policy is used. Start agentd (tclaude agentd), then resume again to apply profile changes."

// standaloneRelaunch is a prepared relaunch to settle once the pane has, or
// has not, started.
type standaloneRelaunch struct {
	convID  string
	pending bool
	out     io.Writer
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
		r.warn(profilesNotReread)
		return r, nil
	}
	changes, warnings, err := StandaloneRelaunchPrepare(convID)
	if errors.Is(err, ErrAgentdUnreachable) {
		r.warn(profilesNotReread)
		return r, nil
	}
	if err != nil {
		return nil, fmt.Errorf("prepare the agent's sandbox: %w", err)
	}
	r.pending = true
	for _, line := range changes {
		if line != "" && out != nil {
			_, _ = fmt.Fprintln(out, line)
		}
	}
	r.warn(warnings...)
	return r, nil
}

// settle reports the launch's outcome to agentd, once.
func (r *standaloneRelaunch) settle(launched bool) {
	if r == nil || !r.pending || StandaloneRelaunchSettle == nil {
		return
	}
	r.pending = false
	r.warn(StandaloneRelaunchSettle(r.convID, launched)...)
}

func (r *standaloneRelaunch) warn(lines ...string) {
	for _, line := range lines {
		if line != "" && r.out != nil {
			_, _ = fmt.Fprintln(r.out, "Warning: "+line)
		}
	}
}
