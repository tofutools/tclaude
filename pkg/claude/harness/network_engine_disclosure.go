package harness

import (
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// Mechanism strings for the filtering engines, per proposal §1.3-2. The
// mechanism a disclosure prints must name what actually runs, so these belong
// to the branch that deploys a proxy and never to a policy that deploys none:
// an operator who selected the proxy engine and authored a loopback-only list
// must read the native host-loopback mechanism, because that is what the floor
// expresses on its own.
const (
	ProxyEngineLinuxMechanism = "tclaude-layer bubblewrap + supervised loopback filtering proxy"

	ProxyEngineDarwinMechanism = "tclaude-layer Seatbelt + host-side filtering proxy"
)

// ProxyEngineCarriageNotice is §5.3's whole-posture disclosure, phrased for
// what the engine carries once it is enforcing. It is deliberately narrower
// than "the sandbox has no network": SOCKS5 in v1 (decision d) means arbitrary
// proxy-aware TCP is genuinely carried and filtered, so the honest sentence
// separates proxy-aware traffic from traffic with no route out at all.
const ProxyEngineCarriageNotice = "The filtering proxy carries HTTP, HTTPS, and any TCP from a client that uses the proxy (SOCKS5). " +
	"UDP, QUIC, ICMP, and TCP from a client that ignores the proxy have no route out of this sandbox: " +
	"they are blocked rather than filtered."

// ProxyEngineNotActivatedNotice is the M2.3 half of the disclosure. The proxy
// engine is authorable before it is activated, and while its capability cells
// are unenforced this launch follows the standing TCL-823 ruling for a selected
// posture whose enforcement is unavailable: widen to open, disclosed. Naming
// the remedy is the point — an operator reading "nothing is enforced" needs to
// know it is pending activation evidence rather than a broken profile.
const ProxyEngineNotActivatedNotice = "The proxy filtering engine is selected but not yet activated: its capability cells stay unenforced " +
	"until the per-harness carriage smokes land, so these rules are not enforced here and outbound network access remains open."

// ProxyEngineLatentSelectionNotice is §1.3-4. Selecting an engine for a policy
// that needs no filtering is not an error; the selection is latent and takes
// effect the moment a rule makes the policy discriminating.
const ProxyEngineLatentSelectionNotice = "This policy needs no filtering engine, so none is deployed. " +
	"The selected Proxy filter would apply if you add a host, domain, CIDR, port, or deny rule."

// ProxyEngineEntryCarriageDetail is §5.3's per-entry disclosure for a
// destination whose authored ports are not obviously HTTP-ish. Those are the
// rows most likely to be reached by a client that never consults the proxy
// environment, which is the difference between "filtered" and "blocked".
const ProxyEngineEntryCarriageDetail = "reachable only by a client that uses the tclaude proxy (HTTP CONNECT or SOCKS5); " +
	"direct TCP from a proxy-unaware client, and all UDP, are blocked by the sandbox floor"

// httpishPorts are the ports whose clients can be assumed to speak HTTP and
// therefore to honor the proxy environment. Every other authored port earns the
// per-entry carriage caveat. An entry that authors NO port is not listed here
// as HTTP-ish or otherwise: the whole-posture notice already covers it, and
// inventing a per-row caveat for an unported entry would repeat that sentence
// on nearly every row.
var httpishPorts = map[int]bool{80: true, 443: true, 8080: true, 8443: true}

// networkEntryNeedsProxyCarriageCaveat reports whether one entry authors a port
// that an HTTP client would not be using.
func networkEntryNeedsProxyCarriageCaveat(
	entry sandboxpolicy.NetworkAllowEntry,
) bool {
	for _, port := range entry.Ports {
		if !httpishPorts[port] {
			return true
		}
	}
	return false
}

// proxyEngineMechanism names the proxy mechanism for one platform.
func proxyEngineMechanism(goos string) string {
	if goos == "darwin" {
		return ProxyEngineDarwinMechanism
	}
	return ProxyEngineLinuxMechanism
}

// networkEngineDisclosure builds the whole-posture engine sentence for the
// network axis, from the authored selection and the engine the policy actually
// deploys. Those are two different facts and the disclosure needs both: a
// selected engine that deploys nothing is the latent case, and a deployed
// engine is a live mechanism claim.
//
// An entirely unset selection produces the empty string. That is what keeps
// engine-unset configurations rendering exactly what they rendered before this
// field existed.
func networkEngineDisclosure(
	selected, deployed sandboxpolicy.NetworkEngine,
) string {
	if selected == sandboxpolicy.NetworkEngineUnset {
		return ""
	}
	sentences := []string{
		"Filtering engine: " + sandboxpolicy.NetworkEngineLabel(selected) + ".",
	}
	switch {
	case deployed == sandboxpolicy.NetworkEngineProxy:
		sentences = append(sentences,
			ProxyEngineCarriageNotice, ProxyEngineNotActivatedNotice)
	case deployed == sandboxpolicy.NetworkEngineUnset &&
		selected == sandboxpolicy.NetworkEngineProxy:
		sentences = append(sentences, ProxyEngineLatentSelectionNotice)
	}
	return strings.Join(sentences, " ")
}
