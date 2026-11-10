package harness

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func TestTheAnthropicPackCoversTheAPIAndTheSubscriptionRefresh(t *testing.T) {
	var pack *sandboxpolicy.NetworkPack
	for _, candidate := range sandboxpolicy.NetworkPackCatalog() {
		if candidate.ID == ClaudeNetworkPack {
			pack = &candidate
			break
		}
	}
	require.NotNil(t, pack, "the %s pack must exist", ClaudeNetworkPack)
	want := append(ClaudeAPIDestinations(), ClaudeLoginRefreshDestinations()...)
	assert.ElementsMatch(t, want, pack.Entries,
		"the pack, the resolver and the launch warning must name the same hosts")
	assert.Contains(t, pack.Label, "subscription")
}

func TestClaudeLoginRefreshNotice(t *testing.T) {
	apiOnly := sandboxpolicy.NetworkRules{
		Mode:  sandboxpolicy.AccessModeList,
		Allow: ClaudeAPIDestinations(),
	}
	withPack := sandboxpolicy.NetworkRules{
		Mode:  sandboxpolicy.AccessModeList,
		Allow: append(ClaudeAPIDestinations(), ClaudeLoginRefreshDestinations()...),
	}
	subscription := ResolvedModelTransport{
		Provider: "anthropic", ProviderResolved: true, ClaudeLogin: ClaudeLoginSubscription,
	}

	notice, ok := ClaudeLoginRefreshNotice(apiOnly, subscription)
	require.True(t, ok, "a subscription login whose list lacks the refresh host must be warned")
	assert.Equal(t, sandboxpolicy.AccessNoticeReasonFilteredModelTraffic, notice.Reason)
	assert.Equal(t, "network", notice.Axis)
	assert.Contains(t, notice.Detail, "signed in with a Claude subscription")
	assert.Contains(t, notice.Detail, "platform.claude.com:443")
	assert.Contains(t, notice.Detail, "include the net-anthropic pack")
	assert.Contains(t, notice.Detail, "about 8 hours")

	_, ok = ClaudeLoginRefreshNotice(withPack, subscription)
	assert.False(t, ok, "a list that reaches the refresh host needs no warning")

	apiKey := subscription
	apiKey.ClaudeLogin = ClaudeLoginNone
	_, ok = ClaudeLoginRefreshNotice(apiOnly, apiKey)
	assert.False(t, ok, "an API-key login never refreshes, so it is never warned")

	unknown := subscription
	unknown.ClaudeLogin = ClaudeLoginUnknown
	notice, ok = ClaudeLoginRefreshNotice(apiOnly, unknown)
	require.True(t, ok)
	assert.Contains(t, notice.Detail, "if this Claude Code is signed in with a Claude subscription")
	assert.Contains(t, notice.Detail, "keychain")

	custom := subscription
	custom.ClaudeLogin = ClaudeLoginCustomOAuth
	notice, ok = ClaudeLoginRefreshNotice(withPack, custom)
	require.True(t, ok, "a moved OAuth endpoint is unchecked even when the default host is allowed")
	assert.Contains(t, notice.Detail, ClaudeCustomOAuthURLEnvVar)

	openNoDeny := sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeOpen,
		Deny: []sandboxpolicy.NetworkAllowEntry{{Domain: "example.com"}}}
	_, ok = ClaudeLoginRefreshNotice(openNoDeny, subscription)
	assert.False(t, ok, "allow-all reaches the refresh host unless a deny rule matches it")

	openDenied := sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeOpen,
		Deny: ClaudeLoginRefreshDestinations()}
	notice, ok = ClaudeLoginRefreshNotice(openDenied, subscription)
	require.True(t, ok)
	assert.Contains(t, notice.Detail, "remove or narrow the deny rule")
}
