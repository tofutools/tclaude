package sandboxpolicy

import (
	"context"
	"slices"

	"github.com/tofutools/tclaude/internal/backend/model"
)

type ProfileScope string

const (
	ScopeGlobal   ProfileScope = "global"
	ScopeGroup    ProfileScope = "group"
	ScopeExplicit ProfileScope = "explicit"
)

type ScopeSelection struct {
	Scope ProfileScope
	Ref   model.SandboxProfileRef
}

// ScopeComposition retains the exact scope inputs. Combined.Root is empty:
// a scope union is not itself a persisted profile revision.
type ScopeComposition struct {
	Applied  []ScopeSelection
	Combined Composition
}

// ComposeScopes applies fixed global/group/explicit precedence, independent of
// caller order. Scope union deliberately differs from include overrides: deny
// dominates write, tmpfs ceilings narrow, and literal/generated collisions fail.
func ComposeScopes(ctx context.Context, selections []ScopeSelection, reader RevisionReader, paths HostPathResolver) (ScopeComposition, error) {
	ordered := slices.Clone(selections)
	seen := map[ProfileScope]bool{}
	for _, selection := range ordered {
		if scopeRank(selection.Scope) < 0 || seen[selection.Scope] {
			return ScopeComposition{}, invalidClosure("sandbox scopes must be unique global, group or explicit selections")
		}
		seen[selection.Scope] = true
	}
	slices.SortFunc(ordered, func(a, b ScopeSelection) int { return scopeRank(a.Scope) - scopeRank(b.Scope) })
	out := ScopeComposition{Applied: ordered, Combined: Composition{Values: model.SandboxPolicy{Environment: model.Environment{}}}}
	for _, selection := range ordered {
		next, err := ComposeIncludes(ctx, selection.Ref, reader, paths)
		if err != nil {
			return ScopeComposition{}, err
		}
		current := &out.Combined.Values
		for name := range next.Values.Environment {
			if slices.Contains(current.AgentDirectories, name) {
				return ScopeComposition{}, invalidClosure("environment variable is both literal and generated across scopes")
			}
		}
		for _, name := range next.Values.AgentDirectories {
			if _, exists := current.Environment[name]; exists {
				return ScopeComposition{}, invalidClosure("environment variable is both literal and generated across scopes")
			}
		}
		for i, rule := range next.Values.Filesystem {
			for _, old := range current.Filesystem {
				if guestKey(old) == guestKey(rule) && accessRank(old.Access) > accessRank(rule.Access) {
					next.Values.Filesystem[i].Access = old.Access
				}
			}
		}
		for i, mount := range next.Values.Tmpfs {
			for _, old := range current.Tmpfs {
				if old.GuestPath != mount.GuestPath || old.Size == "" {
					continue
				}
				if mount.Size == "" {
					next.Values.Tmpfs[i] = old
					continue
				}
				oldBytes, err := parseByteQuantity("tmpfs size", old.Size)
				if err != nil {
					return ScopeComposition{}, err
				}
				newBytes, err := parseByteQuantity("tmpfs size", mount.Size)
				if err != nil {
					return ScopeComposition{}, err
				}
				if oldBytes < newBytes || oldBytes == newBytes && old.Size < mount.Size {
					next.Values.Tmpfs[i] = old
				}
			}
		}
		if err := mergeComposition(&out.Combined, next); err != nil {
			return ScopeComposition{}, err
		}
	}
	if err := validateCompositionValues(out.Combined.Values); err != nil {
		return ScopeComposition{}, err
	}
	return out, nil
}
func scopeRank(scope ProfileScope) int {
	switch scope {
	case ScopeGlobal:
		return 0
	case ScopeGroup:
		return 1
	case ScopeExplicit:
		return 2
	default:
		return -1
	}
}
func accessRank(access model.SandboxFilesystemAccess) int {
	switch access {
	case model.SandboxFilesystemDeny:
		return 2
	case model.SandboxFilesystemWrite:
		return 1
	default:
		return 0
	}
}
