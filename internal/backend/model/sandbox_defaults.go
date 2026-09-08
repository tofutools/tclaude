package model

// SandboxDefaults stores editable registry assignments by ID. Group assignments
// apply only to that group, independently of the group hierarchy.
type SandboxDefaults struct {
	Global   SandboxProfileID
	Groups   map[GroupID]SandboxProfileID
	Revision Revision
}

// Resolve composes assignment identities in v1 precedence order. Values are
// loaded separately for the launch; this does not mutate the saved choice.
func (d SandboxDefaults) Resolve(selected *SandboxSelection) *SandboxSelection {
	if selected != nil && selected.OmitProfiles {
		return nil
	}
	var out SandboxSelection
	if d.Global != "" {
		out.Scopes = append(out.Scopes, SandboxScopeSelection{Scope: SandboxScopeGlobal, Ref: SandboxProfileRef{ProfileID: d.Global}})
	}
	if selected != nil {
		out.GroupID = selected.GroupID
		if profile := d.Groups[selected.GroupID]; profile != "" {
			out.Scopes = append(out.Scopes, SandboxScopeSelection{Scope: SandboxScopeGroup, Ref: SandboxProfileRef{ProfileID: profile}})
		}
		for _, scope := range selected.Scopes {
			if scope.Scope == SandboxScopeExplicit || (scope.Scope == SandboxScopeGroup && selected.GroupID == "") {
				out.Scopes = append(out.Scopes, scope)
			}
		}
	}
	if len(out.Scopes) == 0 {
		return nil
	}
	return &out
}

func SandboxInGroup(selected *SandboxSelection, group GroupID) *SandboxSelection {
	out := SandboxSelection{}
	if selected != nil {
		out = selected.References()
	}
	if !out.OmitProfiles {
		out.GroupID = group
	}
	return &out
}
