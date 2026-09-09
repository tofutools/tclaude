package model

// ConfigurationOptions is a reusable profile's partial launch configuration.
// Nil fields inherit from lower-priority defaults. In particular, an explicit
// false boolean is different from a missing boolean. DesiredConfiguration is
// still the resolved configuration admitted for an agent or execution.
type ConfigurationOptions struct {
	Harness           *string            `json:",omitempty"`
	Model             *string            `json:",omitempty"`
	Effort            *string            `json:",omitempty"`
	ToolGovernance    *ToolGovernance    `json:",omitempty"`
	FastMode          *FastMode          `json:",omitempty"`
	AutoReview        *bool              `json:",omitempty"`
	AutoMemory        *bool              `json:",omitempty"`
	PeerMessaging     *bool              `json:",omitempty"`
	TrustDirectory    *bool              `json:",omitempty"`
	AutoCompactWindow *AutoCompactWindow `json:",omitempty"`
	WorkingDirectory  *string            `json:",omitempty"`
	Approval          *ApprovalMode      `json:",omitempty"`
	Sandbox           *SandboxMode       `json:",omitempty"`
	HostSandbox       *SandboxSelection  `json:",omitempty"`
	Environment       Environment        `json:",omitempty"`
}

// Apply overlays authored values without mutating either input. Provider
// compatibility and final completeness are checked by the application after
// the profile/default stack and launch context have been resolved.
func (o ConfigurationOptions) Apply(base DesiredConfiguration) DesiredConfiguration {
	native := o.NativeOptions()
	out := native.Apply(base)
	if o.WorkingDirectory != nil {
		out.WorkingDirectory = *o.WorkingDirectory
	}
	if o.HostSandbox != nil {
		selection := o.HostSandbox.References()
		out.HostSandbox = &selection
	}
	if base.Environment != nil || o.Environment != nil {
		out.Environment = make(Environment, len(base.Environment)+len(o.Environment))
		for name, value := range base.Environment {
			out.Environment[name] = value
		}
		for name, value := range o.Environment {
			out.Environment[name] = value
		}
	}
	return out
}

// NativeOptions exposes the provider-owned subset to shared launch resolution.
func (o ConfigurationOptions) NativeOptions() NativeConfigurationOptions {
	return NativeConfigurationOptions{Harness: o.Harness, Model: o.Model, Effort: o.Effort, ToolGovernance: o.ToolGovernance, FastMode: o.FastMode, AutoReview: o.AutoReview, AutoMemory: o.AutoMemory, PeerMessaging: o.PeerMessaging, TrustDirectory: o.TrustDirectory, AutoCompactWindow: o.AutoCompactWindow, Approval: o.Approval, Sandbox: o.Sandbox}
}

// AuthoredHarness is empty when a partial profile inherits its harness. It
// describes saved intent, not a resolved launch or the current global default.
func (r ConfigurationProfileRevision) AuthoredHarness() string {
	if r.Options != nil {
		if r.Options.Harness == nil {
			return ""
		}
		return *r.Options.Harness
	}
	return r.Desired.Harness
}
