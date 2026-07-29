package harness

import (
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
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

// ResolvedModelTransport is provider-aware input minted only after launch
// model/provider configuration has resolved. ProviderResolved prevents an
// empty context from masquerading as proof of the harness default. BaseURL is
// optional only for a known first-party provider.
type ResolvedModelTransport struct {
	Model            string `json:"model,omitempty"`
	Provider         string `json:"provider,omitempty"`
	BaseURL          string `json:"base_url,omitempty"`
	ProviderResolved bool   `json:"provider_resolved"`
}

// ModelTransportResolver is deliberately separate from ModelCatalog. A model
// token can be valid while its provider endpoint is unknown; filtered launch
// must distinguish those cases and refuse the latter rather than widening.
type ModelTransportResolver interface {
	ResolveModelTransport(resolved ResolvedModelTransport) (ModelTransportRequirement, error)
}

type staticModelTransport struct {
	provider     string
	template     string
	baseURLHost  string
	destinations []sandboxpolicy.NetworkAllowEntry
}

func (r staticModelTransport) ResolveModelTransport(
	resolved ResolvedModelTransport,
) (ModelTransportRequirement, error) {
	if !resolved.ProviderResolved {
		return ModelTransportRequirement{}, fmt.Errorf(
			"model provider configuration was not resolved")
	}
	provider := strings.ToLower(strings.TrimSpace(resolved.Provider))
	if provider != r.provider {
		if strings.TrimSpace(resolved.BaseURL) == "" {
			return ModelTransportRequirement{}, fmt.Errorf(
				"provider %q has no concrete resolved endpoint", resolved.Provider)
		}
		return customModelTransportRequirement(
			resolved.BaseURL, "resolved "+provider+" provider endpoint")
	}
	if strings.TrimSpace(resolved.BaseURL) != "" {
		custom, err := customModelTransportRequirement(
			resolved.BaseURL, "resolved "+provider+" provider endpoint")
		if err != nil {
			return ModelTransportRequirement{}, err
		}
		if len(custom.Destinations) != 1 ||
			!strings.EqualFold(custom.Destinations[0].Domain, r.baseURLHost) ||
			!slices.Equal(custom.Destinations[0].Ports, []int{443}) {
			return custom, nil
		}
	}
	return ModelTransportRequirement{
		Template:     r.template,
		Destinations: cloneNetworkDestinations(r.destinations),
		ResolvedBy:   "harness default endpoint",
	}, nil
}

type unresolvedOpenCodeModelTransport struct{}

func (unresolvedOpenCodeModelTransport) ResolveModelTransport(
	resolved ResolvedModelTransport,
) (ModelTransportRequirement, error) {
	if resolved.ProviderResolved && strings.TrimSpace(resolved.BaseURL) != "" {
		return customModelTransportRequirement(
			resolved.BaseURL, "resolved OpenCode provider endpoint")
	}
	return ModelTransportRequirement{}, fmt.Errorf(
		"OpenCode model %q does not expose a concrete resolved provider endpoint at the harness descriptor seam; "+
			"OpenCode effective-config model transport resolution is tracked in TCL-826; use Claude Code or Codex with a resolvable provider, or use network open",
		displayResolvedModel(resolved.Model),
	)
}

// IsLocalAccessNetworkPreset recognizes the exact strict Local access wire
// shape. Port-scoped or repeated loopback entries are ordinary Access lists.
func IsLocalAccessNetworkPreset(rules sandboxpolicy.NetworkRules) bool {
	return rules.Mode == sandboxpolicy.AccessModeList &&
		len(rules.Allow) == 1 &&
		rules.Allow[0].Loopback &&
		rules.Allow[0].Host == "" &&
		rules.Allow[0].Domain == "" &&
		rules.Allow[0].CIDR == "" &&
		len(rules.Allow[0].Ports) == 0
}

// IsLocalModelAPIsNetworkPreset recognizes the daemon-normalized wire shape
// behind the editor's Local + model APIs convenience. It is intentionally
// exact: any extra selector, port, subdomain widening, or destination is an
// ordinary Access list.
func IsLocalModelAPIsNetworkPreset(rules sandboxpolicy.NetworkRules) bool {
	if rules.Mode != sandboxpolicy.AccessModeList || len(rules.Allow) != 3 {
		return false
	}
	seenLoopback := false
	seenDomains := map[string]bool{}
	for _, entry := range rules.Allow {
		switch {
		case entry.Loopback:
			if seenLoopback || entry.Host != "" || entry.Domain != "" ||
				entry.CIDR != "" || len(entry.Ports) != 0 {
				return false
			}
			seenLoopback = true
		case entry.Domain == "api.anthropic.com" ||
			entry.Domain == "api.openai.com":
			if entry.Host != "" || entry.CIDR != "" ||
				entry.IncludeSubdomains || entry.Loopback ||
				!slices.Equal(entry.Ports, []int{443}) ||
				seenDomains[entry.Domain] {
				return false
			}
			seenDomains[entry.Domain] = true
		default:
			return false
		}
	}
	return seenLoopback &&
		seenDomains["api.anthropic.com"] &&
		seenDomains["api.openai.com"]
}

// ResolveModelTransportRequirement invokes the harness-owned hook only after
// the launch model has been resolved. M2a establishes this contract and its
// refusal vocabulary; activation belongs to the smoke-gated filtered slices.
func ResolveModelTransportRequirement(
	h *Harness,
	resolved ResolvedModelTransport,
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
	if !resolved.ProviderResolved {
		return ModelTransportRequirement{}, modelTransportCapabilityError(
			h, "model provider configuration was not resolved; choose a resolvable provider or use network open",
		)
	}
	requirement, err := h.ModelTransport.ResolveModelTransport(resolved)
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
		if candidate.CIDR != "" && required.CIDR != "" {
			candidatePrefix, candidateErr := netip.ParsePrefix(candidate.CIDR)
			requiredPrefix, requiredErr := netip.ParsePrefix(required.CIDR)
			if candidateErr == nil && requiredErr == nil &&
				candidatePrefix.Addr().BitLen() == requiredPrefix.Addr().BitLen() &&
				candidatePrefix.Bits() <= requiredPrefix.Bits() &&
				candidatePrefix.Contains(requiredPrefix.Addr()) {
				return true
			}
		}
		if candidate.Loopback && required.Loopback {
			return true
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
	if name == "" {
		name = entry.CIDR
	}
	if entry.Loopback {
		name = sandboxpolicy.FilteredNetworkHostLoopbackName
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

func customModelTransportRequirement(
	rawURL, resolvedBy string,
) (ModelTransportRequirement, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return ModelTransportRequirement{}, fmt.Errorf(
			"resolved provider endpoint %q is invalid: %w", rawURL, err)
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return ModelTransportRequirement{}, fmt.Errorf(
			"resolved provider endpoint %q must use http or https", rawURL)
	}
	if parsed.User != nil || parsed.Hostname() == "" {
		return ModelTransportRequirement{}, fmt.Errorf(
			"resolved provider endpoint %q must have a host and no userinfo", rawURL)
	}
	port := 443
	if parsed.Scheme == "http" {
		port = 80
	}
	if rawPort := parsed.Port(); rawPort != "" {
		port, err = strconv.Atoi(rawPort)
		if err != nil || port < 1 || port > 65535 {
			return ModelTransportRequirement{}, fmt.Errorf(
				"resolved provider endpoint %q has an invalid port", rawURL)
		}
	}
	host := strings.ToLower(parsed.Hostname())
	destination := sandboxpolicy.NetworkAllowEntry{
		Domain: host, Ports: []int{port},
	}
	if address, parseErr := netip.ParseAddr(host); parseErr == nil {
		switch {
		case address.IsLoopback():
			destination = sandboxpolicy.NetworkAllowEntry{
				Loopback: true, Ports: []int{port},
			}
		case address.Is4():
			destination = sandboxpolicy.NetworkAllowEntry{
				CIDR: netip.PrefixFrom(address, 32).String(), Ports: []int{port},
			}
		default:
			destination = sandboxpolicy.NetworkAllowEntry{
				CIDR: netip.PrefixFrom(address, 128).String(), Ports: []int{port},
			}
		}
	} else if strings.EqualFold(host, "localhost") {
		destination = sandboxpolicy.NetworkAllowEntry{
			Loopback: true, Ports: []int{port},
		}
	} else if strings.EqualFold(host, sandboxpolicy.FilteredNetworkHostLoopbackName) {
		destination = sandboxpolicy.NetworkAllowEntry{
			Loopback: true, Ports: []int{port},
		}
	}
	ir, err := sandboxpolicy.CompileFilteredNetworkRules(sandboxpolicy.NetworkRules{
		Mode:  sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{destination},
	})
	if err != nil {
		return ModelTransportRequirement{}, fmt.Errorf(
			"resolved provider endpoint %q is not representable: %w", rawURL, err)
	}
	normalized := ir.Rules[0]
	destination = sandboxpolicy.NetworkAllowEntry{Ports: normalized.Ports}
	switch normalized.Selector {
	case sandboxpolicy.NetworkSelectorDomain:
		destination.Domain = normalized.Value
	case sandboxpolicy.NetworkSelectorCIDR:
		destination.CIDR = normalized.Value
	case sandboxpolicy.NetworkSelectorLoopback:
		destination.Loopback = true
	default:
		return ModelTransportRequirement{}, fmt.Errorf(
			"resolved provider endpoint %q produced unsupported selector %q",
			rawURL, normalized.Selector)
	}
	return ModelTransportRequirement{
		Destinations: []sandboxpolicy.NetworkAllowEntry{destination},
		ResolvedBy:   resolvedBy,
	}, nil
}

func displayResolvedModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return "(harness default)"
	}
	return model
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
