package hub

import (
	"sync"
	"time"
)

// bucket is a token bucket refilled continuously at perMinute/60 per
// second, holding at most perMinute tokens.
type bucket struct {
	capacity float64
	tokens   float64
	last     time.Time
}

func newBucket(perMinute int) *bucket {
	return &bucket{capacity: float64(perMinute), tokens: float64(perMinute)}
}

func (b *bucket) refill(now time.Time) {
	if !b.last.IsZero() {
		b.tokens += now.Sub(b.last).Minutes() * b.capacity
		if b.tokens > b.capacity {
			b.tokens = b.capacity
		}
	}
	b.last = now
}

// bucketPair limits frames and bytes together: a send must fit both.
type bucketPair struct {
	mu     sync.Mutex
	frames *bucket
	bytes  *bucket
}

func newBucketPair(framesPerMin, bytesPerMin int) *bucketPair {
	return &bucketPair{frames: newBucket(framesPerMin), bytes: newBucket(bytesPerMin)}
}

func (p *bucketPair) allow(size int, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.frames.refill(now)
	p.bytes.refill(now)
	if p.frames.tokens < 1 || p.bytes.tokens < float64(size) {
		return false
	}
	p.frames.tokens--
	p.bytes.tokens -= float64(size)
	return true
}
