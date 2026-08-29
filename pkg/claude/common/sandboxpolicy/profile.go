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

// FilesystemGrant is one operator-authored filesystem rule. Path names a
// directory or a single regular file; see canonicalGrantTarget for why those
// two and nothing else.
//
// Path is always a HOST path: it is the authority-bearing side, and it is what
// symlink resolution, protected-root containment and path kind are decided
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
	// Kind is the COMMITMENT that this rule's host path was a regular file when
	// the rule was authored. See GrantKind.
	Kind GrantKind `json:"kind,omitempty"`
}

// GrantKind records what a rule's host path resolved to at authoring time, so
// the authority the operator granted cannot quietly change shape underneath
// them.
//
// The distinction is security-relevant in one direction only. A rule authored
// against a FILE grants exactly one path; if that pathname is later replaced by
// a DIRECTORY, re-resolving it at launch would hand the agent the whole
// replacement tree — a strictly wider authority than the one authored, and one
// the path alone cannot detect, because the path did not change. So the kind
// travels with the rule and is re-checked wherever the path is.
//
// The reverse — a directory rule whose path became a file — is a NARROWING and
// is left alone. It cannot grant more than was authored, and refusing it would
// break the ordinary case of a rule built from a bare path list, which carries
// no commitment at all (GrantsFromDirs; the launch contract's own file binds,
// such as the harness-config floor, arrive that way).
//
// Only the file value is ever stamped. GrantKindUnset therefore means "a
// directory, or a path whose kind was never resolved", which is exactly what
// every rule authored before file rules existed meant — so no stored profile or
// snapshot changes shape, and no migration is needed.
type GrantKind string

const (
	GrantKindUnset GrantKind = ""
	GrantKindFile  GrantKind = "file"
)

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

// FilesystemRootMode is the operator-authored filesystem namespace posture.
// The zero value preserves the historical automatic behavior: network and
// Unix-socket restrictions may still require a constructed root. Inherit is
// an explicit preference for the host root, but it cannot weaken either an
// included/scoped Separate request or another axis that requires construction.
type FilesystemRootMode string

const (
	FilesystemRootAutomatic FilesystemRootMode = ""
	FilesystemRootInherit   FilesystemRootMode = "inherit"
	FilesystemRootSeparate  FilesystemRootMode = "separate"
)

// HarnessConfigAccess is the operator-authored posture for the harness's OWN
// configuration surface — the settings file, hook/skill/agent/command
// directories, and memory file that live inside the harness state root the
// launch contract binds writable.
//
// The zero value is not "no opinion": it means the default read-only floor
// applies. That surface is the harness's policy and persistent-code-execution
// authority, and a confined agent writing it escalates OUT of the sandbox —
// into tclaude's own hardening block, into the next harness-builtin launch,
// and into the human's next unsandboxed harness run. Explicit Write is the
// operator's opt-out; explicit Read pins the floor so a later scope cannot
// opt out beneath a decision already made.
type HarnessConfigAccess string

const (
	// HarnessConfigAccessDefault applies the read-only floor. It is also what
	// every profile authored before this axis existed resolves to, which is the
	// deliberate behavior change: the floor is on unless asked otherwise.
	HarnessConfigAccessDefault HarnessConfigAccess = ""
	// HarnessConfigAccessRead pins the floor explicitly. Composition treats it
	// as strictest, so it cannot be widened by another scope or include.
	HarnessConfigAccessRead HarnessConfigAccess = "read"
	// HarnessConfigAccessWrite is the operator opt-out: the harness config
	// surface stays writable, exactly as it was before the floor existed.
	HarnessConfigAccessWrite HarnessConfigAccess = "write"
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
	FilesystemRoot          FilesystemRootMode   `json:"filesystem_root,omitempty"`
	HarnessConfig           HarnessConfigAccess  `json:"harness_config,omitempty"`
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
	filesystemRoot, err := NormalizeFilesystemRootMode(in.FilesystemRoot)
	if err != nil {
		return Profile{}, nil, err
	}
	harnessConfig, err := NormalizeHarnessConfigAccess(in.HarnessConfig)
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
		Environment: environment, AgentDirectories: agentDirectories, FilesystemRoot: filesystemRoot,
		HarnessConfig: harnessConfig, NetworkAccess: networkAccess,
		Network: network, UnixSockets: unixSockets, ResourceLimits: resourceLimits,
		DarwinAllowMachRegister: in.DarwinAllowMachRegister, PreLaunch: preLaunch,
		Includes: includes,
	}, missing, nil
}

// NormalizeFilesystemRootMode validates one authored root posture.
func NormalizeFilesystemRootMode(in FilesystemRootMode) (FilesystemRootMode, error) {
	switch in {
	case FilesystemRootAutomatic, FilesystemRootInherit, FilesystemRootSeparate:
		return in, nil
	default:
		return "", fmt.Errorf("filesystem_root %q is invalid (want inherit, separate, or omitted for automatic)", in)
	}
}

// NormalizeHarnessConfigAccess validates one authored harness-config posture.
func NormalizeHarnessConfigAccess(in HarnessConfigAccess) (HarnessConfigAccess, error) {
	switch in {
	case HarnessConfigAccessDefault, HarnessConfigAccessRead, HarnessConfigAccessWrite:
		return in, nil
	default:
		return "", fmt.Errorf(
			"harness_config %q is invalid (want read, write, or omitted for the default read-only floor)", in)
	}
}

// HarnessConfigAccessRank orders the axis for composition: strictest wins, and
// the omitted default sits BELOW an explicit write so an operator opt-out is
// reachable at all. An explicit read then outranks that opt-out, which is what
// makes the floor pinnable from a global or group scope.
func HarnessConfigAccessRank(mode HarnessConfigAccess) int {
	switch mode {
	case HarnessConfigAccessRead:
		return 2
	case HarnessConfigAccessWrite:
		return 1
	default:
		return 0
	}
}

// HarnessConfigFloorApplies reports whether a composed posture keeps the
// read-only floor. Consumers must ask this rather than comparing to Read: the
// omitted default keeps the floor too, and that is the whole point.
func HarnessConfigFloorApplies(mode HarnessConfigAccess) bool {
	return mode != HarnessConfigAccessWrite
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
		_, path, mountPath, kind, missing, err := normalizeFilesystemGrant(
			i, grant, allowMissing, protected,
		)
		if err != nil {
			return nil, nil, err
		}
		if missing {
			missingPaths[path] = true
		}
		candidate := FilesystemGrant{
			Path: path, Access: grant.Access, MountPath: mountPath, Kind: kind,
		}
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
		}
		previous.Kind = strictestGrantKind(previous.Kind, candidate.Kind)
		byGuest[guest] = previous
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
) (spelling, resolved, mountPath string, kind GrantKind, missing bool, err error) {
	if grant.Access != AccessRead && grant.Access != AccessWrite && grant.Access != AccessDeny {
		return "", "", "", "", false, fmt.Errorf(
			"filesystem[%d].access %q is invalid (want read, write, or deny)",
			index, grant.Access,
		)
	}
	switch grant.Kind {
	case GrantKindUnset, GrantKindFile:
	default:
		return "", "", "", "", false, fmt.Errorf(
			"filesystem[%d].kind %q is invalid (want %q or omitted)",
			index, grant.Kind, GrantKindFile,
		)
	}
	spelling, err = cleanDirectoryPath(grant.Path)
	if err != nil {
		return "", "", "", "", false, fmt.Errorf("filesystem[%d].path: %w", index, err)
	}
	resolved, missing, file, err := canonicalGrantTarget(spelling, allowMissing)
	if err != nil {
		return "", "", "", "", false, fmt.Errorf("filesystem[%d].path: %w", index, err)
	}
	// The widening this commitment exists to stop: a rule authored against one
	// file whose pathname now holds a directory. Re-resolving alone cannot see
	// it — the path is unchanged and the new target is perfectly valid — so the
	// rule would silently become authority over an entire tree the operator
	// never granted. Checked only when the path EXISTS: a missing path is
	// retained-with-warning and skipped at launch, which is the same inert
	// outcome for either kind.
	if grant.Kind == GrantKindFile && !missing && !file {
		return "", "", "", "", false, fmt.Errorf(
			"filesystem[%d].path %q was a regular file when this rule was authored and is now a directory; the rule grants one file, not the tree that replaced it — re-author the rule if the new shape is intended",
			index, resolved,
		)
	}
	// The commitment CARRIES rather than being re-derived. Deriving it from the
	// live filesystem alone would erase it exactly when it is load-bearing: a
	// missing path resolves as neither file nor directory, so a stored file rule
	// whose path is momentarily absent would come back unstamped, and the next
	// launch — after that pathname reappears as a directory — would have nothing
	// left to check. A path that IS a file additionally stamps an unstamped rule,
	// which is the narrowing arm GrantKind documents.
	kind = grant.Kind
	if file {
		kind = GrantKindFile
	}
	// A deny is the one access a file row cannot carry. Hiding a directory is a
	// mount over it — an empty tmpfs — and there is no equivalent primitive that
	// makes a single FILE absent: every candidate substitutes content (an empty
	// file, /dev/null) rather than removing the name, which is a different rule
	// than the one the operator wrote. Refuse it here, where the message can
	// name the alternative, rather than in an applier that would have to invent
	// a semantics for it. Denying the containing directory and reopening the
	// siblings that are still needed expresses the same intent exactly.
	if file && grant.Access == AccessDeny {
		return "", "", "", "", false, fmt.Errorf(
			"filesystem[%d].path %q is a regular file, which a deny rule cannot name; deny the directory that contains it and reopen the entries that must stay reachable",
			index, resolved,
		)
	}
	if grant.Access != AccessDeny {
		for _, denied := range protected {
			// Guard-biased on purpose: spelling restoration above already
			// aligns the common case, but a not-yet-created grant path has no
			// on-disk spelling to restore, so the residual folded collision has
			// to be refused here rather than admitted.
			if GuardPathsIntersect(resolved, denied) {
				return "", "", "", "", false, fmt.Errorf(
					"filesystem[%d].path %q intersects protected directory %q",
					index, resolved, denied,
				)
			}
		}
	}
	mountPath, err = normalizeMountPath(index, grant, resolved)
	if err != nil {
		return "", "", "", "", false, err
	}
	return spelling, resolved, mountPath, kind, missing, nil
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
	kind      GrantKind
	missing   bool
	info      os.FileInfo
}

type authoredFilesystemGroup struct {
	resolved  string
	mountPath string
	access    Access
	kind      GrantKind
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
		spelling, resolved, mountPath, kind, missing, err := normalizeFilesystemGrant(
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
			access: grant.Access, kind: kind, missing: missing, info: info,
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
				access: candidate.access, kind: candidate.kind,
				missing: candidate.missing, info: candidate.info,
				spellings: map[string]struct{}{},
			})
			groupIndex = len(groups) - 1
		}
		group := &groups[groupIndex]
		if accessRank(candidate.access) > accessRank(group.access) {
			group.access = candidate.access
		}
		group.kind = strictestGrantKind(group.kind, candidate.kind)
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
			Kind: group.kind,
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
	current, _, _, err := canonicalGrantTarget(spelling, allowMissing)
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

// strictestGrantKind merges two commitments for one rule.
//
// A commitment is a RESTRICTION — it says this rule may only ever be one file —
// so the merge keeps the strictest contributor rather than the first one seen.
// File beats unset in both directions, which is what makes folding, cross-scope
// union, and identity coalescing order-independent. Taking the first row's kind
// instead would let an unstamped legacy or bare-path row silently drop a
// stamped row's restriction depending on map iteration or authored order.
//
// This cannot refuse anything a single row would not: normalizeFilesystemGrant
// has already rejected a stamped rule whose path is now a directory, so a
// GrantKindFile reaching a merge means the path is a file or is missing, and
// both contributors agree in the first case.
func strictestGrantKind(left, right GrantKind) GrantKind {
	if left == GrantKindFile || right == GrantKindFile {
		return GrantKindFile
	}
	return GrantKindUnset
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

// canonicalDirectory is canonicalGrantTarget for the callers whose paths are
// structurally directories — the common-rule catalog. A regular file is a real
// authoring error there rather than a narrower grant, so it keeps the historical
// refusal instead of inheriting file support.
func canonicalDirectory(path string, allowMissing bool) (string, bool, error) {
	resolved, missing, file, err := canonicalGrantTarget(path, allowMissing)
	if err != nil {
		return "", false, err
	}
	if file {
		return "", false, fmt.Errorf("path %q is not a directory", resolved)
	}
	return resolved, missing, nil
}

// canonicalGrantTarget resolves one authored filesystem-rule path to its
// canonical host spelling and reports what kind of thing it names.
//
// A rule may name a DIRECTORY or a REGULAR FILE (TCL-1041). The file form
// exists because the most valuable carve-out beneath a denied Home is usually a
// single dotfile — ~/.gitconfig, ~/.netrc — and reopening the whole directory
// that contains it hands over everything else in it too. Nothing else is
// admitted: a socket, device node, or FIFO reached through a grant would be an
// active channel rather than a body of content, and tclaude has separate,
// narrower axes (unix_sockets, the /dev rows the applier turns into --dev-bind)
// for the cases where that is genuinely wanted.
//
// Kind is a property of the HOST path, decided here, on the layer that already
// owns every other filesystem question about a grant. Callers that cannot
// enforce a file rule refuse it by capability (ValidateFileGrantSupport); they
// never re-derive the kind for themselves.
//
// A path that does not exist yet is reported as a directory: there is nothing
// on disk to ask. That is the honest answer and the safe one — a missing
// read/write row is skipped at launch, and a missing row that later appears as
// a file is re-canonicalized by FilesystemForLaunch before it can be applied.
func canonicalGrantTarget(path string, allowMissing bool) (string, bool, bool, error) {
	original := path
	clean, err := cleanDirectoryPath(path)
	if err != nil {
		return "", false, false, err
	}
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		if allowMissing && os.IsNotExist(err) {
			resolved, err = canonicalMissingDirectory(clean)
			if err == nil {
				return resolved, true, false, nil
			}
		}
		return "", false, false, fmt.Errorf("resolve symlinks for %q: %w", original, err)
	}
	// Restore the on-disk spelling so a case- or NFC-variant authoring of a
	// path lands on the same string every other spelling of it does. Without
	// this, EvalSymlinks preserves the caller's casing and the protected-root
	// comparison below — plus grant dedup and deny-dominance — see two
	// directories where a case-insensitive volume has one.
	resolved = CanonicalHostSpelling(filepath.Clean(resolved))
	info, err := os.Stat(resolved)
	if err != nil {
		return "", false, false, fmt.Errorf("stat %q: %w", resolved, err)
	}
	switch {
	case info.IsDir():
		return resolved, false, false, nil
	case info.Mode().IsRegular():
		return resolved, false, true, nil
	default:
		return "", false, false, fmt.Errorf(
			"path %q is neither a directory nor a regular file", resolved)
	}
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

// NormalizeEnvironment validates and canonicalizes operator-authored process
// environment entries. It deliberately shares the sandbox-profile rules: the
// same reserved launch-control names, size limits, UTF-8 requirements, and
// deterministic ordering apply no matter which configuration surface supplied
// an entry.
func NormalizeEnvironment(in []EnvironmentEntry) ([]EnvironmentEntry, error) {
	return normalizeEnvironment(in)
}

// MergeEnvironment overlays tiers from left to right and returns one
// canonical environment. Later tiers win by variable name. Each tier and the
// aggregate are validated, so combining individually valid configurations can
// never bypass the total count or byte limits.
func MergeEnvironment(tiers ...[]EnvironmentEntry) ([]EnvironmentEntry, error) {
	byName := make(map[string]string)
	for _, tier := range tiers {
		normalized, err := normalizeEnvironment(tier)
		if err != nil {
			return nil, err
		}
		for _, entry := range normalized {
			byName[entry.Name] = entry.Value
		}
	}
	merged := make([]EnvironmentEntry, 0, len(byName))
	for name, value := range byName {
		merged = append(merged, EnvironmentEntry{Name: name, Value: value})
	}
	return normalizeEnvironment(merged)
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
