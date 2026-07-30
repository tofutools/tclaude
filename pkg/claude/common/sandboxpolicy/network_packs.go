package sandboxpolicy

import (
	"fmt"
	"sort"
)

// NetworkPack is one release-owned set of outbound destinations. Profiles
// store its stable ID in either the allow or deny pack set; every resolution
// expands both polarities before policy composition.
type NetworkPack struct {
	ID      string
	Label   string
	Group   string
	Entries []NetworkAllowEntry
	Note    string
	Warning string
}

var networkPackRegistry = []NetworkPack{
	{
		ID: "net-local", Label: "Local access",
		Entries: []NetworkAllowEntry{{Loopback: true}},
		Note:    "Host-loopback model servers and local development services.",
		Warning: "OpenCode local-provider launches remain refused: they name no explicit provider endpoint to resolve.",
	},
	{
		ID: "net-anthropic", Label: "Anthropic API", Group: "Cloud model APIs",
		Entries: []NetworkAllowEntry{{Domain: "api.anthropic.com", Ports: []int{443}}},
		Note:    "Anthropic documents the direct Claude API at https://api.anthropic.com.",
	},
	{
		ID: "net-openai-codex", Label: "OpenAI API", Group: "Cloud model APIs",
		Entries: []NetworkAllowEntry{{Domain: "api.openai.com", Ports: []int{443}}},
		Note:    "The supported filtered Codex route uses api.openai.com for API-key model traffic.",
		Warning: "ChatGPT-auth Codex uses different destinations; add the ChatGPT sign-in pack for those. Custom providers and other networked features need their own destinations.",
	},
	{
		ID: "net-openai-chatgpt", Label: "ChatGPT (Codex sign-in)",
		Group: "Cloud model APIs",
		Entries: []NetworkAllowEntry{
			{Domain: "chatgpt.com", Ports: []int{443}},
			{Domain: "auth.openai.com", Ports: []int{443}},
		},
		Note:    "ChatGPT-signed-in Codex reaches its model endpoint at chatgpt.com and refreshes its token at auth.openai.com, per the named CI origin audit.",
		Warning: "A workspace that overrides chatgpt_base_url routes model traffic elsewhere and needs that destination authored explicitly.",
	},
	{
		ID: "net-github", Label: "GitHub essentials",
		Entries: []NetworkAllowEntry{
			{Domain: "github.com"},
			{Domain: "api.github.com"},
			{Domain: "codeload.github.com"},
		},
		Note:    "Essential GitHub API and source-download destinations.",
		Warning: "GitHub Actions, releases, LFS, packages, and artifacts need additional destinations.",
	},
	{
		ID: "net-go-modules", Label: "Public Go modules",
		Entries: []NetworkAllowEntry{
			{Domain: "proxy.golang.org"},
			{Domain: "sum.golang.org"},
		},
		Note:    "The public Go module proxy and checksum database.",
		Warning: "The default GOPROXY may fall back to direct VCS hosts; this pack intentionally omits them.",
	},
	{
		ID: "net-npm", Label: "Public npm registry",
		Entries: []NetworkAllowEntry{{Domain: "registry.npmjs.org"}},
		Note:    "The default public npm registry.",
	},
}

// NetworkPackCatalog returns a deep copy so callers cannot mutate the
// release-owned registry used by launch materialization.
func NetworkPackCatalog() []NetworkPack {
	out := make([]NetworkPack, len(networkPackRegistry))
	for i, pack := range networkPackRegistry {
		out[i] = pack
		out[i].Entries = cloneNetworkRules(NetworkRules{Allow: pack.Entries}).Allow
	}
	return out
}

func networkPackByID(id string) (NetworkPack, bool) {
	for _, pack := range networkPackRegistry {
		if pack.ID == id {
			return pack, true
		}
	}
	return NetworkPack{}, false
}

func normalizeNetworkPackRefs(in []string, field string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for i, id := range in {
		if _, ok := networkPackByID(id); !ok {
			return nil, fmt.Errorf("network.%s[%d] references unknown pack %q", field, i, id)
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

// ExpandNetworkPackEntries returns a deep copy of one pack's destinations for
// editor prediction. It does not confer launch authority.
func ExpandNetworkPackEntries(id string) ([]NetworkAllowEntry, error) {
	pack, ok := networkPackByID(id)
	if !ok {
		return nil, fmt.Errorf("network pack %q is unknown", id)
	}
	return cloneNetworkRules(NetworkRules{Allow: pack.Entries}).Allow, nil
}

// MaterializeNetworkRules turns the compositional authoring representation
// into resolved launch intent. Legacy and already-effective rules pass through
// unchanged.
func MaterializeNetworkRules(in NetworkRules) (NetworkRules, error) {
	if in.Baseline == "" {
		return cloneNetworkRules(in), nil
	}
	denyEntries, err := expandNetworkRuleEntries(in.DenyPacks, in.Deny, "deny")
	if err != nil {
		return NetworkRules{}, err
	}
	switch in.Baseline {
	case NetworkBaselineInherit:
		return NetworkRules{}, nil
	case NetworkBaselineAllow:
		return NetworkRules{Mode: AccessModeOpen, Deny: denyEntries}, nil
	case NetworkBaselineDeny:
	default:
		return NetworkRules{}, fmt.Errorf("network.baseline %q is invalid", in.Baseline)
	}

	allowEntries, err := expandNetworkRuleEntries(in.Packs, in.Allow, "allow")
	if err != nil {
		return NetworkRules{}, err
	}
	if len(allowEntries) == 0 {
		return NetworkRules{Mode: AccessModeClosed, Deny: denyEntries}, nil
	}
	return NetworkRules{
		Mode: AccessModeList, Allow: allowEntries, Deny: denyEntries,
	}, nil
}

func expandNetworkRuleEntries(
	packIDs []string,
	authored []NetworkAllowEntry,
	field string,
) ([]NetworkAllowEntry, error) {
	entries := make([]NetworkAllowEntry, 0, len(authored))
	for _, id := range packIDs {
		pack, ok := networkPackByID(id)
		if !ok {
			return nil, fmt.Errorf("network pack %q is unknown", id)
		}
		entries = append(entries, pack.Entries...)
	}
	entries = append(entries, authored...)
	if len(entries) == 0 {
		return nil, nil
	}
	if len(entries) > MaxNetworkAllowEntries {
		return nil, fmt.Errorf(
			"materialized network %s list has too many entries (maximum %d)",
			field, MaxNetworkAllowEntries,
		)
	}
	resolved, err := normalizeNetworkEntries(entries, field)
	if err != nil {
		return nil, err
	}
	return resolved, nil
}
