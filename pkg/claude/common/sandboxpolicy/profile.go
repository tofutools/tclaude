package sandboxpolicy

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

const (
	MaxProfileNameBytes    = 200
	MaxPathBytes           = 4096
	MaxEnvironmentName     = 128
	MaxEnvironmentValue    = 16 * 1024
	MaxEnvironmentCount    = 128
	MaxEnvironmentBytes    = 64 * 1024
	MaxAgentDirectoryCount = 128
	MaxIncludeCount        = 32
	// MaxIncludeDepth bounds the longest include-EDGE chain reachable from a
	// profile (a profile with no includes has depth 0). Registry write paths
	// and launch-time flattening enforce the same unit and bound, so a policy
	// that persists is always resolvable.
	MaxIncludeDepth = 16
	// FilesystemSpellingsVersion is the authoring-metadata format stored
	// beside the canonical filesystem authority. A nil document means a
	// legacy row whose save-time spellings were not retained.
	FilesystemSpellingsVersion = 1
	// OfflineModelTransportEnv is the one operator-authored TCLAUDE_ control
	// admitted through sandbox-profile environment validation. The
	// tclaude-layer consumes it as an explicit whole-process isolation
	// assertion; every other TCLAUDE_ name remains launch-reserved.
	OfflineModelTransportEnv = "TCLAUDE_OFFLINE_MODEL"
)

type Access string

const (
	AccessRead  Access = "read"
	AccessWrite Access = "write"
	AccessDeny  Access = "deny"
)

// FilesystemGrant is one operator-authored directory rule.
//
// Path is always a HOST path: it is the authority-bearing side, and it is what
// symlink resolution, protected-root containment and directory-ness are decided
// against. MountPath is optional and is the SANDBOX-side path the host
// directory appears at, in the style of a Kubernetes volume mount (TCL-866).
//
// Absent MountPath means "same path inside as outside", which is what every
// rule authored before TCL-866 meant and still means, so an existing profile's
// behavior is unchanged. A remapped rule is only meaningful for read/write:
// AccessDeny keeps host-path semantics because a deny hides a path rather than
// projecting one, so deny + mount_path is a validation error rather than a
// silently ignored field.
//
// MountPath is a namespace path, not a host path. It is validated
// SYNTACTICALLY only — absolute, cleaned, within MaxPathBytes, no control
// characters — because it names a location inside a mount namespace that does
// not exist yet. Resolving it against the host would ask the wrong filesystem.
type FilesystemGrant struct {
	Path      string `json:"path"`
	Access    Access `json:"access"`
	MountPath string `json:"mount_path,omitempty"`
}

// GuestPath is the path this grant occupies INSIDE the sandbox. It is the key
// every namespace-relative question must use: mount-plan ordering and shadowing,
// most-specific-wins evaluation, and what an agent will actually see. For a rule
// with no MountPath it is exactly Path, so callers that predate TCL-866 keep
// their behavior by construction.
func (grant FilesystemGrant) GuestPath() string {
	if grant.MountPath != "" {
		return grant.MountPath
	}
	return grant.Path
}

// IsRemapped reports whether the grant projects its host path onto a different
// sandbox path. Capability gates key on this: only a real mount namespace can
// enforce it, so every other enforcement surface must refuse rather than fall
// back to mounting at the host path.
func (grant FilesystemGrant) IsRemapped() bool {
	return grant.MountPath != "" && grant.MountPath != grant.Path
}

// HasRemappedGrant reports whether any rule in a set is remapped.
func HasRemappedGrant(grants []FilesystemGrant) bool {
	for _, grant := range grants {
		if grant.IsRemapped() {
			return true
		}
	}
	return false
}

// FilesystemSpellingRule associates non-authoritative operator spellings with
// one canonical, access-bearing Filesystem rule. Spellings never grant access:
// they are revalidated against ResolvedPath and only feed MountPlan aliases.
type FilesystemSpellingRule struct {
	ResolvedPath string   `json:"resolved_path"`
	Spellings    []string `json:"spellings"`
}

// FilesystemSpellings is versioned independently from the profile export
// envelope so the DB sidecar can evolve without changing filesystem_json.
// A non-nil empty Rules slice marks a profile authored through this seam.
type FilesystemSpellings struct {
	Version int                      `json:"version"`
	Rules   []FilesystemSpellingRule `json:"rules"`
}

type EnvironmentEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// NetworkAccess is the operator-authored network posture. The empty value
// inherits the harness's existing behavior. Internet and None both request
// an enforced IP-network posture; the Codex adapter uses its native network
// sandbox switch while retaining the agentd control socket.
type NetworkAccess string

const (
	NetworkAccessInherit  NetworkAccess = ""
	NetworkAccessInternet NetworkAccess = "internet"
	NetworkAccessNone     NetworkAccess = "none"
)

// Profile is the harness-neutral, operator-authored capability bundle. It is
// NetworkAccess is optional so existing profiles keep their harness's current
// network behavior. Harness launch posture belongs to spawn profiles instead.
//
// Includes composes other profiles by name, recursively: included profiles
// apply first in listed order, then the profile's own entries override any
// same-path or same-name values they supplied. Unlike Filesystem and
// Environment, Includes keeps its authored order because that order carries
// the override semantics. Flatten expands Includes; Resolve refuses profiles
// that still carry them.
type Profile struct {
	Name                    string               `json:"name"`
	Filesystem              []FilesystemGrant    `json:"filesystem,omitempty"`
	FilesystemSpellings     *FilesystemSpellings `json:"filesystem_spellings,omitempty"`
	Environment             []EnvironmentEntry   `json:"environment,omitempty"`
	AgentDirectories        []string             `json:"agent_directories,omitempty"`
	NetworkAccess           NetworkAccess        `json:"network_access,omitempty"`
	Network                 *NetworkRules        `json:"network,omitempty"`
	UnixSockets             *UnixSocketRules     `json:"unix_sockets,omitempty"`
	ResourceLimits          ResourceLimits       `json:"resource_limits,omitempty"`
	DarwinAllowMachRegister bool                 `json:"darwin_allow_mach_register,omitempty"`
	// PreLaunch is operator-authored shell run inside the sandbox before the
	// harness starts, for setup the declarative fields cannot express. Like
	// Includes it keeps its authored order, because the blocks are sequential
	// statements rather than a set of keys. See pre_launch.go.
	PreLaunch []PreLaunchBlock `json:"pre_launch,omitempty"`
	Includes  []string         `json:"includes,omitempty"`
}

var environmentNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var reservedEnvironmentNames = map[string]struct{}{
	"HOME": {}, "PATH": {}, "SHELL": {}, "TMPDIR": {}, "TMP": {}, "TEMP": {},
	"CLAUDE_CONFIG_DIR": {}, "XDG_CONFIG_HOME": {}, "TMUX": {}, "TMUX_PANE": {},
	"BASH_ENV": {}, "ENV": {},
}

var reservedEnvironmentPrefixes = []string{
	"TCLAUDE_", "CLAUDE_CODE_", "CODEX_", "LD_", "DYLD_",
}

var reservedProfileNames = map[string]struct{}{
	"export": {},
	"import": {},
}

// Normalize validates a profile and returns a canonical copy. It never mutates
// the caller's slices. Filesystem paths are fully symlink-resolved existing
// directories, duplicate paths fold with deny dominating write dominating
// read, and output is
// sorted for deterministic persistence and export. Environment duplicates
// with the same value fold; conflicting values fail rather than depending on
// input order.
func Normalize(in Profile) (Profile, error) {
	profile, _, err := normalize(in, false, false)
	return profile, err
}

// NormalizeForPersistence validates profile data without requiring every
// filesystem path to exist yet. Missing paths are retained in canonical
// lexical form and returned separately so authoring/import surfaces can warn
// the operator. Existing paths receive the same symlink, directory and
// protected-root checks as Normalize. Resolution and snapshot revalidation use
// this variant so a missing rule can survive resolution and become active on a
// later launch after the directory exists and is revalidated.
func NormalizeForPersistence(in Profile) (Profile, []string, error) {
	return normalize(in, true, false)
}

// NormalizeForAuthoring is the create/update boundary. It captures cleaned
// operator spellings before canonicalization, performs identity-confirmed
// case/NFC coalescing, and returns a non-nil versioned spelling document even
// when no alternate spellings remain.
func NormalizeForAuthoring(in Profile) (Profile, []string, error) {
	return normalize(in, true, true)
}

// NormalizeForImport is kept as the portable-transfer spelling for callers
// that want to emphasize that boundary.
func NormalizeForImport(in Profile) (Profile, []string, error) {
	return NormalizeForPersistence(in)
}

func normalize(in Profile, allowMissing, authoring bool) (Profile, []string, error) {
	name, err := normalizeName(in.Name)
	if err != nil {
		return Profile{}, nil, err
	}
	var filesystem []FilesystemGrant
	var filesystemSpellings *FilesystemSpellings
	var missing []string
	if authoring {
		filesystem, filesystemSpellings, missing, err =
			normalizeFilesystemForAuthoring(in.Filesystem, allowMissing)
		if err != nil {
			return Profile{}, nil, err
		}
	} else {
		filesystem, missing, err = normalizeFilesystem(in.Filesystem, allowMissing)
		if err != nil {
			return Profile{}, nil, err
		}
		filesystemSpellings, err = normalizeFilesystemSpellings(
			name, in.Filesystem, filesystem, in.FilesystemSpellings, allowMissing,
		)
		if err != nil {
			return Profile{}, nil, err
		}
	}
	environment, err := normalizeEnvironment(in.Environment)
	if err != nil {
		return Profile{}, nil, err
	}
	agentDirectories, err := normalizeAgentDirectories(in.AgentDirectories, environment)
	if err != nil {
		return Profile{}, nil, err
	}
	includes, err := normalizeIncludes(name, in.Includes)
	if err != nil {
		return Profile{}, nil, err
	}
	preLaunch, err := normalizePreLaunch(in.PreLaunch)
	if err != nil {
		return Profile{}, nil, err
	}
	networkAccess, err := NormalizeNetworkAccess(in.NetworkAccess)
	if err != nil {
		return Profile{}, nil, err
	}
	network, err := normalizeNetworkRules(in.Network)
	if err != nil {
		return Profile{}, nil, err
	}
	if network != nil {
		resolved, err := resolvedNetworkRules(*network)
		if err != nil {
			return Profile{}, nil, err
		}
		if err := validateLegacyNetworkAgreement(networkAccess, resolved.Mode); err != nil {
			return Profile{}, nil, err
		}
	}
	unixSockets, socketMissing, err := normalizeUnixSocketRules(in.UnixSockets, allowMissing)
	if err != nil {
		return Profile{}, nil, err
	}
	missing = append(missing, socketMissing...)
	resourceLimits, err := NormalizeResourceLimits(in.ResourceLimits)
	if err != nil {
		return Profile{}, nil, err
	}
	sort.Strings(missing)
	return Profile{
		Name: name, Filesystem: filesystem, FilesystemSpellings: filesystemSpellings,
		Environment: environment, AgentDirectories: agentDirectories, NetworkAccess: networkAccess,
		Network: network, UnixSockets: unixSockets, ResourceLimits: resourceLimits,
		DarwinAllowMachRegister: in.DarwinAllowMachRegister, PreLaunch: preLaunch,
		Includes: includes,
	}, missing, nil
}

// NormalizeNetworkAccess validates one network posture without requiring a
// complete profile. Harness adapters use it at their final rendering seam.
func NormalizeNetworkAccess(in NetworkAccess) (NetworkAccess, error) {
	switch in {
	case NetworkAccessInherit, NetworkAccessInternet, NetworkAccessNone:
		return in, nil
	default:
		return "", fmt.Errorf("network_access %q is invalid (want internet, none, or omitted to inherit)", in)
	}
}

// normalizeIncludes validates include references syntactically. Whether each
// referenced profile exists (and whether the whole graph stays acyclic) is a
// registry-level invariant checked where the registry is available: at store
// time and again by Flatten at resolution time. Order is preserved verbatim
// because later includes override earlier ones.
func normalizeIncludes(profileName string, in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > MaxIncludeCount {
		return nil, fmt.Errorf("includes has too many entries (maximum %d)", MaxIncludeCount)
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for i, include := range in {
		name, err := normalizeName(include)
		if err != nil {
			return nil, fmt.Errorf("includes[%d]: %w", i, err)
		}
		if name == profileName {
			return nil, fmt.Errorf("includes[%d]: sandbox profile %q must not include itself", i, name)
		}
		if seen[name] {
			return nil, fmt.Errorf("includes[%d]: sandbox profile %q is included more than once", i, name)
		}
		seen[name] = true
		out = append(out, name)
	}
	return out, nil
}

func normalizeAgentDirectories(in []string, environment []EnvironmentEntry) ([]string, error) {
	if len(in) > MaxAgentDirectoryCount {
		return nil, fmt.Errorf("agent_directories has too many entries (maximum %d)", MaxAgentDirectoryCount)
	}
	literal := make(map[string]struct{}, len(environment))
	for _, entry := range environment {
		literal[entry.Name] = struct{}{}
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for i, name := range in {
		if len(name) > MaxEnvironmentName || !environmentNameRE.MatchString(name) {
			return nil, fmt.Errorf("agent_directories[%d] %q is invalid (want an ASCII environment-variable name up to %d bytes)", i, name, MaxEnvironmentName)
		}
		if isReservedEnvironmentName(name) {
			return nil, fmt.Errorf("agent_directories[%d] environment variable %q is reserved", i, name)
		}
		if _, conflict := literal[name]; conflict {
			return nil, fmt.Errorf("agent_directories[%d] environment variable %q also has a literal environment value", i, name)
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	sort.Strings(out)
	if len(environment)+len(out) > MaxEnvironmentCount {
		return nil, fmt.Errorf("environment and agent_directories have too many entries combined (maximum %d)", MaxEnvironmentCount)
	}
	return out, nil
}

func normalizeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("sandbox profile name is required")
	}
	if len(name) > MaxProfileNameBytes {
		return "", fmt.Errorf("sandbox profile name is too long (maximum %d bytes)", MaxProfileNameBytes)
	}
	if strings.ContainsAny(name, `/\\`) {
		return "", fmt.Errorf("sandbox profile name must not contain slashes")
	}
	if _, reserved := reservedProfileNames[strings.ToLower(name)]; reserved {
		return "", fmt.Errorf("sandbox profile name %q is reserved for profile transfer routes", name)
	}
	if !utf8.ValidString(name) || strings.ContainsFunc(name, isControl) {
		return "", fmt.Errorf("sandbox profile name must be valid UTF-8 without control characters")
	}
	return name, nil
}

func normalizeFilesystem(in []FilesystemGrant, allowMissing bool) ([]FilesystemGrant, []string, error) {
	protected, err := protectedPaths()
	if err != nil {
		return nil, nil, err
	}
	// Folding is keyed on the GUEST path, because that is the position a rule
	// occupies inside the sandbox and therefore what "the same rule twice" means
	// there. For an unremapped rule the guest path IS the canonical host path, so
	// this is byte-identical to the pre-TCL-866 behavior. Two rules that project
	// DIFFERENT host directories onto one guest path are a collision rather than
	// a fold, and are rejected below.
	byGuest := make(map[string]FilesystemGrant, len(in))
	missingPaths := map[string]bool{}
	for i, grant := range in {
		_, path, mountPath, missing, err := normalizeFilesystemGrant(
			i, grant, allowMissing, protected,
		)
		if err != nil {
			return nil, nil, err
		}
		if missing {
			missingPaths[path] = true
		}
		candidate := FilesystemGrant{Path: path, Access: grant.Access, MountPath: mountPath}
		guest := candidate.GuestPath()
		previous, exists := byGuest[guest]
		if !exists {
			byGuest[guest] = candidate
			continue
		}
		if previous.Path != candidate.Path {
			return nil, nil, fmt.Errorf(
				"filesystem[%d]: sandbox path %q is claimed by two different host paths %q and %q",
				i, guest, previous.Path, candidate.Path,
			)
		}
		if accessRank(candidate.Access) > accessRank(previous.Access) {
			previous.Access = candidate.Access
			byGuest[guest] = previous
		}
	}
	out := make([]FilesystemGrant, 0, len(byGuest))
	for _, grant := range byGuest {
		out = append(out, grant)
	}
	sortFilesystemGrants(out)
	if err := validateMountPaths(out, protected); err != nil {
		return nil, nil, err
	}
	missing := make([]string, 0, len(missingPaths))
	for path := range missingPaths {
		missing = append(missing, path)
	}
	sort.Strings(missing)
	return out, missing, nil
}

// canonicalGuestPathForComparison resolves a sandbox path the same way
// protectedPaths() resolves its own entries, so the two are compared in one
// spelling. It is used ONLY for comparison: the authored mount_path is stored
// lexically, because it names a location inside a namespace that does not exist
// yet. A path that cannot be resolved at all is returned unchanged, which is the
// common case for a guest path with no host counterpart.
func canonicalGuestPathForComparison(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return CanonicalHostSpelling(filepath.Clean(resolved))
	}
	if resolved, err := canonicalMissingDirectory(path); err == nil {
		return resolved
	}
	return path
}

// mostSpecificHostDeny reports the deny rule that actually governs a host path,
// if any. Only same-path rules take part: a remapped rule confers its access at
// its sandbox path, not at the host path it reads from, so it cannot reopen a
// host subtree for anyone else.
func mostSpecificHostDeny(grants []FilesystemGrant, hostPath string) (string, bool) {
	best := ""
	denied := false
	for _, grant := range grants {
		if grant.IsRemapped() || !pathContainsOrEqual(grant.Path, hostPath) {
			continue
		}
		if len(grant.Path) < len(best) {
			continue
		}
		if len(grant.Path) == len(best) && !denied {
			// Equal length means equal path; normalization already folded those
			// with deny dominating, so this can only reinforce an existing deny.
			continue
		}
		best = grant.Path
		denied = grant.Access == AccessDeny
	}
	return best, denied
}

// sortFilesystemGrants is the canonical persistence order. Guest path leads
// because that is the fold key; the host path breaks ties so the order stays
// total. Unremapped sets sort exactly as they did before TCL-866.
func sortFilesystemGrants(grants []FilesystemGrant) {
	sort.Slice(grants, func(i, j int) bool {
		left, right := grants[i].GuestPath(), grants[j].GuestPath()
		if left != right {
			return left < right
		}
		return grants[i].Path < grants[j].Path
	})
}

// validateMountPaths enforces the cross-rule invariants a remapped grant
// introduces. Every check is a refusal rather than a silent drop: a dropped
// remap would leave the intended guest path empty while the host path stayed
// exposed, which is wrong in both directions.
func validateMountPaths(grants []FilesystemGrant, protected []string) error {
	if !HasRemappedGrant(grants) {
		return nil
	}
	// A guest path that lands on tclaude's own unshadowable machinery would
	// either be overridden later (making the rule a lie) or cut the agent off
	// from coordination. Refuse it where it can still be attributed to a rule.
	sockets := AgentdSocketFloor()
	for _, grant := range grants {
		if !grant.IsRemapped() {
			continue
		}
		guest := grant.GuestPath()
		// Compare BOTH the authored spelling and its canonical form. The stored
		// mount_path stays lexical — it names a namespace location, so resolving
		// it is not meaningful in general — but the protected-root wall is a
		// wall around real directories, and on macOS "/var/…" and
		// "/private/var/…" are one directory. Checking only the authored
		// spelling would let the other one walk straight through.
		for _, candidate := range []string{guest, canonicalGuestPathForComparison(guest)} {
			for _, denied := range protected {
				if GuardPathsIntersect(candidate, denied) {
					return fmt.Errorf(
						"filesystem rule for %q: sandbox path %q intersects protected directory %q",
						grant.Path, guest, denied,
					)
				}
			}
			for _, socket := range sockets {
				if GuardContainsOrEqual(candidate, socket) ||
					GuardContainsOrEqual(candidate, canonicalGuestPathForComparison(socket)) {
					return fmt.Errorf(
						"filesystem rule for %q: sandbox path %q would shadow the agentd control socket %q",
						grant.Path, guest, socket,
					)
				}
			}
		}
		// A deny is expressed against the HOST path, so a remap of a denied
		// subtree would re-expose exactly the content the deny hides, just under
		// another name. Most-specific-wins still applies: a source inside a
		// broad deny that a narrower read/write rule already reopens is not
		// denied, so only a source whose MOST SPECIFIC host-space rule is a deny
		// is refused. That is the same lattice the rest of the package uses,
		// evaluated on the host side because that is where a deny lives.
		if deny, denied := mostSpecificHostDeny(grants, grant.Path); denied {
			return fmt.Errorf(
				"filesystem rule for %q: host path is denied by rule %q, so it must not be mounted at sandbox path %q",
				grant.Path, deny, guest,
			)
		}
	}
	return nil
}

func normalizeFilesystemGrant(
	index int,
	grant FilesystemGrant,
	allowMissing bool,
	protected []string,
) (spelling, resolved, mountPath string, missing bool, err error) {
	if grant.Access != AccessRead && grant.Access != AccessWrite && grant.Access != AccessDeny {
		return "", "", "", false, fmt.Errorf(
			"filesystem[%d].access %q is invalid (want read, write, or deny)",
			index, grant.Access,
		)
	}
	spelling, err = cleanDirectoryPath(grant.Path)
	if err != nil {
		return "", "", "", false, fmt.Errorf("filesystem[%d].path: %w", index, err)
	}
	resolved, missing, err = canonicalDirectory(spelling, allowMissing)
	if err != nil {
		return "", "", "", false, fmt.Errorf("filesystem[%d].path: %w", index, err)
	}
	if grant.Access != AccessDeny {
		for _, denied := range protected {
			// Guard-biased on purpose: spelling restoration above already
			// aligns the common case, but a not-yet-created grant path has no
			// on-disk spelling to restore, so the residual folded collision has
			// to be refused here rather than admitted.
			if GuardPathsIntersect(resolved, denied) {
				return "", "", "", false, fmt.Errorf(
					"filesystem[%d].path %q intersects protected directory %q",
					index, resolved, denied,
				)
			}
		}
	}
	mountPath, err = normalizeMountPath(index, grant, resolved)
	if err != nil {
		return "", "", "", false, err
	}
	return spelling, resolved, mountPath, missing, nil
}

// normalizeMountPath validates the optional sandbox-side path. Validation is
// deliberately syntactic: the guest path names a location in a mount namespace
// that does not exist yet, so resolving it against the host would answer a
// question about the wrong filesystem. A mount path that equals the canonical
// host path is folded away, so "mount /srv/data at /srv/data" persists as the
// ordinary same-path rule it actually is.
func normalizeMountPath(index int, grant FilesystemGrant, resolved string) (string, error) {
	raw := strings.TrimSpace(grant.MountPath)
	if raw == "" {
		return "", nil
	}
	if grant.Access == AccessDeny {
		return "", fmt.Errorf(
			"filesystem[%d].mount_path is not allowed on a deny rule: a deny hides a path rather than projecting one, so it always applies to the host path %q",
			index, grant.Path,
		)
	}
	mountPath, err := cleanDirectoryPath(raw)
	if err != nil {
		return "", fmt.Errorf("filesystem[%d].mount_path: %w", index, err)
	}
	if mountPath == string(filepath.Separator) {
		return "", fmt.Errorf(
			"filesystem[%d].mount_path must not be the sandbox root %q", index, mountPath)
	}
	if mountPath == resolved {
		return "", nil
	}
	return mountPath, nil
}

type authoredFilesystemCandidate struct {
	resolved  string
	spelling  string
	mountPath string
	access    Access
	missing   bool
	info      os.FileInfo
}

type authoredFilesystemGroup struct {
	resolved  string
	mountPath string
	access    Access
	missing   bool
	info      os.FileInfo
	spellings map[string]struct{}
}

func normalizeFilesystemForAuthoring(
	in []FilesystemGrant,
	allowMissing bool,
) ([]FilesystemGrant, *FilesystemSpellings, []string, error) {
	return normalizeFilesystemForAuthoringWithIdentity(in, allowMissing, os.SameFile)
}

func normalizeFilesystemForAuthoringWithIdentity(
	in []FilesystemGrant,
	allowMissing bool,
	sameFile func(os.FileInfo, os.FileInfo) bool,
) ([]FilesystemGrant, *FilesystemSpellings, []string, error) {
	protected, err := protectedPaths()
	if err != nil {
		return nil, nil, nil, err
	}
	candidates := make([]authoredFilesystemCandidate, 0, len(in))
	for i, grant := range in {
		spelling, resolved, mountPath, missing, err := normalizeFilesystemGrant(
			i, grant, allowMissing, protected,
		)
		if err != nil {
			return nil, nil, nil, err
		}
		var info os.FileInfo
		if !missing {
			info, err = os.Stat(resolved)
			if err != nil {
				return nil, nil, nil, fmt.Errorf(
					"filesystem[%d].path: stat %q for authoring identity: %w",
					i, resolved, err,
				)
			}
		}
		candidates = append(candidates, authoredFilesystemCandidate{
			resolved: resolved, spelling: spelling, mountPath: mountPath,
			access: grant.Access, missing: missing, info: info,
		})
	}
	// The representative must not depend on request row order. Folding only
	// nominates identity candidates; os.SameFile below is the authority.
	sort.Slice(candidates, func(i, j int) bool {
		leftKey, rightKey := mountOrderKey(candidates[i].resolved), mountOrderKey(candidates[j].resolved)
		if leftKey != rightKey {
			return leftKey < rightKey
		}
		if candidates[i].resolved != candidates[j].resolved {
			return candidates[i].resolved < candidates[j].resolved
		}
		if candidates[i].mountPath != candidates[j].mountPath {
			return candidates[i].mountPath < candidates[j].mountPath
		}
		if candidates[i].spelling != candidates[j].spelling {
			return candidates[i].spelling < candidates[j].spelling
		}
		return accessRank(candidates[i].access) > accessRank(candidates[j].access)
	})

	groups := make([]authoredFilesystemGroup, 0, len(candidates))
	for _, candidate := range candidates {
		groupIndex := -1
		for i := range groups {
			group := &groups[i]
			// The mount path is part of the group identity: the same host
			// directory projected onto two guest paths is two mounts, not one
			// rule spelled twice. Identity coalescing still answers only the
			// host-side question.
			if group.mountPath != candidate.mountPath {
				continue
			}
			if group.resolved == candidate.resolved ||
				(group.info != nil && candidate.info != nil &&
					mountOrderKey(group.resolved) == mountOrderKey(candidate.resolved) &&
					sameFile(group.info, candidate.info)) {
				groupIndex = i
				break
			}
		}
		if groupIndex < 0 {
			groups = append(groups, authoredFilesystemGroup{
				resolved: candidate.resolved, mountPath: candidate.mountPath,
				access:  candidate.access,
				missing: candidate.missing, info: candidate.info,
				spellings: map[string]struct{}{},
			})
			groupIndex = len(groups) - 1
		}
		group := &groups[groupIndex]
		if accessRank(candidate.access) > accessRank(group.access) {
			group.access = candidate.access
		}
		group.missing = group.missing || candidate.missing
		if candidate.spelling != group.resolved {
			group.spellings[candidate.spelling] = struct{}{}
		}
	}

	filesystem := make([]FilesystemGrant, 0, len(groups))
	missingSet := map[string]struct{}{}
	// Spellings are a property of the HOST path, so groups that differ only by
	// mount path contribute to one rule here rather than to a duplicate row the
	// spelling validator would then reject.
	spellingsByResolved := map[string]map[string]struct{}{}
	claimedGuestPaths := map[string]string{}
	for _, group := range groups {
		grant := FilesystemGrant{
			Path: group.resolved, Access: group.access, MountPath: group.mountPath,
		}
		guest := grant.GuestPath()
		if previous, exists := claimedGuestPaths[guest]; exists && previous != grant.Path {
			return nil, nil, nil, fmt.Errorf(
				"filesystem: sandbox path %q is claimed by two different host paths %q and %q",
				guest, previous, grant.Path,
			)
		}
		claimedGuestPaths[guest] = grant.Path
		filesystem = append(filesystem, grant)
		if group.missing {
			missingSet[group.resolved] = struct{}{}
		}
		if len(group.spellings) == 0 {
			continue
		}
		merged := spellingsByResolved[group.resolved]
		if merged == nil {
			merged = map[string]struct{}{}
			spellingsByResolved[group.resolved] = merged
		}
		for spelling := range group.spellings {
			merged[spelling] = struct{}{}
		}
	}
	spellingRules := make([]FilesystemSpellingRule, 0, len(spellingsByResolved))
	for resolved, set := range spellingsByResolved {
		spellings := make([]string, 0, len(set))
		for spelling := range set {
			spellings = append(spellings, spelling)
		}
		sort.Strings(spellings)
		spellingRules = append(spellingRules, FilesystemSpellingRule{
			ResolvedPath: resolved,
			Spellings:    spellings,
		})
	}
	sortFilesystemGrants(filesystem)
	if err := validateMountPaths(filesystem, protected); err != nil {
		return nil, nil, nil, err
	}
	sort.Slice(spellingRules, func(i, j int) bool {
		return spellingRules[i].ResolvedPath < spellingRules[j].ResolvedPath
	})
	missing := make([]string, 0, len(missingSet))
	for path := range missingSet {
		missing = append(missing, path)
	}
	sort.Strings(missing)
	return filesystem, &FilesystemSpellings{
		Version: FilesystemSpellingsVersion,
		Rules:   spellingRules,
	}, missing, nil
}

func normalizeFilesystemSpellings(
	profileName string,
	authoritativeIn, normalized []FilesystemGrant,
	in *FilesystemSpellings,
	allowMissing bool,
) (*FilesystemSpellings, error) {
	if in == nil {
		return nil, nil
	}
	if in.Version != FilesystemSpellingsVersion {
		return nil, fmt.Errorf(
			"filesystem_spellings version %d is unsupported (want %d)",
			in.Version, FilesystemSpellingsVersion,
		)
	}
	original := make(map[string]struct{}, len(authoritativeIn))
	for _, grant := range authoritativeIn {
		path, err := cleanDirectoryPath(grant.Path)
		if err != nil {
			return nil, fmt.Errorf("filesystem spelling authority %q: %w", grant.Path, err)
		}
		original[path] = struct{}{}
	}
	active := make(map[string]struct{}, len(normalized))
	for _, grant := range normalized {
		active[grant.Path] = struct{}{}
	}
	byResolved := map[string]map[string]struct{}{}
	for i, rule := range in.Rules {
		resolved, err := cleanDirectoryPath(rule.ResolvedPath)
		if err != nil {
			return nil, fmt.Errorf("filesystem_spellings.rules[%d].resolved_path: %w", i, err)
		}
		if resolved != rule.ResolvedPath {
			return nil, fmt.Errorf(
				"filesystem_spellings.rules[%d].resolved_path %q is not canonical lexical form %q",
				i, rule.ResolvedPath, resolved,
			)
		}
		if _, ok := original[resolved]; !ok {
			return nil, fmt.Errorf(
				"filesystem_spellings.rules[%d].resolved_path %q has no authoritative filesystem rule",
				i, resolved,
			)
		}
		if _, ok := active[resolved]; !ok {
			if len(rule.Spellings) > 0 {
				spelling, cleanErr := cleanDirectoryPath(rule.Spellings[0])
				if cleanErr == nil {
					if driftErr := validateFilesystemSpellingTarget(
						profileName, spelling, resolved, allowMissing,
					); driftErr != nil {
						return nil, driftErr
					}
				}
			}
			return nil, fmt.Errorf(
				"sandbox profile %q authoritative filesystem target %q changed before spelling validation",
				profileName, resolved,
			)
		}
		spellings := byResolved[resolved]
		if spellings == nil {
			spellings = map[string]struct{}{}
			byResolved[resolved] = spellings
		}
		for j, raw := range rule.Spellings {
			spelling, err := cleanDirectoryPath(raw)
			if err != nil {
				return nil, fmt.Errorf(
					"filesystem_spellings.rules[%d].spellings[%d]: %w", i, j, err,
				)
			}
			if spelling == resolved {
				continue
			}
			if err := validateFilesystemSpellingTarget(
				profileName, spelling, resolved, allowMissing,
			); err != nil {
				return nil, err
			}
			spellings[spelling] = struct{}{}
		}
	}
	out := &FilesystemSpellings{
		Version: FilesystemSpellingsVersion,
		Rules:   []FilesystemSpellingRule{},
	}
	for resolved, set := range byResolved {
		if len(set) == 0 {
			continue
		}
		spellings := make([]string, 0, len(set))
		for spelling := range set {
			spellings = append(spellings, spelling)
		}
		sort.Strings(spellings)
		out.Rules = append(out.Rules, FilesystemSpellingRule{
			ResolvedPath: resolved,
			Spellings:    spellings,
		})
	}
	sort.Slice(out.Rules, func(i, j int) bool {
		return out.Rules[i].ResolvedPath < out.Rules[j].ResolvedPath
	})
	return out, nil
}

func validateFilesystemSpellingTarget(
	profileName, spelling, resolved string,
	allowMissing bool,
) error {
	current, _, err := canonicalDirectory(spelling, allowMissing)
	if err != nil {
		return fmt.Errorf(
			"sandbox profile %q retained spelling %q originally resolved to %q but its current target is unavailable (%v); re-save the profile to adopt the new target, or remove the retained spelling",
			profileName, spelling, resolved, err,
		)
	}
	return validateDiscoveredFilesystemSpellingTarget(
		profileName, spelling, resolved, current,
	)
}

func validateDiscoveredFilesystemSpellingTarget(
	profileName, spelling, resolved, current string,
) error {
	if !sameDirectoryTarget(resolved, current) {
		return fmt.Errorf(
			"sandbox profile %q retained spelling %q originally resolved to %q but now resolves to %q; re-save the profile to adopt the new target, or remove the retained spelling",
			profileName, spelling, resolved, current,
		)
	}
	return nil
}

func sameDirectoryTarget(left, right string) bool {
	if left == right {
		return true
	}
	if mountOrderKey(left) != mountOrderKey(right) {
		return false
	}
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	return leftErr == nil && rightErr == nil && os.SameFile(leftInfo, rightInfo)
}

func accessRank(access Access) int {
	switch access {
	case AccessDeny:
		return 2
	case AccessWrite:
		return 1
	default:
		return 0
	}
}

func canonicalDirectory(path string, allowMissing bool) (string, bool, error) {
	original := path
	clean, err := cleanDirectoryPath(path)
	if err != nil {
		return "", false, err
	}
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		if allowMissing && os.IsNotExist(err) {
			resolved, err = canonicalMissingDirectory(clean)
			if err == nil {
				return resolved, true, nil
			}
		}
		return "", false, fmt.Errorf("resolve symlinks for %q: %w", original, err)
	}
	// Restore the on-disk spelling so a case- or NFC-variant authoring of a
	// path lands on the same string every other spelling of it does. Without
	// this, EvalSymlinks preserves the caller's casing and the protected-root
	// comparison below — plus grant dedup and deny-dominance — see two
	// directories where a case-insensitive volume has one.
	resolved = CanonicalHostSpelling(filepath.Clean(resolved))
	info, err := os.Stat(resolved)
	if err != nil {
		return "", false, fmt.Errorf("stat %q: %w", resolved, err)
	}
	if !info.IsDir() {
		return "", false, fmt.Errorf("path %q is not a directory", resolved)
	}
	return resolved, false, nil
}

// cleanDirectoryPath applies the lexical path rules shared by canonical
// resolution and Resolve's symlink-alias discovery.
func cleanDirectoryPath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	if len(path) > MaxPathBytes {
		return "", fmt.Errorf("path is too long (maximum %d bytes)", MaxPathBytes)
	}
	if !utf8.ValidString(path) || strings.ContainsFunc(path, isControl) {
		return "", fmt.Errorf("path must be valid UTF-8 without control characters")
	}
	// A leading "~" or "~/" is a convenience alias for the daemon's own home
	// directory (the box these grants apply to). Only the bare-user form is
	// supported — "~otheruser/..." keeps its literal "~" and falls through to
	// the not-absolute error below, rather than guessing another account's home.
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expand %q: resolve home directory: %w", path, err)
		}
		if path == "~" {
			path = home
		} else {
			path = filepath.Join(home, path[len("~/"):])
		}
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path %q is not absolute", path)
	}
	return filepath.Clean(path), nil
}

// canonicalMissingDirectory resolves the longest existing ancestor so an
// imported missing path cannot disguise an existing symlink into a protected
// tree. The unresolved suffix is then appended lexically.
func canonicalMissingDirectory(path string) (string, error) {
	ancestor := path
	suffix := []string{}
	for {
		_, err := os.Lstat(ancestor)
		if err == nil {
			resolved, err := filepath.EvalSymlinks(ancestor)
			if err != nil {
				return "", err
			}
			info, err := os.Stat(resolved)
			if err != nil {
				return "", err
			}
			if !info.IsDir() {
				return "", fmt.Errorf("existing ancestor %q is not a directory", ancestor)
			}
			// Only the existing ancestor can be spelling-restored; the
			// missing suffix has no on-disk name to read yet and is re-attached
			// as authored. Guard comparisons cover that residue.
			resolved = CanonicalHostSpelling(resolved)
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", err
		}
		suffix = append(suffix, filepath.Base(ancestor))
		ancestor = parent
	}
}

// ProtectedPaths returns the canonical tclaude/harness state directories that
// no filesystem rule may ever reach. The invariant is absolute: there is no
// profile, include, launch contract, acknowledgement, or flag that reopens
// them (TCL-791 removed the one former exception). An operator who must work
// without the wall disables the sandbox instead. Adapters and CLI/API surfaces
// use this to explain exactly which protected root a rejected rule touched.
func ProtectedPaths() ([]string, error) { return protectedPaths() }

func protectedPaths() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory for protected sandbox paths: %w", err)
	}
	paths := []string{
		tclcommon.TclaudeDataDir(),
		filepath.Join(home, ".claude", "sessions"),
	}
	for i, path := range paths {
		path = filepath.Clean(path)
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			path = filepath.Clean(resolved)
		} else if os.IsNotExist(err) {
			if resolved, missingErr := canonicalMissingDirectory(path); missingErr == nil {
				path = resolved
			}
		}
		// The wall and the paths tested against it must be spelled the same
		// way, or a case-insensitive volume lets a variant spelling walk
		// through it. canonicalDirectory restores the grant side; this restores
		// the protected side.
		paths[i] = CanonicalHostSpelling(path)
	}
	return paths, nil
}

func pathsIntersect(a, b string) bool {
	return pathContainsOrEqual(a, b) || pathContainsOrEqual(b, a)
}

// PathContainsOrEqual reports whether target is dir or lies beneath it, by
// path segment rather than string prefix. Exported for the lineage and resume
// boundaries, which need the same containment rule this package enforces.
func PathContainsOrEqual(dir, target string) bool { return pathContainsOrEqual(dir, target) }

func pathContainsOrEqual(dir, target string) bool {
	rel, err := filepath.Rel(dir, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func normalizeEnvironment(in []EnvironmentEntry) ([]EnvironmentEntry, error) {
	if len(in) > MaxEnvironmentCount {
		return nil, fmt.Errorf("environment has too many entries (maximum %d)", MaxEnvironmentCount)
	}
	byName := make(map[string]string, len(in))
	total := 0
	for i, entry := range in {
		if len(entry.Name) > MaxEnvironmentName || !environmentNameRE.MatchString(entry.Name) {
			return nil, fmt.Errorf("environment[%d].name %q is invalid (want an ASCII identifier up to %d bytes)", i, entry.Name, MaxEnvironmentName)
		}
		if isReservedEnvironmentName(entry.Name) {
			return nil, fmt.Errorf("environment[%d].name %q is reserved for launch or sandbox control", i, entry.Name)
		}
		if len(entry.Value) > MaxEnvironmentValue {
			return nil, fmt.Errorf("environment[%d].value is too long (maximum %d bytes)", i, MaxEnvironmentValue)
		}
		if !utf8.ValidString(entry.Value) || strings.ContainsRune(entry.Value, '\x00') {
			return nil, fmt.Errorf("environment[%d].value must be valid UTF-8 without NUL bytes", i)
		}
		total += len(entry.Name) + len(entry.Value)
		if total > MaxEnvironmentBytes {
			return nil, fmt.Errorf("environment is too large (maximum %d bytes)", MaxEnvironmentBytes)
		}
		if previous, exists := byName[entry.Name]; exists && previous != entry.Value {
			return nil, fmt.Errorf("environment variable %q appears more than once with conflicting values", entry.Name)
		}
		byName[entry.Name] = entry.Value
	}
	out := make([]EnvironmentEntry, 0, len(byName))
	for name, value := range byName {
		out = append(out, EnvironmentEntry{Name: name, Value: value})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func isReservedEnvironmentName(name string) bool {
	if name == OfflineModelTransportEnv {
		return false
	}
	if _, ok := reservedEnvironmentNames[name]; ok {
		return true
	}
	for _, prefix := range reservedEnvironmentPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }
