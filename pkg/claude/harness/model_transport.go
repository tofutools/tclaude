package harness

import (
	"fmt"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

const SandboxCapabilityModelTransport = "unsupported_filtered_model_transport"

// ModelTransportRequirement is the minimum resolved model/control-plane egress
// for one launch. It is an inspection result, never an implicit policy union.
type ModelTransportRequirement struct {
	Template     string                            `json:"template,omitempty"`
	Destinations []sandboxpolicy.NetworkAllowEntry `json:"destinations"`
	ResolvedBy   string                            `json:"resolved_by"`
}

// ModelTransportResolver is deliberately separate from ModelCatalog. A model
// token can be valid while its provider endpoint is unknown; filtered launch
// must distinguish those cases and refuse the latter rather than widening.
type ModelTransportResolver interface {
	ResolveModelTransport(model string) (ModelTransportRequirement, error)
}

type staticModelTransport struct {
	template     string
	destinations []sandboxpolicy.NetworkAllowEntry
}

func (r staticModelTransport) ResolveModelTransport(string) (ModelTransportRequirement, error) {
	return ModelTransportRequirement{
		Template:     r.template,
		Destinations: cloneNetworkDestinations(r.destinations),
		ResolvedBy:   "harness default endpoint",
	}, nil
}

type unresolvedOpenCodeModelTransport struct{}

func (unresolvedOpenCodeModelTransport) ResolveModelTransport(model string) (ModelTransportRequirement, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		model = "(harness default)"
	}
	return ModelTransportRequirement{}, fmt.Errorf(
		"OpenCode model %q does not expose a resolved provider endpoint at the harness descriptor seam",
		model,
	)
}

// ResolveModelTransportRequirement invokes the harness-owned hook only after
// the launch model has been resolved. M2a establishes this contract and its
// refusal vocabulary; activation belongs to the smoke-gated filtered slices.
func ResolveModelTransportRequirement(
	h *Harness,
	resolvedModel string,
) (ModelTransportRequirement, error) {
	if h == nil {
		return ModelTransportRequirement{}, &SandboxCapabilityError{
			Kind:    SandboxCapabilityModelTransport,
			Message: "filtered network cannot resolve model traffic without a harness",
		}
	}
	if h.ModelTransport == nil {
		return ModelTransportRequirement{}, modelTransportCapabilityError(
			h, "the harness has no resolved model-transport hook",
		)
	}
	requirement, err := h.ModelTransport.ResolveModelTransport(resolvedModel)
	if err != nil {
		return ModelTransportRequirement{}, modelTransportCapabilityError(h, err.Error())
	}
	if len(requirement.Destinations) == 0 {
		return ModelTransportRequirement{}, modelTransportCapabilityError(
			h, "the resolved model transport named no destinations",
		)
	}
	return requirement, nil
}

// ValidateModelTransportCoverage applies the strict-explicit policy: required
// endpoints must already be covered by the authored list. It never appends or
// otherwise mutates policy.
func ValidateModelTransportCoverage(
	h *Harness,
	rules sandboxpolicy.NetworkRules,
	requirement ModelTransportRequirement,
) error {
	if rules.Mode != sandboxpolicy.AccessModeList {
		return modelTransportCapabilityError(
			h, `filtered model preflight requires network.mode "list"`,
		)
	}
	var missing []string
	for _, required := range requirement.Destinations {
		if networkRulesCoverDestination(rules.Allow, required) {
			continue
		}
		missing = append(missing, formatNetworkDestination(required))
	}
	if len(missing) == 0 {
		return nil
	}
	templateRemedy := "add the resolved endpoint"
	if requirement.Template != "" {
		templateRemedy = "include template " + requirement.Template
	}
	return modelTransportCapabilityError(h, fmt.Sprintf(
		"filtered network has no hidden model-traffic bypass; required model destinations not covered: %s; remedies: %s, add the resolved endpoint, choose a resolvable provider, or use network open",
		strings.Join(missing, ", "), templateRemedy,
	))
}

// DescribeModelTransportRequirement is the approved disclosure baseline. It
// becomes launch-visible only when a filtered applier actually consumes the
// list; showing it while M2a still widens to open would be untruthful.
func DescribeModelTransportRequirement(requirement ModelTransportRequirement) string {
	destinations := make([]string, 0, len(requirement.Destinations))
	for _, destination := range requirement.Destinations {
		destinations = append(destinations, formatNetworkDestination(destination))
	}
	detail := "Filtered network: profile allow list only; no hidden model-traffic bypass. Required model destinations: " +
		strings.Join(destinations, ", ")
	if requirement.Template != "" {
		detail += " (covered by " + requirement.Template + ")"
	}
	return detail + "."
}

func modelTransportCapabilityError(h *Harness, detail string) error {
	harnessName := ""
	if h != nil {
		harnessName = h.Name
	}
	return &SandboxCapabilityError{
		Harness: harnessName,
		Kind:    SandboxCapabilityModelTransport,
		Message: detail,
	}
}

func networkRulesCoverDestination(
	authored []sandboxpolicy.NetworkAllowEntry,
	required sandboxpolicy.NetworkAllowEntry,
) bool {
	for _, candidate := range authored {
		if !portsCover(candidate.Ports, required.Ports) {
			continue
		}
		requiredName := required.Host
		if requiredName == "" {
			requiredName = required.Domain
		}
		switch {
		case candidate.Host != "":
			if strings.EqualFold(candidate.Host, requiredName) {
				return true
			}
		case candidate.Domain != "":
			if strings.EqualFold(candidate.Domain, requiredName) {
				return true
			}
			if candidate.IncludeSubdomains &&
				strings.HasSuffix(strings.ToLower(requiredName), "."+strings.ToLower(candidate.Domain)) {
				return true
			}
		}
	}
	return false
}

func portsCover(authored, required []int) bool {
	if len(authored) == 0 {
		return true
	}
	for _, port := range required {
		if !slices.Contains(authored, port) {
			return false
		}
	}
	return true
}

func formatNetworkDestination(entry sandboxpolicy.NetworkAllowEntry) string {
	name := entry.Host
	if name == "" {
		name = entry.Domain
	}
	if len(entry.Ports) == 0 {
		return name
	}
	ports := make([]string, 0, len(entry.Ports))
	for _, port := range entry.Ports {
		ports = append(ports, fmt.Sprintf("%d", port))
	}
	return name + ":" + strings.Join(ports, ",")
}

func cloneNetworkDestinations(
	in []sandboxpolicy.NetworkAllowEntry,
) []sandboxpolicy.NetworkAllowEntry {
	out := make([]sandboxpolicy.NetworkAllowEntry, len(in))
	for i, entry := range in {
		out[i] = entry
		out[i].Ports = append([]int(nil), entry.Ports...)
	}
	return out
}
