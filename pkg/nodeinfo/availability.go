package nodeinfo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/harnesscredentials"
)

// HarnessAvailability contains diagnostics, never credential values or probe
// output beyond a bounded version line. A nil boolean means unknown.
type HarnessAvailability struct {
	Name              string `json:"name"`
	DisplayName       string `json:"display_name"`
	Binary            string `json:"binary"`
	Installed         bool   `json:"installed"`
	Path              string `json:"path,omitempty"`
	Version           string `json:"version,omitempty"`
	LatestVersion     string `json:"latest_version,omitempty"`
	UpdateAvailable   *bool  `json:"update_available"`
	VersionStatus     string `json:"version_status"`
	CredentialPresent *bool  `json:"credential_present"`
	Usable            *bool  `json:"usable"`
}
type Availability struct {
	Schema       int                   `json:"schema"`
	ObservedAt   time.Time             `json:"observed_at"`
	RefreshAfter time.Time             `json:"refresh_after"`
	Harnesses    []HarnessAvailability `json:"harnesses"`
}

// ProbeAvailability uses the daemon's PATH and environment. All argv are fixed;
// no login, network call, or credential-file read is made by this diagnostic.
func ProbeAvailability(ctx context.Context) Availability {
	out := Availability{Schema: 1, ObservedAt: time.Now().UTC(), Harnesses: []HarnessAvailability{}}
	out.RefreshAfter = out.ObservedAt.Add(5 * time.Minute)
	for _, name := range harness.Names() {
		h, _ := harness.Get(name)
		row := HarnessAvailability{Name: name, DisplayName: h.DisplayName, VersionStatus: "not_installed"}
		if h.Spawn == nil {
			row.VersionStatus = "not_spawnable"
			out.Harnesses = append(out.Harnesses, row)
			continue
		}
		row.Binary = h.Spawn.Binary()
		path, err := exec.LookPath(row.Binary)
		if err != nil {
			no := false
			row.Usable = &no
			out.Harnesses = append(out.Harnesses, row)
			continue
		}
		path, err = filepath.Abs(path)
		if err != nil {
			row.VersionStatus = "path_unavailable"
			out.Harnesses = append(out.Harnesses, row)
			continue
		}
		row.Installed = true
		row.Path = path
		row.Version = output(ctx, path, "--version")
		row.VersionStatus = "unknown"
		if row.Version != "" {
			row.VersionStatus = "known"
		}
		for _, key := range h.AvailabilityCredentialEnv {
			if os.Getenv(key) != "" {
				yes := true
				row.CredentialPresent = &yes
				break
			}
		}
		if row.CredentialPresent == nil {
			if home, err := os.UserHomeDir(); err == nil {
				if home, err = filepath.EvalSymlinks(home); err == nil {
					row.CredentialPresent = harnesscredentials.Presence(home, name)
				}
			}
		}
		if h.UsableWithoutCredentials {
			yes := true
			row.Usable = &yes
		}
		out.Harnesses = append(out.Harnesses, row)
	}
	return out
}
