package session

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

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
	codexNativeAdoptionGrace          = time.Minute
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
	runtime, err := db.GetCodexAppServerRuntime(generation)
	if err != nil {
		return err
	}
	profile := db.CodexNativePermissionProfile{Generation: generation, ProfileName: profileName}
	if runtime != nil {
		profile.OwnerAgentID, profile.OwnerConvID, profile.LaunchID = runtime.AgentID, runtime.ConvID, runtime.LaunchID
	}
	opts, err := defaultCodexNativeRegistryOptions()
	if err != nil {
		return err
	}
	return registerCodexNativePermissionProfileFromPath(opts, profile, profilePath, true)
}

// RegisterCodexNativePermissionProfileIfInstalled opportunistically joins an
// ordinary generated Codex launch to an already-valid global registry. Missing
// or invalid setup is a clean skip; only app-server+builtin callers use the
// mandatory registration function above.
func RegisterCodexNativePermissionProfileIfInstalled(profile db.CodexNativePermissionProfile, profilePath string) (bool, error) {
	opts, err := defaultCodexNativeRegistryOptions()
	if err != nil {
		return false, nil
	}
	return registerCodexNativePermissionProfileIfInstalled(opts, profile, profilePath)
}

// CodexNativePermissionProfileRegistration is one generated definition and
// its durable lifecycle identity, prepared for an atomic registry join.
type CodexNativePermissionProfileRegistration struct {
	Profile     db.CodexNativePermissionProfile
	ProfilePath string
}

var codexNativeRegistryBeforeOrdinaryPublish func() error

// SetCodexNativeRegistryBeforeOrdinaryPublish installs the process-level
// adopter used before an ordinary generated profile becomes the next global
// publisher. agentd supplies the live-pane proof implementation; the hook is
// deliberately absent in the lower-level session package itself to avoid an
// import cycle.
func SetCodexNativeRegistryBeforeOrdinaryPublish(fn func() error) {
	codexNativeRegistryBeforeOrdinaryPublish = fn
}

// RegisterCodexNativePermissionProfilesIfInstalled publishes an ordinary
// launch set as one registry generation. First-activation adoption uses the
// batch form so requirements.toml never observes only a prefix of the live
// profiles that were proved before publication.
func RegisterCodexNativePermissionProfilesIfInstalled(registrations []CodexNativePermissionProfileRegistration) (bool, error) {
	opts, err := defaultCodexNativeRegistryOptions()
	if err != nil {
		return false, nil
	}
	return registerCodexNativePermissionProfilesIfInstalled(opts, registrations)
}

func registerCodexNativePermissionProfileIfInstalled(
	opts CodexNativeRegistryOptions, profile db.CodexNativePermissionProfile, profilePath string,
) (bool, error) {
	if validateCodexNativeRegistrySetup(opts) != nil {
		return false, nil
	}
	if codexNativeRegistryBeforeOrdinaryPublish != nil {
		if err := codexNativeRegistryBeforeOrdinaryPublish(); err != nil {
			return false, fmt.Errorf("adopt live generated Codex profiles before registry publication: %w", err)
		}
	}
	return registerCodexNativePermissionProfilesIfInstalled(opts, []CodexNativePermissionProfileRegistration{{
		Profile: profile, ProfilePath: profilePath,
	}})
}

func registerCodexNativePermissionProfilesIfInstalled(
	opts CodexNativeRegistryOptions, registrations []CodexNativePermissionProfileRegistration,
) (bool, error) {
	if validateCodexNativeRegistrySetup(opts) != nil {
		return false, nil
	}
	profiles := make([]db.CodexNativePermissionProfile, 0, len(registrations))
	for _, registration := range registrations {
		data, err := os.ReadFile(registration.ProfilePath)
		if err != nil {
			return false, fmt.Errorf("read generated Codex permission profile: %w", err)
		}
		profile := registration.Profile
		if err := validateStoredNativeProfile(profile.ProfileName, string(data)); err != nil {
			return false, err
		}
		profile.ProfileTOML = string(data)
		profiles = append(profiles, profile)
	}
	if len(profiles) == 0 {
		return true, nil
	}
	return true, withNativeRegistryLock(opts, func() error {
		previous := make([]*db.CodexNativePermissionProfile, len(profiles))
		rollback := func(last int) error {
			var rollbackErr error
			for i := last; i >= 0; i-- {
				rollbackErr = errors.Join(rollbackErr,
					restoreNativePermissionProfile(profiles[i].Generation, previous[i]))
			}
			if rollbackErr == nil {
				rollbackErr = reconcileCodexNativePermissionRegistryLocked(opts)
			}
			return rollbackErr
		}
		for i, profile := range profiles {
			stored, err := db.GetCodexNativePermissionProfile(profile.Generation)
			if err != nil {
				return errors.Join(fmt.Errorf("load prior native Codex permission profile: %w", err),
					rollback(i-1))
			}
			previous[i] = stored
			if err := db.UpsertCodexNativePermissionProfile(profile); err != nil {
				return errors.Join(fmt.Errorf("persist native Codex permission profile: %w", err),
					rollback(i-1))
			}
		}
		if err := reconcileCodexNativePermissionRegistryLocked(opts); err != nil {
			return errors.Join(err, rollback(len(profiles)-1))
		}
		return nil
	})
}

// ActivateCodexNativePermissionProfile publishes a successfully-started
// ordinary launch and prunes its ready-superseded predecessors in the same
// registry critical section. App-server generations use their stronger
// verified-thread readiness path instead.
func ActivateCodexNativePermissionProfile(generation string) error {
	opts, err := defaultCodexNativeRegistryOptions()
	if err != nil {
		return err
	}
	return withNativeRegistryLock(opts, func() error {
		marked, err := db.MarkCodexNativePermissionProfileLaunchReady(generation)
		if err != nil {
			return err
		}
		if !marked {
			return fmt.Errorf("activate generated Codex permission profile %s: durable registry row is missing", generation)
		}
		return reconcileCodexNativePermissionRegistryLocked(opts)
	})
}

func registerCodexNativePermissionProfileFromPath(
	opts CodexNativeRegistryOptions, profile db.CodexNativePermissionProfile, profilePath string, required bool,
) error {
	if !required && validateCodexNativeRegistrySetup(opts) != nil {
		return nil
	}
	data, err := os.ReadFile(profilePath)
	if err != nil {
		return fmt.Errorf("read generated Codex permission profile: %w", err)
	}
	if err := validateStoredNativeProfile(profile.ProfileName, string(data)); err != nil {
		return err
	}
	profile.ProfileTOML = string(data)
	return registerCodexNativePermissionProfileOwned(opts, profile)
}

func registerCodexNativePermissionProfile(
	opts CodexNativeRegistryOptions, generation, profileName, profileTOML string,
) error {
	if err := validateStoredNativeProfile(profileName, profileTOML); err != nil {
		return err
	}
	return registerCodexNativePermissionProfileOwned(opts, db.CodexNativePermissionProfile{
		Generation: generation, ProfileName: profileName, ProfileTOML: profileTOML,
	})
}

func registerCodexNativePermissionProfileOwned(
	opts CodexNativeRegistryOptions, profile db.CodexNativePermissionProfile,
) error {
	if err := validateStoredNativeProfile(profile.ProfileName, profile.ProfileTOML); err != nil {
		return err
	}
	return withNativeRegistryLock(opts, func() error {
		previous, err := db.GetCodexNativePermissionProfile(profile.Generation)
		if err != nil {
			return fmt.Errorf("load prior native Codex permission profile: %w", err)
		}
		if err := db.UpsertCodexNativePermissionProfile(profile); err != nil {
			return fmt.Errorf("persist native Codex permission profile: %w", err)
		}
		if err := reconcileCodexNativePermissionRegistryLocked(opts); err != nil {
			rollbackErr := restoreNativePermissionProfile(profile.Generation, previous)
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
	if runtime, runtimeErr := db.GetCodexAppServerRuntime(generation); runtimeErr != nil {
		return runtimeErr
	} else if runtime != nil {
		profile.OwnerAgentID, profile.OwnerConvID, profile.LaunchID = runtime.AgentID, runtime.ConvID, runtime.LaunchID
		profile.LaunchReady = runtime.State == db.CodexAppServerReady
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
	return CleanupCodexNativePermissionProfiles([]string{generation})
}

// CleanupCodexNativePermissionProfiles removes an exact set of tclaude-owned
// generations. On a configured host, intent and publication are serialized by
// the cross-process registry lock. If the host setup is temporarily missing or
// broken, cleanup_pending is still committed first so a later reconciliation
// retries instead of losing the lifecycle decision.
func CleanupCodexNativePermissionProfiles(generations []string) error {
	if len(generations) == 0 {
		return nil
	}
	opts, err := defaultCodexNativeRegistryOptions()
	if err != nil {
		if markErr := db.MarkCodexNativePermissionProfilesCleanupPending(generations); markErr != nil {
			return errors.Join(err, markErr)
		}
		return err
	}
	if err := validateCodexNativeRegistrySetup(opts); err != nil {
		if markErr := db.MarkCodexNativePermissionProfilesCleanupPending(generations); markErr != nil {
			return errors.Join(err, markErr)
		}
		return err
	}
	return withNativeRegistryLock(opts, func() error {
		if err := db.MarkCodexNativePermissionProfilesCleanupPending(generations); err != nil {
			return err
		}
		return reconcileCodexNativePermissionRegistryLocked(opts)
	})
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
	return withNativeRegistryLock(opts, func() error {
		if err := markOrphanedCodexNativePermissionProfiles(); err != nil {
			return fmt.Errorf("classify orphaned native Codex permission profiles: %w", err)
		}
		return reconcileCodexNativePermissionRegistryLocked(opts)
	})
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

// markOrphanedCodexNativePermissionProfiles is the conservative startup sweep.
// Live-claiming generations are never touched here. A terminal generation is
// retained only when durable conversation/launch posture proves an active
// actor can resume the same native Codex drive. Superseded rows are pruned by
// the preceding ready-successor query; a merely warming/failed successor must
// not evict its predecessor definition.
func markOrphanedCodexNativePermissionProfiles() error {
	profiles, err := db.ListCodexNativePermissionProfiles()
	if err != nil {
		return err
	}
	runtimes, err := db.ListCodexAppServerRuntimes()
	if err != nil {
		return err
	}
	byGeneration := make(map[string]db.CodexAppServerRuntime, len(runtimes))
	for _, runtime := range runtimes {
		byGeneration[runtime.Generation] = runtime
	}
	var cleanup []string
	for _, profile := range profiles {
		if profile.CleanupPending {
			continue
		}
		runtime, ok := byGeneration[profile.Generation]
		if !ok {
			if codexOrdinaryProfileStillLive(profile) ||
				(profile.OwnerConvID != "" && codexGeneratedProfileOwnerResumable(profile.OwnerConvID)) {
				continue
			}
			cleanup = append(cleanup, profile.Generation)
			continue
		}
		switch runtime.State {
		case db.CodexAppServerWarming, db.CodexAppServerRecovering, db.CodexAppServerReady:
			continue
		case db.CodexAppServerDead, db.CodexAppServerUnavailable:
			if codexNativeRuntimeResumable(runtime) {
				continue
			}
			cleanup = append(cleanup, profile.Generation)
		default:
			// Unknown future states fail closed: do not remove an enforcement
			// profile until this binary understands their liveness contract.
			continue
		}
	}
	return db.MarkCodexNativePermissionProfilesCleanupPending(cleanup)
}

func codexOrdinaryProfileStillLive(profile db.CodexNativePermissionProfile) bool {
	if profile.LaunchID == "" {
		return false
	}
	row, err := db.LoadSession(profile.LaunchID)
	if profile.LaunchReady && err == nil && row != nil {
		return row.Status != StatusExited
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		// A read failure cannot prove the owner is gone. Preserve enforcement
		// and let a later reconciliation retry the liveness check.
		return true
	}
	// Registration precedes pane launch and readiness. First activation can also
	// prove a pane before session-new commits its row. Give both narrow handoffs
	// one launch-readiness window; old rowless/not-ready generations remain
	// definitive startup orphans.
	return !profile.CreatedAt.IsZero() && time.Since(profile.CreatedAt) < codexNativeAdoptionGrace
}

func codexNativeRuntimeResumable(runtime db.CodexAppServerRuntime) bool {
	if !codexGeneratedProfileOwnerResumable(runtime.ConvID) {
		return false
	}
	resume, err := db.ConversationResumeProfileForConv(runtime.ConvID)
	if err != nil || resume == nil {
		return false
	}
	posture, err := db.RecordedLaunchPostureForConv(runtime.ConvID)
	if err != nil || posture == nil || posture.CodexAppServer == nil || !*posture.CodexAppServer ||
		posture.HarnessBuiltinMode == nil || posture.SandboxImplementation == nil {
		return false
	}
	return CodexNativeRegistryApplicable(true, resume.Harness, *posture.HarnessBuiltinMode,
		*posture.SandboxImplementation)
}

func codexGeneratedProfileOwnerResumable(convID string) bool {
	resume, err := db.ConversationResumeProfileForConv(convID)
	if err != nil || resume == nil || resume.Harness != harness.CodexName || strings.TrimSpace(resume.Cwd) == "" {
		return false
	}
	if actor, err := db.GetAgentByConv(convID); err != nil || (actor != nil && !actor.Active()) {
		return false
	}
	posture, err := db.RecordedLaunchPostureForConv(convID)
	if err != nil || posture == nil || posture.HarnessBuiltinMode == nil {
		return false
	}
	return *posture.HarnessBuiltinMode == harness.SandboxManagedProfile
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
	if agentdDir := agentipc.CanonicalSocketDir(); agentdDir == "" || profile.Filesystem[agentdDir] != "read" {
		return fmt.Errorf("generated profile %s does not allow the canonical agentd socket directory", name)
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
