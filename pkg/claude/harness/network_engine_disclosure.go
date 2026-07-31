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
// The activated flag decides whether the not-yet-activated sentence still
// applies. It is passed in rather than re-derived because the SAME row already
// decided it: a disclosure that re-answered the activation question could tell
// an operator the rules are unenforced while the cells beside it say Full.
func networkEngineDisclosure(
	selected, deployed sandboxpolicy.NetworkEngine,
	activated bool,
) string {
	if selected == sandboxpolicy.NetworkEngineUnset {
		return ""
	}
	sentences := []string{
		"Filtering engine: " + sandboxpolicy.NetworkEngineLabel(selected) + ".",
	}
	switch {
	case deployed == sandboxpolicy.NetworkEngineProxy:
		// The carriage notice is §5.3 and stays for good: it describes what the
		// engine carries, which does not change when its cells are activated.
		// The not-activated sentence is the one that retires.
		sentences = append(sentences, ProxyEngineCarriageNotice)
		if !activated {
			sentences = append(sentences, ProxyEngineNotActivatedNotice)
		}
	case deployed == sandboxpolicy.NetworkEngineUnset &&
		selected == sandboxpolicy.NetworkEngineProxy:
		sentences = append(sentences, ProxyEngineLatentSelectionNotice)
	}
	return strings.Join(sentences, " ")
}

// proxyEngineActivatedSmokes is the §8.3 activation record: per harness, the
// named CI smokes whose green run is the evidence for that harness's Linux
// `engine: proxy` capability cells.
//
// This map IS the capability matrix row. A harness absent from it has no
// evidence and keeps `EnforceNone`, which is the honest rating for "not
// measured" rather than a pessimistic one — the proposal's activation rule is
// that a cell flips only in the PR carrying its green smoke, and never outlives
// it. The smoke names are recorded here rather than only in a document so that
// renaming a smoke without renaming this row is a visible edit at the seam that
// depends on it, and so a reader of the capability table can find the run that
// justifies it.
//
// Codex is deliberately absent from this first activation even though its arm
// of the same smokes runs green in the same CI run. Flipping one harness at a
// time keeps the review on what the cells MEAN rather than on two harnesses at
// once; adding Codex here is a one-line follow-up backed by that same run.
//
// OpenCode is absent for a different and stronger reason: it launches through
// the agentd-owned server boundary rather than as a plain CLI, so its
// cooperation arm belongs beside the existing OpenCode executor smoke. Until
// that exists it has no evidence at all.
var proxyEngineActivatedSmokes = map[string][]string{
	DefaultName: {
		"TestPinnedProxyHarnessCooperation",
		"TestPinnedProxyToolEgress",
	},
}

// proxyEngineActivated reports whether this harness's proxy-engine cells have
// their evidence on this platform. Linux only: the Darwin floor is M3, and it
// activates on its own Seatbelt smokes rather than inheriting these.
func proxyEngineActivated(harnessName, goos string) bool {
	if goos != "linux" {
		return false
	}
	_, activated := proxyEngineActivatedSmokes[harnessName]
	return activated
}

// ProxyEngineActivationSmokes returns the smokes backing one harness's cells,
// for disclosure surfaces and tests. The result is a copy.
func ProxyEngineActivationSmokes(harnessName string) []string {
	return append([]string(nil), proxyEngineActivatedSmokes[harnessName]...)
}

// §5.1 and §5.2 selector details. The headline is the mirror-image relationship
// to the packet gateway, and both halves of it are stated rather than only the
// flattering one: host and domain LOSE the TTL/shared-IP caveat and become
// genuinely Full, while CIDR DROPS from Full to Partial.
const (
	// ProxyEngineNameSelectorDetail is why a name rule is Full here and only
	// caveated Full under the packet gateway.
	ProxyEngineNameSelectorDetail = "Enforced on the host name the client requests, before resolution. " +
		"There is no DNS-lease caveat: no address-lease window exists, and a shared IP address grants no authority."

	// ProxyEngineCIDRSelectorDetail is the honest cost of the L7 view.
	ProxyEngineCIDRSelectorDetail = "Enforced only when the client asks for this address literally, through the tclaude proxy. " +
		"A name that resolves into this range is not admitted by this rule, and UDP or a client that does not use the proxy is blocked entirely."

	// ProxyEngineLoopbackSelectorDetail states the improvement over the packet
	// gateway's synthetic host-loopback address.
	ProxyEngineLoopbackSelectorDetail = "Host loopback is reached through the filtering proxy on the authored ports. " +
		"Unlike the packet gateway, no synthetic address can substitute for this rule."

	// ProxyEngineLaunchCondition describes the proxy floor's launch gate. It
	// differs from the packet gateway's condition in the outcome, not just the
	// prerequisites: a host that cannot build this floor REFUSES the launch
	// rather than starting it with the rules unenforced.
	ProxyEngineLaunchCondition = "At launch, bubblewrap must create the mount, network and PID namespaces this floor needs, and pidfds must be available. " +
		"A launch that cannot build the floor is refused rather than started with these rules unenforced. " +
		"No pasta, nft, or DNS broker is involved."
)
