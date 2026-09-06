package agentd

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	platformexec "github.com/tofutools/tclaude/pkg/claude/platform/execution"
	platformruntime "github.com/tofutools/tclaude/pkg/claude/platform/runtime"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

type stopRuntimeFactory func(platformruntime.AttemptKey, *lifecycleTarget) platformruntime.StopRuntime

var terminalStopRuntimeFactories = map[string]stopRuntimeFactory{
	harness.DefaultName: func(key platformruntime.AttemptKey, target *lifecycleTarget) platformruntime.StopRuntime {
		return &terminalStopRuntime{key: key, target: target, recipe: claudeStopRecipe()}
	},
	harness.CodexName: func(key platformruntime.AttemptKey, target *lifecycleTarget) platformruntime.StopRuntime {
		return &terminalStopRuntime{key: key, target: target, recipe: codexStopRecipe()}
	},
	harness.CopilotName: func(key platformruntime.AttemptKey, target *lifecycleTarget) platformruntime.StopRuntime {
		return &terminalStopRuntime{key: key, target: target, recipe: copilotStopRecipe()}
	},
}

// bindStopRuntime resolves an existing durable launch into one exact bound
// capability. It never invents an execution for a legacy row and never
// re-selects by conversation once the handle has been returned.
func bindStopRuntime(convID string) (platformruntime.StopRuntime, *db.SessionRow, error) {
	rows, err := db.FindSessionsByConvID(convID)
	if err != nil {
		return nil, nil, err
	}
	var unprovable bool
	for _, row := range rows {
		executionID, parseErr := platformexec.ParseID(strings.TrimSpace(row.ExecutionID.String()))
		if parseErr != nil {
			unprovable = true
			continue
		}
		key, keyErr := stopAttemptKey(row, executionID)
		if keyErr != nil {
			return nil, row, keyErr
		}
		if row.Harness == harness.OpenCodeName {
			runtimeRow, runtimeErr := db.GetOpenCodeRuntime(row.ID)
			if runtimeErr != nil {
				return nil, row, runtimeErr
			}
			if runtimeRow == nil || !openCodeRuntimeBoundToExecution(*runtimeRow, row, executionID) {
				if runtimeRow != nil {
					unprovable = true
				}
				continue
			}
			if !session.IsProcessAlive(runtimeRow.PID) {
				continue
			}
			if !verifyOpenCodeRuntimeForStop(*runtimeRow) {
				unprovable = true
				continue
			}
			return &openCodeStopRuntime{key: key, row: *row, runtime: *runtimeRow}, row, nil
		}
		factory := terminalStopRuntimeFactories[row.Harness]
		if factory == nil {
			return nil, row, fmt.Errorf("no managed Stop adapter for harness %q", row.Harness)
		}
		if row.TmuxSession == "" || !session.IsTmuxSessionAlive(row.TmuxSession) {
			continue
		}
		target, captureErr := captureLifecycleTarget(row)
		if captureErr != nil || target.attempt.ExecutionID != executionID {
			unprovable = true
			continue
		}
		return factory(key, target), row, nil
	}
	if unprovable {
		return nil, nil, fmt.Errorf("managed Stop target has no provable execution identity")
	}
	return nil, nil, nil
}

func stopAttemptKey(row *db.SessionRow, executionID platformexec.ID) (platformruntime.AttemptKey, error) {
	key := platformruntime.AttemptKey{Execution: executionID}
	selection, found, err := db.CurrentConversationSelection(executionID)
	if err != nil {
		return key, err
	}
	if found {
		key.Conversation = selection.Conversation
	}
	agentID, err := db.AgentIDForConv(row.ConvID)
	if err != nil {
		return key, err
	}
	key.Agent = agentID
	return key, nil
}

func openCodeRuntimeBoundToExecution(runtime db.OpenCodeRuntime, row *db.SessionRow, executionID platformexec.ID) bool {
	if row == nil || runtime.SessionID != row.ID || runtime.ConvID != row.ConvID ||
		strings.TrimSpace(runtime.ExecutionBoundaryJSON) == "" {
		return false
	}
	var boundary session.ExecutionBoundary
	if json.Unmarshal([]byte(runtime.ExecutionBoundaryJSON), &boundary) != nil {
		return false
	}
	return boundary.LaunchGeneration == executionID.String() && boundary.Harness.Name == harness.OpenCodeName
}
