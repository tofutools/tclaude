package agentd

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// An unattended trigger, schedule or process that a spawn profile refuses
// stops doing its work without anyone having asked it to. The refusal is
// recorded where that automation's runs show (the firing's action outcome,
// the schedule run, the process node), and the first refusal of each kind is
// also sent to the automation's owner and to the operator, so a stopped
// automation is not silent. Later identical refusals only record; a
// successful spawn from the same source re-arms the notice, as does a daemon
// restart.

var (
	managedRefusalMu       sync.Mutex
	managedRefusalNotified = map[string]bool{}
)

// managedRefusalSource names the automation a refusal stopped.
type managedRefusalSource struct {
	// Key identifies the automation for the first-refusal latch, e.g.
	// "trigger:12", "cron:3" or "process:<run id>".
	Key string
	// Label is how the notice names it, e.g. `trigger "pr-review"`.
	Label     string
	OwnerConv string
	// Process links the operator notice to its run, when set.
	RunID, NodeID, CommandID string
}

// announceManagedProfileRefusal sends the first refusal of each kind from
// source to its owner (when an agent owns it) and to the operator. It reports
// whether it sent anything.
func announceManagedProfileRefusal(source managedRefusalSource, kind, detail string) bool {
	if !isManagedProfileRefusal(kind) {
		return false
	}
	latch := source.Key + "\x00" + kind + "\x00" + detail
	managedRefusalMu.Lock()
	if managedRefusalNotified[latch] {
		managedRefusalMu.Unlock()
		return false
	}
	managedRefusalNotified[latch] = true
	managedRefusalMu.Unlock()

	why := "disabled"
	if kind == "profile_operator_only" {
		why = "operator-only"
	}
	subject := fmt.Sprintf("%s stopped: a spawn profile is %s", source.Label, why)
	body := fmt.Sprintf("%s could not spawn its worker: %s.\n\n"+
		"Every run is refused the same way until the profile is changed or the automation is pointed at another profile. "+
		"This notice is sent once; later refusals show only in the run history.", source.Label, detail)
	if source.OwnerConv != "" {
		if _, err := queueAgentMessage(&db.AgentMessage{ToConv: source.OwnerConv, Subject: subject, Body: body}); err != nil {
			slog.Warn("managed workers: profile refusal notice to owner not sent", "source", source.Key, "error", err)
		}
	}
	if _, err := recordHumanMessageWithProcess("", subject, body, source.RunID, source.NodeID, source.CommandID); err != nil {
		slog.Warn("managed workers: profile refusal notice to operator not sent", "source", source.Key, "error", err)
	}
	return true
}

// rearmManagedProfileRefusal clears source's latch after it spawned a
// worker, so a later refusal is announced again.
func rearmManagedProfileRefusal(key string) {
	managedRefusalMu.Lock()
	defer managedRefusalMu.Unlock()
	for latch := range managedRefusalNotified {
		if strings.HasPrefix(latch, key+"\x00") {
			delete(managedRefusalNotified, latch)
		}
	}
}
