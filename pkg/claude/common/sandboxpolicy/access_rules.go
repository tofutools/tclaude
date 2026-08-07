package sandboxpolicy

import (
	"fmt"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
)

const (
	MaxNetworkAllowEntries         = 128
	MaxEffectiveNetworkDenyEntries = 3 * MaxNetworkAllowEntries
	MaxSocketAllowEntries          = 64
	MaxPortsPerEntry               = 16
	MaxHostBytes                   = 253
)

// AccessMode is one access axis's posture. The empty value is deliberately a
// fourth state: it preserves exactly what a profile meant before the access
// axes existed, rather than guessing either full access or lockdown.
type AccessMode string

const (
	AccessModeUnset  AccessMode = ""
	AccessModeOpen   AccessMode = "open"
	AccessModeClosed AccessMode = "closed"
	AccessModeList   AccessMode = "list"
)

// NetworkBaseline is the authored network posture used by compositional
// profiles. It is deliberately separate from AccessMode: a deny baseline
// materializes to closed when it has no unlocks and to list when it does.
type NetworkBaseline string

const (
	NetworkBaselineInherit NetworkBaseline = "inherit"
	NetworkBaselineAllow   NetworkBaseline = "allow"
	NetworkBaselineDeny    NetworkBaseline = "deny"
)

type NetworkRules struct {
	// Mode is the forever-readable legacy/effective representation. Newly
	// authored profiles use Baseline; resolved launch authority uses Mode.
	Mode      AccessMode          `json:"mode,omitempty"`
	Baseline  NetworkBaseline     `json:"baseline,omitempty"`
	Packs     []string            `json:"packs,omitempty"`
	DenyPacks []string            `json:"deny_packs,omitempty"`
	Allow     []NetworkAllowEntry `json:"allow,omitempty"`
	Deny      []NetworkAllowEntry `json:"deny,omitempty"`
	// Engine names HOW a discriminating rule set is enforced. It is not an
	// access axis: it can neither widen nor narrow the destinations the rest of
	// this struct authorizes, which is why it composes by most-explicit-wins
	// (ResolveNetworkEngine) rather than through the intersection lattice the
	// other fields use. Omitted is the fourth state and never changes behavior.
	Engine NetworkEngine `json:"engine,omitempty"`
}

// NetworkAllowEntry names exactly one outbound destination selector. The
// containing Allow or Deny slice supplies its authored mode.
type NetworkAllowEntry struct {
	Host              string `json:"host,omitempty"`
	Domain            string `json:"domain,omitempty"`
	IncludeSubdomains bool   `json:"include_subdomains,omitempty"`
	CIDR              string `json:"cidr,omitempty"`
	Loopback          bool   `json:"loopback,omitempty"`
	Ports             []int  `json:"ports,omitempty"`
}

type UnixSocketRules struct {
	Mode  AccessMode         `json:"mode"`
	Allow []SocketAllowEntry `json:"allow,omitempty"`
}

// SocketAllowEntry names one socket path or a bounded glob over sibling
// paths. The agentd socket floor is intentionally absent from this editable
// type; AgentdSocketFloor adds it outside operator-authored policy.
type SocketAllowEntry struct {
	Path     string `json:"path,omitempty"`
	PathGlob string `json:"path_glob,omitempty"`
}

// ResolvedAxes is the concrete access intent consumed by capability planning.
// DeriveAccessAxes produces it without mutating the stored compatibility
// fields.
type ResolvedAxes struct {
	Network     NetworkRules
	UnixSockets UnixSocketRules
	// Filesystem is the authored filesystem authority this policy grants,
	// carried alongside the two access axes rather than folded into them.
	//
	// It is here because one capability question genuinely spans both: a
	// filesystem grant can reach a socket inode, and under the proxy filtering
	// engine reaching a resolver socket defeats the engine's name authority
	// exactly as authorizing that socket on the unix_sockets axis does (see
	// NetworkEngineResolverFilesystemConflict). Capability planning could not
	// ask that question while it saw only the two access axes.
	//
	// It is deliberately NOT a third enforceable axis: nothing rates it, nothing
	// widens it, and PlanAccessEnforcement passes it through untouched. Callers
	// that only need the access tiers can keep ignoring it.
	//
	// Not serialized. This type is rendered as `recorded_axes` on plan and
	// profile responses, whose contract is the two ACCESS axes; adding a third
	// key there would change an API shape to restate grants those responses
	// already carry.
	Filesystem []FilesystemGrant `json:"-"`
}

const (
	AccessNoticeClassDegradation = "degradation"
	AccessNoticeClassComposition = "composition"
	AccessNoticeClassLaunch      = "launch"

	AccessNoticeReasonEmptyIntersection     = "empty_intersection"
	AccessNoticeReasonMissingInclude        = "missing_include"
	AccessNoticeReasonUnmaterializedEntries = "unmaterialized_entries"
	AccessNoticeReasonFilteredPrerequisite  = "filtered_prerequisite_probe"
	AccessNoticeReasonFilteredModelTraffic  = "filtered_model_transport"
	// AccessNoticeReasonNetworkEngine carries decision (b)'s disclosure: which
	// filtering engine composition settled on, which profile named it, and
	// which lower layer asked for a different one and lost. It is emitted only
	// when some layer named an engine.
	AccessNoticeReasonNetworkEngine = "network_engine"
	// AccessNoticeReasonOperatorUnenforcedLaunchOverride records the
	// dashboard-only, fresh-spawn authorization to widen an otherwise-refused
	// closed network posture to open. The daemon-written one-shot launch
	// snapshot carries this exact notice through the forked session launcher;
	// profiles cannot author it.
	AccessNoticeReasonOperatorUnenforcedLaunchOverride = "operator_unenforced_launch_override"
	// AccessNoticeReasonUnconfinedImplementation records that the resolved
	// profile chain authored access rules which the selected implementation
	// confines nothing to enforce them with. It exists for `resource-only`,
	// whose chain must keep resolving because resource_limits travel in it,
	// and which therefore inherits whatever filesystem/network rules a global
	// or group profile already carried. Those rules are inert; the operator
	// has to be told so, because a resolved profile that shows up in the
	// snapshot otherwise reads as a policy in force.
	AccessNoticeReasonUnconfinedImplementation = "unconfined_implementation"
	// AccessNoticeReasonResourceCgroupUnavailable records that a launch which
	// asked for the per-agent cgroup with NO ceiling authored did not get one,
	// because the host has no delegated cgroup v2 subtree to create it in. It is
	// deliberately not a refusal: accounting is observability, so refusing would
	// make an agent unlaunchable — and unresumable, where the dashboard's
	// fresh-spawn override does not exist — over counters. A launch that
	// authored a ceiling still fails closed instead of taking this notice.
	AccessNoticeReasonResourceCgroupUnavailable = "resource_cgroup_unavailable"
	// AccessNoticeReasonProxyEngineNoProxyOverride carries the proxy engine's
	// proxy-environment ownership: an inherited NO_PROXY/no_proxy is overridden
	// to empty inside the sandbox rather than honored or refused over. It is
	// emitted only when a non-empty value was actually discarded.
	AccessNoticeReasonProxyEngineNoProxyOverride = "proxy_engine_no_proxy_override"

	AccessNoticeEffectNotEnforced       = "not_enforced"
	AccessNoticeEffectEnforcedWider     = "enforced_wider"
	AccessNoticeEffectLaunchGated       = "launch_gated"
	AccessNoticeEffectNothingAllowed    = "nothing_allowed"
	AccessNoticeEffectNotMaterialized   = "not_materialized"
	AccessNoticeEffectPreviewIncomplete = "preview_incomplete"
	// AccessNoticeEffectMechanismSelected states that the notice reports which
	// mechanism was chosen, not a change to what the policy authorizes. It is
	// its own effect precisely because the existing ones all describe a
	// widening or a gate, and an engine selection is neither.
	AccessNoticeEffectMechanismSelected = "mechanism_selected"
	// AccessNoticeEffectEnvironmentOverridden states that the launch replaced an
	// inherited environment value the running mechanism owns. It is its own
	// effect for the same reason as the one above, in the opposite direction:
	// the existing effects all report enforcement that is weaker than authored,
	// and an override that discards a host exemption is not a weakening — the
	// destinations it would have exempted stay unreachable.
	AccessNoticeEffectEnvironmentOverridden = "environment_overridden"
)

// AccessNotice is the single persisted disclosure record used by access
// enforcement. Its class separates authoring composition, target-dependent
// degradation, and filesystem-dependent launch materialization.
type AccessNotice struct {
	Class   string   `json:"class"`
	Axis    string   `json:"axis"`
	Reason  string   `json:"reason"`
	Effect  string   `json:"effect"`
	Detail  string   `json:"detail"`
	Entries []int    `json:"entries,omitempty"`
	Tiers   []string `json:"tiers,omitempty"`
}

// DeriveAccessAxes applies the forever-supported network_access compatibility
// table. New fields remain pointers in Profile so absent is distinguishable
// from an explicitly authored empty-mode object.
func DeriveAccessAxes(p Profile) (ResolvedAxes, error) {
	network := NetworkRules{}
	sockets := UnixSocketRules{}
	if p.Network != nil {
		var err error
		network, err = resolvedNetworkRules(*p.Network)
		if err != nil {
			return ResolvedAxes{}, err
		}
		if err := validateLegacyNetworkAgreement(p.NetworkAccess, network.Mode); err != nil {
			return ResolvedAxes{}, err
		}
	} else {
		switch p.NetworkAccess {
		case NetworkAccessInherit:
		case NetworkAccessInternet:
			network.Mode = AccessModeOpen
		case NetworkAccessNone:
			network.Mode = AccessModeClosed
		default:
			return ResolvedAxes{}, fmt.Errorf(
				"network_access %q is invalid (want internet, none, or omitted to inherit)",
				p.NetworkAccess,
			)
		}
	}
	if p.UnixSockets != nil {
		sockets = cloneUnixSocketRules(*p.UnixSockets)
	} else if p.Network == nil && p.NetworkAccess == NetworkAccessNone {
		// Today's isolated posture closes ambient pathname sockets while
		// retaining agentd. Preserve that coupled legacy meaning only when the
		// new network axis itself is absent.
		sockets.Mode = AccessModeClosed
	}
	return ResolvedAxes{
		Network:     network,
		UnixSockets: sockets,
		// Taken from the profile this derivation was handed, so every caller
		// that already builds axes from a whole profile — the editor
		// prediction, the spawn guard, the launch boundary — gains the
		// filesystem view without a second construction site to keep in step.
		Filesystem: append([]FilesystemGrant(nil), p.Filesystem...),
	}, nil
}

// resolvedNetworkRules clones the authored rules and, when a baseline is
// present, materializes pack references into the effective mode and entries.
func resolvedNetworkRules(network NetworkRules) (NetworkRules, error) {
	resolved := cloneNetworkRules(network)
	if resolved.Baseline == "" {
		return resolved, nil
	}
	return MaterializeNetworkRules(resolved)
}

func validateLegacyNetworkAgreement(legacy NetworkAccess, mode AccessMode) error {
	if legacy == NetworkAccessInherit {
		return nil
	}
	agree := (legacy == NetworkAccessInternet && mode == AccessModeOpen) ||
		(legacy == NetworkAccessNone && mode == AccessModeClosed)
	if !agree {
		return fmt.Errorf(
			"network_access %q conflicts with network.mode %q; set one or make them agree",
			legacy, mode,
		)
	}
	return nil
}

// LegacyNetworkAccessForExport back-derives the compatibility spelling for
// export. Access lists have no legacy representation and therefore omit it.
func LegacyNetworkAccessForExport(network *NetworkRules, stored NetworkAccess) NetworkAccess {
	if network == nil {
		return stored
	}
	resolved, err := resolvedNetworkRules(*network)
	if err != nil {
		return NetworkAccessInherit
	}
	switch resolved.Mode {
	case AccessModeOpen:
		return NetworkAccessInternet
	case AccessModeClosed:
		return NetworkAccessNone
	case AccessModeUnset:
		return NetworkAccessInherit
	default:
		return NetworkAccessInherit
	}
}

func normalizeNetworkRules(in *NetworkRules) (*NetworkRules, error) {
	if in == nil {
		return nil, nil
	}
	if in.Baseline != "" {
		if in.Mode != AccessModeUnset {
			return nil, fmt.Errorf("network must set baseline or legacy mode, not both")
		}
		switch in.Baseline {
		case NetworkBaselineInherit, NetworkBaselineAllow, NetworkBaselineDeny:
		default:
			return nil, fmt.Errorf(
				"network.baseline %q is invalid (want inherit, allow, or deny)",
				in.Baseline,
			)
		}
		if in.Baseline == NetworkBaselineInherit &&
			(len(in.Packs) > 0 || len(in.DenyPacks) > 0 ||
				len(in.Allow) > 0 || len(in.Deny) > 0) {
			return nil, fmt.Errorf(
				"network packs and entries are not valid with baseline %q",
				NetworkBaselineInherit,
			)
		}
	} else {
		if err := validateAccessMode("network", in.Mode); err != nil {
			return nil, err
		}
		if len(in.Packs) > 0 {
			return nil, fmt.Errorf("network.packs requires the compositional baseline representation")
		}
		if len(in.DenyPacks) > 0 {
			return nil, fmt.Errorf("network.deny_packs requires the compositional baseline representation")
		}
		if in.Mode != AccessModeList && len(in.Allow) > 0 {
			return nil, fmt.Errorf(`network.allow is only valid with mode "list"`)
		}
		if len(in.Deny) > 0 {
			return nil, fmt.Errorf("network.deny requires the compositional baseline representation")
		}
	}
	if len(in.Allow) > MaxNetworkAllowEntries {
		return nil, fmt.Errorf("network.allow has too many entries (maximum %d)", MaxNetworkAllowEntries)
	}
	if len(in.Deny) > MaxNetworkAllowEntries {
		return nil, fmt.Errorf("network.deny has too many entries (maximum %d)", MaxNetworkAllowEntries)
	}
	packs, err := normalizeNetworkPackRefs(in.Packs, "packs")
	if err != nil {
		return nil, err
	}
	denyPacks, err := normalizeNetworkPackRefs(in.DenyPacks, "deny_packs")
	if err != nil {
		return nil, err
	}
	allowPacks := make(map[string]struct{}, len(packs))
	for _, id := range packs {
		allowPacks[id] = struct{}{}
	}
	for _, id := range denyPacks {
		if _, exists := allowPacks[id]; exists {
			return nil, fmt.Errorf(
				"network pack capability %q is authored in both network.packs (allow) and network.deny_packs (deny); choose exactly one mode",
				id,
			)
		}
	}
	if err := ValidateNetworkEngine(in.Engine); err != nil {
		return nil, err
	}
	out := &NetworkRules{
		Mode: in.Mode, Baseline: in.Baseline,
		Packs: packs, DenyPacks: denyPacks,
		Engine: in.Engine,
	}
	out.Allow, err = normalizeNetworkEntries(in.Allow, "allow")
	if err != nil {
		return nil, err
	}
	out.Deny, err = normalizeNetworkEntries(in.Deny, "deny")
	if err != nil {
		return nil, err
	}
	if out.Baseline != "" {
		if _, err := MaterializeNetworkRules(*out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// normalizeEffectiveNetworkRules validates the materialized representation
// frozen into snapshots. Its deny limit is aggregate rather than per-profile:
// resolution may union one complete deny set from each of its three scopes.
func normalizeEffectiveNetworkRules(in *NetworkRules) (*NetworkRules, error) {
	if in == nil {
		return nil, nil
	}
	if in.Baseline != "" || len(in.Packs) > 0 || len(in.DenyPacks) > 0 {
		return nil, fmt.Errorf(
			"effective network rules must be materialized before snapshotting")
	}
	if err := validateAccessMode("network", in.Mode); err != nil {
		return nil, err
	}
	if in.Mode != AccessModeList && len(in.Allow) > 0 {
		return nil, fmt.Errorf(`network.allow is only valid with mode "list"`)
	}
	if in.Mode == AccessModeUnset && len(in.Deny) > 0 {
		return nil, fmt.Errorf(
			"effective network denies require a concrete mode")
	}
	if len(in.Allow) > MaxNetworkAllowEntries {
		return nil, fmt.Errorf("network.allow has too many entries (maximum %d)",
			MaxNetworkAllowEntries)
	}
	if len(in.Deny) > MaxEffectiveNetworkDenyEntries {
		return nil, fmt.Errorf(
			"effective network.deny has too many entries (maximum %d)",
			MaxEffectiveNetworkDenyEntries)
	}
	if err := ValidateNetworkEngine(in.Engine); err != nil {
		return nil, err
	}
	out := &NetworkRules{Mode: in.Mode, Engine: in.Engine}
	var err error
	out.Allow, err = normalizeNetworkEntries(in.Allow, "allow")
	if err != nil {
		return nil, err
	}
	out.Deny, err = normalizeNetworkEntries(in.Deny, "deny")
	if err != nil {
		return nil, err
	}
	return out, nil
}

func normalizeNetworkEntries(in []NetworkAllowEntry, field string) ([]NetworkAllowEntry, error) {
	if in == nil {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]NetworkAllowEntry, 0, len(in))
	for i, entry := range in {
		normalized, err := normalizeNetworkAllowEntry(entry, i, field)
		if err != nil {
			return nil, err
		}
		key := networkEntryKey(normalized)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, normalized)
	}
	sort.Slice(out, func(i, j int) bool {
		return networkEntryKey(out[i]) < networkEntryKey(out[j])
	})
	return out, nil
}

func normalizeNetworkAllowEntry(in NetworkAllowEntry, index int, field string) (NetworkAllowEntry, error) {
	selectors := 0
	if in.Host != "" {
		selectors++
	}
	if in.Domain != "" {
		selectors++
	}
	if in.CIDR != "" {
		selectors++
	}
	if in.Loopback {
		selectors++
	}
	if selectors != 1 {
		return NetworkAllowEntry{}, fmt.Errorf(
			"network.%s[%d] must set exactly one of host, domain, cidr, loopback",
			field, index,
		)
	}
	if in.IncludeSubdomains && in.Domain == "" {
		return NetworkAllowEntry{}, fmt.Errorf(
			"network.%s[%d].include_subdomains is only valid with domain",
			field, index,
		)
	}
	out := in
	var err error
	if out.Host != "" {
		out.Host, err = normalizeDNSName(out.Host)
		if err != nil {
			return NetworkAllowEntry{}, fmt.Errorf("network.%s[%d].host: %w", field, index, err)
		}
	}
	if out.Domain != "" {
		out.Domain, err = normalizeDNSName(out.Domain)
		if err != nil {
			return NetworkAllowEntry{}, fmt.Errorf("network.%s[%d].domain: %w", field, index, err)
		}
	}
	if out.CIDR != "" {
		prefix, parseErr := netip.ParsePrefix(out.CIDR)
		if parseErr != nil {
			return NetworkAllowEntry{}, fmt.Errorf("network.%s[%d].cidr %q is invalid: %w", field, index, out.CIDR, parseErr)
		}
		prefix = prefix.Masked()
		// Ordering is load-bearing and asserted by test: unmapping first means
		// the loopback-row refusal sees one address form, so a mapped spelling
		// (::ffff:127.0.0.1/128 -> 127.0.0.1/32) is still caught by the IPv4
		// authority entries. Running the refusal first would leave it depending
		// on the mapped entries alone.
		prefix = UnmapPrefix(prefix)
		if PrefixIntersectsLoopbackRowAuthority(prefix) {
			// Naming the space matters: 0.0.0.0 and :: reach the host too,
			// so "covers loopback" alone reads as wrong to an operator who
			// authored neither 127.0.0.0/8 nor ::1.
			return NetworkAllowEntry{}, fmt.Errorf(
				`network.%s[%d].cidr %q covers address space governed by loopback rows `+
					`(127.0.0.0/8, ::1, the exact 0.0.0.0 and :: addresses, and their IPv4-mapped forms), `+
					`which CIDR rows cannot govern; use {"loopback": true} for that portion, `+
					`and if the CIDR also covers other addresses, split it and keep CIDR rows for the remainder`,
				field, index, out.CIDR,
			)
		}
		out.CIDR = prefix.String()
	}
	if len(out.Ports) > MaxPortsPerEntry {
		return NetworkAllowEntry{}, fmt.Errorf(
			"network.%s[%d].ports has too many entries (maximum %d)",
			field, index, MaxPortsPerEntry,
		)
	}
	portSet := make(map[int]struct{}, len(out.Ports))
	var ports []int
	if len(out.Ports) > 0 {
		ports = make([]int, 0, len(out.Ports))
	}
	for _, port := range out.Ports {
		if port < 1 || port > 65535 {
			return NetworkAllowEntry{}, fmt.Errorf(
				"network.%s[%d].ports contains %d (want 1..65535)",
				field, index, port,
			)
		}
		if _, exists := portSet[port]; exists {
			continue
		}
		portSet[port] = struct{}{}
		ports = append(ports, port)
	}
	sort.Ints(ports)
	out.Ports = ports
	return out, nil
}

func normalizeDNSName(in string) (string, error) {
	if len(in) > MaxHostBytes {
		return "", fmt.Errorf("is too long (maximum %d bytes)", MaxHostBytes)
	}
	if in == "" || !utf8.ValidString(in) || in != strings.TrimSpace(in) {
		return "", fmt.Errorf("must be an ASCII LDH name")
	}
	if strings.ContainsAny(in, `/:*`) || strings.HasPrefix(in, ".") || strings.HasSuffix(in, ".") {
		return "", fmt.Errorf("must be an ASCII LDH name without scheme, path, port, or wildcard")
	}
	for _, r := range in {
		if r > 127 {
			return "", fmt.Errorf("must be ASCII (IDNs must be punycoded)")
		}
	}
	if addr, err := netip.ParseAddr(in); err == nil {
		if addr.IsLoopback() {
			return "", fmt.Errorf(`IP loopback literals must use {"loopback": true}`)
		}
		return "", fmt.Errorf("IP literals must use cidr")
	}
	for _, label := range strings.Split(in, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("must contain valid LDH labels")
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') &&
				(r < '0' || r > '9') && r != '-' {
				return "", fmt.Errorf("must contain valid LDH labels")
			}
		}
	}
	return strings.ToLower(in), nil
}

func normalizeUnixSocketRules(in *UnixSocketRules, allowMissing bool) (*UnixSocketRules, []string, error) {
	if in == nil {
		return nil, nil, nil
	}
	if err := validateAccessMode("unix_sockets", in.Mode); err != nil {
		return nil, nil, err
	}
	if in.Mode != AccessModeList && len(in.Allow) > 0 {
		return nil, nil, fmt.Errorf(`unix_sockets.allow is only valid with mode "list"`)
	}
	if len(in.Allow) > MaxSocketAllowEntries {
		return nil, nil, fmt.Errorf("unix_sockets.allow has too many entries (maximum %d)", MaxSocketAllowEntries)
	}
	protected, err := ProtectedPaths()
	if err != nil {
		return nil, nil, err
	}
	out := &UnixSocketRules{Mode: in.Mode}
	seen := make(map[string]struct{}, len(in.Allow))
	missingSet := map[string]struct{}{}
	for i, entry := range in.Allow {
		normalized, checkPath, err := normalizeSocketAllowEntry(entry, i)
		if err != nil {
			return nil, nil, err
		}
		for _, denied := range protected {
			// Guard-biased: an allow entry may name a socket that does not
			// exist yet, so a folded collision with a protected root that
			// cannot be refuted must refuse rather than admit.
			if GuardPathsIntersect(checkPath, denied) {
				written := normalized.Path
				if written == "" {
					written = normalized.PathGlob
				}
				return nil, nil, fmt.Errorf(
					"unix_sockets.allow[%d] %q intersects protected directory %q",
					i, written, denied,
				)
			}
		}
		if normalized.Path != "" {
			if _, statErr := os.Lstat(normalized.Path); statErr != nil {
				if !allowMissing || !os.IsNotExist(statErr) {
					return nil, nil, fmt.Errorf("unix_sockets.allow[%d] path %q: %w", i, normalized.Path, statErr)
				}
				missingSet[normalized.Path] = struct{}{}
			}
		}
		key := socketEntryKey(normalized)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out.Allow = append(out.Allow, normalized)
	}
	sort.Slice(out.Allow, func(i, j int) bool {
		return socketEntryKey(out.Allow[i]) < socketEntryKey(out.Allow[j])
	})
	missing := make([]string, 0, len(missingSet))
	for entry := range missingSet {
		missing = append(missing, entry)
	}
	sort.Strings(missing)
	return out, missing, nil
}

func normalizeSocketAllowEntry(in SocketAllowEntry, index int) (SocketAllowEntry, string, error) {
	if (in.Path == "") == (in.PathGlob == "") {
		return SocketAllowEntry{}, "", fmt.Errorf(
			"unix_sockets.allow[%d] must set exactly one of path, path_glob",
			index,
		)
	}
	if in.Path != "" {
		clean, err := cleanDirectoryPath(in.Path)
		if err != nil {
			return SocketAllowEntry{}, "", fmt.Errorf("unix_sockets.allow[%d].path: %w", index, err)
		}
		checkPath, err := canonicalSocketCheckPath(clean)
		if err != nil {
			return SocketAllowEntry{}, "", fmt.Errorf(
				"unix_sockets.allow[%d].path %q: resolve protected-path aliases: %w",
				index, clean, err,
			)
		}
		return SocketAllowEntry{Path: clean}, checkPath, nil
	}
	if strings.Contains(in.PathGlob, "**") {
		return SocketAllowEntry{}, "", fmt.Errorf("unix_sockets.allow[%d].path_glob must not contain **", index)
	}
	clean, err := cleanDirectoryPath(in.PathGlob)
	if err != nil {
		return SocketAllowEntry{}, "", fmt.Errorf("unix_sockets.allow[%d].path_glob: %w", index, err)
	}
	segments := strings.Split(filepath.ToSlash(clean), "/")
	globSegment := -1
	for i, segment := range segments {
		if !strings.Contains(segment, "*") {
			continue
		}
		if globSegment != -1 {
			return SocketAllowEntry{}, "", fmt.Errorf(
				"unix_sockets.allow[%d].path_glob may contain * in only one path segment",
				index,
			)
		}
		globSegment = i
	}
	if globSegment < 0 {
		return SocketAllowEntry{}, "", fmt.Errorf(
			"unix_sockets.allow[%d].path_glob must contain *",
			index,
		)
	}
	literalSegments := segments[:globSegment]
	literalPrefix := filepath.FromSlash(strings.Join(literalSegments, "/"))
	if literalPrefix == "" {
		literalPrefix = string(filepath.Separator)
	}
	if filepath.VolumeName(clean) != "" && filepath.VolumeName(literalPrefix) == "" {
		literalPrefix = filepath.VolumeName(clean) + string(filepath.Separator)
	}
	checkPath, evalErr := filepath.EvalSymlinks(literalPrefix)
	if evalErr != nil {
		return SocketAllowEntry{}, "", fmt.Errorf(
			"unix_sockets.allow[%d].path_glob literal ancestor %q: resolve protected-path aliases: %w",
			index, literalPrefix, evalErr,
		)
	}
	checkPath = filepath.Clean(checkPath)
	info, statErr := os.Stat(checkPath)
	if statErr != nil {
		return SocketAllowEntry{}, "", fmt.Errorf(
			"unix_sockets.allow[%d].path_glob literal ancestor %q: %w",
			index, literalPrefix, statErr,
		)
	}
	if !info.IsDir() {
		return SocketAllowEntry{}, "", fmt.Errorf(
			"unix_sockets.allow[%d].path_glob literal ancestor %q is not a directory",
			index, literalPrefix,
		)
	}
	return SocketAllowEntry{PathGlob: clean}, checkPath, nil
}

// canonicalSocketCheckPath resolves existing symlink aliases while retaining
// the final missing socket name. Unix-socket paths commonly do not exist when
// a profile is authored, but their existing ancestor must still be
// canonicalized before the protected-root containment check (notably for
// macOS's /var -> /private/var alias).
func canonicalSocketCheckPath(clean string) (string, error) {
	if _, err := os.Lstat(clean); err == nil {
		resolved, err := filepath.EvalSymlinks(clean)
		if err != nil {
			return "", err
		}
		return filepath.Clean(resolved), nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	return canonicalMissingDirectory(clean)
}

func validateAccessMode(axis string, mode AccessMode) error {
	switch mode {
	case AccessModeUnset, AccessModeOpen, AccessModeClosed, AccessModeList:
		return nil
	default:
		return fmt.Errorf(
			"%s.mode %q is invalid (want open, closed, list, or omitted)",
			axis, mode,
		)
	}
}

func networkEntryKey(entry NetworkAllowEntry) string {
	selector := "loopback"
	switch {
	case entry.Host != "":
		selector = "host:" + entry.Host
	case entry.Domain != "":
		selector = "domain:" + entry.Domain + ":" + strconv.FormatBool(entry.IncludeSubdomains)
	case entry.CIDR != "":
		selector = "cidr:" + entry.CIDR
	}
	ports := make([]string, len(entry.Ports))
	for i, port := range entry.Ports {
		ports[i] = strconv.Itoa(port)
	}
	return selector + "|ports:" + strings.Join(ports, ",")
}

func socketEntryKey(entry SocketAllowEntry) string {
	if entry.Path != "" {
		return "path:" + entry.Path
	}
	return "glob:" + entry.PathGlob
}

func cloneNetworkRules(in NetworkRules) NetworkRules {
	out := in
	out.Packs = append([]string(nil), in.Packs...)
	out.DenyPacks = append([]string(nil), in.DenyPacks...)
	cloneEntries := func(entries []NetworkAllowEntry) []NetworkAllowEntry {
		if entries == nil {
			return nil
		}
		cloned := make([]NetworkAllowEntry, len(entries))
		for i, entry := range entries {
			cloned[i] = entry
			cloned[i].Ports = append([]int(nil), entry.Ports...)
		}
		return cloned
	}
	out.Allow = cloneEntries(in.Allow)
	out.Deny = cloneEntries(in.Deny)
	return out
}

func cloneUnixSocketRules(in UnixSocketRules) UnixSocketRules {
	out := in
	out.Allow = append([]SocketAllowEntry(nil), in.Allow...)
	return out
}

func cloneNetworkRulesPtr(in *NetworkRules) *NetworkRules {
	if in == nil {
		return nil
	}
	out := cloneNetworkRules(*in)
	return &out
}

func cloneUnixSocketRulesPtr(in *UnixSocketRules) *UnixSocketRules {
	if in == nil {
		return nil
	}
	out := cloneUnixSocketRules(*in)
	return &out
}

// AgentdSocketFloor returns every spelling retained by the control-plane
// migration. These paths are injected outside editable policy and therefore
// cannot be removed by any access-axis payload.
func AgentdSocketFloor() []string {
	paths := append([]string{agentipc.CanonicalSocketPath()}, agentipc.LegacySocketPaths()...)
	out := make([]string, 0, len(paths))
	seen := map[string]struct{}{}
	for _, entry := range paths {
		if entry == "" {
			continue
		}
		entry = filepath.Clean(entry)
		if _, exists := seen[entry]; exists {
			continue
		}
		seen[entry] = struct{}{}
		out = append(out, entry)
	}
	return out
}

// UnixSocketAccess is the enforcement-neutral socket allow result. The floor
// is present in AllowedPaths for every mode, including AllowAll modes, so an
// adapter cannot accidentally treat it as editable policy.
type UnixSocketAccess struct {
	AllowAll     bool
	AllowedPaths []string
	AllowedGlobs []string
}

// UnixSocketMaterialization is the launch-time surface produced from an
// authored socket list. Unmaterialized and Entries retain the authored
// selectors that did not resolve to any live Unix socket, so launch surfaces
// can disclose the narrower rendered surface.
type UnixSocketMaterialization struct {
	Paths          []string `json:"paths,omitempty"`
	Unmaterialized []string `json:"unmaterialized,omitempty"`
	Entries        []int    `json:"entries,omitempty"`
}

// ResolveUnixSocketAccess injects the non-removable agentd floor after
// operator-authored policy has been normalized.
func ResolveUnixSocketAccess(rules UnixSocketRules) UnixSocketAccess {
	out := UnixSocketAccess{
		AllowAll:     rules.Mode == AccessModeUnset || rules.Mode == AccessModeOpen,
		AllowedPaths: AgentdSocketFloor(),
	}
	if rules.Mode != AccessModeList {
		return out
	}
	for _, entry := range rules.Allow {
		if entry.Path != "" {
			out.AllowedPaths = appendUniqueStrings(out.AllowedPaths, entry.Path)
		} else if entry.PathGlob != "" {
			out.AllowedGlobs = appendUniqueStrings(out.AllowedGlobs, entry.PathGlob)
		}
	}
	return out
}

// MaterializeUnixSocketList expands one resolved socket list against the
// launch-time filesystem. Missing exact paths and unmatched globs are omitted:
// socket endpoints are routinely created after a profile is authored. Only
// actual Unix sockets are returned, so a broad sibling glob cannot
// accidentally turn ordinary files into read grants in an adapter.
//
// The agentd floor is not included. Adapters add it independently for every
// socket posture, preserving the floor even when the authored list is empty.
func MaterializeUnixSocketList(rules UnixSocketRules) (UnixSocketMaterialization, error) {
	if rules.Mode != AccessModeList {
		return UnixSocketMaterialization{}, nil
	}
	result := UnixSocketMaterialization{}
	protected, err := ProtectedPaths()
	if err != nil {
		return UnixSocketMaterialization{}, err
	}
	seen := make(map[string]struct{}, len(rules.Allow))
	for i, entry := range rules.Allow {
		selector := entry.Path
		candidates := []string{entry.Path}
		switch {
		case entry.Path != "":
		case entry.PathGlob != "":
			selector = entry.PathGlob
			matches, err := filepath.Glob(entry.PathGlob)
			if err != nil {
				return UnixSocketMaterialization{}, fmt.Errorf(
					"expand unix_sockets.allow[%d] glob %q: %w",
					i, entry.PathGlob, err)
			}
			candidates = matches
		default:
			return UnixSocketMaterialization{}, fmt.Errorf(
				"unix_sockets.allow[%d] has no path or path_glob", i)
		}
		materialized := false
		for _, candidate := range candidates {
			candidate = filepath.Clean(candidate)
			_, statErr := os.Lstat(candidate)
			if os.IsNotExist(statErr) {
				continue
			}
			if statErr != nil {
				return UnixSocketMaterialization{}, fmt.Errorf(
					"inspect unix socket allow path %q: %w", candidate, statErr)
			}
			resolved, statErr := filepath.EvalSymlinks(candidate)
			if os.IsNotExist(statErr) {
				continue
			}
			if statErr != nil {
				return UnixSocketMaterialization{}, fmt.Errorf(
					"resolve unix socket allow path %q: %w", candidate, statErr)
			}
			resolved = filepath.Clean(resolved)
			info, statErr := os.Lstat(resolved)
			if os.IsNotExist(statErr) {
				continue
			}
			if statErr != nil {
				return UnixSocketMaterialization{}, fmt.Errorf(
					"inspect resolved unix socket allow path %q: %w", resolved, statErr)
			}
			if info.Mode()&os.ModeSocket == 0 {
				continue
			}
			for _, denied := range protected {
				if GuardContainsOrEqual(denied, resolved) {
					return UnixSocketMaterialization{}, fmt.Errorf(
						"unix socket allow path %q resolves beneath protected directory %q",
						candidate, denied)
				}
			}
			materialized = true
			if _, exists := seen[resolved]; exists {
				continue
			}
			seen[resolved] = struct{}{}
			result.Paths = append(result.Paths, resolved)
		}
		if !materialized {
			result.Unmaterialized = append(result.Unmaterialized, selector)
			result.Entries = append(result.Entries, i)
		}
	}
	sort.Strings(result.Paths)
	return result, nil
}

// MaterializeUnixSocketPaths returns the live socket paths from one launch-time
// list materialization.
func MaterializeUnixSocketPaths(rules UnixSocketRules) ([]string, error) {
	result, err := MaterializeUnixSocketList(rules)
	return result.Paths, err
}

// PrepareUnixSocketLaunch resolves a rendered socket-list axis exactly once
// for handoff to both disclosure and the target adapter. A non-list axis has no
// materialized launch surface.
func PrepareUnixSocketLaunch(rules UnixSocketRules) (*UnixSocketMaterialization, error) {
	if rules.Mode != AccessModeList {
		return nil, nil
	}
	result, err := MaterializeUnixSocketList(rules)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// ValidateMaterializedUnixSocketPaths ensures a frozen launch surface cannot
// introduce a path outside the authored list. Newly appeared matching sockets
// are deliberately ignored: the frozen paths, and their notice, remain the
// one surface handed to the adapter.
func ValidateMaterializedUnixSocketPaths(
	rules UnixSocketRules,
	paths []string,
) error {
	if rules.Mode != AccessModeList {
		return fmt.Errorf(
			"materialized Unix-socket paths require unix_sockets mode %q",
			AccessModeList)
	}
	current, err := MaterializeUnixSocketList(rules)
	if err != nil {
		return err
	}
	allowed := make(map[string]struct{}, len(current.Paths))
	for _, path := range current.Paths {
		allowed[path] = struct{}{}
	}
	for _, path := range paths {
		if _, ok := allowed[path]; !ok {
			return fmt.Errorf(
				"materialized Unix-socket path %q is no longer a live socket selected by the authored allowlist",
				path)
		}
	}
	return nil
}

// UnixSocketLaunchNotice reports selectors omitted from the launch-time
// rendered surface. This is launch information, not a degradation-ladder
// outcome: the supported allowlist remains enforced over the sockets that
// actually exist.
func UnixSocketLaunchNotice(result *UnixSocketMaterialization) *AccessNotice {
	if result == nil || len(result.Unmaterialized) == 0 {
		return nil
	}
	quoted := make([]string, 0, len(result.Unmaterialized))
	for _, selector := range result.Unmaterialized {
		quoted = append(quoted, strconv.Quote(selector))
	}
	return &AccessNotice{
		Class:  AccessNoticeClassLaunch,
		Axis:   "unix_sockets",
		Reason: AccessNoticeReasonUnmaterializedEntries,
		Effect: AccessNoticeEffectNotMaterialized,
		Detail: fmt.Sprintf(
			"Unix-socket allowlist entries %s were not materialized for this launch; "+
				"missing, unmatched, or non-socket paths are not reachable",
			strings.Join(quoted, ", ")),
		Entries: append([]int(nil), result.Entries...),
	}
}

// intersectNetworkRules composes two already-normalized rules without ever
// widening either side. Baseline authority intersects while deny authority
// unions, so authoring order cannot affect the result:
// (B1-D1) ∩ (B2-D2) = (B1∩B2) - (D1∪D2).
func intersectNetworkRules(left, right NetworkRules) NetworkRules {
	out := intersectNetworkBaselines(left, right)
	out.Deny = unionNetworkEntries(left.Deny, right.Deny)
	out.Engine = mergeNetworkEngines(left.Engine, right.Engine)
	return out
}

// mergeNetworkEngines carries the engine across a merge that intersects
// everything else. The engine is not intersected, because it is not an access
// axis and there is no strictness to compare; it follows most-explicit-wins,
// with right as the more explicit layer.
//
// Both callers order their merges that way. Include flattening folds each
// include in turn and then the profile's own axes last, so a profile beats what
// it includes and a later include beats an earlier one. Tier composition folds
// global, then group, then explicit, which is the same precedence
// ResolveNetworkEngine applies — Resolve still settles the engine through that
// function afterwards, because it is the one that also reports the layers that
// lost.
//
// Carrying it here rather than dropping it is load-bearing: flattening happens
// BEFORE tier resolution, so an engine lost at this seam is gone before the
// resolution that would have disclosed it — the effective policy would name no
// engine, no composition notice would fire, and the launch would run the
// pre-engine default with nothing on the rendered surface to say so.
func mergeNetworkEngines(left, right NetworkEngine) NetworkEngine {
	if right != NetworkEngineUnset {
		return right
	}
	return left
}

func intersectNetworkBaselines(left, right NetworkRules) NetworkRules {
	switch {
	case left.Mode == AccessModeUnset:
		return NetworkRules{
			Mode: right.Mode, Allow: cloneNetworkRules(right).Allow,
		}
	case right.Mode == AccessModeUnset:
		return NetworkRules{
			Mode: left.Mode, Allow: cloneNetworkRules(left).Allow,
		}
	case left.Mode == AccessModeClosed || right.Mode == AccessModeClosed:
		return NetworkRules{Mode: AccessModeClosed}
	case left.Mode == AccessModeOpen:
		return NetworkRules{
			Mode: right.Mode, Allow: cloneNetworkRules(right).Allow,
		}
	case right.Mode == AccessModeOpen:
		return NetworkRules{
			Mode: left.Mode, Allow: cloneNetworkRules(left).Allow,
		}
	}
	out := NetworkRules{Mode: AccessModeList}
	seen := map[string]struct{}{}
	for _, a := range left.Allow {
		for _, b := range right.Allow {
			selector, ok := intersectNetworkSelector(a, b)
			if !ok {
				continue
			}
			ports, ok := intersectPorts(a.Ports, b.Ports)
			if !ok {
				continue
			}
			selector.Ports = ports
			key := networkEntryKey(selector)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			out.Allow = append(out.Allow, selector)
		}
	}
	sort.Slice(out.Allow, func(i, j int) bool {
		return networkEntryKey(out.Allow[i]) < networkEntryKey(out.Allow[j])
	})
	return out
}

func unionNetworkEntries(left, right []NetworkAllowEntry) []NetworkAllowEntry {
	if len(left) == 0 && len(right) == 0 {
		return nil
	}
	out := make([]NetworkAllowEntry, 0, len(left)+len(right))
	seen := make(map[string]struct{}, cap(out))
	for _, entries := range [][]NetworkAllowEntry{left, right} {
		for _, entry := range entries {
			key := networkEntryKey(entry)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			entry.Ports = append([]int(nil), entry.Ports...)
			out = append(out, entry)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return networkEntryKey(out[i]) < networkEntryKey(out[j])
	})
	return out
}

func intersectNetworkSelector(a, b NetworkAllowEntry) (NetworkAllowEntry, bool) {
	switch {
	case a.Host != "":
		if networkSelectorCovers(b, a) {
			a.Ports = nil
			return a, true
		}
	case b.Host != "":
		if networkSelectorCovers(a, b) {
			b.Ports = nil
			return b, true
		}
	case a.Domain != "" && b.Domain != "":
		switch {
		case networkSelectorCovers(a, b):
			b.Ports = nil
			return b, true
		case networkSelectorCovers(b, a):
			a.Ports = nil
			return a, true
		}
	case a.CIDR != "" && b.CIDR != "":
		ap, aErr := netip.ParsePrefix(a.CIDR)
		bp, bErr := netip.ParsePrefix(b.CIDR)
		if aErr == nil && bErr == nil && ap.Addr().BitLen() == bp.Addr().BitLen() {
			if ap.Contains(bp.Addr()) && ap.Bits() <= bp.Bits() {
				b.Ports = nil
				return b, true
			}
			if bp.Contains(ap.Addr()) && bp.Bits() <= ap.Bits() {
				a.Ports = nil
				return a, true
			}
		}
	case a.Loopback && b.Loopback:
		return NetworkAllowEntry{Loopback: true}, true
	}
	return NetworkAllowEntry{}, false
}

func networkSelectorCovers(cover, candidate NetworkAllowEntry) bool {
	if cover.Host != "" {
		return candidate.Host == cover.Host ||
			(candidate.Domain == cover.Host && !candidate.IncludeSubdomains)
	}
	if cover.Domain != "" {
		name := candidate.Host
		if name == "" {
			name = candidate.Domain
		}
		if name == cover.Domain {
			return !candidate.IncludeSubdomains || cover.IncludeSubdomains
		}
		return cover.IncludeSubdomains && strings.HasSuffix(name, "."+cover.Domain)
	}
	if cover.CIDR != "" && candidate.CIDR != "" {
		coverPrefix, coverErr := netip.ParsePrefix(cover.CIDR)
		candidatePrefix, candidateErr := netip.ParsePrefix(candidate.CIDR)
		return coverErr == nil && candidateErr == nil &&
			coverPrefix.Addr().BitLen() == candidatePrefix.Addr().BitLen() &&
			coverPrefix.Contains(candidatePrefix.Addr()) &&
			coverPrefix.Bits() <= candidatePrefix.Bits()
	}
	return cover.Loopback && candidate.Loopback
}

func intersectPorts(left, right []int) ([]int, bool) {
	if len(left) == 0 {
		return append([]int(nil), right...), true
	}
	if len(right) == 0 {
		return append([]int(nil), left...), true
	}
	rightSet := make(map[int]struct{}, len(right))
	for _, port := range right {
		rightSet[port] = struct{}{}
	}
	out := make([]int, 0, min(len(left), len(right)))
	for _, port := range left {
		if _, ok := rightSet[port]; ok {
			out = append(out, port)
		}
	}
	return out, len(out) > 0
}

func intersectUnixSocketRules(left, right UnixSocketRules) UnixSocketRules {
	switch {
	case left.Mode == AccessModeUnset:
		return cloneUnixSocketRules(right)
	case right.Mode == AccessModeUnset:
		return cloneUnixSocketRules(left)
	case left.Mode == AccessModeClosed || right.Mode == AccessModeClosed:
		return UnixSocketRules{Mode: AccessModeClosed}
	case left.Mode == AccessModeOpen:
		return cloneUnixSocketRules(right)
	case right.Mode == AccessModeOpen:
		return cloneUnixSocketRules(left)
	}
	out := UnixSocketRules{Mode: AccessModeList}
	seen := map[string]struct{}{}
	for _, a := range left.Allow {
		for _, b := range right.Allow {
			entry, ok := intersectSocketSelector(a, b)
			if !ok {
				continue
			}
			key := socketEntryKey(entry)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			out.Allow = append(out.Allow, entry)
		}
	}
	sort.Slice(out.Allow, func(i, j int) bool {
		return socketEntryKey(out.Allow[i]) < socketEntryKey(out.Allow[j])
	})
	return out
}

func intersectSocketSelector(a, b SocketAllowEntry) (SocketAllowEntry, bool) {
	if a == b {
		return a, true
	}
	if a.Path != "" && b.PathGlob != "" {
		if socketGlobMatches(b.PathGlob, a.Path) {
			return a, true
		}
	}
	if b.Path != "" && a.PathGlob != "" {
		if socketGlobMatches(a.PathGlob, b.Path) {
			return b, true
		}
	}
	return SocketAllowEntry{}, false
}

func socketGlobMatches(pattern, candidate string) bool {
	matched, err := path.Match(filepath.ToSlash(pattern), filepath.ToSlash(candidate))
	return err == nil && matched
}

func compositionNotice(axis string, tiers []string) AccessNotice {
	scope := strings.Join(tiers, " ∩ ")
	detail := fmt.Sprintf("%s access list composed to empty", axis)
	if scope != "" {
		detail += " across " + scope
	}
	if axis == "network" {
		detail += " — no outbound destination is allowed"
	} else {
		detail += " — no operator-listed Unix socket is allowed (the tclaude agent socket remains reachable)"
	}
	return AccessNotice{
		Class:  AccessNoticeClassComposition,
		Axis:   axis,
		Reason: AccessNoticeReasonEmptyIntersection,
		Effect: AccessNoticeEffectNothingAllowed,
		Detail: detail,
		Tiers:  append([]string(nil), tiers...),
	}
}

// MergeAccessNotices appends structurally distinct notices in order. Launch
// and lifecycle paths may recompute a target verdict while a snapshot already
// carries authoring/resolution notices; neither class may mask or duplicate the
// other.
func MergeAccessNotices(existing []AccessNotice, additions ...AccessNotice) []AccessNotice {
	out := cloneAccessNotices(existing)
	for _, addition := range additions {
		duplicate := false
		for _, current := range out {
			if current.Class == addition.Class &&
				current.Axis == addition.Axis &&
				current.Reason == addition.Reason &&
				current.Effect == addition.Effect &&
				current.Detail == addition.Detail &&
				slicesEqualInts(current.Entries, addition.Entries) &&
				slicesEqualStrings(current.Tiers, addition.Tiers) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			copy := addition
			copy.Entries = append([]int(nil), addition.Entries...)
			copy.Tiers = append([]string(nil), addition.Tiers...)
			out = append(out, copy)
		}
	}
	return out
}

// ReplaceAccessDegradationNotices keeps authoring/composition history while
// replacing the target-dependent degradation plan. A previous launch's
// widening must never become authority for newly resolved intent or a newly
// capable target on resume.
func ReplaceAccessDegradationNotices(
	existing []AccessNotice,
	current ...AccessNotice,
) []AccessNotice {
	kept := make([]AccessNotice, 0, len(existing)+len(current))
	for _, notice := range existing {
		if notice.Class != AccessNoticeClassDegradation {
			kept = append(kept, notice)
		}
	}
	out := MergeAccessNotices(kept, current...)
	if out == nil && (existing != nil || current != nil) {
		return []AccessNotice{}
	}
	return out
}

// ReplaceAccessLaunchNotices keeps durable authoring/composition and current
// degradation records while replacing filesystem-dependent launch
// disclosures. A socket that appears before a later launch must clear its
// earlier unmaterialized notice.
func ReplaceAccessLaunchNotices(
	existing []AccessNotice,
	current ...AccessNotice,
) []AccessNotice {
	kept := make([]AccessNotice, 0, len(existing)+len(current))
	for _, notice := range existing {
		if notice.Class != AccessNoticeClassLaunch {
			kept = append(kept, notice)
		}
	}
	out := MergeAccessNotices(kept, current...)
	if out == nil && (existing != nil || current != nil) {
		return []AccessNotice{}
	}
	return out
}

func slicesEqualInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func slicesEqualStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// UnconfinedAccessRulesNotice reports the disclosure owed when an
// implementation that confines nothing resolves a chain that authored access
// rules anyway.
//
// It lives here, not at either launch seam, because BOTH seams owe the same
// disclosure and they resolve their snapshots by different routes: the daemon
// spawn path plans access before forking, while a direct `tclaude session new`
// reaches the launch with the chain already resolved. One of them having a
// quieter idea of what is inert than the other is exactly the drift this
// function exists to prevent.
//
// It is silent when the chain authored nothing, so a limits-only profile does
// not acquire a warning about rules it never had — a notice that fires on
// every such launch teaches the operator to ignore it.
func UnconfinedAccessRulesNotice(
	implementation Implementation,
	effective EffectiveProfile,
) (AccessNotice, bool) {
	if !implementation.OmitsOSConfinement() {
		return AccessNotice{}, false
	}
	filesystem, err := FilesystemForLaunch(effective)
	if err != nil {
		filesystem = nil
	}
	authored := len(filesystem) > 0 ||
		len(effective.AgentDirectories) > 0 ||
		effective.Network != nil ||
		effective.UnixSockets != nil ||
		effective.NetworkAccess != NetworkAccessInherit
	if !authored {
		return AccessNotice{}, false
	}
	return AccessNotice{
		Class:  AccessNoticeClassDegradation,
		Axis:   "access_rules",
		Reason: AccessNoticeReasonUnconfinedImplementation,
		Effect: AccessNoticeEffectNotEnforced,
		// Deliberately says nothing about what IS enforced. This predicate
		// admits every unconfined implementation, and they do not agree on
		// that: `resource-only` holds a CPU/memory cgroup, `off` refuses
		// limits outright. Naming one implementation's guarantee in a message
		// the other can reach is how a disclosure becomes false later.
		Detail: fmt.Sprintf(
			"sandbox implementation %q enforces no access confinement; the resolved profile chain's filesystem, network and socket rules are recorded but NOT enforced for this launch.",
			implementation),
	}, true
}
