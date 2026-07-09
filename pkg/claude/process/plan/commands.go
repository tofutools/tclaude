package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
)

type CommandKind = state.CommandKind

const (
	CommandKindActivateNode   = state.CommandKindActivateNode
	CommandKindStartAttempt   = state.CommandKindStartAttempt
	CommandKindSettleAttempt  = state.CommandKindSettleAttempt
	CommandKindRecordDecision = state.CommandKindRecordDecision
	CommandKindSetTimer       = state.CommandKindSetTimer
	CommandKindWaitSignal     = state.CommandKindWaitSignal
	CommandKindCompleteRun    = state.CommandKindCompleteRun
)

type Command struct {
	ID               string           `json:"id"`
	IdempotencyKey   string           `json:"idempotencyKey"`
	Kind             CommandKind      `json:"kind"`
	RunID            string           `json:"runId"`
	NodeID           string           `json:"nodeId,omitempty"`
	TargetNodeID     string           `json:"targetNodeId,omitempty"`
	SourceCommandID  string           `json:"sourceCommandId,omitempty"`
	SourceNodeStatus state.NodeStatus `json:"sourceNodeStatus,omitempty"`
	Attempt          int              `json:"attempt,omitempty"`
	MaxAttempts      int              `json:"maxAttempts,omitempty"`
	RunStatus        state.RunStatus  `json:"runStatus,omitempty"`
	NodeStatus       state.NodeStatus `json:"nodeStatus,omitempty"`
	WaitID           string           `json:"waitId,omitempty"`
	WaitKind         state.WaitKind   `json:"waitKind,omitempty"`
	Duration         string           `json:"duration,omitempty"`
	Until            string           `json:"until,omitempty"`
	Signal           string           `json:"signal,omitempty"`
	Performer        *model.Performer `json:"performer,omitempty"`
}

func (c Command) OutstandingCommand(createdAt time.Time) state.OutstandingCommand {
	payload, _ := json.Marshal(c)
	return state.OutstandingCommand{
		ID:             c.ID,
		IdempotencyKey: c.IdempotencyKey,
		PayloadHash:    c.PayloadHash(),
		Payload:        payload,
		NodeID:         c.NodeID,
		Attempt:        c.Attempt,
		Kind:           c.Kind,
		Status:         state.CommandStatusIssued,
		CreatedAt:      createdAt,
	}
}

// PayloadHash binds an issued command to every typed field the executor will
// later use. Recovery can therefore accept the original planner command
// without trusting altered transition or performer fields from the caller.
func (c Command) PayloadHash() string {
	data, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func newCommand(kind CommandKind, runID string, parts ...string) Command {
	keyParts := []string{keyPart(runID), string(kind)}
	for _, part := range parts {
		keyParts = append(keyParts, keyPart(part))
	}
	key := strings.Join(keyParts, "/")
	sum := sha256.Sum256([]byte(key))
	return Command{
		ID:             "cmd_" + hex.EncodeToString(sum[:])[:24],
		IdempotencyKey: key,
		Kind:           kind,
		RunID:          runID,
	}
}

func keyPart(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "_"
	}
	if !strings.ContainsAny(value, "/\n\r\t") {
		return value
	}
	sum := sha256.Sum256([]byte(value))
	return "sha256-" + hex.EncodeToString(sum[:])[:12]
}
