package agentd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

const (
	codexSSHAgentDirectory = "TCL_CODEX_SSH_CONFIG_DIR"
	codexSSHCommandEnv     = "GIT_SSH_COMMAND"
	codexSSHConfigName     = "ssh_config"
	codexSSHDropInDirName  = "ssh_config.d"
	codexSSHSourceName     = "__tclaude_codex_ssh_workaround"
)

var (
	codexSSHSystemConfig = "/etc/ssh/ssh_config"
	codexSSHLookPath     = exec.LookPath
)

func codexSSHSource() sandboxpolicy.ProfileSource {
	return sandboxpolicy.ProfileSource{
		Scope:   sandboxpolicy.ScopeExplicit,
		Profile: codexSSHSourceName,
	}
}

// codexSSHWorkaroundEnabledInSnapshot recovers the resolved birth posture
// from the immutable launch snapshot. Pending Codex enrollment can be resumed
// by the sweeper after the original spawnParams are gone, while the snapshot
// remains durable on the pending row.
func codexSSHWorkaroundEnabledInSnapshot(snapshot *sandboxpolicy.Snapshot) bool {
	if snapshot == nil {
		return false
	}
	for _, name := range snapshot.Effective.AgentDirectories {
		if name == codexSSHAgentDirectory {
			return true
		}
	}
	return false
}

// configureCodexSSHWorkaroundDeclaration adds or removes the private
// agent-directory declaration before ordinary materialization/reconciliation.
// Generated state is deliberately represented inside the frozen snapshot so
// the Codex adapter pins GIT_SSH_COMMAND in both the process environment and
// shell_environment_policy.
func configureCodexSSHWorkaroundDeclaration(snapshot sandboxpolicy.Snapshot, enabled bool) (sandboxpolicy.Snapshot, error) {
	if runtime.GOOS != "linux" || snapshot.ProfilesOmitted {
		enabled = false
	}
	effective := sandboxpolicy.NewSnapshot(snapshot.Effective, snapshot.Applied).Effective
	effective.Filesystem = append([]sandboxpolicy.FilesystemGrant(nil), effective.Filesystem...)
	effective.Environment = append([]sandboxpolicy.EnvironmentEntry(nil), effective.Environment...)
	effective.AgentDirectories = append([]string(nil), effective.AgentDirectories...)

	var oldPath string
	for _, entry := range effective.Environment {
		if entry.Name == codexSSHAgentDirectory {
			oldPath = entry.Value
			break
		}
	}
	if !enabled {
		dirs := effective.AgentDirectories[:0]
		for _, name := range effective.AgentDirectories {
			if name != codexSSHAgentDirectory {
				dirs = append(dirs, name)
			}
		}
		effective.AgentDirectories = dirs
		env := effective.Environment[:0]
		for _, entry := range effective.Environment {
			if entry.Name == codexSSHAgentDirectory {
				continue
			}
			if entry.Name == codexSSHCommandEnv &&
				effective.Provenance.Environment[entry.Name].Profile == codexSSHSourceName {
				continue
			}
			env = append(env, entry)
		}
		effective.Environment = env
		delete(effective.Provenance.AgentDirectories, codexSSHAgentDirectory)
		delete(effective.Provenance.Environment, codexSSHAgentDirectory)
		if effective.Provenance.Environment[codexSSHCommandEnv].Profile == codexSSHSourceName {
			delete(effective.Provenance.Environment, codexSSHCommandEnv)
		}
		if oldPath != "" {
			oldParent := filepath.Dir(oldPath)
			grants := effective.Filesystem[:0]
			for _, grant := range effective.Filesystem {
				if grant.Path == oldPath || grant.Path == oldParent {
					delete(effective.Provenance.Filesystem, grant.Path)
					continue
				}
				grants = append(grants, grant)
			}
			effective.Filesystem = grants
		}
	} else {
		found := false
		for _, name := range effective.AgentDirectories {
			found = found || name == codexSSHAgentDirectory
		}
		if !found {
			effective.AgentDirectories = append(effective.AgentDirectories, codexSSHAgentDirectory)
		}
		sort.Strings(effective.AgentDirectories)
		effective.Provenance.AgentDirectories[codexSSHAgentDirectory] = []sandboxpolicy.ProfileSource{codexSSHSource()}
	}

	configured := sandboxpolicy.NewSnapshot(effective, snapshot.Applied)
	configured.ResolutionGroupID = snapshot.ResolutionGroupID
	configured.ProfilesOmitted = snapshot.ProfilesOmitted
	return sandboxpolicy.RevalidateSnapshot(configured)
}

func prepareCodexSSHWorkaroundForNewLaunch(
	snapshot sandboxpolicy.Snapshot,
	launchKey string,
	enabled bool,
) (sandboxpolicy.Snapshot, func(), error) {
	configured, err := configureCodexSSHWorkaroundDeclaration(snapshot, enabled)
	if err != nil {
		return sandboxpolicy.Snapshot{}, func() {}, fmt.Errorf("configure Codex SSH workaround: %w", err)
	}
	materialized, cleanup, err := materializeAgentDirectories(configured, launchKey)
	if err != nil {
		return sandboxpolicy.Snapshot{}, func() {}, err
	}
	if !enabled || runtime.GOOS != "linux" || configured.ProfilesOmitted {
		return materialized, cleanup, nil
	}
	populated, err := populateCodexSSHWorkaround(materialized)
	if err != nil {
		cleanup()
		return sandboxpolicy.Snapshot{}, func() {}, err
	}
	return populated, cleanup, nil
}

func finalizeCodexSSHWorkaroundForRelaunch(
	snapshot sandboxpolicy.Snapshot,
	enabled bool,
) (sandboxpolicy.Snapshot, error) {
	if !enabled || runtime.GOOS != "linux" || snapshot.ProfilesOmitted {
		return snapshot, nil
	}
	return populateCodexSSHWorkaround(snapshot)
}

// populateCodexSSHWorkaround regenerates the config while the agent is
// offline, then overrides any profile-supplied GIT_SSH_COMMAND. The first
// implementation intentionally wins over Git's core.sshCommand as well; the
// opt-out is the escape hatch while TCL-745 investigates safe composition.
func populateCodexSSHWorkaround(snapshot sandboxpolicy.Snapshot) (sandboxpolicy.Snapshot, error) {
	var configDir string
	for _, entry := range snapshot.Effective.Environment {
		if entry.Name == codexSSHAgentDirectory {
			configDir = filepath.Clean(entry.Value)
			break
		}
	}
	if configDir == "." || filepath.Base(configDir) != codexSSHAgentDirectory {
		return sandboxpolicy.Snapshot{}, fmt.Errorf("Codex SSH workaround directory binding is missing or malformed")
	}
	parent := filepath.Dir(configDir)
	if _, err := removeDirAtNoFollow(parent, filepath.Base(configDir)); err != nil {
		return sandboxpolicy.Snapshot{}, fmt.Errorf("reset Codex SSH workaround directory: %w", err)
	}
	if err := mkdirAllNoFollow(configDir, 0o700); err != nil {
		return sandboxpolicy.Snapshot{}, fmt.Errorf("recreate Codex SSH workaround directory: %w", err)
	}

	configPath, err := writeCodexSSHConfig(configDir)
	if err != nil {
		return sandboxpolicy.Snapshot{}, err
	}
	sshPath, err := codexSSHLookPath("ssh")
	if err != nil {
		return sandboxpolicy.Snapshot{}, fmt.Errorf("locate ssh for Codex SSH workaround: %w", err)
	}
	sshPath, err = filepath.Abs(sshPath)
	if err != nil {
		return sandboxpolicy.Snapshot{}, fmt.Errorf("resolve ssh path for Codex SSH workaround: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(sshPath); resolveErr == nil {
		sshPath = resolved
	}
	command := clcommon.ShellQuoteArg(sshPath) + " -F " + clcommon.ShellQuoteArg(configPath)

	effective := sandboxpolicy.NewSnapshot(snapshot.Effective, snapshot.Applied).Effective
	effective.Environment = append([]sandboxpolicy.EnvironmentEntry(nil), effective.Environment...)
	filtered := effective.Environment[:0]
	for _, entry := range effective.Environment {
		if entry.Name != codexSSHCommandEnv {
			filtered = append(filtered, entry)
		}
	}
	effective.Environment = append(filtered, sandboxpolicy.EnvironmentEntry{
		Name: codexSSHCommandEnv, Value: command,
	})
	effective.Provenance.Environment[codexSSHCommandEnv] = codexSSHSource()
	sort.Slice(effective.Environment, func(i, j int) bool {
		return effective.Environment[i].Name < effective.Environment[j].Name
	})
	populated := sandboxpolicy.NewSnapshot(effective, snapshot.Applied)
	populated.ResolutionGroupID = snapshot.ResolutionGroupID
	return sandboxpolicy.RevalidateSnapshot(populated)
}

func writeCodexSSHConfig(configDir string) (string, error) {
	dropInSource := filepath.Join(filepath.Dir(codexSSHSystemConfig), codexSSHDropInDirName)
	dropInTarget := filepath.Join(configDir, codexSSHDropInDirName)
	if err := mkdirAllNoFollow(dropInTarget, 0o700); err != nil {
		return "", fmt.Errorf("create Codex SSH drop-in directory: %w", err)
	}

	if entries, err := os.ReadDir(dropInSource); err == nil {
		for _, entry := range entries {
			source := filepath.Join(dropInSource, entry.Name())
			info, infoErr := os.Stat(source) // dereference system-owned symlinks
			if infoErr != nil {
				return "", fmt.Errorf("inspect system SSH drop-in %q: %w", source, infoErr)
			}
			if !info.Mode().IsRegular() {
				continue
			}
			data, readErr := os.ReadFile(source)
			if readErr != nil {
				return "", fmt.Errorf("read system SSH drop-in %q: %w", source, readErr)
			}
			if writeErr := writeNewPrivateFile(filepath.Join(dropInTarget, entry.Name()), data); writeErr != nil {
				return "", fmt.Errorf("copy system SSH drop-in %q: %w", source, writeErr)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read system SSH drop-in directory: %w", err)
	}

	var system []byte
	if data, err := os.ReadFile(codexSSHSystemConfig); err == nil {
		system = data
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read system SSH config: %w", err)
	}
	rewritten := strings.ReplaceAll(
		string(system),
		filepath.Clean(dropInSource)+string(filepath.Separator),
		filepath.Clean(dropInTarget)+string(filepath.Separator),
	)
	contents := "Include ~/.ssh/config\n" + rewritten
	if len(system) == 0 {
		contents += "Host *\n"
	}
	configPath := filepath.Join(configDir, codexSSHConfigName)
	if err := writeNewPrivateFile(configPath, []byte(contents)); err != nil {
		return "", fmt.Errorf("write Codex SSH config: %w", err)
	}
	return configPath, nil
}

func writeNewPrivateFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	return errors.Join(writeErr, closeErr)
}
