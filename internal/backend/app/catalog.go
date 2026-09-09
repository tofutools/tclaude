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
	SetConfigurationProfileAvailability(context.Context, ConfigurationProfileAvailabilityWrite) (model.ConfigurationProfile, error)
	SetConfigurationProfileArchived(context.Context, ConfigurationProfileArchiveWrite) (model.ConfigurationProfile, error)
	SaveConfigurationDefaults(context.Context, ConfigurationDefaultsWrite) (model.ConfigurationDefaults, error)
	ConfigurationDefaults(context.Context) (model.ConfigurationDefaults, error)
	SaveConfigurationProfile(context.Context, ConfigurationProfileWrite) (ConfigurationProfileResult, error)
	ConfigurationProfile(context.Context, model.ConfigurationProfileID, model.ConfigurationProfileRevisionID) (ConfigurationProfileResult, error)
	ConfigurationProfiles(context.Context) ([]model.ConfigurationProfile, error)
}

type ConfigurationDefaultsRequestStore interface {
	FindConfigurationDefaultsWrite(context.Context, model.RequestID, string) (model.ConfigurationDefaults, bool, error)
}

type ConfigurationProfileRequestStore interface {
	FindConfigurationProfileWrite(context.Context, model.RequestID, string) (ConfigurationProfileResult, bool, error)
}

type ConfigurationProfileWrite struct {
	OperatorOnlySet    bool
	AliasesSet         bool
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
	OperatorOnly     *bool
	Options          *model.ConfigurationOptions
	Aliases          *[]string
	Startup          *model.ProfileStartup
	Context          RequestContext
	ID               model.ConfigurationProfileID
	RevisionID       model.ConfigurationProfileRevisionID
	Name             string
	Desired          model.DesiredConfiguration
	ExpectedRevision model.Revision
}

type ConfigurationCatalogAPI interface {
	ResolveConfigurationProfile(context.Context, model.Principal, string) (ConfigurationProfileResult, error)
	SetConfigurationProfileAvailability(context.Context, SetConfigurationProfileAvailabilityRequest) (model.ConfigurationProfile, error)
	SetConfigurationProfileArchived(context.Context, SetConfigurationProfileArchivedRequest) (model.ConfigurationProfile, error)
	SaveConfigurationDefaults(context.Context, SaveConfigurationDefaultsRequest) (model.ConfigurationDefaults, error)
	GetConfigurationDefaults(context.Context, model.Principal) (model.ConfigurationDefaults, error)
	SaveConfigurationProfile(context.Context, SaveConfigurationProfileRequest) (ConfigurationProfileResult, error)
	GetConfigurationProfile(context.Context, model.Principal, model.ConfigurationProfileRef) (ConfigurationProfileResult, error)
	ListConfigurationProfiles(context.Context, model.Principal) ([]model.ConfigurationProfile, error)
}

func (s *Service) SaveConfigurationProfile(ctx context.Context, req SaveConfigurationProfileRequest) (ConfigurationProfileResult, error) {
	if err := requireOperator(req.Context.Principal); err != nil {
		return ConfigurationProfileResult{}, err
	}
	if req.Desired.HostSandbox != nil {
		choice := req.Desired.HostSandbox.References()
		req.Desired.HostSandbox = &choice
	}
	if req.Options != nil {
		options := *req.Options
		if options.HostSandbox != nil {
			choice := options.HostSandbox.References()
			options.HostSandbox = &choice
		}
		req.Options = &options
	}
	w, err := prepareConfigurationProfile(req, s.now())
	if err != nil {
		return ConfigurationProfileResult{}, err
	}
	if receipts, ok := s.store.(ConfigurationProfileRequestStore); ok {
		prior, found, err := receipts.FindConfigurationProfileWrite(ctx, w.RequestID, w.RequestFingerprint)
		if found || err != nil {
			return prior, err
		}
	}
	sandbox := req.Desired.HostSandbox
	if req.Options != nil {
		sandbox = req.Options.HostSandbox
	}
	if err := s.verifyLaunchSandbox(ctx, sandbox); err != nil {
		return ConfigurationProfileResult{}, err
	}
	return s.store.SaveConfigurationProfile(ctx, w)
}

func prepareConfigurationProfile(req SaveConfigurationProfileRequest, at time.Time) (ConfigurationProfileWrite, error) {
	if model.ValidateStableID("configuration profile", string(req.ID)) != nil || model.ValidateStableID("configuration revision", string(req.RevisionID)) != nil || req.Context.RequestID.Validate() != nil || strings.TrimSpace(req.Name) == "" || len(req.Name) > 256 {
		return ConfigurationProfileWrite{}, ErrInvalid
	}
	if req.Options != nil {
		if !req.Desired.Equal(model.DesiredConfiguration{}) {
			return ConfigurationProfileWrite{}, fail(ErrInvalid, "choose profile options or a complete configuration")
		}
		if err := validateConfigurationOptions(*req.Options); err != nil {
			return ConfigurationProfileWrite{}, err
		}
	} else if err := validateDesired(req.Desired); err != nil {
		return ConfigurationProfileWrite{}, err
	}
	if req.Startup != nil {
		startup := *req.Startup
		if startup == (model.ProfileStartup{}) {
			req.Startup = nil
		} else {
			if model.ValidateProfileStartup(startup) != nil {
				return ConfigurationProfileWrite{}, ErrInvalid
			}
			req.Startup = &startup
		}
	}
	var aliases []string
	if req.Aliases != nil {
		var err error
		aliases, err = normalizeConfigurationAliases(req.Name, *req.Aliases)
		if err != nil {
			return ConfigurationProfileWrite{}, err
		}
		req.Aliases = &aliases
	}
	payload, _ := json.Marshal(req.Desired)
	// Preserve the original content hash for profiles with no startup suggestions.
	if req.Startup != nil {
		payload, _ = json.Marshal(struct {
			Desired model.DesiredConfiguration
			Startup *model.ProfileStartup
		}{req.Desired, req.Startup})
	}
	if req.Options != nil {
		payload, _ = json.Marshal(struct {
			Options *model.ConfigurationOptions
			Startup *model.ProfileStartup `json:",omitempty"`
		}{req.Options, req.Startup})
	}
	digest := sha256.Sum256(payload)
	input, _ := json.Marshal(struct {
		ID           model.ConfigurationProfileID
		RevisionID   model.ConfigurationProfileRevisionID
		Name         string
		Desired      model.DesiredConfiguration
		Expected     model.Revision
		Startup      *model.ProfileStartup       `json:",omitempty"`
		Aliases      *[]string                   `json:",omitempty"`
		Options      *model.ConfigurationOptions `json:",omitempty"`
		OperatorOnly *bool                       `json:",omitempty"`
	}{req.ID, req.RevisionID, req.Name, req.Desired, req.ExpectedRevision, req.Startup, req.Aliases, req.Options, req.OperatorOnly})
	fingerprint := sha256.Sum256(input)
	now := at.UTC()
	operatorOnly := req.OperatorOnly != nil && *req.OperatorOnly
	return ConfigurationProfileWrite{
		OperatorOnlySet:  req.OperatorOnly != nil,
		AliasesSet:       req.Aliases != nil,
		Profile:          model.ConfigurationProfile{OperatorOnly: operatorOnly, Aliases: aliases, ID: req.ID, Name: req.Name, CurrentRevisionID: req.RevisionID},
		Revision:         model.ConfigurationProfileRevision{Ref: model.ConfigurationProfileRef{ProfileID: req.ID, RevisionID: req.RevisionID, ContentHash: hex.EncodeToString(digest[:])}, Desired: req.Desired, Options: req.Options, Startup: req.Startup, CreatedAt: now},
		ExpectedRevision: req.ExpectedRevision, RequestID: req.Context.RequestID, RequestFingerprint: hex.EncodeToString(fingerprint[:]), At: now,
	}, nil
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

// resolveConfigurationSelection accepts either authored fields or an exact
// immutable catalog selection. A caller cannot attach provenance to unrelated
// configuration values, and a later catalog edit cannot change this selection.
func (s *Service) resolveConfigurationSelectionWithOverrides(ctx context.Context, desired model.DesiredConfiguration, selected *model.ConfigurationProfileRef, overrides *model.ConfigurationOptions) (model.DesiredConfiguration, *model.ConfigurationProfileRef, error) {
	if selected == nil {
		if overrides != nil {
			return desired, nil, fail(ErrInvalid, "launch overrides require a selected profile")
		}
		if desired.HostSandbox != nil {
			choice := desired.HostSandbox.References()
			desired.HostSandbox = &choice
		}
		return desired, nil, s.verifyLaunchSandbox(ctx, desired.HostSandbox)
	}
	if !desired.Equal(model.DesiredConfiguration{}) || selected.ProfileID == "" || selected.RevisionID == "" || selected.ContentHash == "" {
		return desired, nil, fail(ErrInvalid, "select an exact configuration profile or supply desired fields")
	}
	result, err := s.store.ConfigurationProfile(ctx, selected.ProfileID, selected.RevisionID)
	if err != nil {
		return desired, nil, err
	}
	if result.Profile.Archived || result.Revision.Ref != *selected {
		return desired, nil, ErrConflict
	}
	snapshot, err := s.resolveProfileConfigurationWithOverrides(ctx, result, overrides)
	if err != nil {
		return desired, nil, err
	}
	resolved := snapshot.Desired
	if err := s.verifyLaunchSandbox(ctx, resolved.HostSandbox); err != nil {
		return desired, nil, err
	}
	ref := result.Revision.Ref
	return resolved, &ref, nil
}

type SaveConfigurationDefaultsRequest struct {
	Context          RequestContext
	ExpectedRevision model.Revision
	Global           *model.ConfigurationProfileRef
	Harnesses        map[string]model.ConfigurationProfileRef
}
type ConfigurationDefaultsWrite struct {
	Defaults           model.ConfigurationDefaults
	ExpectedRevision   model.Revision
	RequestID          model.RequestID
	RequestFingerprint string
}

func (s *Service) SaveConfigurationDefaults(ctx context.Context, req SaveConfigurationDefaultsRequest) (model.ConfigurationDefaults, error) {
	if err := requireOperator(req.Context.Principal); err != nil {
		return model.ConfigurationDefaults{}, err
	}
	if err := req.Context.RequestID.Validate(); err != nil {
		return model.ConfigurationDefaults{}, fail(ErrInvalid, "%v", err)
	}
	if len(req.Harnesses) > 32 {
		return model.ConfigurationDefaults{}, ErrInvalid
	}
	refs := map[string]model.ConfigurationProfileRef{}
	for harness, ref := range req.Harnesses {
		if harness == "" || harness == "global" || len(harness) > 128 {
			return model.ConfigurationDefaults{}, ErrInvalid
		}
		refs[harness] = ref
	}
	if req.Global != nil {
		refs[""] = *req.Global
	}

	input, _ := json.Marshal(struct {
		Global    *model.ConfigurationProfileRef
		Harnesses map[string]model.ConfigurationProfileRef
		Expected  model.Revision
	}{req.Global, req.Harnesses, req.ExpectedRevision})
	hash := sha256.Sum256(input)
	fingerprint := hex.EncodeToString(hash[:])
	if store, ok := s.store.(ConfigurationDefaultsRequestStore); ok {
		if prior, found, err := store.FindConfigurationDefaultsWrite(ctx, req.Context.RequestID, fingerprint); found || err != nil {
			return prior, err
		}
	}
	var global *model.ConfigurationProfileRef
	harnesses := map[string]model.ConfigurationProfileRef{}
	for harness, ref := range refs {
		// Validate the supplied reference, then use its identity's current settings.
		if err := s.validateProfileDefaultSelection(ctx, ref, harness); err != nil {
			return model.ConfigurationDefaults{}, err
		}
		current, err := s.currentConfigurationDefault(ctx, ref, harness)
		if err != nil {
			return model.ConfigurationDefaults{}, err
		}
		if harness == "" {
			global = current
		} else {
			harnesses[harness] = *current
		}
	}
	return s.store.SaveConfigurationDefaults(ctx, ConfigurationDefaultsWrite{Defaults: model.ConfigurationDefaults{Global: global, Harnesses: harnesses, UpdatedAt: s.now().UTC()}, ExpectedRevision: req.ExpectedRevision, RequestID: req.Context.RequestID, RequestFingerprint: fingerprint})
}
func (s *Service) GetConfigurationDefaults(ctx context.Context, p model.Principal) (model.ConfigurationDefaults, error) {
	if err := requireOperator(p); err != nil {
		return model.ConfigurationDefaults{}, err
	}
	return s.store.ConfigurationDefaults(ctx)
}

func (s *Service) selectConfigurationDefault(ctx context.Context, name string, desired model.DesiredConfiguration, ref *model.ConfigurationProfileRef) (*model.ConfigurationProfileRef, error) {
	if name == "" {
		return ref, nil
	}
	if !desired.Equal(model.DesiredConfiguration{}) || ref != nil {
		return nil, fail(ErrInvalid, "select a default, a profile, or explicit settings")
	}
	defaults, err := s.store.ConfigurationDefaults(ctx)
	if err != nil {
		return nil, err
	}
	if name == "global" {
		if defaults.Global == nil {
			return nil, ErrNotFound
		}
		return s.currentConfigurationDefault(ctx, *defaults.Global, "")
	}
	selected, ok := defaults.Harnesses[name]
	if !ok {
		return nil, ErrNotFound
	}
	return s.currentConfigurationDefault(ctx, selected, name)
}

func (s *Service) currentConfigurationDefault(ctx context.Context, selected model.ConfigurationProfileRef, harness string) (*model.ConfigurationProfileRef, error) {
	current, err := s.store.ConfigurationProfile(ctx, selected.ProfileID, "")
	if err != nil {
		return nil, err
	}
	if current.Profile.Archived {
		return nil, fail(ErrConflict, "default configuration is archived")
	}
	if authored := current.Revision.AuthoredHarness(); harness != "" && authored != "" && authored != harness {
		return nil, fail(ErrConflict, "default configuration no longer uses harness %s; select a matching default", harness)
	}
	ref := current.Revision.Ref
	return &ref, nil
}

type SetConfigurationProfileArchivedRequest struct {
	Context          RequestContext
	ID               model.ConfigurationProfileID
	ExpectedRevision model.Revision
	Archived         bool
}
type ConfigurationProfileArchiveWrite struct {
	ID               model.ConfigurationProfileID
	ExpectedRevision model.Revision
	Archived         bool
	RequestID        model.RequestID
	Fingerprint      string
	At               time.Time
}

func (s *Service) SetConfigurationProfileArchived(ctx context.Context, req SetConfigurationProfileArchivedRequest) (model.ConfigurationProfile, error) {
	if err := requireOperator(req.Context.Principal); err != nil {
		return model.ConfigurationProfile{}, err
	}
	if req.Context.RequestID.Validate() != nil || model.ValidateStableID("configuration profile", string(req.ID)) != nil || req.ExpectedRevision == 0 {
		return model.ConfigurationProfile{}, ErrInvalid
	}
	payload, _ := json.Marshal(struct {
		ID       model.ConfigurationProfileID
		Expected model.Revision
		Archived bool
	}{req.ID, req.ExpectedRevision, req.Archived})
	digest := sha256.Sum256(payload)
	return s.store.SetConfigurationProfileArchived(ctx, ConfigurationProfileArchiveWrite{ID: req.ID, ExpectedRevision: req.ExpectedRevision, Archived: req.Archived, RequestID: req.Context.RequestID, Fingerprint: hex.EncodeToString(digest[:]), At: s.now().UTC()})
}
