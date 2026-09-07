package sandboxpolicy

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/tofutools/tclaude/internal/backend/model"
)

func validateNetwork(n *model.SandboxNetwork) error {
	if n == nil {
		return nil
	}
	switch n.Baseline {
	case model.SandboxNetworkInherit, model.SandboxNetworkAllow, model.SandboxNetworkDeny:
	default:
		return fmt.Errorf("network requires inherit, allow, or deny baseline")
	}
	if n.Baseline == model.SandboxNetworkInherit && len(n.Allow)+len(n.Deny)+len(n.Packs)+len(n.DenyPacks) > 0 {
		return fmt.Errorf("inherited network baseline cannot contain destination rules")
	}
	switch n.Namespace {
	case "", "host", "private":
	default:
		return fmt.Errorf("invalid network namespace")
	}
	switch n.Engine {
	case "", model.SandboxNetworkPacket, model.SandboxNetworkProxy:
	default:
		return fmt.Errorf("invalid network engine")
	}
	if len(n.Allow) > 128 || len(n.Deny) > 128 || len(n.Packs) > 32 || len(n.DenyPacks) > 32 {
		return fmt.Errorf("network rule or pack limit exceeded")
	}
	packs := map[string]bool{}
	for _, id := range n.Packs {
		if len(id) > 128 || !setupName.MatchString(id) {
			return fmt.Errorf("invalid network pack identifier")
		}
		packs[id] = true
	}
	for _, id := range n.DenyPacks {
		if len(id) > 128 || !setupName.MatchString(id) {
			return fmt.Errorf("invalid network pack identifier")
		}
		if packs[id] {
			return fmt.Errorf("network pack appears in both allow and deny sets")
		}
	}
	for _, entries := range [][]model.SandboxDestination{n.Allow, n.Deny} {
		for _, entry := range entries {
			if err := validateDestination(entry); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateDestination(d model.SandboxDestination) error {
	selectors := 0
	for _, v := range []string{d.Host, d.Domain, d.CIDR} {
		if v != "" {
			selectors++
		}
	}
	if d.Loopback {
		selectors++
	}
	if selectors != 1 {
		return fmt.Errorf("network destination requires exactly one host, domain, CIDR, or loopback selector")
	}
	if d.IncludeSubdomains && d.Domain == "" {
		return fmt.Errorf("subdomains require a domain selector")
	}
	for _, v := range []string{d.Host, d.Domain} {
		if v != "" {
			if err := dnsName(v); err != nil {
				return err
			}
		}
	}
	if d.CIDR != "" {
		prefix, err := netip.ParsePrefix(d.CIDR)
		if err != nil {
			return fmt.Errorf("invalid network CIDR")
		}
		// Loopback and unspecified addresses can reach the host. Preserve the
		// explicit loopback-row boundary for IPv4, IPv6, and mapped IPv4 spellings.
		for _, protected := range loopbackAuthority {
			if prefix.Overlaps(protected) {
				return fmt.Errorf("CIDR intersects loopback authority; use an explicit loopback selector")
			}
		}
	}
	if len(d.Ports) > 16 {
		return fmt.Errorf("network destination exceeds 16 ports")
	}
	for _, port := range d.Ports {
		if port == 0 {
			return fmt.Errorf("network port must be in 1..65535")
		}
	}
	return nil
}

var loopbackAuthority = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("0.0.0.0/32"),
	netip.MustParsePrefix("::1/128"), netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::ffff:127.0.0.0/104"), netip.MustParsePrefix("::ffff:0.0.0.0/128"),
}

func dnsName(value string) error {
	if len(value) > 253 || !literal(value) || value != strings.TrimSpace(value) {
		return fmt.Errorf("invalid network DNS name")
	}
	if _, err := netip.ParseAddr(value); err == nil {
		return fmt.Errorf("IP literals require a CIDR or loopback selector")
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("invalid network DNS label")
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' {
				return fmt.Errorf("network DNS labels must be ASCII letters, digits, or hyphens")
			}
		}
	}
	return nil
}
