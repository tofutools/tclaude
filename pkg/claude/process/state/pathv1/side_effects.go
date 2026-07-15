package pathv1

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

func ValidateCommand(record CommandRecord) error {
	// A complete_run_v1 record is only authoritative when its typed completion
	// basis is cryptographically bound and replay-validated. That contract
	// belongs to the later mutation/recovery layer, so this primitive validator
	// must fail closed instead of partially accepting completion commands.
	if record.Identity.Kind == CommandCompleteRun {
		return fmt.Errorf("complete_run_v1 requires the mutation/recovery validator")
	}
	if len(record.Payload) > MaxCommandPayloadBytes {
		return &OverBudgetError{Limit: "payload_bytes", Value: len(record.Payload), Maximum: MaxCommandPayloadBytes}
	}
	id, err := CommandIdentityDigest(record.Identity)
	if err != nil {
		return err
	}
	if record.ID != id {
		return fmt.Errorf("command identity mismatch")
	}
	if record.IdempotencyKey != CommandIdempotencyKey(record.Identity.Kind, record.ID) {
		return fmt.Errorf("command idempotency key mismatch")
	}
	if err := ValidateCommandIdentity(record.Identity); err != nil {
		return err
	}
	if !record.State.Valid() {
		return fmt.Errorf("invalid command state %q", record.State)
	}
	sum := sha256.Sum256(record.Payload)
	if record.PayloadHash != hex.EncodeToString(sum[:]) {
		return fmt.Errorf("command payload hash mismatch")
	}
	return nil
}

func ValidateCommandIdentity(id CommandIdentity) error {
	if !id.Kind.Valid() {
		return fmt.Errorf("invalid command kind %q", id.Kind)
	}
	if id.PayloadSchema != 1 {
		return fmt.Errorf("command payload schema %d, want 1", id.PayloadSchema)
	}
	emptySource := func() bool {
		return id.SourceActivationID == "" && id.SourceGeneration == 0 && id.SourcePathID == "" && id.Attempt == 0
	}
	emptyTarget := func() bool { return id.TargetReservationID == "" && id.TargetGeneration == 0 }
	switch id.Kind {
	case CommandInitializeRouting:
		if !emptySource() || !emptyTarget() || id.InputDigest == "" || id.PlanDigest == "" || id.CauseDigest != "" || id.ResultCode != "" {
			return fmt.Errorf("initialize_routing_v1 identity fields invalid")
		}
	case CommandPerformAttempt:
		if id.SourceActivationID == "" || id.SourceGeneration == 0 || id.SourcePathID != "" || !emptyTarget() || id.InputDigest != "" || id.CauseDigest != "" || id.PlanDigest == "" || id.ResultCode != "" {
			return fmt.Errorf("perform_attempt_v1 identity fields invalid")
		}
	case CommandSettleAttempt:
		if id.SourceActivationID == "" || id.SourceGeneration == 0 || id.SourcePathID != "" || !emptyTarget() || id.InputDigest == "" || id.CauseDigest != "" || id.PlanDigest == "" || id.ResultCode == "" {
			return fmt.Errorf("settle_attempt_v1 identity fields invalid")
		}
	case CommandRoutePaths:
		if id.SourceActivationID == "" || id.SourceGeneration == 0 || id.SourcePathID == "" || !emptyTarget() || id.InputDigest == "" || id.CauseDigest == "" || id.PlanDigest == "" || id.ResultCode == "" {
			return fmt.Errorf("route_paths_v1 identity fields invalid")
		}
	case CommandActivateGeneration:
		if !emptySource() || id.TargetReservationID == "" || id.TargetGeneration == 0 || id.InputDigest == "" || id.CauseDigest == "" || id.PlanDigest == "" || id.ResultCode != "" {
			return fmt.Errorf("activate_generation_v1 identity fields invalid")
		}
	case CommandPropagateCandidateClosure:
		if id.SourceActivationID != "" || id.SourceGeneration != 0 || id.Attempt != 0 || id.TargetReservationID == "" || id.TargetGeneration == 0 || id.InputDigest == "" || id.CauseDigest == "" || id.PlanDigest == "" || id.ResultCode != "" {
			return fmt.Errorf("propagate_candidate_closure_v1 identity fields invalid")
		}
	case CommandSettleDetachedSink:
		if id.SourceActivationID != "" || id.SourceGeneration != 0 || id.SourcePathID == "" || id.Attempt != 0 || id.TargetReservationID == "" || id.TargetGeneration == 0 || id.InputDigest == "" || id.PlanDigest != "" || id.ResultCode == "" {
			return fmt.Errorf("settle_detached_sink_v1 identity fields invalid")
		}
	case CommandCompleteRun:
		if !emptySource() || !emptyTarget() || id.InputDigest == "" || id.CauseDigest != "" || id.PlanDigest == "" || id.ResultCode == "" {
			return fmt.Errorf("complete_run_v1 identity fields invalid")
		}
	}
	return nil
}

func ValidateSideEffect(effect SideEffectIdentity) error {
	if !effect.Kind.Valid() || effect.Kind == SideEffectCommand {
		return fmt.Errorf("invalid non-command side-effect kind %q", effect.Kind)
	}
	var want string
	var err error
	switch effect.Kind {
	case SideEffectAttempt:
		want, err = AttemptIdentity(effect.RunID, effect.ActivationID, effect.Attempt)
	case SideEffectWait:
		want, err = WaitIdentity(effect.RunID, effect.ActivationID, effect.Attempt, effect.WaitKind)
	case SideEffectTimer:
		want, err = TimerIdentity(effect.RunID, effect.ActivationID, effect.Attempt, effect.SourceCommandID)
	case SideEffectContact:
		want, err = ContactIdentity(effect.RunID, effect.ActivationID, effect.Attempt, effect.Assignee)
	case SideEffectObligation:
		want, err = ObligationIdentity(effect.RunID, effect.ActivationID, effect.Attempt, effect.WaitKind, effect.Assignee)
	case SideEffectBlock:
		want, err = BlockIdentity(effect.RunID, effect.ActivationID, effect.BlockedAttempt)
	}
	if err != nil {
		return err
	}
	if effect.ID != want {
		return fmt.Errorf("side-effect identity mismatch")
	}
	if effect.RunID == "" || effect.ActivationID == "" {
		return fmt.Errorf("side-effect lacks run/activation identity")
	}
	switch effect.Kind {
	case SideEffectAttempt:
		if effect.BlockedAttempt != 0 || effect.WaitKind != "" || effect.SourceCommandID != "" || effect.Assignee != "" {
			return fmt.Errorf("attempt has noncanonical unused identity fields")
		}
	case SideEffectWait:
		if effect.BlockedAttempt != 0 || effect.WaitKind == "" || effect.SourceCommandID != "" || effect.Assignee != "" {
			return fmt.Errorf("wait identity fields invalid")
		}
	case SideEffectTimer:
		if effect.BlockedAttempt != 0 || effect.WaitKind != "" || effect.SourceCommandID == "" || effect.Assignee != "" {
			return fmt.Errorf("timer identity fields invalid")
		}
	case SideEffectContact:
		if effect.BlockedAttempt != 0 || effect.WaitKind != "" || effect.SourceCommandID != "" || effect.Assignee == "" {
			return fmt.Errorf("contact identity fields invalid")
		}
	case SideEffectObligation:
		if effect.BlockedAttempt != 0 || effect.WaitKind == "" || effect.SourceCommandID != "" || effect.Assignee == "" {
			return fmt.Errorf("obligation identity fields invalid")
		}
	case SideEffectBlock:
		if effect.Attempt != 0 || effect.WaitKind != "" || effect.SourceCommandID != "" || effect.Assignee != "" {
			return fmt.Errorf("block has noncanonical unused identity fields")
		}
	}
	allowed := map[SideEffectKind]map[string]bool{SideEffectAttempt: {"claimed": true, "running": true, "reconciling": true, "observed": true, "failed": true, "canceled": true}, SideEffectWait: {"pending": true, "satisfied": true, "canceled": true}, SideEffectTimer: {"pending": true, "satisfied": true, "canceled": true}, SideEffectContact: {"scheduled": true, "due": true, "paused": true, "completed": true, "canceled": true}, SideEffectObligation: {"pending": true, "satisfied": true, "canceled": true}, SideEffectBlock: {"blocked": true, "resolved_retry": true, "resolved_skip": true, "resolved_cancel": true}}
	if !allowed[effect.Kind][effect.State] {
		return fmt.Errorf("invalid %s state %q", effect.Kind, effect.State)
	}
	return nil
}

func ActiveSideEffect(effect SideEffectIdentity) bool {
	switch effect.Kind {
	case SideEffectAttempt:
		return effect.State == "claimed" || effect.State == "running" || effect.State == "reconciling"
	case SideEffectWait, SideEffectTimer, SideEffectObligation:
		return effect.State == "pending"
	case SideEffectContact:
		return effect.State == "scheduled" || effect.State == "due" || effect.State == "paused"
	case SideEffectBlock:
		return effect.State == "blocked"
	}
	return false
}
