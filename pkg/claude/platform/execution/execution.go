// Package execution defines the platform identity of one authoritative
// runtime attempt. An execution is independent of its logical conversation
// and of terminal or control-channel attachments.
package execution

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sync/atomic"
	"time"
)

const encodedIDLength = 32

// ID identifies one authoritative runtime attempt. Its current encoding is
// deliberately identical to the historical exit-callback generation so the
// platform can adopt the proven launch fence without minting a second source
// of execution truth.
type ID string

// AttemptRef identifies an execution and the legacy session row that carries
// its compatibility projection. LegacySessionID is a locator, not execution
// identity and not evidence that the referenced process is authoritative.
type AttemptRef struct {
	ExecutionID     ID
	LegacySessionID string
}

var (
	randomRead      = rand.Read
	fallbackCounter atomic.Uint64
)

// NewID allocates a non-secret execution identity. Randomness is preferred;
// the hash fallback remains unique within a process and fresh across launches
// even when the host RNG is temporarily unavailable. Execution authority
// still requires separately validated process evidence.
func NewID() ID {
	var raw [encodedIDLength / 2]byte
	if _, err := randomRead(raw[:]); err == nil {
		return ID(hex.EncodeToString(raw[:]))
	}
	seed := fmt.Sprintf("%d\x00%d\x00%d", time.Now().UnixNano(), os.Getpid(), fallbackCounter.Add(1))
	sum := sha256.Sum256([]byte(seed))
	return ID(hex.EncodeToString(sum[:len(raw)]))
}

// ParseID validates the compatibility representation of an execution ID.
func ParseID(raw string) (ID, error) {
	if len(raw) != encodedIDLength {
		return "", fmt.Errorf("execution id must be %d lowercase hexadecimal characters", encodedIDLength)
	}
	for _, c := range raw {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return "", fmt.Errorf("execution id must be %d lowercase hexadecimal characters", encodedIDLength)
		}
	}
	return ID(raw), nil
}

func (id ID) String() string { return string(id) }
