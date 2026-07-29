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
	MaxNetworkAllowEntries = 128
	MaxSocketAllowEntries  = 64
	MaxPortsPerEntry       = 16
	MaxHostBytes           = 253
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
	Mode     AccessMode          `json:"mode,omitempty"`
	Baseline NetworkBaseline     `json:"baseline,omitempty"`
	Packs    []string            `json:"packs,omitempty"`
	Allow    []NetworkAllowEntry `json:"allow,omitempty"`
}

// NetworkAllowEntry names exactly one outbound destination selector.
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
}

const (
	AccessNoticeClassDegradation = "degradation"
	AccessNoticeClassComposition = "composition"
	AccessNoticeClassLaunch      = "launch"

	AccessNoticeReasonEmptyIntersection     = "empty_intersection"
	AccessNoticeReasonUnmaterializedEntries = "unmaterialized_entries"
	AccessNoticeReasonFilteredPrerequisite  = "filtered_prerequisite_probe"
	AccessNoticeReasonFilteredModelTraffic  = "filtered_model_transport"
	// AccessNoticeReasonOperatorUnenforcedLaunchOverride records the
	// dashboard-only, fresh-spawn authorization to widen an otherwise-refused
	// closed network posture to open. The daemon-written one-shot launch
	// snapshot carries this exact notice through the forked session launcher;
	// profiles cannot author it.
	AccessNoticeReasonOperatorUnenforcedLaunchOverride = "operator_unenforced_launch_override"

	AccessNoticeEffectNotEnforced     = "not_enforced"
	AccessNoticeEffectEnforcedWider   = "enforced_wider"
	AccessNoticeEffectLaunchGated     = "launch_gated"
	AccessNoticeEffectNothingAllowed  = "nothing_allowed"
	AccessNoticeEffectNotMaterialized = "not_materialized"
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
	return ResolvedAxes{Network: network, UnixSockets: sockets}, nil
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
		if in.Baseline != NetworkBaselineDeny &&
			(len(in.Packs) > 0 || len(in.Allow) > 0) {
			return nil, fmt.Errorf(
				"network packs and allow entries are only valid with baseline %q",
				NetworkBaselineDeny,
			)
		}
	} else {
		if err := validateAccessMode("network", in.Mode); err != nil {
			return nil, err
		}
		if len(in.Packs) > 0 {
			return nil, fmt.Errorf("network.packs requires the compositional baseline representation")
		}
		if in.Mode != AccessModeList && len(in.Allow) > 0 {
			return nil, fmt.Errorf(`network.allow is only valid with mode "list"`)
		}
	}
	if len(in.Allow) > MaxNetworkAllowEntries {
		return nil, fmt.Errorf("network.allow has too many entries (maximum %d)", MaxNetworkAllowEntries)
	}
	packs, err := normalizeNetworkPackRefs(in.Packs)
	if err != nil {
		return nil, err
	}
	out := &NetworkRules{Mode: in.Mode, Baseline: in.Baseline, Packs: packs}
	seen := make(map[string]struct{}, len(in.Allow))
	for i, entry := range in.Allow {
		normalized, err := normalizeNetworkAllowEntry(entry, i)
		if err != nil {
			return nil, err
		}
		key := networkEntryKey(normalized)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out.Allow = append(out.Allow, normalized)
	}
	sort.Slice(out.Allow, func(i, j int) bool {
		return networkEntryKey(out.Allow[i]) < networkEntryKey(out.Allow[j])
	})
	if out.Baseline != "" {
		if _, err := MaterializeNetworkRules(*out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func normalizeNetworkAllowEntry(in NetworkAllowEntry, index int) (NetworkAllowEntry, error) {
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
			"network.allow[%d] must set exactly one of host, domain, cidr, loopback",
			index,
		)
	}
	if in.IncludeSubdomains && in.Domain == "" {
		return NetworkAllowEntry{}, fmt.Errorf(
			"network.allow[%d].include_subdomains is only valid with domain",
			index,
		)
	}
	out := in
	var err error
	if out.Host != "" {
		out.Host, err = normalizeDNSName(out.Host)
		if err != nil {
			return NetworkAllowEntry{}, fmt.Errorf("network.allow[%d].host: %w", index, err)
		}
	}
	if out.Domain != "" {
		out.Domain, err = normalizeDNSName(out.Domain)
		if err != nil {
			return NetworkAllowEntry{}, fmt.Errorf("network.allow[%d].domain: %w", index, err)
		}
	}
	if out.CIDR != "" {
		prefix, parseErr := netip.ParsePrefix(out.CIDR)
		if parseErr != nil {
			return NetworkAllowEntry{}, fmt.Errorf("network.allow[%d].cidr %q is invalid: %w", index, out.CIDR, parseErr)
		}
		prefix = prefix.Masked()
		if prefixIntersectsLoopback(prefix) {
			return NetworkAllowEntry{}, fmt.Errorf(
				`network.allow[%d].cidr covers loopback; use {"loopback": true} instead`,
				index,
			)
		}
		out.CIDR = prefix.String()
	}
	if len(out.Ports) > MaxPortsPerEntry {
		return NetworkAllowEntry{}, fmt.Errorf(
			"network.allow[%d].ports has too many entries (maximum %d)",
			index, MaxPortsPerEntry,
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
				"network.allow[%d].ports contains %d (want 1..65535)",
				index, port,
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

func prefixIntersectsLoopback(prefix netip.Prefix) bool {
	if prefix.Addr().Is4() {
		loopback := netip.MustParsePrefix("127.0.0.0/8")
		return prefixesIntersect(prefix, loopback)
	}
	for _, loopback := range []netip.Prefix{
		netip.MustParsePrefix("::1/128"),
		// IPv4-mapped IPv6 addresses use a 96-bit ::ffff: prefix. Retain
		// the IPv4 loopback /8 beneath it so both a single mapped address
		// and a wider mapped range must use the dedicated loopback selector.
		netip.MustParsePrefix("::ffff:127.0.0.0/104"),
	} {
		if prefixesIntersect(prefix, loopback) {
			return true
		}
	}
	return false
}

func prefixesIntersect(a, b netip.Prefix) bool {
	if a.Addr().BitLen() != b.Addr().BitLen() {
		return false
	}
	return a.Contains(b.Addr()) || b.Contains(a.Addr())
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
			if pathsIntersect(checkPath, denied) {
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
	if in.Allow == nil {
		out.Allow = nil
		return out
	}
	out.Allow = make([]NetworkAllowEntry, len(in.Allow))
	for i, entry := range in.Allow {
		out.Allow[i] = entry
		out.Allow[i].Ports = append([]int(nil), entry.Ports...)
	}
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
				if PathContainsOrEqual(denied, resolved) {
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
// widening either side. Unset is absorbed; closed dominates; two lists retain
// only selector and port overlap.
func intersectNetworkRules(left, right NetworkRules) NetworkRules {
	switch {
	case left.Mode == AccessModeUnset:
		return cloneNetworkRules(right)
	case right.Mode == AccessModeUnset:
		return cloneNetworkRules(left)
	case left.Mode == AccessModeClosed || right.Mode == AccessModeClosed:
		return NetworkRules{Mode: AccessModeClosed}
	case left.Mode == AccessModeOpen:
		return cloneNetworkRules(right)
	case right.Mode == AccessModeOpen:
		return cloneNetworkRules(left)
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
