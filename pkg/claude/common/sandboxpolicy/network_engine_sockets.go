package sandboxpolicy

import (
	"path/filepath"
	"sort"
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

// NetworkEngineResolverSocketConflict reports the first authored Unix-socket
// selector that would hand a known resolver back to a sandbox running under the
// proxy engine, together with the resolver path it reaches.
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

// NetworkEngineResolverSocketRefusal is the capability-phrased refusal text for
// one conflict, with its remedies named.
func NetworkEngineResolverSocketRefusal(selector, resolver string) string {
	return "missing capability proxy_engine_name_authority: the Proxy filter engine decides host and domain rules on the name the sandbox asks for, " +
		"and the authored Unix-socket rule " + selector + " reaches the system resolver at " + resolver + ", " +
		"which converts names to addresses inside the sandbox and leaves those rules with no name to decide; " +
		"remove that socket rule, or select the Packet filter engine, whose DNS broker holds name authority with a resolver socket present"
}
