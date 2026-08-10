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
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
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
	if err != nil || filepath.Clean(resolved) != filepath.Clean(opts.ManagedDir) {
		return &CodexNativeRegistryError{CodexNativeRegistryWrongTarget,
			fmt.Sprintf("%s does not resolve directly to %s", opts.SystemDir, opts.ManagedDir)}
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
	if info.Mode().Perm()&0o022 != 0 {
		return &CodexNativeRegistryError{CodexNativeRegistryUnsafeMode, filepath.Base(path) + " must not be group/world writable"}
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
	if err := db.UpsertCodexNativePermissionProfile(db.CodexNativePermissionProfile{
		Generation: generation, ProfileName: profileName, ProfileTOML: string(data),
	}); err != nil {
		return fmt.Errorf("persist native Codex permission profile: %w", err)
	}
	if err := reconcileCodexNativePermissionRegistry(opts); err != nil {
		_ = db.DeleteCodexNativePermissionProfile(generation)
		return err
	}
	return nil
}

func UnregisterCodexNativePermissionProfile(generation string) error {
	if strings.TrimSpace(generation) == "" {
		return nil
	}
	opts, err := defaultCodexNativeRegistryOptions()
	if err != nil {
		return err
	}
	if err := db.DeleteCodexNativePermissionProfile(generation); err != nil {
		return err
	}
	return reconcileCodexNativePermissionRegistry(opts)
}

func ReconcileCodexNativePermissionRegistry() error {
	opts, err := defaultCodexNativeRegistryOptions()
	if err != nil {
		return err
	}
	profiles, err := db.ListCodexNativePermissionProfiles()
	if err != nil || len(profiles) == 0 {
		return err
	}
	return reconcileCodexNativePermissionRegistry(opts)
}

func reconcileCodexNativePermissionRegistry(opts CodexNativeRegistryOptions) error {
	if err := validateCodexNativeRegistrySetup(opts); err != nil {
		return err
	}
	lock, err := acquireNativeRegistryLock(filepath.Join(opts.ManagedDir, "registry.lock"))
	if err != nil {
		return fmt.Errorf("lock native Codex permission registry: %w", err)
	}
	defer lock.Close()
	// Revalidate after taking the cross-process lock. This closes replacement
	// races between the readiness check and the first managed write.
	if err := validateCodexNativeRegistrySetup(opts); err != nil {
		return err
	}
	profiles, err := db.ListCodexNativePermissionProfiles()
	if err != nil {
		return err
	}
	for _, profile := range profiles {
		if err := validateStoredNativeProfile(profile.ProfileName, profile.ProfileTOML); err != nil {
			return fmt.Errorf("validate persisted Codex permission profile %s: %w", profile.ProfileName, err)
		}
	}
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
	if err := atomicWriteNativeRegistryFile(configPath, []byte(unionConfig)); err != nil {
		return fmt.Errorf("publish Codex permission definitions: %w", err)
	}
	if err := atomicWriteNativeRegistryFile(requirementsPath, []byte(renderNativeRegistryRequirements(profiles))); err != nil {
		return fmt.Errorf("publish Codex permission allowlist: %w", err)
	}
	// Removals happen only after the exact allowlist no longer names them.
	if err := atomicWriteNativeRegistryFile(configPath, []byte(desiredConfig)); err != nil {
		return fmt.Errorf("prune Codex permission definitions: %w", err)
	}
	return nil
}

func validateStoredNativeProfile(name, content string) error {
	name, err := harness.ValidateCodexProfileName(name)
	if err != nil || name == "" || !strings.HasPrefix(name, harness.CodexAgentProfile+"-") {
		return errors.New("native Codex registry accepts only generated tclaude-agent profile names")
	}
	var parsed struct {
		DefaultPermissions string         `toml:"default_permissions"`
		Permissions        map[string]any `toml:"permissions"`
	}
	if err := toml.Unmarshal([]byte(content), &parsed); err != nil {
		return fmt.Errorf("decode generated profile TOML: %w", err)
	}
	if parsed.DefaultPermissions != name || parsed.Permissions[name] == nil {
		return fmt.Errorf("generated profile content does not define and select %s", name)
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
	defer os.Remove(tmpName)
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
