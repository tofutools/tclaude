package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tofutools/tclaude/internal/backend/model"
)

type ProcessSnippetRequest struct {
	Context          RequestContext
	ID               string
	Action           string
	Name             string
	Selection        json.RawMessage
	ExpectedRevision model.Revision
}
type ProcessSnippetStore interface {
	ListProcessSnippets(context.Context) ([]model.ProcessSnippet, error)
	WriteProcessSnippet(context.Context, ProcessSnippetRequest, time.Time) (model.ProcessSnippet, error)
}
type ProcessSnippetAPI interface {
	ListProcessSnippets(context.Context, model.Principal) ([]model.ProcessSnippet, error)
	WriteProcessSnippet(context.Context, ProcessSnippetRequest) (model.ProcessSnippet, error)
}

// ValidateProcessSelection checks a bounded fragment, not graph executability.
// References and incomplete nodes are intentionally preserved for later authoring.
func ValidateProcessSelection(data json.RawMessage) error {
	_, err := CanonicalProcessSelection(data)
	return err
}

// CanonicalProcessSelection validates and normalizes keys for case-sensitive browser reads
// and comparison of retained pre-normalization request receipts.
func CanonicalProcessSelection(data json.RawMessage) (json.RawMessage, error) {
	if len(data) == 0 || len(data) > 256<<10 || !utf8.Valid(data) {
		return nil, ErrInvalid
	}
	var selection model.ProcessSelection
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&selection) != nil || d.Decode(new(any)) != io.EOF || selection.Version != 1 || selection.Edges == nil || selection.Positions == nil || len(selection.Nodes) == 0 || len(selection.Nodes) > 100 || len(selection.Edges) > 300 || len(selection.Positions) > 100 {
		return nil, ErrInvalid
	}
	ids := map[model.WorkNodeID]bool{}
	for _, n := range selection.Nodes {
		if model.ValidateStableID("node", string(n.ID)) != nil || ids[n.ID] {
			return nil, ErrInvalid
		}
		ids[n.ID] = true
		switch n.Kind {
		case model.WorkNodeTask, model.WorkNodeDecision, model.WorkNodeFork, model.WorkNodeJoin, model.WorkNodeWait, model.WorkNodeEnd:
		default:
			return nil, ErrInvalid
		}
	}
	for _, e := range selection.Edges {
		if !ids[e.From] || !ids[e.To] {
			return nil, ErrInvalid
		}
	}
	for id, p := range selection.Positions {
		if !ids[id] || math.IsNaN(p.X) || math.IsNaN(p.Y) || math.IsInf(p.X, 0) || math.IsInf(p.Y, 0) || math.Abs(p.X) > 1e7 || math.Abs(p.Y) > 1e7 {
			return nil, ErrInvalid
		}
	}
	out, err := json.Marshal(selection)
	if err != nil || len(out) > 256<<10 {
		return nil, ErrInvalid
	}
	return out, nil
}
func (s *Service) ListProcessSnippets(ctx context.Context, p model.Principal) ([]model.ProcessSnippet, error) {
	if err := requireOperator(p); err != nil {
		return nil, err
	}
	store, ok := s.store.(ProcessSnippetStore)
	if !ok {
		return nil, ErrUnsupported
	}
	snippets, err := store.ListProcessSnippets(ctx)
	for i := range snippets {
		normalized, normalizeErr := CanonicalProcessSelection(snippets[i].Selection)
		snippets[i].Selection = normalized
		snippets[i].Available = normalizeErr == nil
		if !snippets[i].Available {
			snippets[i].Selection = nil
		}
	}
	return snippets, err
}
func (s *Service) WriteProcessSnippet(ctx context.Context, in ProcessSnippetRequest) (model.ProcessSnippet, error) {
	if err := requireOperator(in.Context.Principal); err != nil {
		return model.ProcessSnippet{}, err
	}
	if model.ValidateStableID("snippet", in.ID) != nil || in.Context.RequestID.Validate() != nil || in.ExpectedRevision >= math.MaxInt64 {
		return model.ProcessSnippet{}, ErrInvalid
	}
	switch in.Action {
	case "create":
		normalized, normalizeErr := CanonicalProcessSelection(in.Selection)
		if in.ExpectedRevision != 0 || normalizeErr != nil {
			return model.ProcessSnippet{}, ErrInvalid
		}
		in.Selection = normalized
	case "rename", "delete":
		if in.ExpectedRevision == 0 || len(in.Selection) != 0 {
			return model.ProcessSnippet{}, ErrInvalid
		}
	default:
		return model.ProcessSnippet{}, ErrInvalid
	}
	if in.Action == "delete" {
		if in.Name != "" {
			return model.ProcessSnippet{}, ErrInvalid
		}
	} else {
		if strings.TrimSpace(in.Name) != in.Name || in.Name == "" || len(in.Name) > 160 || utf8.RuneCountInString(in.Name) > 80 || !utf8.ValidString(in.Name) || strings.IndexFunc(in.Name, unicode.IsControl) >= 0 {
			return model.ProcessSnippet{}, ErrInvalid
		}
	}
	store, ok := s.store.(ProcessSnippetStore)
	if !ok {
		return model.ProcessSnippet{}, ErrUnsupported
	}
	result, err := store.WriteProcessSnippet(ctx, in, s.now().UTC())
	normalized, normalizeErr := CanonicalProcessSelection(result.Selection)
	result.Selection = normalized
	result.Available = !result.Deleted && normalizeErr == nil
	if !result.Available {
		result.Selection = nil
	}
	return result, err
}
