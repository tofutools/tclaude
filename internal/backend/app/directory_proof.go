package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

const directoryProofTTL = 2 * time.Minute
const directoryProofLimit = 8

// DirectoryProofRequired is the public, transient challenge. The caller creates
// these markers from inside its own sandbox and resends the same request.
type DirectoryProofRequired struct {
	Token       string   `json:"token"`
	Filename    string   `json:"filename"`
	Directories []string `json:"dirs"`
}

func (*DirectoryProofRequired) Error() string {
	return "create the requested write-proof marker in each directory from your own sandbox, then retry the same request with write_proof_token"
}

func (*DirectoryProofRequired) Unwrap() error { return ErrUnauthorized }

type directoryProofChallenge struct {
	caller  [32]byte
	intent  [32]byte
	dirs    []string
	expires time.Time
}

// Challenges are deliberately transient like v1. A daemon restart simply asks
// for a new proof; completed operation receipts are resolved before this gate.
type directoryProofChallenges struct {
	mu     sync.Mutex
	active map[string]directoryProofChallenge
}

func proofIdentity(value any) ([32]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(raw), nil
}

// verify consumes only this caller's token, on both success and failed checks.
// Intent excludes the proof token itself and includes the resolved launch
// configuration, so a mutable profile edit cannot reuse an earlier proof.
func (c *directoryProofChallenges) verify(ctx context.Context, host ports.DirectoryWriteProof, caller model.Principal, intent any, token string, paths []string, now time.Time) ([]string, error) {
	if host == nil {
		return nil, fail(ErrUnavailable, "directory write-proof support is unavailable")
	}
	dirs, err := host.ResolveProofDirectories(ctx, paths)
	if err != nil || len(dirs) == 0 {
		return nil, fail(ErrInvalid, "launch directories cannot be resolved for write proof")
	}
	callerID, err := proofIdentity(caller)
	if err != nil {
		return nil, err
	}
	intentID, err := proofIdentity(intent)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	challenge, found := c.active[token]
	if found && challenge.caller == callerID {
		delete(c.active, token)
	}
	c.mu.Unlock()
	if !found || challenge.caller != callerID || !now.Before(challenge.expires) || challenge.intent != intentID || !slices.Equal(challenge.dirs, dirs) {
		return nil, c.mint(callerID, intentID, dirs, now)
	}
	if err := host.VerifyProofMarkers(ctx, dirs, token); err != nil {
		return nil, fail(ErrUnauthorized, "directory write proof failed: %v", err)
	}
	return slices.Clone(dirs), nil
}

func (c *directoryProofChallenges) mint(caller, intent [32]byte, dirs []string, now time.Time) error {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return fmt.Errorf("generate directory proof: %w", err)
	}
	token := hex.EncodeToString(entropy[:])
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == nil {
		c.active = make(map[string]directoryProofChallenge)
	}
	count, oldest := 0, ""
	for key, entry := range c.active {
		if !now.Before(entry.expires) {
			delete(c.active, key)
			continue
		}
		if entry.caller == caller {
			count++
			if oldest == "" || entry.expires.Before(c.active[oldest].expires) {
				oldest = key
			}
		}
	}
	if count >= directoryProofLimit {
		delete(c.active, oldest)
	}
	c.active[token] = directoryProofChallenge{caller: caller, intent: intent, dirs: slices.Clone(dirs), expires: now.Add(directoryProofTTL)}
	return &DirectoryProofRequired{Token: token, Filename: ports.DirectoryWriteProofPrefix + token, Directories: slices.Clone(dirs)}
}
