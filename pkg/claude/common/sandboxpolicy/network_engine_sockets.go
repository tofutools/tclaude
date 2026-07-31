package sandboxpolicy

import (
	"path/filepath"
	"sort"
	"strings"
)

// knownResolverSocketPaths are the socket-backed NSS resolution services the
// proxy engine's floor deliberately leaves unreachable, in both their /run and
// /var/run spellings.
//
// The proxy engine holds name authority: it decides an authored host or domain
// rule on the name the client asks for, before any resolution happens, and the
// floor synthesizes a loopback-only /etc/hosts so nothing inside the sandbox
// can turn a name into a literal on its own (see ProxyNetworkHostsFile). The
// boundary of that guarantee is stated at the synthesis site: the sandbox still
// inherits the host's nsswitch configuration and NSS modules, so authorizing
// one of these sockets restores exactly the name-to-literal conversion the
// hosts file exists to prevent. A resolved name becomes a literal, the literal
// is offered to the proxy as a CIDR-shaped target, and the host/domain rules
// that were supposed to be authoritative never see the name.
//
// Two authored axes reach these inodes and both are checked against THIS list:
// the unix_sockets axis, which authorizes a socket by path, and the filesystem
// axis, whose grants bind the directory the socket lives in. Adding a resolver
// here closes it on both at once, which is the reason the list is not split.
//
// This is a KNOWN-sockets list, not a proof of exhaustiveness. It refuses the
// resolvers a real host actually ships; it cannot refuse an arbitrary private
// resolver an operator builds. The engine's name authority is therefore
// defended here and disclosed at the synthesis site, not claimed absolute.
var knownResolverSocketPaths = []string{
	"/run/nscd/socket",
	"/run/systemd/resolve/io.systemd.Resolve",
	"/run/systemd/resolve/io.systemd.Resolve.Monitor",
	"/var/lib/sss/pipes/nss",
	"/var/run/nscd/socket",
	"/var/run/systemd/resolve/io.systemd.Resolve",
	"/var/run/systemd/resolve/io.systemd.Resolve.Monitor",
}

// KnownResolverSocketPaths returns the refused paths, sorted, for disclosure
// and tests.
func KnownResolverSocketPaths() []string {
	out := append([]string(nil), knownResolverSocketPaths...)
	sort.Strings(out)
	return out
}

// NetworkEngineResolverSocketConflict reports the first authored Unix-socket selector that
// would hand a known resolver back to a sandbox running under the proxy engine,
// together with the resolver path it reaches.
//
// SCOPE: this checks the Unix-socket axis. The FILESYSTEM axis reaches the same
// socket inodes through a grant covering the resolver's directory, and is
// checked by NetworkEngineResolverFilesystemConflict below over this same list.
// The two are separate functions because they refuse different authored rows
// and therefore report different selectors; they are NOT separate lists, and a
// resolver added to knownResolverSocketPaths is refused on both axes at once.
//
// It is deliberately a pure function of authored policy: it matches selectors
// rather than probing the host, so the preview and the launch reach the same
// verdict on a machine where the resolver happens not to be running. A glob is
// matched against the known paths for the same reason a literal is — a
// selector that would cover the socket authorizes it the moment it appears.
//
// The engine passed in must be the DEPLOYED engine. A policy that deploys no
// engine has no name authority to defend: nothing is being filtered, so a
// resolver socket takes nothing away.
func NetworkEngineResolverSocketConflict(
	engine NetworkEngine,
	sockets UnixSocketRules,
) (selector string, resolver string, found bool) {
	if engine != NetworkEngineProxy || sockets.Mode != AccessModeList {
		return "", "", false
	}
	for _, entry := range sockets.Allow {
		if path := filepath.Clean(entry.Path); entry.Path != "" {
			for _, resolver := range knownResolverSocketPaths {
				if path == resolver {
					return entry.Path, resolver, true
				}
			}
			continue
		}
		if entry.PathGlob == "" {
			continue
		}
		for _, resolver := range knownResolverSocketPaths {
			if matched, err := filepath.Match(entry.PathGlob, resolver); err == nil &&
				matched {
				return entry.PathGlob, resolver, true
			}
		}
	}
	return "", "", false
}

// NetworkEngineResolverFilesystemConflict reports the first authored FILESYSTEM
// grant that hands a known resolver socket back to a sandbox running under the
// proxy engine, together with the resolver path it reaches.
//
// This is the other half of the same guarantee the Unix-socket check above
// defends, and it exists because the socket axis was never the only way to the
// inode. tclaude builds the sandbox root from the authored grants, so a grant
// covering a resolver's directory binds that directory — socket and all — into
// the namespace. A READ-ONLY bind does not help: read-only governs the
// filesystem operations, and connect(2) on a unix socket is not one of them. So
// access mode is deliberately not part of "reaches it"; only a deny is.
//
// Matching is on the HOST path, because that is the authority-bearing side and
// the side the inode lives on. A remapped grant (mount_path) still reaches the
// same inode, just at a different position inside the namespace, so remapping
// is not an escape from this check — it only changes where the socket appears.
//
// Shadowing is honored the way the mount renderer honors it: entries fold by
// GUEST path and the most specific one wins, so a deny that lands on the
// resolver's own guest position cancels a broader read/write grant that would
// otherwise expose it. A deny cannot be remapped (the profile layer refuses
// deny + mount_path), which is what keeps that comparison well defined.
//
// FALSE-POSITIVE DIRECTION, stated because it is a real cost and not a bug: a
// broad grant such as /run — or /, if a profile ever authored one — covers a
// known resolver path and therefore refuses the proxy engine, even for an
// operator who never intended to reach a resolver. That is the fail-closed
// direction: the alternative is a proxy engine that silently loses name
// authority on a policy that looked fine. The refusal names the remedy.
//
// TWO BOUNDARIES, both stated rather than claimed away. The list is a
// KNOWN-paths list rather than a proof of exhaustiveness (see the comment on
// knownResolverSocketPaths). And the MATCHING is lexical over canonicalized
// authored paths: symlinks are covered, because the profile layer resolves them
// before this check sees a grant, but a host BIND MOUNT of a resolver directory
// into a granted subtree is not. tclaude does not create such a mount, and an
// operator who has made one has already reshaped the host namespace beneath the
// grant they authored; the honest statement is that this refuses grants that
// name the resolver, not that it proves no path reaches it.
func NetworkEngineResolverFilesystemConflict(
	engine NetworkEngine,
	filesystem []FilesystemGrant,
) (selector string, resolver string, found bool) {
	if engine != NetworkEngineProxy {
		return "", "", false
	}
	for _, resolverPath := range knownResolverSocketPaths {
		// Per guest position, the most specific covering grant wins, exactly as
		// renderMountPlanSections folds entries. Anything less would let a
		// deliberate deny of the resolver read as a conflict, or — worse — let a
		// deny at an unrelated position cancel a grant it never shadows.
		type winner struct {
			access      Access
			selector    string
			specificity int
		}
		winners := map[string]winner{}
		for _, grant := range filesystem {
			grantPath := filepath.Clean(grant.Path)
			if grant.Path == "" || !pathCovers(grantPath, resolverPath) {
				continue
			}
			// The guest position this grant puts the resolver at. The suffix is
			// joined rather than concatenated so a grant rooted at "/" — whose
			// cleaned path has no trailing separator to reuse — cannot fuse the
			// mount path into the first path element.
			guest := filepath.Join(filepath.Clean(grant.GuestPath()),
				strings.TrimPrefix(resolverPath, grantPath))
			previous, seen := winners[guest]
			if seen {
				if previous.specificity > len(grantPath) {
					continue
				}
				// Equal specificity is the same authored position spelled
				// twice, which the renderer and the profile normalizer both
				// resolve by access rank — deny wins. Taking first-wins here
				// would report a conflict for rows whose rendered plan hides
				// the socket.
				if previous.specificity == len(grantPath) &&
					accessRank(previous.access) >= accessRank(grant.Access) {
					continue
				}
			}
			winners[guest] = winner{
				access:      grant.Access,
				selector:    grant.Path,
				specificity: len(grantPath),
			}
		}
		// Sorted so the reported selector is deterministic when a policy exposes
		// one resolver at more than one guest position.
		positions := make([]string, 0, len(winners))
		for position := range winners {
			positions = append(positions, position)
		}
		sort.Strings(positions)
		for _, position := range positions {
			if winners[position].access != AccessDeny {
				return winners[position].selector, resolverPath, true
			}
		}
	}
	return "", "", false
}

// pathCovers reports whether an authored directory path is the target path
// itself or an ancestor of it. Both sides are already cleaned absolute paths.
func pathCovers(authored, target string) bool {
	if authored == target {
		return true
	}
	if authored == "/" {
		return true
	}
	return strings.HasPrefix(target, authored+"/")
}

// NetworkEngineResolverFilesystemRefusal is the capability-phrased refusal text
// for one filesystem conflict, with its remedies named. It states the same
// missing capability as the socket refusal because the same guarantee is what
// fails; only the authored row that takes it away differs.
func NetworkEngineResolverFilesystemRefusal(selector, resolver string) string {
	return "missing capability proxy_engine_name_authority: the Proxy filter engine decides host and domain rules on the name the sandbox asks for, " +
		"and the authored filesystem grant " + selector + " binds the system resolver socket at " + resolver + " into the sandbox, " +
		"which converts names to addresses inside the sandbox and leaves those rules with no name to decide " +
		"(a read-only grant does not prevent this: read-only does not stop connect(2) on a socket); " +
		"narrow that grant so it does not cover " + resolver + ", deny " + resolver + " explicitly, or select the Packet filter engine, whose DNS broker holds name authority with a resolver socket present"
}

// NetworkEngineResolverSocketRefusal is the capability-phrased refusal text for
// one conflict, with its remedies named.
func NetworkEngineResolverSocketRefusal(selector, resolver string) string {
	return "missing capability proxy_engine_name_authority: the Proxy filter engine decides host and domain rules on the name the sandbox asks for, " +
		"and the authored rule " + selector + " reaches the system resolver at " + resolver + ", " +
		"which converts names to addresses inside the sandbox and leaves those rules with no name to decide; " +
		"remove or deny that rule, or select the Packet filter engine, whose DNS broker holds name authority with a resolver socket present"
}
