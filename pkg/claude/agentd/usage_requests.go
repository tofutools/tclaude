package agentd

import (
	"fmt"
	"math"
	"slices"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Usage requests are operator access requests for models.proxy: a peer's
// operator asks to bill model calls to this node's gateway, for a dollar
// amount and a time. The approver picks the gateway (when any was asked)
// and may lower the amount or shorten the time, never raise them.

// usageGateways are this node's enabled model gateways, the approver's
// choices.
func usageGateways() []string {
	cfg, err := config.Load()
	if err != nil || cfg == nil || cfg.Agent == nil || cfg.Agent.ModelProxyDisabled {
		return nil
	}
	var out []string
	for name, gw := range cfg.Agent.HTTPProxies {
		if gw.ModelPolicy != nil && gw.ModelPolicy.Enabled {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// validUsageUSD bounds a requested or granted amount (0: no cap).
func validUsageUSD(v float64) bool {
	return v >= 0 && v <= maxShareUSD && !math.IsInf(v, 0)
}

// usageAmountText is the request's shape for the approver: amount and
// gateway.
func usageAmountText(maxUSD float64, proxy string) string {
	amount := "no cap"
	if maxUSD > 0 {
		amount = fmt.Sprintf("up to $%.2f", maxUSD)
	}
	if proxy == "" {
		proxy = "any gateway"
	}
	return amount + " on " + proxy
}

// decideUsage applies an approval's gateway and amount to a usage request,
// refusing anything wider than what was asked.
func decideUsage(pa *db.FederationPeerAccessRequest, proxy *string, maxUSD *float64) error {
	if proxy != nil {
		pa.GrantProxy = *proxy
	}
	if maxUSD != nil {
		pa.GrantMaxUSD = *maxUSD
	}
	if pa.Proxy != "" && pa.GrantProxy != pa.Proxy {
		return fmt.Errorf("approval cannot change the requested gateway")
	}
	if pa.GrantProxy != "" && !slices.Contains(usageGateways(), pa.GrantProxy) {
		return fmt.Errorf("no such enabled model gateway")
	}
	if !validUsageUSD(pa.GrantMaxUSD) || pa.MaxUSD > 0 && (pa.GrantMaxUSD == 0 || pa.GrantMaxUSD > pa.MaxUSD) {
		return fmt.Errorf("approval cannot raise the requested amount")
	}
	return nil
}

// usageShareCapUSD is the cap an approved usage request writes: the amount,
// added to a live capped share for the same gateway ("$5 more"), whose spend
// keeps counting from its start. An expired share starts over
// (ApproveFederationPeerAccessRequest), so the amount is the whole cap.
func usageShareCapUSD(pa db.FederationPeerAccessRequest) float64 {
	if pa.GrantMaxUSD <= 0 {
		return 0
	}
	scope := ""
	if pa.GrantProxy != "" {
		scope = "http_proxy=" + pa.GrantProxy
	}
	grants, err := db.ListFederationPeerGrants(pa.Peer)
	if err != nil {
		return pa.GrantMaxUSD
	}
	for _, g := range grants {
		if g.Slug == PermModelsProxy && g.Scope == scope && g.SpawnPolicy.MaxUSD > 0 {
			return math.Min(maxShareUSD, g.SpawnPolicy.MaxUSD+pa.GrantMaxUSD)
		}
	}
	return pa.GrantMaxUSD
}
