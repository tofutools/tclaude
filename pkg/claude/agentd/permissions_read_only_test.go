package agentd

import "testing"

// The editors' read/write filter rides on read_only, so it must say "read"
// for a slug that only looks and "write" for one that changes something.
func TestPermissionRegistryReadOnly(t *testing.T) {
	want := map[string]bool{
		PermFleetRead:          true,
		PermGroupsRosterRead:   true,
		PermAgentContextInfo:   true,
		PermSessionsWatch:      true,
		PermDashboardCapture:   true,
		PermMessageDirect:      false,
		PermDrawingBoardsWrite: false,
		PermConfigExport:       false,
		PermAgentSpawn:         false,
	}
	for _, slug := range permissionRegistry {
		if expected, ok := want[slug.Slug]; ok {
			if slug.ReadOnly != expected {
				t.Errorf("%s: read_only = %v, want %v", slug.Slug, slug.ReadOnly, expected)
			}
			delete(want, slug.Slug)
		}
	}
	for slug := range want {
		t.Errorf("%s is not in the registry", slug)
	}
}
