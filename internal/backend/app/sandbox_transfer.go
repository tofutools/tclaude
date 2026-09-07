package app

import (
	"context"
	"encoding/json"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/sandboxpolicy"
)

type SandboxImportSelection struct {
	Source     model.SandboxProfileRef        `json:"source"`
	ID         model.SandboxProfileID         `json:"id"`
	RevisionID model.SandboxProfileRevisionID `json:"revision_id"`
	Name       string                         `json:"name"`
}
type ImportSandboxProfilesRequest struct {
	Context    RequestContext
	Bundle     sandboxpolicy.Bundle
	Selections []SandboxImportSelection
}
type SandboxImportResult struct {
	Root     model.SandboxProfileRef
	Profiles []SandboxProfileResult
	Repeated bool
}
type SandboxTransferStore interface {
	ImportSandboxProfiles(context.Context, ImportSandboxProfilesRequest, time.Time) (SandboxImportResult, error)
}
type SandboxTransferAPI interface {
	ExportSandboxBundle(context.Context, model.Principal, model.SandboxProfileRef) (sandboxpolicy.Bundle, error)
	InspectSandboxBundle(context.Context, model.Principal, sandboxpolicy.Bundle) (sandboxpolicy.Bundle, error)
	ImportSandboxProfiles(context.Context, ImportSandboxProfilesRequest) (SandboxImportResult, error)
}

func (s *Service) ExportSandboxBundle(ctx context.Context, p model.Principal, ref model.SandboxProfileRef) (sandboxpolicy.Bundle, error) {
	closure, err := s.InspectSandboxClosure(ctx, p, ref)
	if err != nil {
		return sandboxpolicy.Bundle{}, err
	}
	bundle := sandboxpolicy.Bundle{Format: sandboxpolicy.BundleFormat, Version: 1, Root: ref}
	for _, entry := range closure.Entries {
		profile, err := s.store.SandboxProfile(ctx, entry.Ref.ProfileID)
		if err != nil {
			return sandboxpolicy.Bundle{}, err
		}
		bundle.Entries = append(bundle.Entries, sandboxpolicy.BundleEntry{Ref: entry.Ref, Name: profile.Profile.Name, Policy: entry.Policy})
	}
	return s.InspectSandboxBundle(ctx, p, bundle)
}
func (s *Service) InspectSandboxBundle(ctx context.Context, p model.Principal, b sandboxpolicy.Bundle) (sandboxpolicy.Bundle, error) {
	if err := requireOperator(p); err != nil {
		return sandboxpolicy.Bundle{}, err
	}
	out, err := sandboxpolicy.InspectBundle(ctx, b)
	if err != nil {
		return sandboxpolicy.Bundle{}, fail(ErrInvalid, "%v", err)
	}
	return out, nil
}
func (s *Service) ImportSandboxProfiles(ctx context.Context, req ImportSandboxProfilesRequest) (SandboxImportResult, error) {
	if _, _, err := PrepareSandboxImport(ctx, req, s.now().UTC()); err != nil {
		return SandboxImportResult{}, err
	}
	store, ok := s.store.(SandboxTransferStore)
	if !ok {
		return SandboxImportResult{}, ErrUnsupported
	}
	return store.ImportSandboxProfiles(ctx, req, s.now().UTC())
}

// PrepareSandboxImport is shared with the transaction owner. Imports create new
// profiles only; every included revision receives its own independent target.
// Nothing selects defaults, restores authority, or runs authored setup.
func PrepareSandboxImport(ctx context.Context, req ImportSandboxProfilesRequest, now time.Time) (SandboxImportResult, []byte, error) {
	if err := validateEffectContext(req.Context); err != nil {
		return SandboxImportResult{}, nil, err
	}
	if err := requireOperator(req.Context.Principal); err != nil {
		return SandboxImportResult{}, nil, err
	}
	bundle, err := sandboxpolicy.InspectBundle(ctx, req.Bundle)
	if err != nil {
		return SandboxImportResult{}, nil, fail(ErrInvalid, "%v", err)
	}
	if len(req.Selections) != len(bundle.Entries) {
		return SandboxImportResult{}, nil, fail(ErrInvalid, "select every included revision")
	}
	selected := map[model.SandboxProfileRef]SandboxImportSelection{}
	ids := map[model.SandboxProfileID]bool{}
	revisions := map[model.SandboxProfileRevisionID]bool{}
	for _, selection := range req.Selections {
		if _, ok := selected[selection.Source]; ok || ids[selection.ID] || revisions[selection.RevisionID] || selection.RevisionID.Validate() != nil {
			return SandboxImportResult{}, nil, ErrInvalid
		}
		selected[selection.Source] = selection
		ids[selection.ID] = true
		revisions[selection.RevisionID] = true
	}
	result := SandboxImportResult{Profiles: []SandboxProfileResult{}}
	refs := map[model.SandboxProfileRef]model.SandboxProfileRef{}
	for _, entry := range bundle.Entries {
		selection, ok := selected[entry.Ref]
		if !ok {
			return SandboxImportResult{}, nil, ErrInvalid
		}
		policy := entry.Policy
		for i, old := range policy.Includes {
			replacement, ok := refs[old]
			if !ok {
				return SandboxImportResult{}, nil, ErrInvalid
			}
			policy.Includes[i] = replacement
		}
		save := SaveSandboxProfileRequest{Context: req.Context, ID: selection.ID, Name: selection.Name, Policy: policy}
		if err := ValidateSandboxProfileSave(save); err != nil {
			return SandboxImportResult{}, nil, err
		}
		hash, err := sandboxpolicy.ContentHash(policy)
		if err != nil {
			return SandboxImportResult{}, nil, err
		}
		ref := model.SandboxProfileRef{ProfileID: selection.ID, RevisionID: selection.RevisionID, ContentHash: hash}
		refs[entry.Ref] = ref
		result.Profiles = append(result.Profiles, SandboxProfileResult{
			Profile:  model.SandboxProfile{ID: selection.ID, Name: selection.Name, HeadRevisionID: selection.RevisionID, Revision: 1, CreatedAt: now, UpdatedAt: now},
			Revision: model.SandboxProfileRevision{Ref: ref, Number: 1, Policy: policy, Author: req.Context.Principal, RequestID: req.Context.RequestID, CreatedAt: now},
		})
	}
	result.Root = refs[bundle.Root]
	// Retain exact authored request order and content, excluding invocation time.
	intent, err := json.Marshal(struct {
		Action     string
		Bundle     sandboxpolicy.Bundle
		Selections []SandboxImportSelection
	}{"import", req.Bundle, req.Selections})
	return result, intent, err
}
