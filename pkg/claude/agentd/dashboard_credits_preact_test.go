package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

func TestDashboardCreditsPreactBoundary(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		data, err := fs.ReadFile(dashboardAssetsFS, name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		return string(data)
	}

	state := read("js/credits-state.js")
	for _, forbidden := range []string{"document", "querySelector", "innerHTML", "morphInto", "fetch("} {
		if strings.Contains(state, forbidden) {
			t.Errorf("credits state contains forbidden DOM/fetch knowledge %q", forbidden)
		}
	}
	island := read("js/credits-island.js")
	for _, forbidden := range []string{"innerHTML", "morphInto", "fetch(", "./refresh.js"} {
		if strings.Contains(island, forbidden) {
			t.Errorf("credits island bypasses its component/state boundary with %q", forbidden)
		}
	}
	for _, needle := range []string{
		`id="slop-credits"`, `class="slop-credits"`,
		`title="Slop credits — climbs on every jackpot this session"`,
		"current.credits.toLocaleString()", "slop-credits-bump",
		"export function CreditsLeaderboard(", "key=${entry.conv}", "data-key=${entry.conv}",
		`class="vegas-leaderboard-title"`, `class="vegas-leaderboard-list"`,
		"export function mountCreditsIsland(",
		"registerCleanup(() => render(null, counterHost))",
		"registerCleanup(() => render(null, leaderboardHost))",
	} {
		if !strings.Contains(island, needle) {
			t.Errorf("credits Preact contract missing %q", needle)
		}
	}
	loader := read("js/preact-loader.js")
	for _, needle := range []string{
		"creditsLeaderboardHost: '#vegas-leaderboard'",
		"counterHost: hosts.creditsHost",
		"leaderboardHost: hosts.creditsLeaderboardHost",
	} {
		if !strings.Contains(loader, needle) {
			t.Errorf("credits Preact loader contract missing %q", needle)
		}
	}

	bridge := read("js/slop-credits.js")
	for _, needle := range []string{
		"state.recordWin(d.fx, d.conv)",
		"state.publishSnapshot(getSnapshot())",
		"removeEventListener('tclaude:slopfx', onSlopFx)",
		"removeEventListener('tclaude:snapshot', onSnapshot)",
		"return cleanup",
	} {
		if !strings.Contains(bridge, needle) {
			t.Errorf("credits event/leaderboard bridge missing %q", needle)
		}
	}
	for _, retired := range []string{
		"function renderCounter(", "renderCreditsLeaderboard", "morphInto", "innerHTML",
		"getElementById('slop-credits')", "isSlopActiveImpl", "let credits = 0",
	} {
		if strings.Contains(bridge, retired) {
			t.Errorf("credits migration left retired shell mutation %q", retired)
		}
	}
}
