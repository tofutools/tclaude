package sandboxpolicy

import "fmt"

// NetworkEngine names how a discriminating network rule set is enforced. It is
// deliberately not an access axis: two profiles that agree on rules and
// disagree on engine authorize exactly the same destinations. They differ in
// mechanism, protocol carriage, and honest capability rating.
type NetworkEngine string

const (
	// NetworkEngineUnset is the fourth state: the layer expressed no opinion
	// and yields to the next one. Unset never changes behavior.
	NetworkEngineUnset NetworkEngine = ""
	// NetworkEnginePacket is the bubblewrap netns + nftables + pasta + DNS
	// broker gateway.
	NetworkEnginePacket NetworkEngine = "packet"
	// NetworkEngineProxy is the deny-default floor plus a tclaude-owned
	// host-side filtering proxy.
	NetworkEngineProxy NetworkEngine = "proxy"
)

// ValidateNetworkEngine rejects spellings outside the authored vocabulary.
func ValidateNetworkEngine(engine NetworkEngine) error {
	switch engine {
	case NetworkEngineUnset, NetworkEnginePacket, NetworkEngineProxy:
		return nil
	default:
		return fmt.Errorf(
			"network.engine %q is invalid (want packet, proxy, or omitted to inherit)",
			engine,
		)
	}
}

// NetworkEngineLayer names one profile scope in the composition stack.
type NetworkEngineLayer string

const (
	NetworkEngineLayerSession NetworkEngineLayer = "session"
	NetworkEngineLayerGroup   NetworkEngineLayer = "group"
	NetworkEngineLayerGlobal  NetworkEngineLayer = "global"
)

// networkEngineLayerRank orders the layers by explicitness. Precedence is a
// property of the layer, never of the order a caller happens to pass them in.
var networkEngineLayerRank = map[NetworkEngineLayer]int{
	NetworkEngineLayerSession: 0,
	NetworkEngineLayerGroup:   1,
	NetworkEngineLayerGlobal:  2,
}

// NetworkEngineSelection is one layer's authored engine opinion. Source is the
// human-readable profile identity used by disclosure ("group profile
// frontend-team"); it is never load-bearing for resolution.
type NetworkEngineSelection struct {
	Layer  NetworkEngineLayer
	Engine NetworkEngine
	Source string
}

// ResolvedNetworkEngine is the outcome of most-explicit-wins resolution. It
// carries everything the rendered surface needs to make a mechanism swap
// non-silent: the winner, the layer it came from, and every lower-precedence
// layer whose different engine lost.
type ResolvedNetworkEngine struct {
	Engine NetworkEngine
	// Layer and Source are empty when no layer named an engine.
	Layer  NetworkEngineLayer
	Source string
	// Overridden lists, most-explicit first, the layers that named an engine
	// other than the winner. Layers that agreed with the winner are absent:
	// agreement is not an override and must not be rendered as one.
	Overridden []NetworkEngineSelection
}

// NamesEngine reports whether any layer expressed an opinion.
func (r ResolvedNetworkEngine) NamesEngine() bool {
	return r.Engine != NetworkEngineUnset
}

// ResolveNetworkEngine applies decision (b): session-explicit > group >
// global, most explicit wins outright. Every layer that omits an engine is
// absorbed and yields to the next.
//
// This is deliberately not how mode/baseline/allow/deny compose. It is safe
// for one specific reason: an engine can neither widen nor narrow the authored
// destination set, so there is no strictness to compare. What does change is
// mechanism, protocol carriage, and the honest capability rating — which is
// why the losing layers are reported rather than discarded.
func ResolveNetworkEngine(
	selections []NetworkEngineSelection,
) (ResolvedNetworkEngine, error) {
	seen := make(map[NetworkEngineLayer]struct{}, len(selections))
	ordered := make([]NetworkEngineSelection, 0, len(selections))
	for _, selection := range selections {
		if _, known := networkEngineLayerRank[selection.Layer]; !known {
			return ResolvedNetworkEngine{}, fmt.Errorf(
				"network engine layer %q is invalid (want session, group, or global)",
				selection.Layer,
			)
		}
		if _, exists := seen[selection.Layer]; exists {
			return ResolvedNetworkEngine{}, fmt.Errorf(
				"network engine layer %q is named more than once", selection.Layer,
			)
		}
		seen[selection.Layer] = struct{}{}
		if err := ValidateNetworkEngine(selection.Engine); err != nil {
			return ResolvedNetworkEngine{}, fmt.Errorf(
				"%s profile: %w", selection.Layer, err,
			)
		}
		if selection.Engine == NetworkEngineUnset {
			continue
		}
		ordered = append(ordered, selection)
	}
	sortNetworkEngineSelections(ordered)
	if len(ordered) == 0 {
		return ResolvedNetworkEngine{}, nil
	}
	winner := ordered[0]
	out := ResolvedNetworkEngine{
		Engine: winner.Engine,
		Layer:  winner.Layer,
		Source: winner.Source,
	}
	for _, loser := range ordered[1:] {
		if loser.Engine == winner.Engine {
			continue
		}
		out.Overridden = append(out.Overridden, loser)
	}
	return out, nil
}

func sortNetworkEngineSelections(in []NetworkEngineSelection) {
	// Insertion sort over at most three layers; a stable order by rank is all
	// that is needed and this keeps the precedence rule readable in one place.
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 &&
			networkEngineLayerRank[in[j].Layer] < networkEngineLayerRank[in[j-1].Layer]; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}

// DeployedNetworkEngine answers the only engine question a launch or a preview
// ever needs: which filtering engine does THIS policy actually deploy?
//
// It is the composition of the two halves that must never be re-derived apart —
// the discrimination predicate below, and the authored engine selection resolved
// by ResolveNetworkEngine — and it is deliberately the single place they meet.
// Both the launch path and PredictAccessEnforcement call it, so a preview cannot
// name a mechanism the launch does not run.
//
// The unset result is load-bearing rather than a missing value: a policy that
// asks for no distinction between destinations deploys NO engine, whatever a
// layer selected. Selecting an engine for such a policy is not an error (the
// selection is latent and takes effect when a rule is added); it simply does not
// start a process. Selecting nothing under a discriminating policy keeps the
// packet gateway, which is what every launch ran before an engine existed.
//
// It requires materialized launch intent for the same reason the discrimination
// predicate does: a caller that forgot to expand packs fails closed rather than
// deploying by guess.
func DeployedNetworkEngine(
	rules NetworkRules,
	selected NetworkEngine,
) (NetworkEngine, error) {
	if err := ValidateNetworkEngine(selected); err != nil {
		return NetworkEngineUnset, err
	}
	discriminating, err := NetworkRulesAreDiscriminating(rules)
	if err != nil {
		return NetworkEngineUnset, err
	}
	if !discriminating {
		return NetworkEngineUnset, nil
	}
	if selected == NetworkEngineUnset {
		return NetworkEnginePacket, nil
	}
	return selected, nil
}

// NetworkRulesAreDiscriminating implements the proposal's Discriminating()
// predicate: the resolved policy asks for a distinction between destinations,
// and therefore needs a filtering engine deployed to make it.
//
// It is the single predicate consumed by both the launch path and enforcement
// prediction, so a preview can never claim a mechanism the launch does not
// run. It requires materialized launch intent: an unresolved baseline or an
// unexpanded pack reference is an error rather than a guess, so a caller that
// forgets to materialize fails closed instead of silently deploying nothing.
//
// Closed and unset postures are not discriminating even when they carry deny
// entries. A closed posture authorizes no destination at all, so there is
// nothing for a proxy to filter and the deny rows have no work to do; the
// deny-default floor alone expresses the policy exactly. An open posture with
// denies is discriminating, because the denies are the only thing separating
// reachable from unreachable.
func NetworkRulesAreDiscriminating(rules NetworkRules) (bool, error) {
	if rules.Baseline != "" || len(rules.Packs) > 0 || len(rules.DenyPacks) > 0 {
		return false, fmt.Errorf(
			"network discrimination requires materialized launch intent")
	}
	if err := validateAccessMode("network", rules.Mode); err != nil {
		return false, err
	}
	switch rules.Mode {
	case AccessModeUnset, AccessModeClosed:
		return false, nil
	case AccessModeOpen:
		return len(rules.Deny) > 0, nil
	case AccessModeList:
		if len(rules.Allow) == 0 {
			// An empty allow list under a deny baseline authorizes exactly
			// nothing, which is the closed posture by another spelling. Its
			// deny rows have no work to do and the floor expresses it alone.
			return false, nil
		}
		if len(rules.Deny) > 0 {
			return true, nil
		}
		for _, entry := range rules.Allow {
			// Loopback is the one selector the floor expresses natively on
			// both platforms, including its authored ports, so a loopback-only
			// list needs no engine. Every other selector names a destination
			// the floor cannot tell apart from any other.
			if !entry.Loopback {
				return true, nil
			}
		}
		return false, nil
	default:
		return false, fmt.Errorf("network mode %q is invalid", rules.Mode)
	}
}
