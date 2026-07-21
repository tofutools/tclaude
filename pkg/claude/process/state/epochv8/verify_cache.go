package epochv8

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"fmt"
	"sync"
	"sync/atomic"
)

// This file implements the bounded, process-local memo of SUCCESSFUL
// VerifyCheckpointV8 results. Schema-8 verification replays the complete
// runtime receipt chain (inner path-v1 witness replay plus canonicalization),
// so structurally repeated verifications of the same checkpoint — Decode then
// Encode, coherent store loads, per-epoch diff publication, constructor
// current+successor checks — used to multiply that full-replay cost until a
// trivial run starved the engine lease heartbeat.
//
// Safety model:
//   - The lookup key never trusts the checkpoint's claimed digest: it is a
//     domain-separated SHA-256 over the exact canonical serialized wire
//     (which includes the claimed digest), and every entry retains those
//     exact canonical bytes. A hit additionally requires bytes.Equal against
//     the retained bytes, so neither a fingerprint collision nor a forged
//     claimed digest can inherit a previous success.
//   - Only successes are stored. Errors, over-budget results, and any other
//     failure are never cached and never shared beyond an exact-byte
//     coalesced flight.
//   - The memo is memory-only, bounded in both entry count and retained
//     bytes, with deterministic LRU eviction. Oversize inputs are verified
//     normally but never retained.
//   - Full replay always happens outside the cache mutex, and the cache never
//     returns aliases of retained bytes to callers.
const (
	verifyCacheMaxEntries       = 64
	verifyCacheMaxRetainedBytes = 64 << 20
	verifyCacheFingerprintTag   = identityPrefix + "verified-checkpoint-cache/v1"
)

// checkpointVerifyCache is the single production memo behind
// VerifyCheckpointV8. Tests construct private instances via newVerifyCache
// instead of mutating this instance's bounds.
var checkpointVerifyCache = newVerifyCache(verifyCacheMaxEntries, verifyCacheMaxRetainedBytes)

type verifyCache struct {
	maxEntries int
	maxBytes   int

	mu       sync.Mutex
	entries  map[string]*list.Element
	order    *list.List // front is most recently used; values are *verifyCacheEntry
	retained int
	inflight map[string]*verifyCacheFlight

	// replays counts full uncached replays performed through this cache. It
	// exists so same-package tests can assert call bounds; it is not part of
	// any exported surface.
	replays atomic.Uint64
}

type verifyCacheEntry struct {
	key       string
	canonical []byte
}

// verifyCacheFlight coalesces concurrent cold misses. Joiners are admitted
// only on exact-byte equality with the leader's input, so two byte strings
// that somehow share a fingerprint never share one in-flight verification.
type verifyCacheFlight struct {
	canonical []byte
	done      chan struct{}
	err       error
}

func newVerifyCache(maxEntries, maxBytes int) *verifyCache {
	return &verifyCache{
		maxEntries: maxEntries,
		maxBytes:   maxBytes,
		entries:    make(map[string]*list.Element),
		order:      list.New(),
		inflight:   make(map[string]*verifyCacheFlight),
	}
}

func verifyCacheFingerprint(canonical []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte(verifyCacheFingerprintTag))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(canonical)
	return string(h.Sum(nil))
}

type verifyCacheOutcome int

const (
	verifyCacheHit verifyCacheOutcome = iota
	verifyCacheLead
	verifyCacheJoin
	verifyCacheIndependent
)

func (c *verifyCache) verify(checkpoint *CheckpointV8) error {
	if checkpoint == nil {
		return fmt.Errorf("%w: checkpoint is nil", ErrInvalid)
	}
	canonical, err := marshalCheckpointWire(checkpoint.wire)
	if err != nil {
		// The uncached verifier's first step is this exact wire-budget
		// marshal; surface its error unchanged and cache nothing.
		return err
	}
	key := verifyCacheFingerprint(canonical)
	outcome, flight := c.acquire(key, canonical)
	switch outcome {
	case verifyCacheHit:
		return nil
	case verifyCacheLead:
		replayErr := c.replay(checkpoint)
		c.settle(key, flight, canonical, replayErr)
		return replayErr
	case verifyCacheJoin:
		<-flight.done
		if flight.err == nil {
			return nil
		}
		// Identical bytes make the leader's failure deterministic, but errors
		// are never cached or shared: fail closed by replaying independently.
		return c.replay(checkpoint)
	default:
		// A fingerprint collision with different exact bytes: verify
		// independently of the colliding entry or flight.
		replayErr := c.replay(checkpoint)
		if replayErr == nil {
			c.mu.Lock()
			c.storeLocked(key, canonical)
			c.mu.Unlock()
		}
		return replayErr
	}
}

// acquire classifies one verification attempt under the cache mutex. The
// entry check and flight registration share a critical section with settle,
// so a caller that arrives after a leader settled sees the stored entry
// rather than starting a redundant replay.
func (c *verifyCache) acquire(key string, canonical []byte) (verifyCacheOutcome, *verifyCacheFlight) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.entries[key]; ok {
		entry := element.Value.(*verifyCacheEntry)
		if bytes.Equal(entry.canonical, canonical) {
			c.order.MoveToFront(element)
			return verifyCacheHit, nil
		}
		return verifyCacheIndependent, nil
	}
	if flight, ok := c.inflight[key]; ok {
		if bytes.Equal(flight.canonical, canonical) {
			return verifyCacheJoin, flight
		}
		return verifyCacheIndependent, nil
	}
	flight := &verifyCacheFlight{canonical: canonical, done: make(chan struct{})}
	c.inflight[key] = flight
	return verifyCacheLead, flight
}

func (c *verifyCache) settle(key string, flight *verifyCacheFlight, canonical []byte, err error) {
	c.mu.Lock()
	if c.inflight[key] == flight {
		delete(c.inflight, key)
	}
	if err == nil {
		c.storeLocked(key, canonical)
	}
	c.mu.Unlock()
	flight.err = err
	close(flight.done)
}

// storeLocked retains one successful verification. Oversize inputs are
// skipped entirely; eviction is strict deterministic LRU over both the entry
// and retained-byte bounds. Callers must hold c.mu.
func (c *verifyCache) storeLocked(key string, canonical []byte) {
	if c.maxEntries <= 0 || len(canonical) > c.maxBytes {
		return
	}
	if element, ok := c.entries[key]; ok {
		entry := element.Value.(*verifyCacheEntry)
		if !bytes.Equal(entry.canonical, canonical) {
			// Fingerprint collision with a stored entry: keep the newest
			// exact bytes. Lookups stay fail-closed either way because a hit
			// requires exact-byte equality.
			c.retained += len(canonical) - len(entry.canonical)
			entry.canonical = canonical
		}
		c.order.MoveToFront(element)
	} else {
		c.entries[key] = c.order.PushFront(&verifyCacheEntry{key: key, canonical: canonical})
		c.retained += len(canonical)
	}
	for c.order.Len() > c.maxEntries || c.retained > c.maxBytes {
		oldest := c.order.Back()
		if oldest == nil {
			return
		}
		entry := oldest.Value.(*verifyCacheEntry)
		c.order.Remove(oldest)
		delete(c.entries, entry.key)
		c.retained -= len(entry.canonical)
	}
}

// replay is the only path into the full uncached verifier.
func (c *verifyCache) replay(checkpoint *CheckpointV8) error {
	c.replays.Add(1)
	return verifyCheckpointV8Uncached(checkpoint)
}

func (c *verifyCache) replayCount() uint64 {
	return c.replays.Load()
}

// resetForTest drops all retained entries, modeling a process restart with a
// cold cache. Same-package tests only; concurrent verifications through the
// instance remain safe but may re-replay.
func (c *verifyCache) resetForTest() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]*list.Element)
	c.order = list.New()
	c.retained = 0
}

// retainedForTest reports the current entry count and retained byte total.
func (c *verifyCache) retainedForTest() (entries, retainedBytes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len(), c.retained
}
