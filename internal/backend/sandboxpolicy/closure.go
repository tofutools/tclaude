package sandboxpolicy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/tofutools/tclaude/internal/backend/model"
)

// ErrInvalidClosure distinguishes an invalid authored graph from a reader failure.
var ErrInvalidClosure = errors.New("invalid sandbox include closure")

func invalidClosure(message string) error { return fmt.Errorf("%w: %s", ErrInvalidClosure, message) }

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
	Root             model.SandboxProfileRef
	Entries          []ClosureEntry
	ResolvedIncludes map[model.SandboxProfileRevisionID][]model.SandboxProfileRef
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
	return resolve(ctx, root, reader, false)
}

// CurrentRevisionReader resolves editable registry entries for a new operation.
// RevisionReader alone is sufficient when reading a self-contained export.
type CurrentRevisionReader interface {
	CurrentSandboxRef(context.Context, model.SandboxProfileID) (model.SandboxProfileRef, error)
}

func ResolveCurrent(ctx context.Context, root model.SandboxProfileRef, reader RevisionReader) (Closure, error) {
	return resolve(ctx, root, reader, true)
}
func resolve(ctx context.Context, root model.SandboxProfileRef, reader RevisionReader, current bool) (Closure, error) {
	if reader == nil {
		return Closure{}, invalidClosure("sandbox revision reader is required")
	}
	resolver := closureResolver{reader: reader, current: current, refs: map[model.SandboxProfileID]model.SandboxProfileRef{}, edges: map[model.SandboxProfileRevisionID][]model.SandboxProfileRef{}, seen: map[model.SandboxProfileRevisionID]model.SandboxProfileRef{}, height: map[model.SandboxProfileRevisionID]int{}, active: map[model.SandboxProfileRevisionID]bool{}}
	if _, err := resolver.visit(ctx, root, 0); err != nil {
		return Closure{}, err
	}
	return Closure{Root: resolver.entries[len(resolver.entries)-1].Ref, Entries: resolver.entries, ResolvedIncludes: resolver.edges}, nil
}

type closureResolver struct {
	reader  RevisionReader
	current bool
	refs    map[model.SandboxProfileID]model.SandboxProfileRef
	seen    map[model.SandboxProfileRevisionID]model.SandboxProfileRef
	height  map[model.SandboxProfileRevisionID]int
	active  map[model.SandboxProfileRevisionID]bool
	entries []ClosureEntry
	edges   map[model.SandboxProfileRevisionID][]model.SandboxProfileRef
	bytes   int
}

func (r *closureResolver) visit(ctx context.Context, ref model.SandboxProfileRef, depth int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if r.current || ref.RevisionID == "" {
		if reader, ok := r.reader.(CurrentRevisionReader); ok {
			actual, found := r.refs[ref.ProfileID]
			if !found {
				var err error
				actual, err = reader.CurrentSandboxRef(ctx, ref.ProfileID)
				if err != nil {
					return 0, err
				}
				r.refs[ref.ProfileID] = actual
			}
			ref = actual
		}
	}
	if err := ValidateRef(ref); err != nil {
		return 0, invalidClosure(err.Error())
	}
	if depth > 16 {
		return 0, invalidClosure("sandbox include depth exceeds 16 edges")
	}
	if r.active[ref.RevisionID] {
		return 0, invalidClosure("sandbox include cycle")
	}
	if previous, exists := r.seen[ref.RevisionID]; exists {
		if previous != ref {
			return 0, invalidClosure("sandbox revision has conflicting exact references")
		}
		height := r.height[ref.RevisionID]
		if depth+height > 16 {
			return 0, invalidClosure("sandbox include depth exceeds 16 edges")
		}
		return height, nil
	}
	if len(r.seen) >= 128 {
		return 0, invalidClosure("sandbox include closure exceeds 128 revisions")
	}
	r.seen[ref.RevisionID] = ref
	r.active[ref.RevisionID] = true
	policy, err := r.reader.ReadSandboxRevision(ctx, ref)
	if err != nil {
		return 0, err
	}
	hash, err := ContentHash(policy)
	if err != nil {
		return 0, invalidClosure(err.Error())
	}
	if hash != ref.ContentHash {
		return 0, invalidClosure("sandbox revision content does not match its pinned hash")
	}
	data, err := json.Marshal(policy)
	if err != nil {
		return 0, err
	}
	r.bytes += len(data)
	if r.bytes > 16<<20 {
		return 0, invalidClosure("sandbox include closure exceeds 16 MiB")
	}
	var detached model.SandboxPolicy
	if err := json.Unmarshal(data, &detached); err != nil {
		return 0, err
	}
	height := 0
	resolvedIncludes := append([]model.SandboxProfileRef(nil), detached.Includes...)
	for i, include := range detached.Includes {
		childHeight, err := r.visit(ctx, include, depth+1)
		if err != nil {
			return 0, err
		}
		height = max(height, childHeight+1)
		if actual, ok := r.refs[include.ProfileID]; ok {
			resolvedIncludes[i] = actual
		}
	}
	r.edges[ref.RevisionID] = resolvedIncludes
	r.active[ref.RevisionID] = false
	r.height[ref.RevisionID] = height
	r.entries = append(r.entries, ClosureEntry{Ref: ref, Policy: detached})
	return height, nil
}
