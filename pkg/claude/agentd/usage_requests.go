package agentd

import (
	"fmt"
	"math"
	"slices"
	"strings"

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
	gateways := usageGateways()
	if len(gateways) == 0 {
		return fmt.Errorf("this node has no enabled model gateway to share")
	}
	if pa.GrantProxy != "" && !slices.Contains(gateways, pa.GrantProxy) {
		return fmt.Errorf("no such enabled model gateway")
	}
	if !validUsageUSD(pa.GrantMaxUSD) || pa.MaxUSD > 0 && (pa.GrantMaxUSD == 0 || pa.GrantMaxUSD > pa.MaxUSD) {
		return fmt.Errorf("approval cannot raise the requested amount")
	}
	return usageConflict(pa)
}

// usageConflict refuses an approval that would narrow the peer's access, or
// whose share another cap would keep from being used. Caps are each a limit
// of their own (modelShareGrants), so:
//   - a cap on a share that is uncapped would narrow it;
//   - a cap on every gateway would also limit a gateway the peer may use
//     with no cap;
//   - a share on one gateway does not lift a capped share on every gateway,
//     which still binds; adding to that one is what helps.
func usageConflict(pa *db.FederationPeerAccessRequest) error {
	if pa.GrantMaxUSD <= 0 {
		return nil
	}
	grants, err := db.ListFederationPeerGrants(pa.Peer)
	if err != nil {
		return fmt.Errorf("could not read the peer's grants")
	}
	scope := ""
	if pa.GrantProxy != "" {
		scope = "http_proxy=" + pa.GrantProxy
	}
	for _, g := range grants {
		if g.Slug != PermModelsProxy {
			continue
		}
		capped := g.SpawnPolicy.MaxUSD > 0
		switch {
		case g.Scope == scope && !capped:
			return fmt.Errorf("the peer already uses %s with no cap; extend or revoke that grant instead", gatewayText(pa.GrantProxy))
		case scope == "" && !capped:
			return fmt.Errorf("the peer uses gateway %s with no cap, and a cap on every gateway would limit it; approve for one gateway", strings.TrimPrefix(g.Scope, "http_proxy="))
		case scope != "" && g.Scope == "" && capped:
			return fmt.Errorf("the peer's share on every gateway (max $%.2f) also limits %s; approve for any gateway to add to it", g.SpawnPolicy.MaxUSD, pa.GrantProxy)
		}
	}
	return nil
}

func gatewayText(proxy string) string {
	if proxy == "" {
		return "every gateway"
	}
	return "gateway " + proxy
}
