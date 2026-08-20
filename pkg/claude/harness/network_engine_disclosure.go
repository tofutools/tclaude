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

// ProxyEngineOpenCodeCarriageNotice is the per-harness half of §5.3, and it
// exists because "what the ENGINE carries" and "what THIS CLIENT uses" are two
// different facts that a single carriage sentence would smear together.
//
// SCOPE, and it is the whole correctness of this sentence. The measured fact is
// about OpenCode's OWN MODEL PATH and nothing else: 1.18.6 routes model traffic
// over HTTP CONNECT and ignores ALL_PROXY, measured under one-carriage
// isolation in TestOpenCodeProxyCarriageCooperation and reproduced behind the
// real floor in TestOpenCodeProxyFloorCooperation.
//
// It must NOT be widened into a claim about the sandbox. The launcher injects
// ALL_PROXY=socks5h://… into EVERY proxy-engine launch (proxyNetworkSandboxEnv)
// and the proxy serves SOCKS5 on that endpoint, so a tool or subprocess that
// honors it — curl, git, go, pip, an MCP stdio server — does use the SOCKS5
// carriage and IS decided by the authored policy. The floor arm's own
// in-namespace probe proves it inside an OpenCode launch: it carries its
// declared destination over SOCKS5 and the policy ALLOWS it.
//
// An earlier draft of this sentence said a SOCKS-needing destination "has no
// route out of this sandbox", which the cited run disproves and which would
// have told an operator an authored rule was dead when it is live. Over-stating
// what is blocked is not the safe direction: it invites authoring less policy,
// not more.
//
// The two plain-CLI harnesses carry no such sentence because their model path
// is not the open question — TestPinnedProxyToolEgress records their ordinary
// tool traffic carrying over BOTH carriages. No equivalent measurement exists
// for OpenCode's tools, and this sentence says so rather than guessing either
// way.
const ProxyEngineOpenCodeCarriageNotice = "This harness's own model requests use the HTTP CONNECT carriage only and ignore ALL_PROXY, " +
	"so an authored rule that only a SOCKS5-carried model request would reach is never exercised by it. " +
	"Tools and subprocesses that honor ALL_PROXY still use the SOCKS5 carriage and are filtered by the authored policy; " +
	"this harness's tool egress has not been measured."

// ProxyEngineNotActivatedNotice is the M2.3 half of the disclosure. The proxy
// engine is authorable before it is activated, and while its capability cells
// are unenforced this launch follows the standing TCL-823 ruling for a selected
// posture whose enforcement is unavailable: widen to open, disclosed. Naming
// the remedy is the point — an operator reading "nothing is enforced" needs to
// know it is pending activation evidence rather than a broken profile.
// It says "for this target" rather than naming the smokes as the only blocker,
// because activation is per harness, platform AND sandbox implementation: a
// configuration can be unactivated for a reason the carriage smokes will never
// change, and a notice promising that landing them is the remedy would be wrong
// for exactly those configurations.
const ProxyEngineNotActivatedNotice = "The proxy filtering engine is selected but not activated for this target: its capability cells stay unenforced here, " +
	"so these rules are not enforced and outbound network access remains open. " +
	"Activation is per harness, platform and sandbox implementation, and lands with the carriage smokes that prove each one."

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
	harnessName string,
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
		// Per-harness carriage, after the engine's own sentence and before the
		// activation one, so a reader gets: what the engine carries, what THIS
		// client uses of it, and whether the cells are backed.
		if notice := proxyEngineHarnessCarriageNotice(harnessName); notice != "" {
			sentences = append(sentences, notice)
		}
		if !activated {
			sentences = append(sentences, ProxyEngineNotActivatedNotice)
		}
	case deployed == sandboxpolicy.NetworkEngineUnset &&
		selected == sandboxpolicy.NetworkEngineProxy:
		sentences = append(sentences, ProxyEngineLatentSelectionNotice)
	}
	return strings.Join(sentences, " ")
}

// proxyEngineHarnessCarriageNotice returns the measured per-harness carriage
// sentence, or empty for a harness whose measurement adds nothing to the
// engine's own carriage notice.
//
// Keyed by harness for the same reason the activation record is: carriage is a
// fact about a client, measured per harness by a named smoke, and a sentence
// that applied to every harness would be describing the engine instead.
func proxyEngineHarnessCarriageNotice(harnessName string) string {
	if harnessName == OpenCodeName {
		return ProxyEngineOpenCodeCarriageNotice
	}
	return ""
}

// proxyEngineActivatedSmokes is the §8.3 activation record: per harness, the
// named CI smokes whose green run is the evidence for that harness and
// platform's `engine: proxy` capability cells.
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
// Codex was deliberately held back from the first activation (TCL-884) even
// though its arm of the same smokes was green in the same run, so that review
// could be about what the cells MEAN rather than about two harnesses at once.
// TCL-888 makes its record from that same evidence: nothing new is proven by
// adding the row, which is exactly the point — a cell flips when the record is
// made, and the record is made against runs already named.
//
// The two smokes are not the same KIND of evidence, and the row would be read
// wrong without saying so. TestPinnedProxyHarnessCooperation is the
// harness-dependent half: it has a real per-harness scenario that launches that
// harness's pinned binary and records which carriage reached its model origin.
// TestPinnedProxyToolEgress is floor evidence shared by every row — it exercises
// ordinary tool traffic through the floor and builds one launch spec, so it is
// cited by both harnesses because its subject is the floor rather than the
// client on top of it.
//
// OpenCode joins in TCL-891, and unlike Codex it DOES prove something new: its
// seam could not deploy this engine at all until that ticket generalized the
// inherited-descriptor contract, so its row rests on a smoke that had no way to
// exist before. Its evidence is therefore its own, not a second reading of the
// plain-CLI runs.
//
// With every registered harness now listed on Linux, the activation rule needs
// a subject that cannot evaporate — a boundary test whose unlisted set empties
// on success passes silently. TestProxyEngineActivationIsScopedToItsEvidence
// keeps it by asserting the record-to-cells coupling in BOTH directions over
// every registered harness and both platforms, and by failing if either side of
// that coupling has no subject at all. OpenCode's Darwin row cites its distinct
// agentd-owned server smoke rather than inheriting the plain-CLI evidence.
var proxyEngineActivatedSmokes = map[string]map[string][]string{
	"linux": {
		DefaultName: {
			"TestPinnedProxyHarnessCooperation",
			"TestPinnedProxyToolEgress",
		},
		CodexName: {
			"TestPinnedProxyHarnessCooperation",
			"TestPinnedProxyToolEgress",
		},
		// TCL-891. OpenCode's row cites a DIFFERENT harness-dependent smoke from
		// the two above, and that is the substance of the ticket rather than a
		// naming detail: it launches through the agentd-owned Unix-relay server
		// boundary, not as a plain CLI, and that boundary REFUSED this engine
		// outright until the inherited-descriptor contract was generalized to it.
		// TestOpenCodeProxyFloorCooperation is the first measurement of OpenCode
		// behind a real proxy floor that was possible at all — green named run
		// 30654121316, the run in which the smoke first executed.
		//
		// TestPinnedProxyToolEgress is cited here on the same terms it is cited
		// above: its subject is the FLOOR, not the client on top of it, and the
		// proxy floor is one construction shared by every row. It is NOT a claim
		// about OpenCode's own tools — see ProxyEngineOpenCodeCarriageNotice, which
		// says what is blocked for this harness rather than what was measured of
		// its tool egress.
		OpenCodeName: {
			"TestOpenCodeProxyFloorCooperation",
			"TestPinnedProxyToolEgress",
		},
	},
	"darwin": {
		DefaultName:  {"TestPinnedProxyHarnessCooperationDarwin"},
		CodexName:    {"TestPinnedProxyHarnessCooperationDarwin"},
		OpenCodeName: {"TestOpenCodeProxyCooperationDarwin"},
	},
}

// proxyEngineActivated reports whether this harness's proxy-engine cells have
// their evidence on this platform. Each platform has its own named smokes;
// evidence never crosses the platform boundary.
func proxyEngineActivated(harnessName, goos string) bool {
	_, activated := proxyEngineActivatedSmokes[goos][harnessName]
	return activated
}

// ProxyEngineActivated is proxyEngineActivated for callers outside this
// package. The launch seam needs it because a gate that keyed only on "the
// deployed engine is the proxy" would relax a launch on a platform whose proxy
// cells enforce nothing, turning a refusal into an open-network start.
func ProxyEngineActivated(harnessName, goos string) bool {
	return proxyEngineActivated(harnessName, goos)
}

// ProxyEngineActivationSmokes returns the smokes backing one harness's cells,
// for disclosure surfaces and tests. The result is a copy.
func ProxyEngineActivationSmokes(harnessName string) []string {
	return ProxyEngineActivationSmokesForPlatform(harnessName, "linux")
}

// ProxyEngineActivationSmokesForPlatform returns the evidence backing one
// harness's cells on one platform. The result is a copy.
func ProxyEngineActivationSmokesForPlatform(harnessName, goos string) []string {
	return append([]string(nil), proxyEngineActivatedSmokes[goos][harnessName]...)
}

// §5.1 and §5.2 selector details. The headline is the mirror-image relationship
// to the packet gateway, and both halves of it are stated rather than only the
// flattering one: host and domain LOSE the TTL/shared-IP caveat and become
// genuinely Full, while CIDR DROPS from Full to Partial.
const (
	// ProxyEngineNameSelectorDetail is why an ALLOW name rule is Full here and
	// only caveated Full under the packet gateway.
	//
	// It carries the private-destination blocker too. That blocker refuses an
	// authored name whose ANSWER lands in loopback, link-local, RFC1918, CGNAT
	// or other reserved space unless a cidr or loopback rule covers it — which
	// is narrower than the operator authored, and a profile that works under
	// the packet gateway can stop working here. Narrower is not a security
	// over-claim, but an undisclosed narrowing is still a rendered surface that
	// does not match the mechanism.
	ProxyEngineNameSelectorDetail = "Enforced on the host name the client requests, before resolution. " +
		"There is no DNS-lease caveat: no address-lease window exists, and a shared IP address grants no authority. " +
		"A name that resolves into loopback, link-local, private or other reserved address space is refused unless a cidr or loopback rule also covers it."

	// ProxyEngineDenyNameSelectorDetail is why a DENY name rule is Partial.
	//
	// The proxy decides on the identity the CLIENT states, and the client
	// chooses whether to state a name or an address. A name deny is never
	// matched against an IP literal — there is no TLS interception to recover
	// the name from — so a client that asks for the denied host's address
	// directly is not covered by the rule. Under a default-allow baseline that
	// literal is simply allowed; under a list baseline it is allowed whenever
	// some cidr rule covers the address.
	//
	// This is rated Partial UNCONDITIONALLY rather than only under an open
	// baseline, even though the escape needs a reachable literal. Whether one
	// is reachable depends on the whole rule set, and a cell that flipped
	// between Full and Partial as unrelated cidr rows were edited would be a
	// rating an operator cannot reason about. Partial with the escape named is
	// the honest, stable answer.
	ProxyEngineDenyNameSelectorDetail = "Enforced on the host name the client requests, before resolution, with no DNS-lease caveat. " +
		"It does not cover a client that asks for the address literally: a name deny is not matched against an IP literal, " +
		"so add a cidr deny for the addresses this name resolves to if the destination must be blocked by address as well."

	// ProxyEngineCIDRSelectorDetail is the honest cost of the L7 view.
	ProxyEngineCIDRSelectorDetail = "Enforced only when the client asks for this address literally, through the tclaude proxy. " +
		"A name that resolves into this range is not admitted by this rule, and UDP or a client that does not use the proxy is blocked entirely."

	// ProxyEngineDenyCIDRSelectorDetail is the DENY cidr cell's own string.
	// It exists because reusing the allow-shaped one (TCL-890) stated the
	// opposite of what the engine does: that detail says a name resolving into
	// the range is not admitted by the rule, and for a deny that is backwards.
	// Dialer.Connect asks EvaluateResolvedAddress per candidate, and that
	// re-applies cidr deny rows to the resolved literal under both baselines,
	// so a name resolving into a denied range IS refused, and the proxy
	// connects to the exact address it cleared.
	//
	// The rating stays PARTIAL, and it is worth recording why the tempting
	// raise to Full is wrong, because the correction above removes the reason
	// everyone remembers.
	//
	// TCL-890 disclosed TWO escapes here. One is now CLOSED:
	//
	//	A target in the host-loopback identity space is decided by the loopback
	//	rows alone — match() takes that branch before any cidr row is
	//	considered — so a cidr deny overlapping that space never applied. That
	//	shape is no longer authorable (TCL-899): the compiler refuses such a
	//	row against sandboxpolicy.PrefixIntersectsLoopbackRowAuthority, naming the
	//	loopback selector as the remedy, so an operator can no longer author a
	//	deny that silently never fires. The cell stopped disclosing it, because
	//	a disclosure that outlives its escape teaches a workaround for a hole
	//	that is not there.
	//
	// One remains, and it alone is what holds the rating down:
	//
	//	An address is not a destination. The same host restated in another
	//	address family (a NAT64 or 6to4 embedding of a denied v4 address) is a
	//	different address, and a v4 cidr row does not match it. Under an
	//	allowlist baseline the reserved-space blocker refuses those anyway;
	//	under a default-allow baseline they are reachable. TCL-899 does not
	//	cover this — that is routable embedded space, not host identity — so it
	//	stays disclosed rather than force-fixed.
	//
	// It is named in the string rather than left for an operator to find, with
	// the remedy that actually covers it.
	ProxyEngineDenyCIDRSelectorDetail = "Enforced on the address the connection actually uses, through the tclaude proxy: " +
		"a name that resolves into this range is refused as well, because every resolved address is matched against this rule before the connection is made. " +
		"It does not cover the same destination reached under a different address — a NAT64 or 6to4 form of a denied IPv4 address is not matched by an IPv4 rule — " +
		"so deny those forms too if they must be blocked. " +
		"UDP or a client that does not use the proxy is blocked entirely."

	// ProxyEngineLoopbackSelectorDetail states the improvement over the packet
	// gateway's synthetic host-loopback address.
	ProxyEngineLoopbackSelectorDetail = "Host loopback is reached through the filtering proxy on the authored ports. " +
		"Unlike the packet gateway, no synthetic address can substitute for this rule."

	// ProxyEngineLaunchCondition describes the proxy floor's launch gate. It
	// differs from the packet gateway's condition in the outcome, not just the
	// prerequisites: a host that cannot build this floor REFUSES the launch
	// rather than starting it with the rules unenforced.
	ProxyEngineLaunchCondition = "At launch, bubblewrap must create the mount, network, PID, and IPC namespaces this floor needs, and pidfds must be available. " +
		"A launch that cannot build the floor is refused rather than started with these rules unenforced. " +
		"No pasta, nft, or DNS broker is involved."
)
