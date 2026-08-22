package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/probehelper"
)

const (
	stackedSandboxBindingVersion = 2
	stackedBoundExecutableRoot   = "/tmp/.tclaude-stacked-harness"
	stackedBoundExecutablePath   = "/tmp/.tclaude-stacked-harness/bin/engine"
	stackedBoundCodexRuntimeRoot = "/tmp/.tclaude-stacked-codex"
)

var stackedProbeHelperSourcePath = "/proc/self/exe"

type stackedSandboxBindingFile struct {
	StagePath   string `json:"stage_path"`
	Destination string `json:"destination"`
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	Mode        uint32 `json:"mode"`
}

type stackedSandboxBindingManifest struct {
	Version                   int                         `json:"version"`
	StageRoot                 string                      `json:"stage_root"`
	Engine                    stackedSandboxBindingFile   `json:"engine"`
	RuntimeFiles              []stackedSandboxBindingFile `json:"runtime_files,omitempty"`
	ProbeHelper               *stackedSandboxBindingFile  `json:"probe_helper,omitempty"`
	FreezeClaudeManagedPolicy bool                        `json:"freeze_claude_managed_policy,omitempty"`
	ManagedPolicy             []stackedSandboxBindingFile `json:"managed_policy,omitempty"`
}

type StackedSandboxRuntimeBinding struct {
	HostPath    string
	SandboxPath string
}

// StackedSandboxProof is the launch-owned authority carried from the exact
// behavioral probe into the final outer relay. The relay reopens and re-hashes
// every staged file into sealed memfds, then passes those immutable
// descriptors into bubblewrap. Bubblewrap copies the sealed descriptors into
// a private tmpfs image and remounts that image read-only before exec, so the
// engine has a stable self-reexec path without making its verified bytes
// mutable. ReadyPath closes cleanup ordering: once written, bubblewrap
// inherited every proof fd and the parent may remove the staging names without
// changing the executed bytes.
type StackedSandboxProof struct {
	Executable harness.NestedSandboxExecutable
	// VersionProbePath is the launch-owned staged engine whose bytes are bound
	// into the outer sandbox. Unlike Executable.Path (the in-sandbox target), it
	// is directly executable by the parent for exact-version proof.
	VersionProbePath string
	RuntimeRoot      string
	RuntimeBindings  []StackedSandboxRuntimeBinding
	ManifestPath     string
	ManifestSHA256   string
	ReadyPath        string
	stageRoot        string
}

// completeProbe removes the Go-only proof helper from the authority carried
// into the final harness launch. The exact outer behavioral probe has already
// consumed its sealed descriptor; the interactive agent gets only the proven
// engine/runtime and frozen managed policy.
func (proof *StackedSandboxProof) completeProbe() error {
	if proof == nil || proof.ManifestPath == "" || proof.ManifestSHA256 == "" {
		return fmt.Errorf("stacked binding proof is missing")
	}
	data, err := os.ReadFile(proof.ManifestPath)
	if err != nil {
		return fmt.Errorf("read stacked probe binding manifest: %w", err)
	}
	if stackedBindingDigest(data) != proof.ManifestSHA256 {
		return fmt.Errorf("stacked probe binding manifest changed")
	}
	var manifest stackedSandboxBindingManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("parse stacked probe binding manifest: %w", err)
	}
	if manifest.ProbeHelper == nil ||
		manifest.ProbeHelper.Destination != probehelper.BoundPath {
		return fmt.Errorf("stacked probe helper authority is missing")
	}
	if err := os.Remove(manifest.ProbeHelper.StagePath); err != nil {
		return fmt.Errorf("remove consumed stacked probe helper: %w", err)
	}
	manifest.ProbeHelper = nil
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("encode final stacked binding manifest: %w", err)
	}
	finalPath := filepath.Join(proof.stageRoot, "launch-manifest.json")
	if err := os.WriteFile(finalPath, encoded, 0o600); err != nil {
		return fmt.Errorf("write final stacked binding manifest: %w", err)
	}
	proof.ManifestPath = finalPath
	proof.ManifestSHA256 = stackedBindingDigest(encoded)
	return nil
}

// Revalidate closes the ordinary construction interval before the final relay
// is spawned. The relay repeats this check against open descriptors and is the
// exec authority; this early check gives a synchronous named refusal instead
// of waiting for a pane whose relay correctly failed closed.
func (proof *StackedSandboxProof) Revalidate() error {
	if proof == nil || proof.ManifestPath == "" || proof.ManifestSHA256 == "" {
		return fmt.Errorf("stacked binding proof is missing")
	}
	data, err := os.ReadFile(proof.ManifestPath)
	if err != nil {
		return fmt.Errorf("read stacked binding manifest: %w", err)
	}
	if stackedBindingDigest(data) != proof.ManifestSHA256 {
		return fmt.Errorf("stacked binding manifest changed after capability probe")
	}
	var manifest stackedSandboxBindingManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("parse stacked binding manifest: %w", err)
	}
	if manifest.Version != stackedSandboxBindingVersion ||
		filepath.Clean(manifest.StageRoot) != filepath.Clean(proof.stageRoot) {
		return fmt.Errorf("stacked binding manifest authority changed")
	}
	files := append([]stackedSandboxBindingFile{manifest.Engine}, manifest.RuntimeFiles...)
	if manifest.ProbeHelper != nil {
		files = append(files, *manifest.ProbeHelper)
	}
	files = append(files, manifest.ManagedPolicy...)
	for _, binding := range files {
		file, err := os.Open(binding.StagePath)
		if err != nil {
			return fmt.Errorf("open stacked binding file %q: %w", binding.StagePath, err)
		}
		info, statErr := file.Stat()
		hash := sha256.New()
		_, hashErr := io.Copy(hash, file)
		closeErr := file.Close()
		switch {
		case statErr != nil:
			return fmt.Errorf("inspect stacked binding file %q: %w", binding.StagePath, statErr)
		case !info.Mode().IsRegular() || info.Size() != binding.Size:
			return fmt.Errorf("stacked binding file %q changed shape", binding.StagePath)
		case hashErr != nil:
			return fmt.Errorf("hash stacked binding file %q: %w", binding.StagePath, hashErr)
		case closeErr != nil:
			return fmt.Errorf("close stacked binding file %q: %w", binding.StagePath, closeErr)
		case hex.EncodeToString(hash.Sum(nil)) != binding.SHA256:
			return fmt.Errorf("stacked binding file %q changed after capability probe", binding.StagePath)
		}
	}
	return nil
}

func (proof *StackedSandboxProof) Cleanup() {
	if proof == nil {
		return
	}
	if proof.stageRoot != "" {
		_ = os.RemoveAll(proof.stageRoot)
		proof.stageRoot = ""
	}
	if proof.ReadyPath != "" {
		_ = os.Remove(proof.ReadyPath)
		proof.ReadyPath = ""
	}
}

func prepareStackedSandboxProof(
	h *harness.Harness,
	executable harness.NestedSandboxExecutable,
) (*StackedSandboxProof, error) {
	root, err := os.MkdirTemp("", "tclaude-stacked-binding-")
	if err != nil {
		return nil, fmt.Errorf("create launch-owned binding root: %w", err)
	}
	proof := &StackedSandboxProof{stageRoot: root}
	ok := false
	defer func() {
		if !ok {
			proof.Cleanup()
		}
	}()

	manifest := stackedSandboxBindingManifest{
		Version:   stackedSandboxBindingVersion,
		StageRoot: root,
	}
	if h != nil && h.Name == harness.CodexName {
		engine, runtimeFiles, stageErr := stageCodexRuntimeClosure(root, executable)
		if stageErr != nil {
			return nil, stackedSandboxRefusal(
				"stacked_codex_engine_binding",
				fmt.Sprintf("stage exact Codex runtime closure: %v", stageErr),
			)
		}
		manifest.Engine = engine
		manifest.RuntimeFiles = runtimeFiles
	} else {
		engine, stageErr := stageStackedSandboxFile(
			executable.Path,
			filepath.Join(root, "engine"),
			stackedBoundExecutablePath,
			0o500,
		)
		if stageErr != nil {
			return nil, fmt.Errorf("stage exact nested sandbox engine: %w", stageErr)
		}
		manifest.Engine = engine
	}
	if h != nil && h.Name == harness.DefaultName {
		manifest.FreezeClaudeManagedPolicy = true
		policy, policyErr := harness.SnapshotClaudeManagedPolicy()
		if policyErr != nil {
			return nil, stackedSandboxRefusal(
				"stacked_claude_inner_policy",
				fmt.Sprintf("capture outranking managed policy: %v", policyErr),
			)
		}
		if policyErr := validateStackedClaudeManagedPolicy(policy); policyErr != nil {
			return nil, stackedSandboxRefusal(
				"stacked_claude_inner_policy",
				policyErr.Error(),
			)
		}
		for index, file := range policy {
			staged, stageErr := stageStackedSandboxBytes(
				file.Data,
				filepath.Join(root, "policy", fmt.Sprintf("%04d.json", index)),
				file.Destination,
				0o400,
			)
			if stageErr != nil {
				return nil, stackedSandboxRefusal(
					"stacked_claude_inner_policy",
					fmt.Sprintf("freeze managed policy %q: %v", file.Destination, stageErr),
				)
			}
			manifest.ManagedPolicy = append(manifest.ManagedPolicy, staged)
		}
		helper, stageErr := stageStackedSandboxFile(
			stackedProbeHelperSourcePath,
			filepath.Join(root, "probe-helper"),
			probehelper.BoundPath,
			0o500,
		)
		if stageErr != nil {
			return nil, stackedSandboxRefusal(
				"stacked_claude_probe_helper",
				fmt.Sprintf(
					"stage the running Go probe helper from /proc/self/exe: %v",
					stageErr,
				),
			)
		}
		manifest.ProbeHelper = &helper
	}

	manifestPath := filepath.Join(root, "manifest.json")
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("encode stacked binding manifest: %w", err)
	}
	if err := os.WriteFile(manifestPath, encoded, 0o600); err != nil {
		return nil, fmt.Errorf("write stacked binding manifest: %w", err)
	}
	ready, err := os.CreateTemp("", "tclaude-stacked-ready-")
	if err != nil {
		return nil, fmt.Errorf("reserve stacked binding readiness path: %w", err)
	}
	readyPath := ready.Name()
	if err := ready.Close(); err != nil {
		return nil, fmt.Errorf("close stacked binding readiness reservation: %w", err)
	}
	if err := os.Remove(readyPath); err != nil {
		return nil, fmt.Errorf("prepare stacked binding readiness path: %w", err)
	}

	proof.Executable = executable
	proof.VersionProbePath = manifest.Engine.StagePath
	proof.Executable.Path = manifest.Engine.Destination
	if len(manifest.RuntimeFiles) > 0 {
		proof.RuntimeRoot = stackedBoundCodexRuntimeRoot
		for _, file := range manifest.RuntimeFiles {
			proof.RuntimeBindings = append(proof.RuntimeBindings, StackedSandboxRuntimeBinding{
				HostPath: file.StagePath, SandboxPath: file.Destination,
			})
		}
	}
	proof.ManifestPath = manifestPath
	proof.ManifestSHA256 = stackedBindingDigest(encoded)
	proof.ReadyPath = readyPath
	ok = true
	return proof, nil
}

func stageCodexRuntimeClosure(
	stageRoot string,
	executable harness.NestedSandboxExecutable,
) (
	stackedSandboxBindingFile,
	[]stackedSandboxBindingFile,
	error,
) {
	runtimeRoot := filepath.Clean(executable.RuntimeRoot)
	if runtimeRoot == "." || !filepath.IsAbs(runtimeRoot) {
		return stackedSandboxBindingFile{}, nil,
			fmt.Errorf("codex native runtime root is missing")
	}
	engineRelative, err := filepath.Rel(runtimeRoot, executable.Path)
	if err != nil || engineRelative != filepath.Join("bin", "codex") {
		return stackedSandboxBindingFile{}, nil,
			fmt.Errorf("codex engine is outside its recognized runtime root")
	}
	var engine stackedSandboxBindingFile
	var runtimeFiles []stackedSandboxBindingFile
	err = filepath.WalkDir(runtimeRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		// The official standalone layout includes a convenience
		// <release>/codex -> bin/codex symlink. It is not part of the native
		// runtime closure: the authenticated regular bin/codex is the launch
		// authority and the staged engine destination is explicit. Ignore
		// convenience symlinks rather than following them or treating them as
		// independently executable bytes.
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("runtime entry %q is not a regular file", path)
		}
		relative, err := filepath.Rel(runtimeRoot, path)
		if err != nil || relative == "." || relative == ".." ||
			strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("runtime entry %q escapes its root", path)
		}
		mode := os.FileMode(0o400)
		if info.Mode().Perm()&0o111 != 0 {
			mode = 0o500
		}
		staged, err := stageStackedSandboxFile(
			path,
			filepath.Join(stageRoot, "runtime", relative),
			filepath.Join(stackedBoundCodexRuntimeRoot, relative),
			mode,
		)
		if err != nil {
			return err
		}
		if relative == engineRelative {
			engine = staged
		} else {
			runtimeFiles = append(runtimeFiles, staged)
		}
		return nil
	})
	if err != nil {
		return stackedSandboxBindingFile{}, nil, err
	}
	if engine.StagePath == "" {
		return stackedSandboxBindingFile{}, nil,
			fmt.Errorf("codex runtime closure omitted bin/codex")
	}
	sort.Slice(runtimeFiles, func(i, j int) bool {
		return runtimeFiles[i].Destination < runtimeFiles[j].Destination
	})
	return engine, runtimeFiles, nil
}

func stackedBindingDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func validateStackedClaudeManagedPolicy(
	files []harness.ClaudeManagedPolicyFile,
) error {
	required := map[string]bool{
		"enabled":                   true,
		"failIfUnavailable":         true,
		"allowUnsandboxedCommands":  false,
		"enableWeakerNestedSandbox": false,
	}
	for _, file := range files {
		var document map[string]any
		if err := json.Unmarshal(file.Data, &document); err != nil {
			return fmt.Errorf(
				"managed policy %q is not parseable: %v",
				file.Destination,
				err,
			)
		}
		rawSandbox, present := document["sandbox"]
		if !present {
			continue
		}
		sandbox, ok := rawSandbox.(map[string]any)
		if !ok {
			return fmt.Errorf(
				"managed policy %q has an unprovable sandbox block",
				file.Destination,
			)
		}
		for key, want := range required {
			raw, present := sandbox[key]
			if !present {
				continue
			}
			got, ok := raw.(bool)
			if !ok || got != want {
				return fmt.Errorf(
					"managed policy %q overrides sandbox.%s away from the required stacked posture",
					file.Destination,
					key,
				)
			}
		}
	}
	return nil
}

func stageStackedSandboxFile(
	source, target, destination string,
	mode os.FileMode,
) (stackedSandboxBindingFile, error) {
	input, err := os.Open(source)
	if err != nil {
		return stackedSandboxBindingFile{}, err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return stackedSandboxBindingFile{}, err
	}
	output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return stackedSandboxBindingFile{}, err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(output, hash), input)
	syncErr := output.Sync()
	closeErr := output.Close()
	switch {
	case copyErr != nil:
		return stackedSandboxBindingFile{}, copyErr
	case syncErr != nil:
		return stackedSandboxBindingFile{}, syncErr
	case closeErr != nil:
		return stackedSandboxBindingFile{}, closeErr
	}
	return stackedSandboxBindingFile{
		StagePath:   target,
		Destination: destination,
		SHA256:      hex.EncodeToString(hash.Sum(nil)),
		Size:        written,
		Mode:        uint32(mode.Perm()),
	}, nil
}

func stageStackedSandboxBytes(
	data []byte,
	target, destination string,
	mode os.FileMode,
) (stackedSandboxBindingFile, error) {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return stackedSandboxBindingFile{}, err
	}
	if err := os.WriteFile(target, data, mode); err != nil {
		return stackedSandboxBindingFile{}, err
	}
	hash := sha256.Sum256(data)
	return stackedSandboxBindingFile{
		StagePath:   target,
		Destination: destination,
		SHA256:      hex.EncodeToString(hash[:]),
		Size:        int64(len(data)),
		Mode:        uint32(mode.Perm()),
	}, nil
}
