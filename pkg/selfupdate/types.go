// Package selfupdate stages verified official updates and durable rollback
// records. It never accepts a download URL, install path or shell command from
// the caller. Only a validated release version and operation are inputs.
package selfupdate

import (
	"context"
	"debug/buildinfo"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/common/buildversion"
	"golang.org/x/mod/semver"
)

const module = "github.com/tofutools/tclaude"
const buildInfoFlag = "--tclaude-build-info"

var binaryPackages = map[string]string{"tclaude": module, "tclaude-agentd": module + "/cmd/tclaude-agentd", "tclaude-hub": module + "/cmd/tclaude-hub"}

type Binary struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Version string `json:"version"`
	Method  string `json:"install_method"`
}
type Request struct {
	Action  string `json:"action"`
	Version string `json:"version,omitempty"`
}

func (r Request) Validate() error {
	if r.Action != "check" && r.Action != "apply" && r.Action != "rollback" {
		return fmt.Errorf("action must be check, apply or rollback")
	}
	if r.Version != "" && (!semver.IsValid(r.Version) || semver.Canonical(r.Version) != r.Version || len(r.Version) > 80) {
		return fmt.Errorf("version must be a canonical v-prefixed semver")
	}
	if r.Action == "rollback" && r.Version != "" {
		return fmt.Errorf("rollback does not accept a version")
	}
	return nil
}

type Job struct {
	CurrentVersion  string     `json:"current_version"`
	UpdateAvailable *bool      `json:"update_available"`
	ID              string     `json:"id"`
	Action          string     `json:"action"`
	Version         string     `json:"version,omitempty"`
	Actor           string     `json:"actor"`
	State           string     `json:"state"`
	Phase           string     `json:"phase"`
	StartedAt       time.Time  `json:"started_at"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
	Error           string     `json:"error,omitempty"`
	Warnings        []string   `json:"warnings"`
	RestartRequired bool       `json:"restart_required"`
}
type Status struct {
	CurrentVersion    string     `json:"current_version"`
	InstallMethod     string     `json:"install_method"`
	ProtocolVersion   int        `json:"protocol_version"`
	Binaries          []Binary   `json:"binaries"`
	LatestVersion     string     `json:"latest_version,omitempty"`
	CheckedAt         *time.Time `json:"checked_at,omitempty"`
	UpdateAvailable   *bool      `json:"update_available"`
	RollbackAvailable bool       `json:"rollback_available"`
	Warnings          []string   `json:"warnings"`
	Job               *Job       `json:"job,omitempty"`
}

func inspectBinary(name, path string) (Binary, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return Binary{}, err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return Binary{}, err
	}
	bi, err := buildinfo.ReadFile(resolved)
	if err != nil {
		return Binary{}, fmt.Errorf("%s is not a readable Go binary", name)
	}
	if bi.Main.Path != module || bi.Path != binaryPackages[name] || bi.Main.Replace != nil {
		return Binary{}, fmt.Errorf("%s is not an unmodified tclaude module binary", name)
	}
	out := Binary{Name: name, Path: resolved, Version: bi.Main.Version, Method: "source"}
	hasVCS := false
	for _, setting := range bi.Settings {
		hasVCS = hasVCS || setting.Key == "vcs.revision"
	}
	if semver.IsValid(bi.Main.Version) && !hasVCS {
		out.Method = "go_install"
	}
	for _, setting := range bi.Settings {
		if setting.Key == "-ldflags" {
			for _, flag := range strings.Fields(setting.Value) {
				if flag == "github.com/tofutools/tclaude/pkg/common/buildversion.InstallMethod=release" {
					out.Method = "release"
				}
				if strings.HasPrefix(flag, "main.version=") {
					if value := strings.TrimPrefix(flag, "main.version="); value != "" {
						out.Version = value
					}
				}
			}
		}
	}
	if out.Method == "release" && !semver.IsValid(out.Version) {
		out.Method = "source"
	}
	return out, nil
}
func Discover() ([]Binary, error) {
	paths := map[string]string{}
	for name := range binaryPackages {
		if p, err := exec.LookPath(name); err == nil {
			paths[name] = p
		}
	}
	own, err := os.Executable()
	if err != nil {
		return nil, err
	}
	bi, err := buildinfo.ReadFile(own)
	if err != nil {
		return nil, err
	}
	for name, pkg := range binaryPackages {
		if bi.Path == pkg {
			paths[name] = own
		}
	}
	out := []Binary{}
	for _, name := range []string{"tclaude", "tclaude-agentd", "tclaude-hub"} {
		if p, ok := paths[name]; ok {
			b, err := inspectBinary(name, p)
			if err != nil {
				return nil, err
			}
			out = append(out, b)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no installed tclaude binaries found")
	}
	return out, nil
}
func inspectStaged(ctx context.Context, path string) (buildversion.Info, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, buildInfoFlag)
	cmd.WaitDelay = 100 * time.Millisecond
	var out limitedBuffer
	out.limit = 4096
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return buildversion.Info{}, fmt.Errorf("updated binary build-info probe failed: %w", err)
	}
	var info buildversion.Info
	if err := json.Unmarshal(out.data, &info); err != nil {
		return info, fmt.Errorf("updated binary build-info invalid: %w", err)
	}
	return info, nil
}
