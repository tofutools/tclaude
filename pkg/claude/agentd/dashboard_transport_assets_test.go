package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

func TestDashboardSnapshotTransportWiring(t *testing.T) {
	refreshBytes, err := fs.ReadFile(dashboardAssetsFS, "js/refresh.js")
	if err != nil {
		t.Fatal(err)
	}
	refresh := string(refreshBytes)
	for _, needle := range []string{
		"(onGroups && groups?.visibility.value.retired) ? get('/api/retired?'",
		"const staticVersion = lastSnapshot?.static_version || ''",
		"{ credentials: 'same-origin', cache: 'no-store' }",
		"data.static_unchanged && prevSnap.static_version === data.static_version",
		"data[key] = prevSnap[key]",
	} {
		if !strings.Contains(refresh, needle) {
			t.Errorf("refresh.js missing transport optimization %q", needle)
		}
	}
}
