package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

func TestDashboardJobsPreactBoundary(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		data, err := fs.ReadFile(dashboardAssetsFS, name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		return string(data)
	}

	state := read("js/jobs-state.js")
	for _, forbidden := range []string{"document", "querySelector", "innerHTML", "fetch("} {
		if strings.Contains(state, forbidden) {
			t.Errorf("Jobs state contains forbidden DOM/fetch knowledge %q", forbidden)
		}
	}
	lifecycle := read("js/island-lifecycle.js")
	for _, forbidden := range []string{"from 'preact'", `from "preact"`, "@preact/signals", "./jobs-"} {
		if strings.Contains(lifecycle, forbidden) {
			t.Errorf("island lifecycle contains feature/runtime dependency %q", forbidden)
		}
	}
	for _, needle := range []string{
		"claimHosts(name, hosts)",
		"registerFeatureState(name, feature.state)",
		"renderIslandLoadFailure(hosts[0]",
		"releaseHosts(name, hosts)",
	} {
		if !strings.Contains(lifecycle, needle) {
			t.Errorf("island lifecycle wiring missing %q", needle)
		}
	}
	island := read("js/jobs-island.js")
	for _, forbidden := range []string{"innerHTML", "morphInto", "./refresh.js", "fetch("} {
		if strings.Contains(island, forbidden) {
			t.Errorf("Jobs island bypasses its component/action boundary with %q", forbidden)
		}
	}
	for _, coreModule := range []string{"js/refresh.js", "js/modal-cron.js", "js/dashboard.js"} {
		if strings.Contains(read(coreModule), "./jobs-state.js") {
			t.Errorf("%s statically imports Jobs state outside the guarded loader", coreModule)
		}
	}

	for _, needle := range []string{
		`<div id="jobs-root"></div>`,
		`<span id="jobs-badge-root"></span>`,
		"await mountJobsFeature({",
		"const jobsDescriptor = createIslandDescriptor({",
		"return mountIslandDescriptor(jobsDescriptor, actionDependencies)",
		"jobsActive ? get('/api/jobs?' + jobs.params.value)",
		"jobs.beginRequest(requestId)",
		"!jobs.acceptsRequest(requestId)",
		"jobsResult.ok) jobs.syncServedOffset",
		"jobs.commitRequest(requestId)",
		"jobs.failRequest(requestId, jobsResult.error)",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("Jobs Preact wiring missing %q", needle)
		}
	}

	for _, retired := range []string{
		"function renderJobsTab(",
		"function renderJobs(",
		"renderJobsTab();",
		"bindFilter('jobs')",
		"case 'export-job-dismiss'",
		"case 'cron-run-now'",
		"setInterval(pollJobs",
	} {
		if strings.Contains(dashboardAssets, retired) {
			t.Errorf("Jobs migration left retired legacy path %q", retired)
		}
	}
}
