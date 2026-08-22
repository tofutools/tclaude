package session

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

const ExecutionBoundaryVersion = 1

// ExecutionBoundary is launch-time evidence about the process boundary that
// actually ran a session. It complements the operator-authored sandbox
// snapshot: this records launch-adapter additions such as executable mounts,
// constructed-root entries, PATH injection, and user-namespace identity.
type ExecutionBoundary struct {
	Version               int                       `json:"version"`
	LaunchGeneration      string                    `json:"launch_generation,omitempty"`
	SandboxImplementation string                    `json:"sandbox_implementation"`
	Platform              string                    `json:"platform"`
	Harness               ExecutionHarness          `json:"harness"`
	Tclaude               *ExecutionBinary          `json:"tclaude,omitempty"`
	Launcher              *ExecutionBinary          `json:"launcher,omitempty"`
	PATH                  ExecutionPATH             `json:"path"`
	Identity              ExecutionIdentityMapping  `json:"identity"`
	RootMode              string                    `json:"root_mode"`
	AutomaticEntries      []ExecutionNamespaceEntry `json:"automatic_namespace_entries"`
	OuterLayerRenderInput *TclaudeLayerLaunchSpec   `json:"outer_layer_render_input,omitempty"`
}

type ExecutionHarness struct {
	Name         string   `json:"name"`
	LookupName   string   `json:"lookup_name"`
	HostPath     string   `json:"host_path,omitempty"`
	SandboxPath  string   `json:"sandbox_path,omitempty"`
	Resolution   string   `json:"resolution"`
	RuntimeRoots []string `json:"runtime_roots,omitempty"`
}

type ExecutionBinary struct {
	Name        string `json:"name"`
	HostPath    string `json:"host_path"`
	SandboxPath string `json:"sandbox_path,omitempty"`
	Exposure    string `json:"exposure"`
}

type ExecutionPATH struct {
	Host                    string   `json:"host"`
	LaunchBase              string   `json:"launch_base"`
	BeforePreLaunch         string   `json:"before_pre_launch"`
	Construction            []string `json:"construction"`
	PreLaunchBlocks         []string `json:"pre_launch_blocks,omitempty"`
	PreLaunchDeclaresPATH   []string `json:"pre_launch_declares_path,omitempty"`
	FinalValueKnown         bool     `json:"final_value_known"`
	FinalValueUnknownReason string   `json:"final_value_unknown_reason,omitempty"`
}

type ExecutionUnixIdentity struct {
	UID    int   `json:"uid"`
	GID    int   `json:"gid"`
	Groups []int `json:"groups,omitempty"`
}

type ExecutionIdentityMapping struct {
	Host          ExecutionUnixIdentity `json:"host"`
	Sandbox       ExecutionUnixIdentity `json:"sandbox"`
	UserNamespace bool                  `json:"user_namespace"`
	Mapping       string                `json:"mapping"`
}

type ExecutionNamespaceEntry struct {
	Kind   string `json:"kind"`
	Source string `json:"source,omitempty"`
	Target string `json:"target"`
	Access string `json:"access,omitempty"`
	Origin string `json:"origin"`
}

type ExecutionBoundaryInput struct {
	LaunchGeneration          string
	SandboxImplementation     string
	HarnessName               string
	HarnessLookupName         string
	HarnessExecutable         string
	HarnessExecutableResolved bool
	HarnessSandboxPath        string
	HarnessRuntimeRoots       []string
	HarnessRuntimeBindings    []StackedSandboxRuntimeBinding
	LauncherBinary            string
	LauncherBinaryResolved    bool
	Cwd                       string
	Environment               map[string]string
	PreLaunch                 []sandboxpolicy.PreLaunchBlock
	LayerSpec                 *TclaudeLayerLaunchSpec
}

// BuildExecutionBoundary freezes the launch-adapter facts that are otherwise
// lost once the shell command has been handed to tmux.
func BuildExecutionBoundary(input ExecutionBoundaryInput) (*ExecutionBoundary, error) {
	hostIdentity := currentExecutionIdentity()
	basePATH := input.Environment["PATH"]
	if basePATH == "" {
		basePATH = os.Getenv("PATH")
	}
	out := &ExecutionBoundary{
		Version: ExecutionBoundaryVersion, LaunchGeneration: input.LaunchGeneration,
		SandboxImplementation: input.SandboxImplementation,
		Platform:              runtime.GOOS,
		Harness: ExecutionHarness{
			Name: input.HarnessName, LookupName: input.HarnessLookupName,
			Resolution: "shell PATH lookup", RuntimeRoots: append([]string(nil), input.HarnessRuntimeRoots...),
		},
		PATH: ExecutionPATH{
			Host: os.Getenv("PATH"), LaunchBase: basePATH, BeforePreLaunch: basePATH,
			Construction:    []string{"launch environment PATH (profile value when set, otherwise inherited host PATH)"},
			FinalValueKnown: true,
		},
		Identity: ExecutionIdentityMapping{
			Host: hostIdentity, Sandbox: hostIdentity, Mapping: "host identity retained",
		},
		RootMode:         "host-inherited",
		AutomaticEntries: []ExecutionNamespaceEntry{},
	}
	if executable := strings.TrimSpace(input.HarnessExecutable); executable != "" {
		resolved, err := resolveRecordedExecutionPath(executable, input.HarnessExecutableResolved)
		if err != nil {
			return nil, fmt.Errorf("record harness executable: %w", err)
		}
		out.Harness.HostPath = resolved
		out.Harness.SandboxPath = resolved
		if sandboxPath := strings.TrimSpace(input.HarnessSandboxPath); sandboxPath != "" {
			out.Harness.SandboxPath = sandboxPath
		}
		out.Harness.Resolution = "absolute executable resolved before sandbox launch"
	} else if candidate := resolveRecordedPATHExecutable(input.HarnessLookupName, basePATH, input.Cwd); candidate != "" {
		out.Harness.HostPath = candidate
		out.Harness.SandboxPath = candidate
		out.Harness.Resolution = "resolved from launch PATH before exec"
	}
	if len(input.HarnessRuntimeBindings) == 0 {
		for _, root := range out.Harness.RuntimeRoots {
			out.AutomaticEntries = append(out.AutomaticEntries, ExecutionNamespaceEntry{
				Kind: "bind", Source: root, Target: root, Access: "read-only", Origin: "harness runtime closure",
			})
		}
	} else {
		for _, binding := range input.HarnessRuntimeBindings {
			out.AutomaticEntries = append(out.AutomaticEntries, ExecutionNamespaceEntry{
				Kind: "bind", Source: binding.HostPath, Target: binding.SandboxPath,
				Access: "read-only", Origin: "staged nested harness runtime closure",
			})
		}
	}
	preLaunch := input.PreLaunch
	if input.LayerSpec == nil {
		qualifyHarnessPATHResolution(&out.Harness, preLaunch)
		recordExecutionPATHPreLaunch(&out.PATH, preLaunch)
		return out, nil
	}
	spec := *input.LayerSpec
	preLaunch = spec.Effective.PreLaunch
	qualifyHarnessPATHResolution(&out.Harness, preLaunch)
	out.OuterLayerRenderInput = &spec
	_, _, _, readOnlyBinds, _, plan, err := tclaudeLayerSpecRenderInput(spec)
	if err != nil {
		return nil, fmt.Errorf("record outer-layer render input: %w", err)
	}
	constructed := tclaudeLayerPlanUsesConstructedRoot(plan)
	if constructed && runtime.GOOS == "linux" {
		out.RootMode = "constructed"
		for _, path := range tclaudeLayerStaticOSPaths {
			info, statErr := os.Lstat(path)
			if os.IsNotExist(statErr) {
				continue
			}
			if statErr != nil {
				return nil, fmt.Errorf("record static root path %q: %w", path, statErr)
			}
			entry := ExecutionNamespaceEntry{Target: path, Origin: "constructed-root static OS surface"}
			if info.Mode()&os.ModeSymlink != 0 {
				entry.Kind = "symlink"
				entry.Source, statErr = os.Readlink(path)
			} else {
				entry.Kind, entry.Source, entry.Access = "bind", path, "read-only"
			}
			if statErr != nil {
				return nil, fmt.Errorf("record static root path %q: %w", path, statErr)
			}
			out.AutomaticEntries = append(out.AutomaticEntries, entry)
		}
		tclaudeSource, err := resolveRecordedExecutable(tclaudeLayerTclaudeCLIPath())
		if err != nil {
			return nil, fmt.Errorf("record tclaude executable: %w", err)
		}
		out.Tclaude = &ExecutionBinary{
			Name: "tclaude", HostPath: tclaudeSource,
			SandboxPath: tclaudeLayerConstructedRootTclaudePath, Exposure: "single-file read-only bind",
		}
		out.AutomaticEntries = append(out.AutomaticEntries, ExecutionNamespaceEntry{
			Kind: "bind", Source: tclaudeSource, Target: tclaudeLayerConstructedRootTclaudePath,
			Access: "read-only", Origin: "tclaude coordination CLI",
		}, ExecutionNamespaceEntry{
			Kind:   "bind",
			Source: filepath.Join(tclcommon.TclaudeDataDir(), tclaudeLayerConstructedRootBashEnvState),
			Target: tclaudeLayerConstructedRootBashEnvPath,
			Access: "read-only", Origin: "constructed-root Bash PATH repair",
		})
		out.PATH.BeforePreLaunch = tclaudeLayerConstructedRootTclaudeBin + string(os.PathListSeparator) + basePATH
		out.PATH.Construction = append(out.PATH.Construction,
			"prepend /.tclaude/bin after generated launch environment exports",
			"when BASH_ENV is unset, arm a one-shot Bash startup fragment")
	} else if runtime.GOOS == "darwin" {
		out.RootMode = "host-inherited (Seatbelt policy; no mount namespace)"
	}
	for _, bind := range readOnlyBinds {
		out.AutomaticEntries = append(out.AutomaticEntries, ExecutionNamespaceEntry{
			Kind: "bind", Source: bind.Source, Target: bind.Target,
			Access: "read-only", Origin: "daemon-final launch contract",
		})
	}
	launcher, err := resolveRecordedExecutionPath(input.LauncherBinary, input.LauncherBinaryResolved)
	if err != nil {
		return nil, fmt.Errorf("record outer-layer launcher: %w", err)
	}
	out.Launcher = &ExecutionBinary{Name: filepath.Base(launcher), HostPath: launcher, Exposure: "host-side namespace launcher"}
	if runtime.GOOS == "linux" && tclaudeLayerPlanFloorPosture(plan) == sandboxpolicy.NetworkFiltered {
		out.Identity.Sandbox.UID = 0
		out.Identity.Sandbox.GID = 0
		out.Identity.Sandbox.Groups = nil
		out.Identity.UserNamespace = true
		out.Identity.Mapping = "invoking host uid/gid mapped to namespace uid/gid 0:0 (--unshare-user --uid 0 --gid 0)"
	}
	recordExecutionPATHPreLaunch(&out.PATH, preLaunch)
	return out, nil
}

func qualifyHarnessPATHResolution(harness *ExecutionHarness, blocks []sandboxpolicy.PreLaunchBlock) {
	if len(blocks) > 0 && harness.Resolution == "resolved from launch PATH before exec" {
		harness.Resolution = "candidate resolved from launch PATH before pre-launch blocks; live executable observation is authoritative"
	}
}

func recordExecutionPATHPreLaunch(path *ExecutionPATH, blocks []sandboxpolicy.PreLaunchBlock) {
	for _, block := range blocks {
		path.PreLaunchBlocks = append(path.PreLaunchBlocks, block.Name)
		if slices.Contains(block.Exports, "PATH") {
			path.PreLaunchDeclaresPATH = append(path.PreLaunchDeclaresPATH, block.Name)
		}
	}
	if len(blocks) > 0 {
		path.FinalValueKnown = false
		path.FinalValueUnknownReason = "operator pre-launch shell blocks run after PATH construction and may mutate PATH"
	}
}

func resolveRecordedExecutable(path string) (string, error) {
	return resolveRecordedExecutionPath(path, false)
}

func resolveRecordedExecutionPath(path string, alreadyResolved bool) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if alreadyResolved {
		return filepath.Clean(abs), nil
	}
	return filepath.EvalSymlinks(abs)
}

func resolveRecordedPATHExecutable(name, path, cwd string) string {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsRune(name, filepath.Separator) {
		return ""
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = cwd
		} else if !filepath.IsAbs(dir) {
			dir = filepath.Join(cwd, dir)
		}
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		resolved, err := filepath.EvalSymlinks(candidate)
		if err == nil {
			return resolved
		}
	}
	return ""
}
