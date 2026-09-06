package agentd

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// freshLaunchConfigurationRequest is the authored part of the first
// configuration-domain cutover. Empty mode/implementation values mean
// "inherit through the captured profile tiers", not an explicit choice.
type freshLaunchConfigurationRequest struct {
	Harness               string
	HarnessBuiltinMode    string
	SandboxImplementation string
}

// capturedLaunchProfile is an immutable-by-construction value snapshot of one
// source consulted for a fresh launch. ID and UpdatedAt are the versions the
// store actually exposes; this deliberately does not claim a transaction-wide
// revision across independently read rows.
type capturedLaunchProfile struct {
	Profile     db.SpawnProfile
	Source      string
	DefaultTier bool
	Kind        capturedLaunchProfileKind
	ID          int64
	UpdatedAt   time.Time
}

type capturedLaunchProfileKind string

const (
	capturedNamedProfile  capturedLaunchProfileKind = "named"
	capturedGroupProfile  capturedLaunchProfileKind = "group_default"
	capturedGlobalProfile capturedLaunchProfileKind = "global_default"
)

// freshLaunchConfigurationInput is the complete captured input consumed by the
// transport-free resolver. Profiles are values, not caller-owned pointers.
type freshLaunchConfigurationInput struct {
	Request            freshLaunchConfigurationRequest
	GroupID            int64
	GroupName          string
	NamedProfileHandle string
	CapturedAt         time.Time
	Profiles           []capturedLaunchProfile
}

type freshLaunchCaptureIssueKind string

const (
	freshLaunchProfileMissing freshLaunchCaptureIssueKind = "profile_missing"
	freshLaunchProfileRead    freshLaunchCaptureIssueKind = "profile_read"
)

type freshLaunchCaptureIssue struct {
	Kind    freshLaunchCaptureIssueKind
	Handle  string
	Message string
	Err     error
}

// freshLaunchConfigurationCapture carries a usable partial snapshot even when
// the explicitly named profile failed to resolve. The HTTP spawn uses the
// canonical name from that snapshot for its authority footprint, then exposes
// the issue only after authorization; the read-only defaults endpoint maps it
// immediately.
type freshLaunchConfigurationCapture struct {
	Input freshLaunchConfigurationInput
	Issue *freshLaunchCaptureIssue
}

type freshLaunchValueState string

const (
	freshLaunchInherited freshLaunchValueState = "inherited"
	freshLaunchSelected  freshLaunchValueState = "selected"
)

type freshLaunchSkippedChoice struct {
	Source           string
	Field            string
	Reason           string
	ProfileID        int64
	ProfileUpdatedAt time.Time
}

type freshLaunchSelection struct {
	Selected         string
	Effective        string
	Source           string
	State            freshLaunchValueState
	ProfileID        int64
	ProfileUpdatedAt time.Time
	Skipped          []freshLaunchSkippedChoice
}

type freshLaunchRefusalKind string

const (
	freshLaunchInvalidHarness        freshLaunchRefusalKind = "invalid_harness"
	freshLaunchInvalidMode           freshLaunchRefusalKind = "invalid_sandbox"
	freshLaunchInvalidImplementation freshLaunchRefusalKind = "invalid_sandbox_implementation"
)

// freshLaunchConfigurationRefusal is a domain result. It intentionally has no
// HTTP status; each transport maps it onto its existing error wire.
type freshLaunchConfigurationRefusal struct {
	Kind    freshLaunchRefusalKind
	Field   string
	Profile string
	Err     error
}

func (r *freshLaunchConfigurationRefusal) Error() string {
	if r == nil || r.Err == nil {
		return ""
	}
	if r.Profile != "" {
		return fmt.Sprintf("profile %q: %v", r.Profile, r.Err)
	}
	return r.Err.Error()
}

type freshLaunchConfiguration struct {
	Harness               *harness.Harness
	HarnessSelection      freshLaunchSelection
	HarnessBuiltinMode    freshLaunchSelection
	SandboxImplementation freshLaunchSelection
}

func (c freshLaunchConfigurationCapture) canonicalSpawnProfile() string {
	if c.Issue != nil && c.Input.NamedProfileHandle != "" {
		return ""
	}
	if len(c.Input.Profiles) > 0 {
		return c.Input.Profiles[0].Profile.Name
	}
	return ""
}

func (c freshLaunchConfigurationCapture) profile(kind capturedLaunchProfileKind) *db.SpawnProfile {
	for _, captured := range c.Input.Profiles {
		if captured.Kind == kind {
			profile := cloneSpawnProfile(captured.Profile)
			return &profile
		}
	}
	return nil
}

func (c freshLaunchConfigurationCapture) legacyProfileTiers() []launchProfileTier {
	tiers := make([]launchProfileTier, 0, len(c.Input.Profiles))
	for i := range c.Input.Profiles {
		profile := cloneSpawnProfile(c.Input.Profiles[i].Profile)
		tiers = append(tiers, launchProfileTier{
			profile: &profile, source: c.Input.Profiles[i].Source,
			defaultTier: c.Input.Profiles[i].DefaultTier,
		})
	}
	return tiers
}

// captureFreshLaunchConfiguration is the only store-reading adapter for the
// captured cohort. Ambient default failures retain the historical graceful
// fallback; an explicit named-profile issue is retained for the caller to map.
func captureFreshLaunchConfiguration(
	g *db.AgentGroup, profileHandle string, request freshLaunchConfigurationRequest,
) freshLaunchConfigurationCapture {
	handle := strings.TrimSpace(profileHandle)
	input := freshLaunchConfigurationInput{
		Request: freshLaunchConfigurationRequest{
			Harness:               strings.TrimSpace(request.Harness),
			HarnessBuiltinMode:    strings.TrimSpace(request.HarnessBuiltinMode),
			SandboxImplementation: strings.TrimSpace(request.SandboxImplementation),
		},
		NamedProfileHandle: handle,
		CapturedAt:         time.Now(),
	}
	if g != nil {
		input.GroupID, input.GroupName = g.ID, g.Name
	}
	capture := freshLaunchConfigurationCapture{Input: input}
	if handle != "" {
		profile, err := db.ResolveSpawnProfile(handle)
		switch {
		case err != nil:
			capture.Issue = &freshLaunchCaptureIssue{
				Kind: freshLaunchProfileRead, Handle: handle,
				Message: fmt.Sprintf("reading spawn profile %q", handle), Err: err,
			}
		case profile == nil:
			capture.Issue = &freshLaunchCaptureIssue{
				Kind: freshLaunchProfileMissing, Handle: handle,
				Message: fmt.Sprintf("no such spawn profile %q", handle),
			}
		default:
			capture.Input.Profiles = append(capture.Input.Profiles,
				capturedProfile(profile, namedLaunchProfileSource(profile, handle), false, capturedNamedProfile))
		}
	}
	if g != nil && strings.TrimSpace(g.DefaultProfile) != "" {
		profile, err := db.ResolveSpawnProfile(g.DefaultProfile)
		if err != nil {
			slog.Warn("spawn: failed to capture group default profile",
				"group", g.Name, "profile", g.DefaultProfile, "error", err)
		} else if profile == nil {
			slog.Warn("spawn: group default profile no longer exists",
				"group", g.Name, "profile", g.DefaultProfile)
		} else {
			capture.Input.Profiles = append(capture.Input.Profiles,
				capturedProfile(profile, profileSource(profile, agent.ProvGroupProfileSource), true, capturedGroupProfile))
		}
	}
	global, err := db.GlobalDefaultSpawnProfile()
	if err != nil {
		slog.Warn("spawn: failed to capture global default profile", "error", err)
	} else if global != nil {
		capture.Input.Profiles = append(capture.Input.Profiles,
			capturedProfile(global, profileSource(global, agent.ProvGlobalProfileSource), true, capturedGlobalProfile))
	}
	return capture
}

func capturedProfile(
	profile *db.SpawnProfile, source string, defaultTier bool, kind capturedLaunchProfileKind,
) capturedLaunchProfile {
	copy := cloneSpawnProfile(*profile)
	return capturedLaunchProfile{
		Profile: copy, Source: source, DefaultTier: defaultTier, Kind: kind,
		ID: copy.ID, UpdatedAt: copy.UpdatedAt,
	}
}

func namedLaunchProfileSource(profile *db.SpawnProfile, handle string) string {
	source := profileSource(profile, agent.ProvCLIProfileSource)
	if profile != nil && handle != profile.Name {
		return fmt.Sprintf(`profile %q via alias %q`, profile.Name, handle)
	}
	return source
}

// cloneSpawnProfile severs every mutable reference in the authoring row. This
// is intentionally complete even though the first resolver slice reads only
// three strings: the captured tiers are also handed to the still-unmigrated
// field resolvers during this incremental cutover.
func cloneSpawnProfile(in db.SpawnProfile) db.SpawnProfile {
	out := in
	out.Aliases = append([]string(nil), in.Aliases...)
	out.RoleRefs = append([]string(nil), in.RoleRefs...)
	out.Environment = append([]sandboxpolicy.EnvironmentEntry(nil), in.Environment...)
	cloneBool := func(value *bool) *bool {
		if value == nil {
			return nil
		}
		copy := *value
		return &copy
	}
	out.CopilotAPI = cloneBool(in.CopilotAPI)
	out.CodexAppServer = cloneBool(in.CodexAppServer)
	out.FastMode = cloneBool(in.FastMode)
	out.AutoReview = cloneBool(in.AutoReview)
	out.TrustDir = cloneBool(in.TrustDir)
	out.RemoteControl = cloneBool(in.RemoteControl)
	out.AutoMemory = cloneBool(in.AutoMemory)
	out.PeerMessaging = cloneBool(in.PeerMessaging)
	out.SSHWorkaround = cloneBool(in.SSHWorkaround)
	out.SyncWorktree = cloneBool(in.SyncWorktree)
	out.FetchLatestWorktree = cloneBool(in.FetchLatestWorktree)
	out.AutoFocus = cloneBool(in.AutoFocus)
	out.IncludeGroupDefaultContext = cloneBool(in.IncludeGroupDefaultContext)
	out.IsOwner = cloneBool(in.IsOwner)
	if in.ContextFeatures != nil {
		out.ContextFeatures = make(map[string]string, len(in.ContextFeatures))
		for key, value := range in.ContextFeatures {
			out.ContextFeatures[key] = value
		}
	}
	if in.PermissionOverrides != nil {
		out.PermissionOverrides = make(map[string]db.PermissionOverride, len(in.PermissionOverrides))
		for key, value := range in.PermissionOverrides {
			out.PermissionOverrides[key] = value
		}
	}
	return out
}

func resolveFreshLaunchConfiguration(
	input freshLaunchConfigurationInput,
) (freshLaunchConfiguration, *freshLaunchConfigurationRefusal) {
	harnessName := harness.DefaultName
	harnessSource := agent.ProvHarnessDefault
	harnessState := freshLaunchInherited
	var harnessProfileID int64
	var harnessProfileUpdatedAt time.Time
	if input.Request.Harness != "" {
		harnessName, harnessSource = harnessOrDefault(input.Request.Harness), agent.ProvExplicit
		harnessState = freshLaunchSelected
	} else if len(input.Profiles) > 0 {
		harnessName = harnessOrDefault(input.Profiles[0].Profile.Harness)
		harnessSource = input.Profiles[0].Source
		harnessState = freshLaunchSelected
		harnessProfileID = input.Profiles[0].ID
		harnessProfileUpdatedAt = input.Profiles[0].UpdatedAt
	}
	h, err := resolveSpawnHarness(harnessName)
	if err != nil {
		return freshLaunchConfiguration{}, &freshLaunchConfigurationRefusal{
			Kind: freshLaunchInvalidHarness, Field: "harness", Err: err}
	}
	mode, refusal := resolveFreshLaunchString(
		"sandbox", input.Request.HarnessBuiltinMode, h.Name, input.Profiles,
		func(profile db.SpawnProfile) string { return profile.Sandbox },
		func(raw string) (string, error) { return harness.ValidateHarnessBuiltinMode(h, raw) },
		freshLaunchInvalidMode,
	)
	if refusal != nil {
		return freshLaunchConfiguration{}, refusal
	}
	implementation, refusal := resolveFreshLaunchString(
		sandboxImplementationField, input.Request.SandboxImplementation, h.Name, input.Profiles,
		func(profile db.SpawnProfile) string { return profile.SandboxImplementation },
		func(raw string) (string, error) { return validateSandboxImplementationForHarness(h, raw) },
		freshLaunchInvalidImplementation,
	)
	if refusal != nil {
		return freshLaunchConfiguration{}, refusal
	}
	effectiveImplementation, err := sandboxpolicy.NormalizeImplementation(implementation.Selected)
	if err != nil {
		return freshLaunchConfiguration{}, &freshLaunchConfigurationRefusal{
			Kind: freshLaunchInvalidImplementation, Field: sandboxImplementationField, Err: err}
	}
	effectiveMode, err := harness.ResolveHarnessBuiltinMode(h, mode.Selected)
	if err != nil {
		return freshLaunchConfiguration{}, &freshLaunchConfigurationRefusal{
			Kind: freshLaunchInvalidMode, Field: "sandbox", Err: err}
	}
	effectiveMode, err = harness.ResolveNativeHarnessBuiltinMode(h, effectiveMode, effectiveImplementation)
	if err != nil {
		return freshLaunchConfiguration{}, &freshLaunchConfigurationRefusal{
			Kind: freshLaunchInvalidMode, Field: "sandbox", Err: err}
	}
	mode.Effective = effectiveMode
	implementation.Effective = string(effectiveImplementation)
	return freshLaunchConfiguration{
		Harness: h,
		HarnessSelection: freshLaunchSelection{
			Selected: h.Name, Effective: h.Name, Source: harnessSource, State: harnessState,
			ProfileID: harnessProfileID, ProfileUpdatedAt: harnessProfileUpdatedAt,
		},
		HarnessBuiltinMode: mode, SandboxImplementation: implementation,
	}, nil
}

func resolveFreshLaunchString(
	field, explicitValue, harnessName string,
	profiles []capturedLaunchProfile,
	profileValue func(db.SpawnProfile) string,
	validate func(string) (string, error),
	refusalKind freshLaunchRefusalKind,
) (freshLaunchSelection, *freshLaunchConfigurationRefusal) {
	if raw := strings.TrimSpace(explicitValue); raw != "" {
		value, err := validate(raw)
		if err != nil {
			return freshLaunchSelection{}, &freshLaunchConfigurationRefusal{
				Kind: refusalKind, Field: field, Err: err}
		}
		return freshLaunchSelection{Selected: value, Source: agent.ProvExplicit, State: freshLaunchSelected}, nil
	}
	selection := freshLaunchSelection{Source: agent.ProvHarnessDefault, State: freshLaunchInherited}
	for _, tier := range profiles {
		raw := strings.TrimSpace(profileValue(tier.Profile))
		if raw == "" {
			continue
		}
		value, err := validate(raw)
		if err == nil {
			selection.Selected, selection.Source, selection.State = value, tier.Source, freshLaunchSelected
			selection.ProfileID, selection.ProfileUpdatedAt = tier.ID, tier.UpdatedAt
			return selection, nil
		}
		if profileMatchesHarness(&tier.Profile, harnessName) {
			return freshLaunchSelection{}, &freshLaunchConfigurationRefusal{
				Kind: refusalKind, Field: field, Profile: tier.Profile.Name, Err: err}
		}
		selection.Skipped = append(selection.Skipped, freshLaunchSkippedChoice{
			Source: tier.Source, Field: field,
			Reason:    fmt.Sprintf("not valid for %s", harnessName),
			ProfileID: tier.ID, ProfileUpdatedAt: tier.UpdatedAt,
		})
	}
	return selection, nil
}

func freshLaunchSelectionNote(selection freshLaunchSelection) string {
	notes := make([]string, 0, len(selection.Skipped))
	for _, skipped := range selection.Skipped {
		notes = append(notes, fmt.Sprintf("%s %s ignored (%s)",
			skipped.Source, skipped.Field, skipped.Reason))
	}
	return strings.Join(notes, "; ")
}
