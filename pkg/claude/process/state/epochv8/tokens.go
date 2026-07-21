package epochv8

import "fmt"

// HandoffToken projects one current protected owner as a candidate-independent
// opaque capability-free handle. It is meaningful only at the exact outer and
// runtime bindings from which it was derived.
func HandoffToken(checkpoint *CheckpointV8, owner OwnerIdentity) (string, error) {
	if err := VerifyCheckpointV8(checkpoint); err != nil {
		return "", err
	}
	protected, err := protectedClosure(checkpoint.wire.Authorities)
	if err != nil {
		return "", err
	}
	found := false
	for _, authority := range protected {
		if authority.Identity == owner {
			found = true
			break
		}
	}
	if !found {
		return "", fmt.Errorf("%w: handoff owner is not protected", ErrInvalid)
	}
	return handoffToken(checkpoint, owner)
}

func handoffToken(checkpoint *CheckpointV8, owner OwnerIdentity) (string, error) {
	return digestValue("process-preview-handoff/v1", struct {
		RunID   string         `json:"runId"`
		Binding Binding        `json:"binding"`
		Runtime RuntimeBinding `json:"runtime"`
		Owner   OwnerIdentity  `json:"owner"`
	}{checkpoint.wire.Anchor.RunID, checkpoint.Binding(), checkpoint.wire.RuntimeBinding, owner})
}

// ResolveHandoffToken maps an opaque current-binding token back to exactly one
// protected owner. Collision and unknown-token cases fail closed.
func ResolveHandoffToken(checkpoint *CheckpointV8, token string) (OwnerIdentity, error) {
	if !canonicalDigest(token) {
		return "", fmt.Errorf("%w: handoff token is invalid", ErrInvalid)
	}
	if err := VerifyCheckpointV8(checkpoint); err != nil {
		return "", err
	}
	protected, err := protectedClosure(checkpoint.wire.Authorities)
	if err != nil {
		return "", err
	}
	var matched OwnerIdentity
	for _, authority := range protected {
		candidate, tokenErr := handoffToken(checkpoint, authority.Identity)
		if tokenErr != nil {
			return "", tokenErr
		}
		if candidate != token {
			continue
		}
		if matched != "" {
			return "", fmt.Errorf("%w: handoff token is ambiguous", ErrInvalid)
		}
		matched = authority.Identity
	}
	if matched == "" {
		return "", fmt.Errorf("%w: handoff token is stale or unknown", ErrInvalid)
	}
	return matched, nil
}
