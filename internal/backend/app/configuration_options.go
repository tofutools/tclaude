package app

import (
	"context"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/tofutools/tclaude/internal/backend/model"
)

func validateConfigurationOptions(options model.ConfigurationOptions) error {
	for _, value := range []*string{options.Harness, options.Model, options.Effort, options.WorkingDirectory} {
		if value != nil && (!utf8.ValidString(*value) || strings.ContainsRune(*value, 0)) {
			return fail(ErrInvalid, "profile options require valid text without NUL")
		}
	}
	if options.Harness != nil && strings.TrimSpace(*options.Harness) == "" {
		return fail(ErrInvalid, "omit an inherited harness instead of selecting an empty harness")
	}
	if options.Approval != nil && *options.Approval == "" {
		return fail(ErrInvalid, "omit an inherited approval instead of selecting an empty approval")
	}
	if options.Sandbox != nil && *options.Sandbox == "" {
		return fail(ErrInvalid, "omit inherited confinement instead of selecting empty confinement")
	}
	return validateConfigurationFields(options.Apply(model.DesiredConfiguration{}), true)
}

// resolveProfileConfiguration resolves only partial profiles through the current
// global default and the provider's declared native defaults. Existing complete
// profiles keep their exact saved configuration. This performs no native work.
type ResolvedProfileConfiguration struct {
	Selected         model.ConfigurationProfileRef
	Desired          model.DesiredConfiguration
	DefaultsRevision *model.Revision
	GlobalProfile    *model.ConfigurationProfileRef
}

func (s *Service) resolveProfileConfigurationWithOverrides(ctx context.Context, selected ConfigurationProfileResult, overrides *model.ConfigurationOptions) (ResolvedProfileConfiguration, error) {
	if selected.Revision.Options == nil {
		desired, err := s.applyConfigurationOverrides(selected.Revision.Desired, overrides)
		return ResolvedProfileConfiguration{Selected: selected.Revision.Ref, Desired: desired}, err
	}

	resolved, err := s.resolveConfigurationOptions(ctx, selected.Profile.ID, *selected.Revision.Options, overrides)
	resolved.Selected = selected.Revision.Ref
	return resolved, err
}

func (s *Service) resolveConfigurationOptions(ctx context.Context, selectedID model.ConfigurationProfileID, options model.ConfigurationOptions, overrides *model.ConfigurationOptions) (ResolvedProfileConfiguration, error) {
	if err := validateConfigurationOptions(options); err != nil {
		return ResolvedProfileConfiguration{}, err
	}
	layers := []model.ConfigurationOptions{}
	defaults, err := s.store.ConfigurationDefaults(ctx)
	if err != nil {
		return ResolvedProfileConfiguration{}, err
	}
	var globalRef *model.ConfigurationProfileRef
	if defaults.Global != nil && defaults.Global.ProfileID != selectedID {
		global, err := s.store.ConfigurationProfile(ctx, defaults.Global.ProfileID, "")
		if err != nil {
			return ResolvedProfileConfiguration{}, err
		}
		if global.Profile.Archived {
			return ResolvedProfileConfiguration{}, fail(ErrConflict, "global default profile is archived")
		}
		if err := ConfigurationProfileEnabled(global.Profile); err != nil {
			return ResolvedProfileConfiguration{}, err
		}
		if global.Revision.Options != nil {
			if err := validateConfigurationOptions(*global.Revision.Options); err != nil {
				return ResolvedProfileConfiguration{}, err
			}
		}
		ref := global.Revision.Ref
		globalRef = &ref
		layers = append(layers, profileConfigurationOptions(global.Revision))
	}
	layers = append(layers, options)
	// V1's final harness fallback is Claude. Selecting it does not enable or
	// install a provider; the configured registry must still supply it.
	harness := "claude"
	for _, layer := range layers {
		if layer.Harness != nil && strings.TrimSpace(*layer.Harness) != "" {
			harness = *layer.Harness
		}
	}
	if overrides != nil && overrides.Harness != nil {
		harness = *overrides.Harness
	}
	if s.providers == nil {
		return ResolvedProfileConfiguration{}, fail(ErrUnsupported, "configure harness %s before resolving profile defaults", harness)
	}
	provider, ok := s.providers.Provider(harness)
	if !ok || provider.Capabilities().LaunchPolicy == nil {
		return ResolvedProfileConfiguration{}, fail(ErrUnsupported, "harness %s does not declare launch defaults", harness)
	}
	policy := provider.Capabilities().LaunchPolicy
	resolved := model.DesiredConfiguration{Harness: harness, Approval: policy.DefaultApproval, Sandbox: policy.DefaultSandbox}
	for _, layer := range layers {
		if layer.Harness != nil && *layer.Harness != harness {
			layer.Model, layer.Effort = nil, nil
		}
		layer.Harness = nil
		if harness != "codex" {
			layer.FastMode, layer.AutoReview = nil, nil
		}
		if harness != "opencode" {
			layer.ToolGovernance = nil
		}
		if layer.Approval != nil && !slices.Contains(policy.SupportedApproval, *layer.Approval) {
			layer.Approval = nil
		}
		if layer.Sandbox != nil && !slices.Contains(policy.SupportedSandbox, *layer.Sandbox) {
			layer.Sandbox = nil
		}
		resolved = layer.Apply(resolved)
	}
	resolved, err = s.applyConfigurationOverrides(resolved, overrides)
	if err != nil {
		return ResolvedProfileConfiguration{}, err
	}
	if err := validateLaunchConfiguration(resolved); err != nil {
		return ResolvedProfileConfiguration{}, err
	}
	return ResolvedProfileConfiguration{Desired: resolved, DefaultsRevision: &defaults.Revision, GlobalProfile: globalRef}, nil
}

func profileConfigurationOptions(revision model.ConfigurationProfileRevision) model.ConfigurationOptions {
	if revision.Options != nil {
		return *revision.Options
	}
	desired := revision.Desired
	return model.ConfigurationOptions{Harness: &desired.Harness, Model: &desired.Model, Effort: &desired.Effort, ToolGovernance: &desired.ToolGovernance, FastMode: &desired.FastMode, AutoReview: &desired.AutoReview, WorkingDirectory: &desired.WorkingDirectory, Approval: &desired.Approval, Sandbox: &desired.Sandbox, HostSandbox: desired.HostSandbox, Environment: desired.Environment}
}

// Explicit launch settings are validated as intent, never discarded as an
// incompatible inherited profile tier. Context-only overrides need no provider.
func (s *Service) applyConfigurationOverrides(base model.DesiredConfiguration, overrides *model.ConfigurationOptions) (model.DesiredConfiguration, error) {
	if overrides == nil {
		return base, nil
	}
	if err := validateConfigurationOptions(*overrides); err != nil {
		return model.DesiredConfiguration{}, err
	}
	native := overrides.NativeOptions()
	resolved, err := s.resolveNativeConfigurationOverrides("launch", base, &native)
	if err != nil {
		return model.DesiredConfiguration{}, err
	}
	context := model.ConfigurationOptions{WorkingDirectory: overrides.WorkingDirectory, HostSandbox: overrides.HostSandbox, Environment: overrides.Environment}
	resolved = context.Apply(resolved)
	if overrides.Approval != nil || overrides.Sandbox != nil {
		if s.providers == nil {
			return model.DesiredConfiguration{}, fail(ErrUnsupported, "configure harness %s before checking explicit launch policies", resolved.Harness)
		}
		provider, ok := s.providers.Provider(resolved.Harness)
		if !ok || provider.Capabilities().LaunchPolicy == nil {
			return model.DesiredConfiguration{}, fail(ErrUnsupported, "harness %s does not declare launch policy support", resolved.Harness)
		}
		policy := provider.Capabilities().LaunchPolicy
		if overrides.Approval != nil && !slices.Contains(policy.SupportedApproval, resolved.Approval) {
			return model.DesiredConfiguration{}, fail(ErrInvalid, "approval %s is unsupported by %s", resolved.Approval, resolved.Harness)
		}
		if overrides.Sandbox != nil && !slices.Contains(policy.SupportedSandbox, resolved.Sandbox) {
			return model.DesiredConfiguration{}, fail(ErrInvalid, "confinement %s is unsupported by %s", resolved.Sandbox, resolved.Harness)
		}
	}
	return resolved, validateLaunchConfiguration(resolved)
}

// A per-harness default supplies context only when the launch does not specify
// a harness. Copy the options so resolving a request cannot mutate its caller.
func configurationOverridesForDefault(overrides *model.ConfigurationOptions, name string) *model.ConfigurationOptions {
	if name == "" || name == "global" || (overrides != nil && overrides.Harness != nil) {
		return overrides
	}
	var result model.ConfigurationOptions
	if overrides != nil {
		result = *overrides
	}
	result.Harness = &name
	return &result
}

// Validate reusable selection without resolving missing launch context against
// whatever defaults happen to exist while the selection is being saved.
func (s *Service) validateProfileDefaultSelection(ctx context.Context, ref model.ConfigurationProfileRef, harness string) error {
	profile, err := s.store.ConfigurationProfile(ctx, ref.ProfileID, ref.RevisionID)
	if err != nil {
		return err
	}
	if profile.Profile.Archived || profile.Revision.Ref != ref {
		return ErrConflict
	}
	if err := ConfigurationProfileEnabled(profile.Profile); err != nil {
		return err
	}
	if authored := profile.Revision.AuthoredHarness(); harness != "" && authored != "" && authored != harness {
		return fail(ErrInvalid, "default harness does not match profile")
	}
	if profile.Revision.Options != nil {
		if err := validateConfigurationOptions(*profile.Revision.Options); err != nil {
			return err
		}
		return s.verifyLaunchSandbox(ctx, profile.Revision.Options.HostSandbox)
	}
	if err := validateDesired(profile.Revision.Desired); err != nil {
		return err
	}
	return s.verifyLaunchSandbox(ctx, profile.Revision.Desired.HostSandbox)
}

// Inline member options are explicit launch intent over the current global and
// provider defaults. They are not saved as an invented configuration profile.
func (s *Service) resolveInlineConfiguration(ctx context.Context, options model.ConfigurationOptions) (model.DesiredConfiguration, error) {
	resolved, err := s.resolveConfigurationOptions(ctx, "", model.ConfigurationOptions{}, &options)
	return resolved.Desired, err
}
