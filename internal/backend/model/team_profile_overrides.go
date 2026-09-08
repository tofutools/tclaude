package model

// TeamProfileOverrides retains only explicitly authored member choices. Nil
// fields continue to follow the saved profile on the next deployment.
type TeamProfileOverrides struct {
	Harness  *string       `json:",omitempty"`
	Model    *string       `json:",omitempty"`
	Effort   *string       `json:",omitempty"`
	Approval *ApprovalMode `json:",omitempty"`
	Sandbox  *SandboxMode  `json:",omitempty"`
}

func (o *TeamProfileOverrides) Apply(base DesiredConfiguration) DesiredConfiguration {
	if o == nil {
		return base
	}
	if o.Harness != nil {
		if *o.Harness != base.Harness {
			// Model and effort names belong to the selected harness. An explicit
			// switch must not inherit a different provider's model or effort.
			base.Model, base.Effort = "", ""
		}
		base.Harness = *o.Harness
	}
	if o.Model != nil {
		base.Model = *o.Model
	}
	if o.Effort != nil {
		base.Effort = *o.Effort
	}
	if o.Approval != nil {
		base.Approval = *o.Approval
	}
	if o.Sandbox != nil {
		base.Sandbox = *o.Sandbox
	}
	return base
}
