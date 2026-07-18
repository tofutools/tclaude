package dashsnap

import (
	"fmt"
	"strconv"
	"strings"
)

// Shard is a deterministic 1-based slice of the state matrix: shard i of n
// captures every state whose index (after any filter) is congruent to i-1
// modulo n. Round-robin assignment — rather than contiguous chunks — keeps the
// shards balanced as the matrix grows: expensive states cluster by feature
// area (e.g. the long-settle process-editor block), and both skins interleave
// evenly into every shard, so each shard's wall-clock stays close to total/n
// no matter where new states are appended.
type Shard struct {
	Index, Total int
}

// ParseShard parses an "i/n" spec (e.g. "2/4"). An empty or all-whitespace
// spec means "no sharding" and returns the identity Shard{1, 1}.
func ParseShard(spec string) (Shard, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return Shard{Index: 1, Total: 1}, nil
	}
	idxStr, totalStr, ok := strings.Cut(spec, "/")
	if !ok {
		return Shard{}, fmt.Errorf("shard spec %q: want the form i/n, e.g. 2/4", spec)
	}
	idx, err := strconv.Atoi(strings.TrimSpace(idxStr))
	if err != nil {
		return Shard{}, fmt.Errorf("shard spec %q: bad index: %w", spec, err)
	}
	total, err := strconv.Atoi(strings.TrimSpace(totalStr))
	if err != nil {
		return Shard{}, fmt.Errorf("shard spec %q: bad total: %w", spec, err)
	}
	if total < 1 || idx < 1 || idx > total {
		return Shard{}, fmt.Errorf("shard spec %q: want 1 <= i <= n", spec)
	}
	return Shard{Index: idx, Total: total}, nil
}

// Enabled reports whether this shard actually partitions the matrix.
func (s Shard) Enabled() bool { return s.Total > 1 }

// Pick returns this shard's deterministic subset of states, preserving order.
// The identity shard returns the input unchanged.
func (s Shard) Pick(states []State) []State {
	if !s.Enabled() {
		return states
	}
	picked := make([]State, 0, (len(states)+s.Total-1)/s.Total)
	for i, st := range states {
		if i%s.Total == s.Index-1 {
			picked = append(picked, st)
		}
	}
	return picked
}

// Suffix is a filename-safe tag ("-shard2of4") for the shard's output dir, or
// "" for the identity shard.
func (s Shard) Suffix() string {
	if !s.Enabled() {
		return ""
	}
	return fmt.Sprintf("-shard%dof%d", s.Index, s.Total)
}
