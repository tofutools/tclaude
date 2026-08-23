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
	"github.com/tofutools/tclaude/pkg/claude/harness"
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
	codexSSHUserConfig   = "~/.ssh/config"
	codexSSHLookPath     = exec.LookPath
)

func codexSSHSource() sandboxpolicy.ProfileSource {
	return sandboxpolicy.ProfileSource{
		Scope:   sandboxpolicy.ScopeExplicit,
		Profile: codexSSHSourceName,
	}
}

// codexSSHWorkaroundApplies reports whether this launch exposes the ownership
// translation the private SSH config exists to avoid. Codex's managed native
// sandbox has always needed the workaround. Every harness in tclaude's packet
// sandbox needs it: both namespace-root and caller-identity mappings leave host
// root unmapped, so root-owned SSH drop-ins appear as nobody:nogroup.
func codexSSHWorkaroundApplies(
	harnessName, harnessBuiltinMode, sandboxImplementation string,
	snapshot *sandboxpolicy.Snapshot,
) bool {
	if runtime.GOOS != "linux" {
		return false
	}
	implementation, err := sandboxpolicy.NormalizeImplementation(sandboxImplementation)
	if err != nil {
		return false
	}
	if implementation == sandboxpolicy.ImplementationHarnessBuiltin {
		return harnessName == harness.CodexName &&
			harnessBuiltinMode == harness.SandboxManagedProfile
	}
	if !implementation.UsesTclaudeLayer() || snapshot == nil {
		return false
	}
	axes, err := sandboxpolicy.PlannedEffectiveAccessAxes(snapshot.Effective)
	if err != nil {
		return false
	}
	engine, err := sandboxpolicy.DeployedNetworkEngineForRules(axes.Network)
	return err == nil && engine == sandboxpolicy.NetworkEnginePacket
}

func sshWorkaroundImplementationEligible(
	harnessName, harnessBuiltinMode, sandboxImplementation string,
) bool {
	implementation, err := sandboxpolicy.NormalizeImplementation(sandboxImplementation)
	if err != nil {
		return false
	}
	if implementation == sandboxpolicy.ImplementationHarnessBuiltin {
		return harnessName == harness.CodexName &&
			harnessBuiltinMode == harness.SandboxManagedProfile
	}
	return implementation.UsesTclaudeLayer()
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
// every harness receives GIT_SSH_COMMAND through the process environment; the
// Codex adapter also pins it in shell_environment_policy.
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
		return sandboxpolicy.Snapshot{}, func() {}, fmt.Errorf("configure SSH workaround: %w", err)
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
		return sandboxpolicy.Snapshot{}, fmt.Errorf("codex SSH workaround directory binding is missing or malformed")
	}
	parent := filepath.Dir(configDir)
	if _, err := removeDirAtNoFollow(parent, filepath.Base(configDir)); err != nil {
		return sandboxpolicy.Snapshot{}, fmt.Errorf("reset SSH workaround directory: %w", err)
	}
	if err := mkdirAllNoFollow(configDir, 0o700); err != nil {
		return sandboxpolicy.Snapshot{}, fmt.Errorf("recreate SSH workaround directory: %w", err)
	}

	configPath, err := writeCodexSSHConfig(configDir)
	if err != nil {
		return sandboxpolicy.Snapshot{}, err
	}
	sshPath, err := codexSSHLookPath("ssh")
	if err != nil {
		return sandboxpolicy.Snapshot{}, fmt.Errorf("locate ssh for SSH workaround: %w", err)
	}
	sshPath, err = filepath.Abs(sshPath)
	if err != nil {
		return sandboxpolicy.Snapshot{}, fmt.Errorf("resolve ssh path for SSH workaround: %w", err)
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
	mirror := codexSSHConfigMirror{
		configDir:      configDir,
		systemBase:     filepath.Dir(codexSSHSystemConfig),
		targetBySource: map[string]string{},
		visiting:       map[string]bool{},
	}
	system, err := mirror.render(codexSSHSystemConfig)
	if errors.Is(err, os.ErrNotExist) {
		system = nil
	} else if err != nil {
		return "", err
	}
	// Include is textual and can leave the parser inside a non-matching Host or
	// Match block. Reset to an all-host context before evaluating the mirrored
	// system config, otherwise its leading drop-in Include can be skipped.
	contents := "Include " + quoteSSHConfigPath(codexSSHUserConfig) + "\nHost *\n" + string(system)
	configPath := filepath.Join(configDir, codexSSHConfigName)
	if err := writeNewPrivateFile(configPath, []byte(contents)); err != nil {
		return "", fmt.Errorf("write SSH workaround config: %w", err)
	}
	return configPath, nil
}

type codexSSHConfigMirror struct {
	configDir      string
	systemBase     string
	targetBySource map[string]string
	visiting       map[string]bool
	next           int
}

// render copies every file reached by a system-config Include into the private
// config tree and rewrites the directive to the copied file. Expanding globs to
// explicit Include lines preserves OpenSSH's lexical order while avoiding both
// ownership checks on the original and the changed relative-path base caused
// by using ssh -F with an agent-owned config.
func (m *codexSSHConfigMirror) render(source string) ([]byte, error) {
	canonical, err := filepath.EvalSymlinks(source)
	if err != nil {
		return nil, err
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return nil, err
	}
	if m.visiting[canonical] {
		return nil, fmt.Errorf("system SSH config include cycle at %q", source)
	}
	m.visiting[canonical] = true
	defer delete(m.visiting, canonical)

	data, err := os.ReadFile(canonical)
	if err != nil {
		return nil, err
	}
	var out strings.Builder
	for _, line := range strings.SplitAfter(string(data), "\n") {
		patterns, include, parseErr := parseSSHIncludePatterns(line)
		if parseErr != nil {
			return nil, fmt.Errorf("parse system SSH Include in %q: %w", source, parseErr)
		}
		if !include {
			out.WriteString(line)
			continue
		}
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		for _, pattern := range patterns {
			if strings.HasPrefix(pattern, "~") || strings.ContainsAny(pattern, "%$") {
				out.WriteString(indent + "Include " + quoteSSHConfigPath(pattern) + "\n")
				continue
			}
			if !filepath.IsAbs(pattern) {
				pattern = filepath.Join(m.systemBase, pattern)
			}
			matches, globErr := filepath.Glob(pattern)
			if globErr != nil {
				return nil, fmt.Errorf("expand system SSH Include %q: %w", pattern, globErr)
			}
			for _, match := range matches {
				info, statErr := os.Stat(match) // dereference trusted system symlinks
				if statErr != nil {
					return nil, fmt.Errorf("inspect system SSH include %q: %w", match, statErr)
				}
				if !info.Mode().IsRegular() {
					continue
				}
				target, copyErr := m.copyIncluded(match)
				if copyErr != nil {
					return nil, copyErr
				}
				out.WriteString(indent + "Include " + quoteSSHConfigPath(target) + "\n")
			}
		}
	}
	return []byte(out.String()), nil
}

func (m *codexSSHConfigMirror) copyIncluded(source string) (string, error) {
	canonical, err := filepath.EvalSymlinks(source)
	if err != nil {
		return "", err
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return "", err
	}
	if m.visiting[canonical] {
		return "", fmt.Errorf("system SSH config include cycle at %q", source)
	}
	if target := m.targetBySource[canonical]; target != "" {
		return target, nil
	}

	var target string
	dropInSource := filepath.Join(m.systemBase, codexSSHDropInDirName)
	if rel, relErr := filepath.Rel(dropInSource, source); relErr == nil &&
		rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		target = filepath.Join(m.configDir, codexSSHDropInDirName, rel)
	} else {
		m.next++
		target = filepath.Join(m.configDir, "includes",
			fmt.Sprintf("%03d-%s", m.next, filepath.Base(source)))
	}
	if err := mkdirAllNoFollow(filepath.Dir(target), 0o700); err != nil {
		return "", fmt.Errorf("create private SSH include directory: %w", err)
	}
	m.targetBySource[canonical] = target
	rendered, err := m.render(canonical)
	if err != nil {
		delete(m.targetBySource, canonical)
		return "", fmt.Errorf("copy system SSH include %q: %w", source, err)
	}
	if err := writeNewPrivateFile(target, rendered); err != nil {
		delete(m.targetBySource, canonical)
		return "", fmt.Errorf("copy system SSH include %q: %w", source, err)
	}
	return target, nil
}

func parseSSHIncludePatterns(line string) ([]string, bool, error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return nil, false, nil
	}
	keywordEnd := strings.IndexAny(line, " \t=")
	if keywordEnd < 0 || !strings.EqualFold(line[:keywordEnd], "Include") {
		return nil, false, nil
	}
	line = strings.TrimSpace(line[keywordEnd:])
	if strings.HasPrefix(line, "=") {
		line = strings.TrimSpace(line[1:])
	}
	var patterns []string
	for len(line) > 0 {
		line = strings.TrimLeft(line, " \t")
		if line == "" || line[0] == '#' {
			break
		}
		var token strings.Builder
		var quote byte
		escaped := false
		i := 0
		for ; i < len(line); i++ {
			ch := line[i]
			if escaped {
				token.WriteByte(ch)
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if quote != 0 {
				if ch == quote {
					quote = 0
				} else {
					token.WriteByte(ch)
				}
				continue
			}
			if ch == '"' {
				quote = ch
				continue
			}
			if ch == ' ' || ch == '\t' {
				break
			}
			token.WriteByte(ch)
		}
		if escaped || quote != 0 {
			return nil, true, fmt.Errorf("unterminated quoted or escaped path")
		}
		if token.Len() > 0 {
			patterns = append(patterns, token.String())
		}
		line = line[i:]
	}
	return patterns, true, nil
}

func quoteSSHConfigPath(path string) string {
	if !strings.ContainsAny(path, " \t#\"\\") {
		return path
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(path) + `"`
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
