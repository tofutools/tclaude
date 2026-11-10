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
		"fetchVisibleGroupListPages(",
		"const staticVersion = lastSnapshot?.static_version || ''",
		"{ credentials: 'same-origin', cache: 'no-store', signal: abort.signal }",
		"restoreUnchangedStatics(data, prevSnap)",
	} {
		if !strings.Contains(refresh, needle) {
			t.Errorf("refresh.js missing transport optimization %q", needle)
		}
	}
	storeBytes, err := fs.ReadFile(dashboardAssetsFS, "js/snapshot-store.js")
	if err != nil {
		t.Fatal(err)
	}
	store := string(storeBytes)
	for _, needle := range []string{
		"!next?.static_unchanged || !prev || prev.static_version !== next.static_version",
		"next[key] = next[key] ?? prev[key]",
	} {
		if !strings.Contains(store, needle) {
			t.Errorf("snapshot-store.js missing transport optimization %q", needle)
		}
	}
}
