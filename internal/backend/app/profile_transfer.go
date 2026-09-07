package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/tofutools/tclaude/internal/backend/model"
)

const ConfigurationBundleFormat = "tclaude-configuration-profiles"
const ConfigurationBundleLimit = 128
const ConfigurationBundleMaxBytes = 1 << 20

type ConfigurationBundle struct {
	Format   string                     `json:"format"`
	Version  int                        `json:"version"`
	Profiles []ConfigurationBundleEntry `json:"profiles"`
}
type ConfigurationBundleEntry struct {
	Key      string                     `json:"key"`
	Name     string                     `json:"name"`
	Desired  model.DesiredConfiguration `json:"desired"`
	Startup  *model.ProfileStartup      `json:"startup,omitempty"`
	Archived bool                       `json:"archived"`
}
type ConfigurationImportSelection struct {
	Key              string                               `json:"key"`
	ID               model.ConfigurationProfileID         `json:"id"`
	RevisionID       model.ConfigurationProfileRevisionID `json:"revision_id"`
	ExpectedRevision model.Revision                       `json:"expected_revision"`
	Name             string                               `json:"name"`
}
type ImportConfigurationsRequest struct {
	Context    RequestContext
	Bundle     ConfigurationBundle
	Selections []ConfigurationImportSelection
}
type ConfigurationImportResult struct {
	Profiles []ConfigurationProfileResult
	Repeated bool
}
type ConfigurationBundleWrite struct {
	RequestID   model.RequestID
	Fingerprint string
	Profiles    []ConfigurationProfileWrite
}
type ConfigurationTransferStore interface {
	ImportConfigurations(context.Context, ConfigurationBundleWrite) (ConfigurationImportResult, error)
}
type ConfigurationTransferAPI interface {
	InspectConfigurationBundle(context.Context, model.Principal, ConfigurationBundle) (ConfigurationBundle, error)
	ImportConfigurations(context.Context, ImportConfigurationsRequest) (ConfigurationImportResult, error)
}

// Inspection validates authored values without allocating IDs or changing the catalog.
func (s *Service) InspectConfigurationBundle(_ context.Context, principal model.Principal, bundle ConfigurationBundle) (ConfigurationBundle, error) {
	if err := requireOperator(principal); err != nil {
		return ConfigurationBundle{}, err
	}
	if bundle.Format != ConfigurationBundleFormat || bundle.Version != 1 || len(bundle.Profiles) == 0 || len(bundle.Profiles) > ConfigurationBundleLimit {
		return ConfigurationBundle{}, fail(ErrInvalid, "unsupported configuration bundle format/version or entry count")
	}
	data, err := json.Marshal(bundle)
	if err != nil || len(data) > ConfigurationBundleMaxBytes {
		return ConfigurationBundle{}, ErrInvalid
	}
	seen := map[string]bool{}
	for i := range bundle.Profiles {
		entry := &bundle.Profiles[i]
		if entry.Key == "" || len(entry.Key) > 256 || !utf8.ValidString(entry.Key) || seen[entry.Key] || strings.TrimSpace(entry.Name) == "" || len(entry.Name) > 256 || !utf8.ValidString(entry.Name) || strings.ContainsRune(entry.Name, 0) {
			return ConfigurationBundle{}, ErrInvalid
		}
		seen[entry.Key] = true
		for _, text := range []string{entry.Desired.Harness, entry.Desired.Model, entry.Desired.WorkingDirectory} {
			if !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
				return ConfigurationBundle{}, ErrInvalid
			}
		}
		if err := validateDesired(entry.Desired); err != nil {
			return ConfigurationBundle{}, err
		}
		if entry.Startup != nil && model.ValidateProfileStartup(*entry.Startup) != nil {
			return ConfigurationBundle{}, ErrInvalid
		}
	}
	return bundle, nil
}

func (s *Service) ImportConfigurations(ctx context.Context, req ImportConfigurationsRequest) (ConfigurationImportResult, error) {
	bundle, err := s.InspectConfigurationBundle(ctx, req.Context.Principal, req.Bundle)
	if err != nil {
		return ConfigurationImportResult{}, err
	}
	if req.Context.RequestID.Validate() != nil || len(req.Selections) == 0 || len(req.Selections) > len(bundle.Profiles) {
		return ConfigurationImportResult{}, ErrInvalid
	}
	entries := map[string]ConfigurationBundleEntry{}
	for _, entry := range bundle.Profiles {
		entries[entry.Key] = entry
	}
	keys, targets := map[string]bool{}, map[model.ConfigurationProfileID]bool{}
	write := ConfigurationBundleWrite{RequestID: req.Context.RequestID}
	for _, selected := range req.Selections {
		entry, ok := entries[selected.Key]
		if !ok || keys[selected.Key] || targets[selected.ID] || !utf8.ValidString(selected.Name) || strings.ContainsRune(selected.Name, 0) {
			return ConfigurationImportResult{}, ErrInvalid
		}
		keys[selected.Key], targets[selected.ID] = true, true
		w, err := prepareConfigurationProfile(SaveConfigurationProfileRequest{Context: req.Context, ID: selected.ID, RevisionID: selected.RevisionID, ExpectedRevision: selected.ExpectedRevision, Name: selected.Name, Desired: entry.Desired, Startup: entry.Startup}, s.now())
		if err != nil {
			return ConfigurationImportResult{}, err
		}
		w.Profile.Archived = entry.Archived
		write.Profiles = append(write.Profiles, w)
	}
	data, err := json.Marshal(struct {
		Bundle     ConfigurationBundle
		Selections []ConfigurationImportSelection
	}{bundle, req.Selections})
	if err != nil {
		return ConfigurationImportResult{}, err
	}
	hash := sha256.Sum256(data)
	write.Fingerprint = hex.EncodeToString(hash[:])
	store, ok := s.store.(ConfigurationTransferStore)
	if !ok {
		return ConfigurationImportResult{}, ErrUnsupported
	}
	return store.ImportConfigurations(ctx, write)
}
