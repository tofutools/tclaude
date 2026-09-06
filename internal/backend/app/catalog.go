package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
)

type ConfigurationCatalogStore interface {
	SaveConfigurationProfile(context.Context, ConfigurationProfileWrite) (ConfigurationProfileResult, error)
	ConfigurationProfile(context.Context, model.ConfigurationProfileID, model.ConfigurationProfileRevisionID) (ConfigurationProfileResult, error)
	ConfigurationProfiles(context.Context) ([]model.ConfigurationProfile, error)
}

type ConfigurationProfileWrite struct {
	Profile            model.ConfigurationProfile
	Revision           model.ConfigurationProfileRevision
	ExpectedRevision   model.Revision
	RequestID          model.RequestID
	RequestFingerprint string
	At                 time.Time
}

type ConfigurationProfileResult struct {
	Profile  model.ConfigurationProfile
	Revision model.ConfigurationProfileRevision
}

type SaveConfigurationProfileRequest struct {
	Context          RequestContext
	ID               model.ConfigurationProfileID
	RevisionID       model.ConfigurationProfileRevisionID
	Name             string
	Desired          model.DesiredConfiguration
	ExpectedRevision model.Revision
}

type ConfigurationCatalogAPI interface {
	SaveConfigurationProfile(context.Context, SaveConfigurationProfileRequest) (ConfigurationProfileResult, error)
	GetConfigurationProfile(context.Context, model.Principal, model.ConfigurationProfileRef) (ConfigurationProfileResult, error)
	ListConfigurationProfiles(context.Context, model.Principal) ([]model.ConfigurationProfile, error)
}

func (s *Service) SaveConfigurationProfile(ctx context.Context, req SaveConfigurationProfileRequest) (ConfigurationProfileResult, error) {
	if err := requireOperator(req.Context.Principal); err != nil {
		return ConfigurationProfileResult{}, err
	}
	if model.ValidateStableID("configuration profile", string(req.ID)) != nil || model.ValidateStableID("configuration revision", string(req.RevisionID)) != nil || req.Context.RequestID.Validate() != nil || strings.TrimSpace(req.Name) == "" || len(req.Name) > 256 {
		return ConfigurationProfileResult{}, ErrInvalid
	}
	if err := validateDesired(req.Desired); err != nil {
		return ConfigurationProfileResult{}, err
	}
	payload, _ := json.Marshal(req.Desired)
	digest := sha256.Sum256(payload)
	input, _ := json.Marshal(struct {
		ID         model.ConfigurationProfileID
		RevisionID model.ConfigurationProfileRevisionID
		Name       string
		Desired    model.DesiredConfiguration
		Expected   model.Revision
	}{req.ID, req.RevisionID, req.Name, req.Desired, req.ExpectedRevision})
	fingerprint := sha256.Sum256(input)
	now := s.now().UTC()
	return s.store.SaveConfigurationProfile(ctx, ConfigurationProfileWrite{
		Profile:          model.ConfigurationProfile{ID: req.ID, Name: req.Name, CurrentRevisionID: req.RevisionID},
		Revision:         model.ConfigurationProfileRevision{Ref: model.ConfigurationProfileRef{ProfileID: req.ID, RevisionID: req.RevisionID, ContentHash: hex.EncodeToString(digest[:])}, Desired: req.Desired, CreatedAt: now},
		ExpectedRevision: req.ExpectedRevision, RequestID: req.Context.RequestID, RequestFingerprint: hex.EncodeToString(fingerprint[:]), At: now,
	})
}

func (s *Service) GetConfigurationProfile(ctx context.Context, principal model.Principal, ref model.ConfigurationProfileRef) (ConfigurationProfileResult, error) {
	if err := requireOperator(principal); err != nil {
		return ConfigurationProfileResult{}, err
	}
	result, err := s.store.ConfigurationProfile(ctx, ref.ProfileID, ref.RevisionID)
	if err == nil && ref.ContentHash != "" && ref.ContentHash != result.Revision.Ref.ContentHash {
		return ConfigurationProfileResult{}, ErrConflict
	}
	return result, err
}
func (s *Service) ListConfigurationProfiles(ctx context.Context, principal model.Principal) ([]model.ConfigurationProfile, error) {
	if err := requireOperator(principal); err != nil {
		return nil, err
	}
	return s.store.ConfigurationProfiles(ctx)
}
