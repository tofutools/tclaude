package sandboxpolicy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/tofutools/tclaude/internal/backend/model"
)

// RevisionReader reads immutable authored content. It must not substitute a
// profile's current head when the requested revision is unavailable.
type RevisionReader interface {
	ReadSandboxRevision(context.Context, model.SandboxProfileRef) (model.SandboxPolicy, error)
}

type ClosureEntry struct {
	Ref    model.SandboxProfileRef
	Policy model.SandboxPolicy
}

// Closure retains the include graph and authored edge order. Entries are unique
// and topologically ordered; they must not be applied as a flat sequence because
// a shared include may be overridden differently by two including profiles.
// Host planning, including canonical-path precedence, remains a separate step.
type Closure struct {
	Root    model.SandboxProfileRef
	Entries []ClosureEntry
}

// ContentHash validates before hashing the complete authored shape. This is a
// content identity, not a statement that two different spellings have identical
// host authority. Host aliases and implementation-specific expansions are only
// known after host planning.
func ContentHash(policy model.SandboxPolicy) (string, error) {
	if err := Validate(policy); err != nil {
		return "", err
	}
	if len(policy.Environment) == 0 {
		policy.Environment = nil
	}
	data, err := json.Marshal(policy)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}

// Resolve verifies every exact reference and bounds the full include graph.
// The returned content is detached from the reader's mutable Go values. No
// filesystem or environment lookup is performed here.
func Resolve(ctx context.Context, root model.SandboxProfileRef, reader RevisionReader) (Closure, error) {
	if reader == nil {
		return Closure{}, fmt.Errorf("sandbox revision reader is required")
	}
	resolver := closureResolver{reader: reader, seen: map[model.SandboxProfileRevisionID]model.SandboxProfileRef{}, height: map[model.SandboxProfileRevisionID]int{}, active: map[model.SandboxProfileRevisionID]bool{}}
	if _, err := resolver.visit(ctx, root, 0); err != nil {
		return Closure{}, err
	}
	return Closure{Root: root, Entries: resolver.entries}, nil
}

type closureResolver struct {
	reader  RevisionReader
	seen    map[model.SandboxProfileRevisionID]model.SandboxProfileRef
	height  map[model.SandboxProfileRevisionID]int
	active  map[model.SandboxProfileRevisionID]bool
	entries []ClosureEntry
	bytes   int
}

func (r *closureResolver) visit(ctx context.Context, ref model.SandboxProfileRef, depth int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := ValidateRef(ref); err != nil {
		return 0, err
	}
	if depth > 16 {
		return 0, fmt.Errorf("sandbox include depth exceeds 16 edges")
	}
	if r.active[ref.RevisionID] {
		return 0, fmt.Errorf("sandbox include cycle")
	}
	if previous, exists := r.seen[ref.RevisionID]; exists {
		if previous != ref {
			return 0, fmt.Errorf("sandbox revision has conflicting exact references")
		}
		height := r.height[ref.RevisionID]
		if depth+height > 16 {
			return 0, fmt.Errorf("sandbox include depth exceeds 16 edges")
		}
		return height, nil
	}
	if len(r.seen) >= 128 {
		return 0, fmt.Errorf("sandbox include closure exceeds 128 revisions")
	}
	r.seen[ref.RevisionID] = ref
	r.active[ref.RevisionID] = true
	policy, err := r.reader.ReadSandboxRevision(ctx, ref)
	if err != nil {
		return 0, err
	}
	hash, err := ContentHash(policy)
	if err != nil {
		return 0, err
	}
	if hash != ref.ContentHash {
		return 0, fmt.Errorf("sandbox revision content does not match its pinned hash")
	}
	data, err := json.Marshal(policy)
	if err != nil {
		return 0, err
	}
	r.bytes += len(data)
	if r.bytes > 16<<20 {
		return 0, fmt.Errorf("sandbox include closure exceeds 16 MiB")
	}
	var detached model.SandboxPolicy
	if err := json.Unmarshal(data, &detached); err != nil {
		return 0, err
	}
	height := 0
	for _, include := range detached.Includes {
		childHeight, err := r.visit(ctx, include, depth+1)
		if err != nil {
			return 0, err
		}
		height = max(height, childHeight+1)
	}
	r.active[ref.RevisionID] = false
	r.height[ref.RevisionID] = height
	r.entries = append(r.entries, ClosureEntry{Ref: ref, Policy: detached})
	return height, nil
}
