package model

// NativeConfigurationOptions retains explicit provider choices. Nil fields
// inherit from the lower-priority configuration.
type NativeConfigurationOptions struct {
	AutoReview     *bool           `json:",omitempty"`
	FastMode       *FastMode       `json:",omitempty"`
	ToolGovernance *ToolGovernance `json:",omitempty"`
	Harness        *string         `json:",omitempty"`
	Model          *string         `json:",omitempty"`
	Effort         *string         `json:",omitempty"`
	Approval       *ApprovalMode   `json:",omitempty"`
	Sandbox        *SandboxMode    `json:",omitempty"`
}

func (o *NativeConfigurationOptions) Apply(base DesiredConfiguration) DesiredConfiguration {
	if o == nil {
		return base
	}
	if o.Harness != nil {
		if *o.Harness != base.Harness {
			// Model and effort names belong to the selected harness. An explicit
			// switch must not inherit a different provider's model or effort.
			base.Model, base.Effort = "", ""
			base.ToolGovernance = ""
			base.FastMode = ""
			base.AutoReview = false
		}
		base.Harness = *o.Harness
	}
	if o.Model != nil {
		base.Model = *o.Model
	}
	if o.Effort != nil {
		base.Effort = *o.Effort
	}
	if o.AutoReview != nil {
		base.AutoReview = *o.AutoReview
	}
	if o.FastMode != nil {
		base.FastMode = *o.FastMode
	}
	if o.ToolGovernance != nil {
		base.ToolGovernance = *o.ToolGovernance
	}
	if o.Approval != nil {
		base.Approval = *o.Approval
	}
	if o.Sandbox != nil {
		base.Sandbox = *o.Sandbox
	}
	return base
}
