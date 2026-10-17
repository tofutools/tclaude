package db

const HostBreakGlassSlug = "host.break-glass"
const HostBreakGlassGrantOrigin = "human:break-glass"

// ListHostBreakGlassGrants reads only deliberate grants from the dedicated
// operator surface. Ordinary defaults, grants, sudo requests and spawn seeds
// cannot authorize this capability. Empty convID selects the whole local board.
func ListHostBreakGlassGrants(convID string) ([]*SudoGrant, error) {
	var rows []*SudoGrant
	var err error
	if convID == "" {
		rows, err = ListAllActiveSudoGrants()
	} else {
		rows, err = ListActiveSudoGrants(convID)
	}
	if err != nil {
		return nil, err
	}
	out := []*SudoGrant{}
	for _, row := range rows {
		if row.Slug == HostBreakGlassSlug && row.GrantedBy == HostBreakGlassGrantOrigin {
			out = append(out, row)
		}
	}
	return out, nil
}
