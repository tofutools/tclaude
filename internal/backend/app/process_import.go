package app

import (
	"context"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/processimport"
)

type ConvertProcessImportRequest struct {
	Principal model.Principal
	Source    string
	ID        model.DefinitionID
	Bindings  map[string]processimport.Binding
}

type ProcessImportResult struct {
	Draft   DefinitionDraft
	Notices []string
}

type ProcessImportAPI interface {
	InspectProcessImport(context.Context, model.Principal, string) (processimport.Inspection, error)
	ConvertProcessImport(context.Context, ConvertProcessImportRequest) (ProcessImportResult, error)
}

func (s *Service) InspectProcessImport(_ context.Context, principal model.Principal, source string) (processimport.Inspection, error) {
	if principal.Kind != model.PrincipalOperator {
		return processimport.Inspection{}, ErrUnauthorized
	}
	result, err := processimport.Inspect(source)
	if err != nil {
		return result, fail(ErrInvalid, "%v", err)
	}
	return result, nil
}

// ConvertProcessImport returns an unsaved draft. The ordinary definition
// authoring transaction remains the sole publication path.
func (s *Service) ConvertProcessImport(ctx context.Context, req ConvertProcessImportRequest) (ProcessImportResult, error) {
	if req.Principal.Kind != model.PrincipalOperator {
		return ProcessImportResult{}, ErrUnauthorized
	}
	if err := req.ID.Validate(); err != nil {
		return ProcessImportResult{}, fail(ErrInvalid, "%v", err)
	}
	converted, err := processimport.Convert(req.Source, req.Bindings)
	if err != nil {
		return ProcessImportResult{}, fail(ErrInvalid, "%v", err)
	}
	for path, executable := range converted.ProgramExecutables {
		ref := req.Bindings[path].Performer.Program.Profile
		revision, err := s.store.ProgramProfileRevision(ctx, ref.RevisionID)
		if err != nil {
			return ProcessImportResult{}, err
		}
		if revision.ProfileID != ref.ProfileID || revision.ContentHash != ref.ContentHash {
			return ProcessImportResult{}, ErrConflict
		}
		if revision.Executable != executable || len(revision.ArgumentPrefix) != 0 {
			return ProcessImportResult{}, fail(ErrInvalid, "%s: selected program must match the literal executable with no argument prefix", path)
		}
	}
	draft := DefinitionDraft{ID: req.ID, Name: converted.Name, Kind: model.DefinitionProcess, SchemaVersion: 1, Source: converted.Source, Parameters: converted.Parameters, Process: &converted.Process, EditorLayout: converted.Layout}
	if _, err = s.ValidateDefinition(ctx, ValidateDefinitionRequest{Principal: req.Principal, Draft: draft}); err != nil {
		return ProcessImportResult{}, err
	}
	return ProcessImportResult{Draft: draft, Notices: converted.Notices}, nil
}
