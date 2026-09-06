package agentd

import (
	"strings"
	"sync"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	platformexec "github.com/tofutools/tclaude/pkg/claude/platform/execution"
)

// executionRuntime owns daemon-local serialization and lifecycle operations
// for managed runtime attempts. Cross-process launch exclusion remains in the
// session launcher; durable recovery leasing remains in the recovery store.
type executionRuntime struct {
	launchLocks         sync.Map // map[stable actor or unowned conv]*sync.Mutex
	recoveryCommitLocks sync.Map // map[legacy conv locator]*sync.Mutex
}

var managedExecutionRuntime = &executionRuntime{}

func (r *executionRuntime) launchLock(convID string) *sync.Mutex {
	key := strings.TrimSpace(convID)
	// Conversation replacement does not replace the stable actor whose
	// execution lifecycle is serialized. Historical and current locators must
	// therefore resolve to the same lock.
	if agentID, err := db.AgentIDForConv(key); err == nil && agentID != "" {
		key = "agent:" + agentID
	}
	lock, _ := r.launchLocks.LoadOrStore(key, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func (r *executionRuntime) recoveryCommitLock(convID string) *sync.Mutex {
	lock, _ := r.recoveryCommitLocks.LoadOrStore(convID, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

type stopOperationContext struct {
	outcome              platformexec.StopOutcome
	convergenceScheduled bool
}

type stopOperationResult struct {
	legacy memberOpResult
	wait   softExitOutcome
	stop   platformexec.StopOutcome
}

func (r *executionRuntime) stop(
	convID string,
	force bool,
	lifecycleAction, relatedEventID string,
	waitPolicy stopWaitPolicy,
) stopOperationResult {
	lock := r.launchLock(convID)
	lock.Lock()
	defer lock.Unlock()
	return r.stopUnderLaunchLock(convID, force, lifecycleAction, relatedEventID, waitPolicy)
}

// stopUnderLaunchLock owns one Stop operation after its caller has joined the
// stable-actor lifecycle critical section. Delayed effects retain the target
// captured by stopOneConvEffectUnderLaunchLock and never reselect a successor.
func (r *executionRuntime) stopUnderLaunchLock(
	convID string,
	force bool,
	lifecycleAction, relatedEventID string,
	waitPolicy stopWaitPolicy,
) stopOperationResult {
	ctx := &stopOperationContext{outcome: platformexec.StopOutcome{State: platformexec.StopFailed}}
	legacy, waited := stopOneConvEffectUnderLaunchLock(
		convID, force, lifecycleAction, relatedEventID, waitPolicy, ctx,
	)
	outcome := ctx.outcome
	if waitPolicy.wait && outcome.State != platformexec.StopNoExecution {
		switch waited {
		case softExitClosed:
			if outcome.State != platformexec.StopFailed {
				outcome.State = platformexec.StopCompleted
			}
		case softExitEscalated:
			outcome.State = platformexec.StopCompleted
			outcome.Escalated = true
		case softExitStuck:
			outcome.State = platformexec.StopUnresolved
		case softExitUnattempted:
			outcome.State = platformexec.StopFailed
		}
	}
	if !waitPolicy.wait && outcome.State == platformexec.StopFailed && ctx.convergenceScheduled {
		outcome.State = platformexec.StopAccepted
	}
	return stopOperationResult{legacy: legacy, wait: waited, stop: outcome}
}
