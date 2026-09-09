package app

import (
	"context"
	"slices"

	"github.com/tofutools/tclaude/internal/backend/model"
)

type PutDenialRequest struct {
	Principal        model.Principal
	Denial           model.AuthorityDenial
	ExpectedRevision model.Revision
}
type DeleteDenialRequest struct {
	Principal        model.Principal
	DenialID         model.DenialID
	ExpectedRevision model.Revision
}
type DenialResult struct{ Denial model.AuthorityDenial }

func ValidateAuthorityDenial(denial model.AuthorityDenial) error {
	if err := denial.ID.Validate(); err != nil {
		return fail(ErrInvalid, "%v", err)
	}
	if !slices.Contains(allActions, denial.Action) {
		return fail(ErrInvalid, "unknown denied action")
	}
	switch denial.Subject.Kind {
	case model.AuthorityAgent:
		if denial.Subject.ExecutionID != "" || denial.Subject.AgentID.Validate() != nil {
			return fail(ErrInvalid, "denial requires an exact agent")
		}
	case model.AuthorityExecution:
		if denial.Subject.AgentID != "" || denial.Subject.ExecutionID.Validate() != nil {
			return fail(ErrInvalid, "denial requires an exact execution")
		}
	default:
		return fail(ErrInvalid, "invalid denial subject")
	}
	return nil
}
func (s *Service) PutDenial(ctx context.Context, req PutDenialRequest) (DenialResult, error) {
	if err := requireOperator(req.Principal); err != nil {
		return DenialResult{}, err
	}
	if err := ValidateAuthorityDenial(req.Denial); err != nil {
		return DenialResult{}, err
	}
	denial := req.Denial
	now := s.now().UTC()
	if req.ExpectedRevision == 0 {
		denial.CreatedAt = now
	}
	denial.UpdatedAt = now
	saved, err := s.store.PutDenial(ctx, denial, req.ExpectedRevision)
	return DenialResult{Denial: saved}, err
}
func (s *Service) DeleteDenial(ctx context.Context, req DeleteDenialRequest) error {
	if err := requireOperator(req.Principal); err != nil {
		return err
	}
	return s.store.DeleteDenial(ctx, req.DenialID, req.ExpectedRevision)
}
