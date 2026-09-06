package agentd

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/platform/execution"
)

// managedResumeAdmission is the daemon-side handoff for one accepted Resume.
// The public IDs correlate durable evidence; secret is written only to the
// inherited one-shot pipe and is never included in SpawnArgs or argv.
type managedResumeAdmission struct {
	operation *db.ResumeOperationRow
	secret    []byte
}

func admitManagedResume(convID, kind string, recovery *db.AgentRecovery) (*managedResumeAdmission, error) {
	if convID == "" {
		return nil, errors.New("managed resume requires conversation")
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return nil, fmt.Errorf("generate private resume child claim: %w", err)
	}
	intended := execution.NewID()
	operation := &db.ResumeOperationRow{
		ID: execution.NewOperationID(), Kind: kind, ConvID: convID,
		Attempt:   execution.AttemptRef{ExecutionID: intended},
		ClaimHash: db.ResumeClaimHash(secret[:]), State: execution.ResumeRequested,
		LaunchPhase: "requested", Revision: 1, RequestedAt: time.Now().UTC(),
	}
	if agentID, err := db.AgentIDForConv(convID); err != nil {
		return nil, fmt.Errorf("resolve resume agent identity: %w", err)
	} else {
		operation.AgentID = agentID
	}
	if recovery != nil {
		operation.RecoveryAgentID = recovery.AgentID
		operation.RecoveryGeneration = recovery.PredecessorGeneration
	}
	if predecessor, err := db.LatestInsertedSessionIDForConv(convID); err == nil && predecessor != "" {
		operation.Predecessor.LegacySessionID = predecessor
		if identity, identityErr := db.GetSessionExitLaunchIdentity(predecessor); identityErr == nil {
			if parsed, parseErr := execution.ParseID(identity.Generation); parseErr == nil {
				operation.Predecessor.ExecutionID = parsed
				if selection, found, selectionErr := db.CurrentConversationSelection(parsed); selectionErr != nil {
					return nil, fmt.Errorf("capture resume logical conversation: %w", selectionErr)
				} else if found && selection.Reference.Value == convID {
					operation.LogicalConversationID = string(selection.Conversation)
				}
			}
		}
	} else if err != nil {
		return nil, fmt.Errorf("capture resume predecessor: %w", err)
	}
	if operation.LogicalConversationID == "" {
		// Legacy predecessors predate binding history. The operation still owns
		// an explicit logical target; the exact resumed SessionStart is the first
		// authoritative reference admitted into it.
		operation.LogicalConversationID = string(db.NewLogicalConversationID())
	}
	if err := db.CreateResumeOperation(*operation); err != nil {
		return nil, fmt.Errorf("persist resume request: %w", err)
	}
	if err := db.TransitionResumeOperation(operation.ID, operation.Revision,
		execution.ResumeAccepted, "accepted", ""); err != nil {
		return nil, fmt.Errorf("accept resume operation: %w", err)
	}
	operation.State = execution.ResumeAccepted
	operation.LaunchPhase = "accepted"
	operation.Revision++
	return &managedResumeAdmission{operation: operation, secret: secret[:]}, nil
}

// claimPipe creates the inherited one-shot descriptor consumed by session new.
// The caller owns and closes the returned read end after SpawnResume returns.
func (a *managedResumeAdmission) claimPipe() (*os.File, error) {
	if a == nil || a.operation == nil || len(a.secret) == 0 {
		return nil, errors.New("resume admission has no private claim")
	}
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create resume claim pipe: %w", err)
	}
	if _, err := io.Copy(writeEnd, bytes.NewReader(a.secret)); err != nil {
		_ = readEnd.Close()
		_ = writeEnd.Close()
		return nil, fmt.Errorf("write resume claim pipe: %w", err)
	}
	if err := writeEnd.Close(); err != nil {
		_ = readEnd.Close()
		return nil, fmt.Errorf("close resume claim pipe: %w", err)
	}
	return readEnd, nil
}
