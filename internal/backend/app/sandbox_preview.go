package app

import (
	"context"
	"errors"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/sandboxpolicy"
)

type SandboxPreviewAPI interface {
	PreviewSandboxPolicy(context.Context, model.Principal, model.SandboxPolicy) (SandboxPolicyPreview, error)
}

type SandboxPolicyPathPreview struct {
	// Source is absent for this draft and exact for an included revision.
	Source      *model.SandboxProfileRef `json:",omitempty"`
	Observation ports.SandboxPathObservation
}

type SandboxPolicyPreview struct {
	ContentHash     string
	Composition     sandboxpolicy.Composition
	Materialization sandboxpolicy.PolicyMaterialization
	Includes        []sandboxpolicy.ClosureEntry
	Paths           []SandboxPolicyPathPreview
}

func (s *Service) WithSandboxPathInspector(inspector ports.SandboxPathInspector) *Service {
	s.sandboxPaths = inspector
	return s
}

// PreviewSandboxPolicy validates the draft and its pinned includes, then reads
// filesystem observations and include composition. It does not predict enforcement.
func (s *Service) PreviewSandboxPolicy(ctx context.Context, principal model.Principal, policy model.SandboxPolicy) (SandboxPolicyPreview, error) {
	if err := requireOperator(principal); err != nil {
		return SandboxPolicyPreview{}, err
	}
	hash, err := sandboxpolicy.ContentHash(policy)
	if err != nil {
		return SandboxPolicyPreview{}, fail(ErrInvalid, "%v", err)
	}
	if s.sandboxPaths == nil {
		return SandboxPolicyPreview{}, fail(ErrUnavailable, "sandbox path inspection is not configured")
	}
	draft := model.SandboxProfileRef{ProfileID: model.SandboxProfileID("preview_" + hash[:55]), RevisionID: model.SandboxProfileRevisionID("preview_" + hash[:55]), ContentHash: hash}
	closure, err := sandboxpolicy.ResolveCurrent(ctx, draft, sandboxDraftReader{store: s.store, ref: draft, policy: policy})
	if err != nil {
		if errors.Is(err, sandboxpolicy.ErrInvalidClosure) {
			return SandboxPolicyPreview{}, fail(ErrInvalid, "%v", err)
		}
		return SandboxPolicyPreview{}, err
	}
	result := SandboxPolicyPreview{ContentHash: hash, Includes: []sandboxpolicy.ClosureEntry{}, Paths: []SandboxPolicyPathPreview{}}
	for _, entry := range closure.Entries {
		observations, err := s.sandboxPaths.InspectSandboxPaths(ctx, entry.Policy.Filesystem)
		if err != nil {
			return SandboxPolicyPreview{}, fail(ErrUnavailable, "sandbox paths could not be inspected: %v", err)
		}
		var source *model.SandboxProfileRef
		if entry.Ref != draft {
			ref := entry.Ref
			source = &ref
			result.Includes = append(result.Includes, entry)
		}
		for _, observation := range observations {
			result.Paths = append(result.Paths, SandboxPolicyPathPreview{Source: source, Observation: observation})
		}
	}
	composition, err := sandboxpolicy.MaterializeIncludes(ctx, draft, sandboxDraftReader{store: s.store, ref: draft, policy: policy}, s.sandboxPaths)
	if err != nil {
		if errors.Is(err, sandboxpolicy.ErrInvalidClosure) {
			return SandboxPolicyPreview{}, fail(ErrInvalid, "%v", err)
		}
		return SandboxPolicyPreview{}, fail(ErrUnavailable, "sandbox composition could not be resolved: %v", err)
	}
	result.Composition = composition.Composition
	result.Materialization = composition
	return result, nil
}

type sandboxDraftReader struct {
	store  SandboxProfileStore
	ref    model.SandboxProfileRef
	policy model.SandboxPolicy
}

func (r sandboxDraftReader) ReadSandboxRevision(ctx context.Context, ref model.SandboxProfileRef) (model.SandboxPolicy, error) {
	if ref == r.ref {
		return r.policy, nil
	}
	return r.store.ReadSandboxRevision(ctx, ref)
}

func (r sandboxDraftReader) CurrentSandboxRef(ctx context.Context, id model.SandboxProfileID) (model.SandboxProfileRef, error) {
	if id == r.ref.ProfileID {
		return r.ref, nil
	}
	profile, err := r.store.SandboxProfile(ctx, id)
	return profile.Revision.Ref, err
}
