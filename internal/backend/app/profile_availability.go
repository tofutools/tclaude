package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/tofutools/tclaude/internal/backend/model"
	"strings"
	"time"
	"unicode/utf8"
)

type ProfileDisabledError struct{ Name, Reason string }

func (e *ProfileDisabledError) Error() string {
	reason := e.Reason
	if reason == "" {
		reason = "no reason provided"
	}
	return fmt.Sprintf("Configuration %q is disabled: %s", e.Name, reason)
}
func (e *ProfileDisabledError) Unwrap() error { return ErrConflict }
func ConfigurationProfileEnabled(profile model.ConfigurationProfile) error {
	if profile.Disabled {
		return &ProfileDisabledError{Name: profile.Name, Reason: profile.DisabledReason}
	}
	return nil
}

type SetConfigurationProfileAvailabilityRequest struct {
	Context          RequestContext
	ID               model.ConfigurationProfileID
	ExpectedRevision model.Revision
	Disabled         bool
	Reason           *string
}
type ConfigurationProfileAvailabilityWrite struct {
	ID               model.ConfigurationProfileID
	ExpectedRevision model.Revision
	Disabled         bool
	Reason           *string
	RequestID        model.RequestID
	Fingerprint      string
	At               time.Time
}

func (s *Service) SetConfigurationProfileAvailability(ctx context.Context, req SetConfigurationProfileAvailabilityRequest) (model.ConfigurationProfile, error) {
	if err := requireOperator(req.Context.Principal); err != nil {
		return model.ConfigurationProfile{}, err
	}
	if err := validateEffectContext(req.Context); err != nil {
		return model.ConfigurationProfile{}, err
	}
	if model.ValidateStableID("profile", string(req.ID)) != nil {
		return model.ConfigurationProfile{}, ErrInvalid
	}
	if req.Reason != nil {
		value := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(*req.Reason, "\r\n", "\n"), "\r", "\n"))
		if len(value) > 1024 || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			return model.ConfigurationProfile{}, ErrInvalid
		}
		req.Reason = &value
	}
	payload, _ := json.Marshal(struct {
		Action   string
		ID       model.ConfigurationProfileID
		Expected model.Revision
		Disabled bool
		Reason   *string
	}{"availability", req.ID, req.ExpectedRevision, req.Disabled, req.Reason})
	digest := sha256.Sum256(payload)
	return s.store.SetConfigurationProfileAvailability(ctx, ConfigurationProfileAvailabilityWrite{ID: req.ID, ExpectedRevision: req.ExpectedRevision, Disabled: req.Disabled, Reason: req.Reason, RequestID: req.Context.RequestID, Fingerprint: hex.EncodeToString(digest[:]), At: s.now().UTC()})
}
