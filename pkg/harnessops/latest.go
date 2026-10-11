package harnessops

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/nodeinfo"
	"golang.org/x/mod/semver"
)

var latest struct {
	sync.Mutex
	running  bool
	checked  time.Time
	versions map[string]string
}
var versionPattern = regexp.MustCompile(`\b([0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?)`)

// RefreshLatestAsync is a bounded metadata read; polling never waits for npm.
func RefreshLatestAsync() {
	latest.Lock()
	if latest.running || time.Since(latest.checked) < time.Hour {
		latest.Unlock()
		return
	}
	latest.running = true
	latest.checked = time.Now()
	latest.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		versions := map[string]string{}
		for _, r := range recipes {
			req, err := http.NewRequestWithContext(ctx, "GET", "https://registry.npmjs.org/"+url.PathEscape(r.Package)+"/latest", nil)
			if err != nil {
				continue
			}
			resp, err := client.Do(req)
			if err != nil {
				continue
			}
			var metadata struct {
				Version string `json:"version"`
			}
			if resp.StatusCode == 200 {
				_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&metadata)
			}
			_ = resp.Body.Close()
			if semver.IsValid("v" + metadata.Version) {
				versions[r.Harness] = metadata.Version
			}
		}
		latest.Lock()
		latest.versions = versions
		latest.running = false
		latest.Unlock()
	}()
}
func AddLatest(a nodeinfo.Availability) nodeinfo.Availability {
	a.Harnesses = append([]nodeinfo.HarnessAvailability{}, a.Harnesses...)
	latest.Lock()
	defer latest.Unlock()
	for i := range a.Harnesses {
		row := &a.Harnesses[i]
		row.LatestVersion = latest.versions[row.Name]
		match := versionPattern.FindStringSubmatch(row.Version)
		if row.LatestVersion != "" && len(match) > 1 && semver.IsValid("v"+match[1]) {
			available := semver.Compare("v"+row.LatestVersion, "v"+match[1]) > 0
			row.UpdateAvailable = &available
		}
	}
	return a
}
