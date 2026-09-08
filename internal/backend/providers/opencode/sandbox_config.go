package opencode

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
)

// The native config loader creates this exact file before loading settings.
// Seed it once before the directory becomes read-only, preserving authored files.
const nativeConfigGitignore = "node_modules\npackage.json\npackage-lock.json\nbun.lock\n.gitignore"

func (r *Runtime) attachmentEnvironment() []string {
	environment := r.provider.runtimeEnvironment(r.stateRoot)
	if r.artifact == nil || r.nativeConfigDirectory == "" {
		return environment
	}
	// The presentation client runs outside the server's Linux mount namespace.
	// Use its retained source settings, not the unmounted private destination.
	if filepath.Base(r.nativeConfigDirectory) == "opencode" {
		return host.MergeEnvironment(environment, []string{"XDG_CONFIG_HOME=" + filepath.Dir(r.nativeConfigDirectory), "OPENCODE_CONFIG_DIR="})
	}
	return host.MergeEnvironment(environment, []string{"OPENCODE_CONFIG_DIR=" + r.nativeConfigDirectory})
}

func prepareSandboxConfiguration(nativeConfig, stateRoot string) (string, []host.SandboxProviderResource, error) {
	privateConfig := filepath.Join(stateRoot, "config", "opencode")
	source := nativeConfig
	if source != "" {
		_, err := os.Stat(source)
		if os.IsNotExist(err) {
			source = ""
		} else if err != nil {
			return "", nil, err
		}
	}
	if source == "" {
		source = privateConfig
	}
	canonical, err := filepath.EvalSymlinks(source)
	if err != nil {
		return "", nil, err
	}
	if runtime.GOOS == "darwin" && filepath.Base(canonical) != "opencode" {
		return "", nil, fmt.Errorf("macOS OpenCode configuration requires an opencode app directory; directory remapping is unavailable")
	}
	if err := prepareNativeConfigGitignore(canonical); err != nil {
		return "", nil, err
	}
	resources := []host.SandboxProviderResource{{Path: canonical, Access: model.SandboxFilesystemRead}}
	configHome := filepath.Dir(canonical)
	if runtime.GOOS == "linux" {
		configHome = filepath.Dir(privateConfig)
		if canonical != privateConfig {
			resources = append(resources, host.SandboxOpenCodeConfiguration(canonical, stateRoot, model.SandboxFilesystemRead))
		}
	}
	return configHome, resources, nil
}

func prepareNativeConfigGitignore(path string) error {
	root, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	file, err := root.OpenFile(".gitignore", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if os.IsExist(err) {
		info, statErr := root.Lstat(".gitignore")
		if statErr != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("OpenCode configuration .gitignore must be a regular file")
		}
		return nil
	}
	if err != nil {
		return err
	}
	_, writeErr := io.WriteString(file, nativeConfigGitignore)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		_ = root.Remove(".gitignore")
		return writeErr
	}
	return closeErr
}
