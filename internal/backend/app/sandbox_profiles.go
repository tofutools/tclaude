package app

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/sandboxpolicy"
)

type SandboxProfileStore interface {
	sandboxpolicy.RevisionReader
	SetSandboxProfileArchived(context.Context, SetSandboxProfileArchivedRequest, time.Time) (SandboxProfileResult, error)
	SaveSandboxProfile(context.Context, SaveSandboxProfileRequest, model.SandboxProfileRevisionID, time.Time) (SandboxProfileResult, error)
	SandboxProfile(context.Context, model.SandboxProfileID) (SandboxProfileResult, error)
	ListSandboxProfiles(context.Context, bool) ([]model.SandboxProfile, error)
}

type SaveSandboxProfileRequest struct {
	Context          RequestContext
	ID               model.SandboxProfileID
	ExpectedRevision model.Revision
	Name             string
	Policy           model.SandboxPolicy
}

type SandboxProfileResult struct {
	Profile  model.SandboxProfile
	Revision model.SandboxProfileRevision
	Repeated bool
}

// ValidateSandboxProfileSave is shared with the transaction owner so an invalid
// direct persistence call cannot bypass the authoring or operator boundary.
func ValidateSandboxProfileSave(req SaveSandboxProfileRequest) error {
	if err := validateEffectContext(req.Context); err != nil {
		return err
	}
	if err := requireOperator(req.Context.Principal); err != nil {
		return err
	}
	if err := req.ID.Validate(); err != nil {
		return fail(ErrInvalid, "%v", err)
	}
	if req.ExpectedRevision >= math.MaxInt64 {
		return ErrInvalid
	}
	if req.Name == "" || req.Name != strings.TrimSpace(req.Name) || len(req.Name) > 200 || !utf8.ValidString(req.Name) || strings.ContainsRune(req.Name, 0) {
		return fail(ErrInvalid, "sandbox profile requires a name of at most 200 bytes")
	}
	if err := sandboxpolicy.Validate(req.Policy); err != nil {
		return fail(ErrInvalid, "%v", err)
	}
	return nil
}

func (s *Service) SaveSandboxProfile(ctx context.Context, req SaveSandboxProfileRequest) (SandboxProfileResult, error) {
	if err := ValidateSandboxProfileSave(req); err != nil {
		return SandboxProfileResult{}, err
	}
	return s.store.SaveSandboxProfile(ctx, req, model.SandboxProfileRevisionID(s.newID("sandbox_rev_")), s.now().UTC())
}

func (s *Service) GetSandboxProfile(ctx context.Context, principal model.Principal, id model.SandboxProfileID) (SandboxProfileResult, error) {
	if err := requireOperator(principal); err != nil {
		return SandboxProfileResult{}, err
	}
	if err := id.Validate(); err != nil {
		return SandboxProfileResult{}, fail(ErrInvalid, "%v", err)
	}
	return s.store.SandboxProfile(ctx, id)
}

func (s *Service) ListSandboxProfiles(ctx context.Context, principal model.Principal, includeArchived bool) ([]model.SandboxProfile, error) {
	if err := requireOperator(principal); err != nil {
		return nil, err
	}
	return s.store.ListSandboxProfiles(ctx, includeArchived)
}

// InspectSandboxClosure reads only pinned authoring data. It deliberately does
// not describe host availability, canonical paths, or successful enforcement.
func (s *Service) InspectSandboxClosure(ctx context.Context, principal model.Principal, ref model.SandboxProfileRef) (sandboxpolicy.Closure, error) {
	if err := requireOperator(principal); err != nil {
		return sandboxpolicy.Closure{}, err
	}
	closure, err := sandboxpolicy.Resolve(ctx, ref, s.store)
	if errors.Is(err, sandboxpolicy.ErrInvalidClosure) {
		return sandboxpolicy.Closure{}, fail(ErrInvalid, "%v", err)
	}
	return closure, err
}

type SetSandboxProfileArchivedRequest struct {
	Context          RequestContext
	ID               model.SandboxProfileID
	ExpectedRevision model.Revision
	Archived         bool
}

func ValidateSandboxArchive(req SetSandboxProfileArchivedRequest) error {
	if err := validateEffectContext(req.Context); err != nil {
		return err
	}
	if err := requireOperator(req.Context.Principal); err != nil {
		return err
	}
	if err := req.ID.Validate(); err != nil {
		return fail(ErrInvalid, "%v", err)
	}
	if req.ExpectedRevision == 0 || req.ExpectedRevision >= math.MaxInt64 {
		return ErrInvalid
	}
	return nil
}

func (s *Service) SetSandboxProfileArchived(ctx context.Context, req SetSandboxProfileArchivedRequest) (SandboxProfileResult, error) {
	if err := ValidateSandboxArchive(req); err != nil {
		return SandboxProfileResult{}, err
	}
	return s.store.SetSandboxProfileArchived(ctx, req, s.now().UTC())
}

type SandboxProfileAPI interface {
	SaveSandboxProfile(context.Context, SaveSandboxProfileRequest) (SandboxProfileResult, error)
	GetSandboxProfile(context.Context, model.Principal, model.SandboxProfileID) (SandboxProfileResult, error)
	ListSandboxProfiles(context.Context, model.Principal, bool) ([]model.SandboxProfile, error)
	InspectSandboxClosure(context.Context, model.Principal, model.SandboxProfileRef) (sandboxpolicy.Closure, error)
	SetSandboxProfileArchived(context.Context, SetSandboxProfileArchivedRequest) (SandboxProfileResult, error)
}
