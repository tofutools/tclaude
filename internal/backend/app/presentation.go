package app

import (
	"context"
	"github.com/tofutools/tclaude/internal/backend/model"
	"math"
)

type PresentationResult struct {
	Preferences model.PresentationPreferences
	Channels    []model.RadioChannel
}
type PutPresentationRequest struct {
	Principal        model.Principal
	Preferences      model.PresentationPreferences
	ExpectedRevision model.Revision
}

func (s *Service) ReadPresentation(ctx context.Context, principal model.Principal) (PresentationResult, error) {
	if err := requireOperator(principal); err != nil {
		return PresentationResult{}, err
	}
	prefs, err := s.store.ReadPresentation(ctx)
	return PresentationResult{Preferences: prefs, Channels: model.RadioChannels()}, err
}
func (s *Service) PutPresentation(ctx context.Context, in PutPresentationRequest) (PresentationResult, error) {
	if err := requireOperator(in.Principal); err != nil {
		return PresentationResult{}, err
	}
	if in.ExpectedRevision >= math.MaxInt64 {
		return PresentationResult{}, ErrInvalid
	}
	p := in.Preferences
	if p.Mode != "regular" && p.Mode != "slop" && p.Mode != "wizard" {
		return PresentationResult{}, ErrInvalid
	}
	for _, v := range []float64{p.MusicVolume, p.EffectsVolume} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
			return PresentationResult{}, ErrInvalid
		}
	}
	known := p.Channel == ""
	for _, c := range model.RadioChannels() {
		if c.ID == p.Channel {
			known = true
		}
	}
	if !known {
		return PresentationResult{}, ErrInvalid
	}
	prefs, err := s.store.PutPresentation(ctx, p, in.ExpectedRevision)
	return PresentationResult{Preferences: prefs, Channels: model.RadioChannels()}, err
}
