package session

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

const CodexNativeRegistrySetupDoc = "https://github.com/tofutools/tclaude/blob/main/docs/codex-native-permission-registry.md"

const (
	CodexNativeRegistryMissingSymlink = "codex_native_registry_missing_symlink"
	CodexNativeRegistryWrongTarget    = "codex_native_registry_wrong_target"
	CodexNativeRegistryUnsafeOwner    = "codex_native_registry_unsafe_ownership"
	CodexNativeRegistryUnsafeMode     = "codex_native_registry_unsafe_mode"
	CodexNativeRegistryConflict       = "codex_native_registry_unmanaged_files"
)

const (
	registryConfigMarker       = "# Managed by tclaude: Codex native permission registry v1\n"
	registryRequirementsMarker = "# Managed by tclaude: Codex native permission requirements v1\n"
)

type CodexNativeRegistryError struct {
	Code   string
	Detail string
}

func (e *CodexNativeRegistryError) Error() string {
	return fmt.Sprintf("native Codex permission-profile integration is not ready (%s): %s; see %s",
		e.Code, e.Detail, CodexNativeRegistrySetupDoc)
}

type CodexNativeRegistryOptions struct {
	SystemDir  string
	ManagedDir string
	RootUID    uint32
	UserUID    uint32
}

func defaultCodexNativeRegistryOptions() (CodexNativeRegistryOptions, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return CodexNativeRegistryOptions{}, err
	}
	return CodexNativeRegistryOptions{
		SystemDir:  "/etc/codex",
		ManagedDir: filepath.Join(home, ".tclaude", "data", "codex-sb-cfg"),
		RootUID:    0,
		UserUID:    currentUID(),
	}, nil
}

// CodexNativeRegistryApplicable is deliberately narrower than "app-server".
// The tclaude-layer and stacked implementations already confine the executor
// themselves and must not acquire a host-level setup dependency.
func CodexNativeRegistryApplicable(appServer bool, harnessName, sandboxMode, implementation string) bool {
	impl, err := sandboxpolicy.NormalizeImplementation(implementation)
	return err == nil && appServer && harnessName == harness.CodexName &&
		sandboxMode == harness.SandboxManagedProfile && impl == sandboxpolicy.ImplementationHarnessBuiltin
}

func ValidateCodexNativeRegistrySetup() error {
	opts, err := defaultCodexNativeRegistryOptions()
	if err != nil {
		return err
	}
	return validateCodexNativeRegistrySetup(opts)
}

func validateCodexNativeRegistrySetup(opts CodexNativeRegistryOptions) error {
	link, err := os.Lstat(opts.SystemDir)
	if os.IsNotExist(err) {
		return &CodexNativeRegistryError{CodexNativeRegistryMissingSymlink, opts.SystemDir + " is missing"}
	}
	if err != nil {
		return fmt.Errorf("inspect native Codex registry symlink: %w", err)
	}
	if link.Mode()&os.ModeSymlink == 0 {
		return &CodexNativeRegistryError{CodexNativeRegistryMissingSymlink, opts.SystemDir + " is not a symlink"}
	}
	uid, ok := fileOwnerUID(link)
	if !ok || uid != opts.RootUID {
		return &CodexNativeRegistryError{CodexNativeRegistryUnsafeOwner, opts.SystemDir + " must be root-owned"}
	}
	rawTarget, err := os.Readlink(opts.SystemDir)
	if err != nil {
		return fmt.Errorf("read native Codex registry symlink: %w", err)
	}
	if !filepath.IsAbs(rawTarget) || filepath.Clean(rawTarget) != filepath.Clean(opts.ManagedDir) {
		return &CodexNativeRegistryError{CodexNativeRegistryWrongTarget,
			fmt.Sprintf("%s must point exactly to %s", opts.SystemDir, opts.ManagedDir)}
	}
	resolved, err := filepath.EvalSymlinks(opts.SystemDir)
	canonicalTarget, targetErr := filepath.EvalSymlinks(opts.ManagedDir)
	if err != nil || targetErr != nil || filepath.Clean(resolved) != filepath.Clean(canonicalTarget) {
		return &CodexNativeRegistryError{CodexNativeRegistryWrongTarget,
			fmt.Sprintf("%s does not resolve directly to %s", opts.SystemDir, opts.ManagedDir)}
	}
	if err := validateNativeRegistryDirectoryChain(opts); err != nil {
		return err
	}
	target, err := os.Lstat(opts.ManagedDir)
	if err != nil {
		return &CodexNativeRegistryError{CodexNativeRegistryWrongTarget, "managed target is missing or unreadable"}
	}
	if target.Mode()&os.ModeSymlink != 0 || !target.IsDir() {
		return &CodexNativeRegistryError{CodexNativeRegistryWrongTarget, "managed target must be a real directory"}
	}
	uid, ok = fileOwnerUID(target)
	if !ok || uid != opts.UserUID {
		return &CodexNativeRegistryError{CodexNativeRegistryUnsafeOwner, "managed target must be owned by the current user"}
	}
	if target.Mode().Perm() != 0o700 {
		return &CodexNativeRegistryError{CodexNativeRegistryUnsafeMode, "managed target mode must be 0700"}
	}
	for name, marker := range map[string]string{
		"config.toml": configMarkerForValidation(), "requirements.toml": registryRequirementsMarker,
		"registry.lock": "",
	} {
		if err := validateNativeRegistryFile(filepath.Join(opts.ManagedDir, name), opts.UserUID, marker); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(opts.ManagedDir)
	if err != nil {
		return fmt.Errorf("list managed Codex registry directory: %w", err)
	}
	for _, entry := range entries {
		switch entry.Name() {
		case "config.toml", "requirements.toml", "registry.lock":
			continue
		}
		if strings.HasPrefix(entry.Name(), ".tclaude-registry-") {
			// A concurrent atomic publisher creates this regular 0600 temporary
			// before rename while holding registry.lock. Validate its inode but do
			// not make pre-lock readiness spuriously fail on that safe transient.
			if err := validateNativeRegistryFile(filepath.Join(opts.ManagedDir, entry.Name()), opts.UserUID, ""); err != nil {
				return err
			}
			continue
		}
		return &CodexNativeRegistryError{CodexNativeRegistryConflict,
			fmt.Sprintf("unexpected unmanaged entry %s exists in the registry directory", entry.Name())}
	}
	return nil
}

func validateNativeRegistryDirectoryChain(opts CodexNativeRegistryOptions) error {
	managed := filepath.Clean(opts.ManagedDir)
	dataDir := filepath.Dir(managed)
	tclaudeDir := filepath.Dir(dataDir)
	if managed == "." || dataDir == "." || tclaudeDir == "." ||
		filepath.Base(managed) != "codex-sb-cfg" || filepath.Base(dataDir) != "data" ||
		filepath.Base(tclaudeDir) != ".tclaude" {
		return &CodexNativeRegistryError{CodexNativeRegistryWrongTarget,
			"managed target must be the exact ~/.tclaude/data/codex-sb-cfg path"}
	}
	for _, path := range []string{tclaudeDir, dataDir, managed} {
		info, err := os.Lstat(path)
		if err != nil {
			return &CodexNativeRegistryError{CodexNativeRegistryWrongTarget,
				fmt.Sprintf("managed path component %s is missing or unreadable", path)}
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return &CodexNativeRegistryError{CodexNativeRegistryWrongTarget,
				fmt.Sprintf("managed path component %s must be a real directory", path)}
		}
		owner, ok := fileOwnerUID(info)
		if !ok || owner != opts.UserUID {
			return &CodexNativeRegistryError{CodexNativeRegistryUnsafeOwner,
				fmt.Sprintf("managed path component %s must be owned by the current user", path)}
		}
		if info.Mode().Perm()&0o022 != 0 {
			return &CodexNativeRegistryError{CodexNativeRegistryUnsafeMode,
				fmt.Sprintf("managed path component %s must not be group/world writable", path)}
		}
	}
	return nil
}

func configMarkerForValidation() string { return registryConfigMarker }

func validateNativeRegistryFile(path string, uid uint32, marker string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect managed Codex registry file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return &CodexNativeRegistryError{CodexNativeRegistryConflict, filepath.Base(path) + " must be a regular file, not a symlink"}
	}
	owner, ok := fileOwnerUID(info)
	if !ok || owner != uid {
		return &CodexNativeRegistryError{CodexNativeRegistryUnsafeOwner, filepath.Base(path) + " must be owned by the current user"}
	}
	if info.Mode().Perm() != 0o600 {
		return &CodexNativeRegistryError{CodexNativeRegistryUnsafeMode, filepath.Base(path) + " mode must be 0600"}
	}
	if info.Size() > 16<<20 {
		return &CodexNativeRegistryError{CodexNativeRegistryConflict,
			filepath.Base(path) + " is unexpectedly large"}
	}
	if marker != "" {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("read managed Codex registry file: %w", readErr)
		}
		if !bytes.HasPrefix(data, []byte(marker)) {
			return &CodexNativeRegistryError{CodexNativeRegistryConflict,
				filepath.Base(path) + " exists but was not created by tclaude; refusing to adopt it"}
		}
	}
	return nil
}

func RegisterCodexNativePermissionProfile(generation, profileName, profilePath string) error {
	opts, err := defaultCodexNativeRegistryOptions()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(profilePath)
	if err != nil {
		return fmt.Errorf("read generated Codex permission profile: %w", err)
	}
	if err := validateStoredNativeProfile(profileName, string(data)); err != nil {
		return err
	}
	return registerCodexNativePermissionProfile(opts, generation, profileName, string(data))
}

func registerCodexNativePermissionProfile(
	opts CodexNativeRegistryOptions, generation, profileName, profileTOML string,
) error {
	if err := validateStoredNativeProfile(profileName, profileTOML); err != nil {
		return err
	}
	profile := db.CodexNativePermissionProfile{
		Generation: generation, ProfileName: profileName, ProfileTOML: profileTOML,
	}
	return withNativeRegistryLock(opts, func() error {
		previous, err := db.GetCodexNativePermissionProfile(generation)
		if err != nil {
			return fmt.Errorf("load prior native Codex permission profile: %w", err)
		}
		if err := db.UpsertCodexNativePermissionProfile(profile); err != nil {
			return fmt.Errorf("persist native Codex permission profile: %w", err)
		}
		if err := reconcileCodexNativePermissionRegistryLocked(opts); err != nil {
			rollbackErr := restoreNativePermissionProfile(generation, previous)
			if rollbackErr == nil {
				rollbackErr = reconcileCodexNativePermissionRegistryLocked(opts)
			}
			if rollbackErr != nil {
				return fmt.Errorf("%w; rollback native Codex permission profile: %v", err, rollbackErr)
			}
			return err
		}
		return nil
	})
}

// RestoreCodexNativePermissionProfile compensates a lifecycle transition that
// removed the profile but failed before persisting its replacement drive. The
// row deliberately remains durable when publication fails so restart
// reconciliation can converge it and native recovery stays fail-closed.
func RestoreCodexNativePermissionProfile(generation, profileName, profileTOML string) error {
	opts, err := defaultCodexNativeRegistryOptions()
	if err != nil {
		return err
	}
	return restoreCodexNativePermissionProfile(opts, generation, profileName, profileTOML)
}

func restoreCodexNativePermissionProfile(
	opts CodexNativeRegistryOptions, generation, profileName, profileTOML string,
) error {
	if err := validateStoredNativeProfile(profileName, profileTOML); err != nil {
		return err
	}
	profile := db.CodexNativePermissionProfile{
		Generation: generation, ProfileName: profileName, ProfileTOML: profileTOML,
	}
	return withNativeRegistryLock(opts, func() error {
		if err := db.UpsertCodexNativePermissionProfile(profile); err != nil {
			return fmt.Errorf("restore durable native Codex permission profile: %w", err)
		}
		return reconcileCodexNativePermissionRegistryLocked(opts)
	})
}

func UnregisterCodexNativePermissionProfile(generation string) error {
	if strings.TrimSpace(generation) == "" {
		return nil
	}
	opts, err := defaultCodexNativeRegistryOptions()
	if err != nil {
		return err
	}
	// Old app-server rows can predate the native registry. Let their explicit
	// compatibility rollback proceed on an unconfigured host. Registration
	// cannot succeed while this exact topology validation fails; on a prepared
	// host the definitive existence check remains inside the cross-process lock.
	existing, err := db.GetCodexNativePermissionProfile(generation)
	if err != nil {
		return err
	}
	if existing == nil && validateCodexNativeRegistrySetup(opts) != nil {
		return nil
	}
	return unregisterCodexNativePermissionProfile(opts, generation)
}

func unregisterCodexNativePermissionProfile(opts CodexNativeRegistryOptions, generation string) error {
	return withNativeRegistryLock(opts, func() error {
		existing, err := db.GetCodexNativePermissionProfile(generation)
		if err != nil {
			return err
		}
		if existing == nil {
			return nil
		}
		// Persist cleanup intent before touching the generated files. If a
		// publication fails, startup reconciliation will retry the removal
		// instead of resurrecting an orphaned allowlist entry from the row.
		if err := db.MarkCodexNativePermissionProfileCleanupPending(generation); err != nil {
			return err
		}
		if err := reconcileCodexNativePermissionRegistryLocked(opts); err != nil {
			return err
		}
		return nil
	})
}

func restoreNativePermissionProfile(generation string, previous *db.CodexNativePermissionProfile) error {
	if previous != nil {
		return db.UpsertCodexNativePermissionProfile(*previous)
	}
	return db.DeleteCodexNativePermissionProfile(generation)
}

func ReconcileCodexNativePermissionRegistry() error {
	opts, err := defaultCodexNativeRegistryOptions()
	if err != nil {
		return err
	}
	profiles, err := db.ListCodexNativePermissionProfiles()
	if err != nil {
		return err
	}
	if len(profiles) == 0 {
		// An unconfigured host has no native-registry obligation merely because
		// agentd started. A configured host still converges stale generated
		// entries back to the safe bundled-only baseline after a restart.
		if err := validateCodexNativeRegistrySetup(opts); err != nil {
			return nil
		}
	}
	return reconcileCodexNativePermissionRegistry(opts)
}

func reconcileCodexNativePermissionRegistry(opts CodexNativeRegistryOptions) error {
	return withNativeRegistryLock(opts, func() error {
		return reconcileCodexNativePermissionRegistryLocked(opts)
	})
}

func withNativeRegistryLock(opts CodexNativeRegistryOptions, fn func() error) error {
	if err := validateCodexNativeRegistrySetup(opts); err != nil {
		return err
	}
	lock, err := acquireNativeRegistryLock(filepath.Join(opts.ManagedDir, "registry.lock"))
	if err != nil {
		return fmt.Errorf("lock native Codex permission registry: %w", err)
	}
	defer func() { _ = lock.Close() }()
	// Revalidate after taking the cross-process lock. This closes replacement
	// races between the readiness check and the first managed write.
	if err := validateCodexNativeRegistrySetup(opts); err != nil {
		return err
	}
	return fn()
}

func reconcileCodexNativePermissionRegistryLocked(opts CodexNativeRegistryOptions) error {
	if _, err := db.PruneSupersededCodexNativePermissionProfiles(); err != nil {
		return fmt.Errorf("prune superseded native Codex permission profiles: %w", err)
	}
	profiles, err := db.ListCodexNativePermissionProfiles()
	if err != nil {
		return err
	}
	active := profiles[:0]
	for _, profile := range profiles {
		if profile.CleanupPending {
			continue
		}
		if err := validateStoredNativeProfile(profile.ProfileName, profile.ProfileTOML); err != nil {
			return fmt.Errorf("validate persisted Codex permission profile %s: %w", profile.ProfileName, err)
		}
		active = append(active, profile)
	}
	profiles = active
	configPath := filepath.Join(opts.ManagedDir, "config.toml")
	requirementsPath := filepath.Join(opts.ManagedDir, "requirements.toml")
	oldConfig, readErr := os.ReadFile(configPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		return readErr
	}
	desiredConfig := renderNativeRegistryConfig(profiles)
	unionConfig := appendMissingNativeDefinitions(string(oldConfig), profiles)
	if strings.TrimSpace(unionConfig) == "" {
		unionConfig = desiredConfig
	}
	// The only permitted intermediate widening is a defined-but-not-yet-allowed
	// profile. Never publish an allowlist entry before its complete definition.
	if err := writeNativeRegistryFile(configPath, []byte(unionConfig)); err != nil {
		return fmt.Errorf("publish Codex permission definitions: %w", err)
	}
	if err := writeNativeRegistryFile(requirementsPath, []byte(renderNativeRegistryRequirements(profiles))); err != nil {
		return fmt.Errorf("publish Codex permission allowlist: %w", err)
	}
	// Removals happen only after the exact allowlist no longer names them.
	if err := writeNativeRegistryFile(configPath, []byte(desiredConfig)); err != nil {
		return fmt.Errorf("prune Codex permission definitions: %w", err)
	}
	if err := db.DeletePendingCodexNativePermissionProfiles(); err != nil {
		return fmt.Errorf("finish native Codex permission profile cleanup: %w", err)
	}
	return nil
}

var writeNativeRegistryFile = atomicWriteNativeRegistryFile

func validateStoredNativeProfile(name, content string) error {
	name, err := harness.ValidateCodexProfileName(name)
	if err != nil || name == "" || !strings.HasPrefix(name, harness.CodexAgentProfile+"-") {
		return errors.New("native Codex registry accepts only generated tclaude-agent profile names")
	}
	var parsed struct {
		DefaultPermissions string `toml:"default_permissions"`
		Features           struct {
			NetworkProxy      *bool `toml:"network_proxy"`
			UseLegacyLandlock *bool `toml:"use_legacy_landlock"`
		} `toml:"features"`
		Permissions map[string]struct {
			Extends    string            `toml:"extends"`
			Filesystem map[string]string `toml:"filesystem"`
			Network    struct {
				Enabled     bool              `toml:"enabled"`
				UnixSockets map[string]string `toml:"unix_sockets"`
			} `toml:"network"`
		} `toml:"permissions"`
	}
	if err := toml.NewDecoder(strings.NewReader(content)).DisallowUnknownFields().Decode(&parsed); err != nil {
		return fmt.Errorf("decode generated profile TOML: %w", err)
	}
	profile, ok := parsed.Permissions[name]
	if parsed.DefaultPermissions != name || !ok || len(parsed.Permissions) != 1 {
		return fmt.Errorf("generated profile content does not define and select %s", name)
	}
	if profile.Extends != ":workspace" {
		return fmt.Errorf("generated profile %s has unexpected base %q", name, profile.Extends)
	}
	for path, access := range profile.Filesystem {
		if !filepath.IsAbs(path) || (access != "read" && access != "write" && access != "none") {
			return fmt.Errorf("generated profile %s has invalid filesystem rule %q=%q", name, path, access)
		}
	}
	for path, access := range profile.Network.UnixSockets {
		if !filepath.IsAbs(path) || access != "allow" {
			return fmt.Errorf("generated profile %s has invalid Unix-socket rule %q=%q", name, path, access)
		}
	}
	privateStateDir := tclcommon.TclaudeDataDir()
	if privateStateDir == "" || profile.Filesystem[privateStateDir] != "none" {
		return fmt.Errorf("generated profile %s does not deny tclaude private state", name)
	}
	tmuxDir, err := clcommon.TclaudeTmuxSocketDir()
	if err != nil || tmuxDir == "" || profile.Filesystem[tmuxDir] != "none" {
		return fmt.Errorf("generated profile %s does not deny the tmux control directory", name)
	}
	agentdSocket := agentipc.CanonicalSocketPath()
	if agentdSocket == "" || profile.Filesystem[agentdSocket] != "read" ||
		profile.Network.UnixSockets[agentdSocket] != "allow" {
		return fmt.Errorf("generated profile %s does not allow the canonical agentd socket", name)
	}
	if !strings.Contains(content, "[permissions."+name+"]") {
		return fmt.Errorf("generated profile %s has no canonical permission table", name)
	}
	return nil
}

func nativeDefinition(profile db.CodexNativePermissionProfile) string {
	needle := "[permissions." + profile.ProfileName + "]"
	idx := strings.Index(profile.ProfileTOML, needle)
	if idx < 0 {
		return ""
	}
	return strings.TrimSpace(profile.ProfileTOML[idx:]) + "\n"
}

func renderNativeRegistryConfig(profiles []db.CodexNativePermissionProfile) string {
	var b strings.Builder
	b.WriteString(registryConfigMarker)
	b.WriteString("# Generated atomically; do not edit. Ordinary Codex defaults to :workspace.\n\n")
	b.WriteString("default_permissions = \":workspace\"\n\n")
	for _, profile := range profiles {
		b.WriteString(nativeDefinition(profile))
		b.WriteByte('\n')
	}
	return b.String()
}

func appendMissingNativeDefinitions(old string, profiles []db.CodexNativePermissionProfile) string {
	if !strings.HasPrefix(old, registryConfigMarker) {
		return ""
	}
	var b strings.Builder
	b.WriteString(strings.TrimSpace(old))
	b.WriteString("\n\n")
	for _, profile := range profiles {
		if strings.Contains(old, "[permissions."+profile.ProfileName+"]") {
			continue
		}
		b.WriteString(nativeDefinition(profile))
		b.WriteByte('\n')
	}
	return b.String()
}

func renderNativeRegistryRequirements(profiles []db.CodexNativePermissionProfile) string {
	names := []string{":read-only", ":workspace", ":danger-full-access"}
	for _, profile := range profiles {
		names = append(names, profile.ProfileName)
	}
	sort.Strings(names[3:])
	var b strings.Builder
	b.WriteString(registryRequirementsMarker)
	b.WriteString("# Generated atomically; do not edit.\n\n")
	b.WriteString("default_permissions = \":workspace\"\n\n[allowed_permission_profiles]\n")
	for _, name := range names {
		fmt.Fprintf(&b, "%q = true\n", name)
	}
	return b.String()
}

func atomicWriteNativeRegistryFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tclaude-registry-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err == nil {
		err = dir.Sync()
		_ = dir.Close()
	}
	return err
}
