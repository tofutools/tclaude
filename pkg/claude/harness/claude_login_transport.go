package harness

import (
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// Claude Code's filtered-network sign-in refresh.
//
// Read from the Claude Code 2.1.318 binary's OAuth config, and matching what a
// subscription login was observed to reach (tofutools/tclaude-feedback#3):
//
//   - BASE_API_URL  https://api.anthropic.com  — model traffic, plus the
//     /api/oauth/profile, /api/oauth/usage and /api/oauth/claude_cli/* calls
//   - TOKEN_URL     https://platform.claude.com/v1/oauth/token — where a
//     Claude subscription (claude.ai) login refreshes its access token
//
// The authorize pages (platform.claude.com/oauth/authorize,
// claude.com/cai/oauth/authorize) belong to the first browser sign-in, which
// happens outside the sandbox, so they are not named.
//
// The refresh host is NOT part of the model-transport requirement: an API-key
// launch never contacts it, and a subscription launch works until its access
// token expires. A filtered launch that cannot reach it is warned at launch
// (ClaudeLoginRefreshNotice) rather than refused.

const (
	// ClaudeNetworkPack is the release-owned pack for Claude Code: the API
	// host and the subscription sign-in refresh host.
	ClaudeNetworkPack = "net-anthropic"

	claudeAPIHost        = "api.anthropic.com"
	claudeOAuthTokenHost = "platform.claude.com"

	// ClaudeCustomOAuthURLEnvVar moves Claude Code's OAuth endpoints to an
	// approved custom origin. tclaude does not follow it.
	ClaudeCustomOAuthURLEnvVar = "CLAUDE_CODE_CUSTOM_OAUTH_URL"
)

// ClaudeAPIDestinations and ClaudeLoginRefreshDestinations are the destination
// sets of Claude Code's first-party route, shared by the resolver, the launch
// warning and the network pack so they cannot drift.
func ClaudeAPIDestinations() []sandboxpolicy.NetworkAllowEntry {
	return []sandboxpolicy.NetworkAllowEntry{{Domain: claudeAPIHost, Ports: []int{443}}}
}

func ClaudeLoginRefreshDestinations() []sandboxpolicy.NetworkAllowEntry {
	return []sandboxpolicy.NetworkAllowEntry{{Domain: claudeOAuthTokenHost, Ports: []int{443}}}
}

// ClaudeLogin says what a Claude Code launch signs in with, as far as the
// launch resolver can tell. It only decides whether a launch is warned about
// its sign-in refresh; it never gates anything.
type ClaudeLogin string

const (
	// ClaudeLoginNone: the launch uses an API key or a long-lived token
	// (ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, CLAUDE_CODE_OAUTH_TOKEN), or
	// has no stored login at all, so nothing refreshes.
	ClaudeLoginNone ClaudeLogin = ""
	// ClaudeLoginSubscription: a stored Claude subscription login with a
	// refresh token was found.
	ClaudeLoginSubscription ClaudeLogin = "subscription"
	// ClaudeLoginUnknown: the login could be a subscription, but tclaude cannot
	// read where it is stored (the macOS keychain).
	ClaudeLoginUnknown ClaudeLogin = "unknown"
	// ClaudeLoginCustomOAuth: a subscription login whose OAuth endpoints were
	// moved by CLAUDE_CODE_CUSTOM_OAUTH_URL.
	ClaudeLoginCustomOAuth ClaudeLogin = "custom_oauth"
)

// ClaudeLoginRefreshNotice warns a filtered Claude Code launch whose network
// rules cannot reach the sign-in refresh host. It returns false when there is
// nothing to say: no subscription login, a non-first-party route, or rules
// that already reach the host. It adds no access.
func ClaudeLoginRefreshNotice(
	rules sandboxpolicy.NetworkRules,
	resolved ResolvedModelTransport,
) (sandboxpolicy.AccessNotice, bool) {
	if resolved.ClaudeLogin == ClaudeLoginNone {
		return sandboxpolicy.AccessNotice{}, false
	}
	notice := sandboxpolicy.AccessNotice{
		Class:  sandboxpolicy.AccessNoticeClassDegradation,
		Axis:   "network",
		Reason: sandboxpolicy.AccessNoticeReasonFilteredModelTraffic,
		Effect: sandboxpolicy.AccessNoticeEffectPreviewIncomplete,
	}
	if resolved.ClaudeLogin == ClaudeLoginCustomOAuth {
		notice.Detail = "Filtered network: " + ClaudeCustomOAuthURLEnvVar + " moves Claude Code's " +
			"sign-in refresh to a custom endpoint, which tclaude did not check against the network " +
			"rules. If Claude Code loses the model once its access token expires, allow that " +
			"endpoint's host in every layer that has a network allow list."
		return notice, true
	}
	var missing []string
	for _, destination := range ClaudeLoginRefreshDestinations() {
		if claudeRulesReach(rules, destination) {
			continue
		}
		missing = append(missing, formatNetworkDestination(destination))
	}
	if len(missing) == 0 {
		return sandboxpolicy.AccessNotice{}, false
	}
	host := strings.Join(missing, ", ")
	fix := "include the " + ClaudeNetworkPack + " pack (it covers the API and the sign-in refresh) " +
		"or allow " + host + " in every layer that has a network allow list"
	if rules.Mode == sandboxpolicy.AccessModeOpen {
		fix = "remove or narrow the deny rule that matches " + host
	}
	what := "this Claude Code is signed in with a Claude subscription, which refreshes its access token at " +
		host + ", and the network rules do not reach it"
	if resolved.ClaudeLogin == ClaudeLoginUnknown {
		what = "if this Claude Code is signed in with a Claude subscription (tclaude cannot read the " +
			"macOS keychain to tell), it refreshes its access token at " + host +
			", and the network rules do not reach it"
	}
	notice.Detail = "Filtered network: " + what + ". The model keeps working until the current access " +
		"token expires (about 8 hours after its last refresh), then stops. To fix it, " + fix +
		". An API-key login does not need it."
	return notice, true
}

// claudeRulesReach reports whether planned rules let a connection to
// destination through: an allow list must cover it, and an allow-all baseline
// must not deny it.
func claudeRulesReach(
	rules sandboxpolicy.NetworkRules,
	destination sandboxpolicy.NetworkAllowEntry,
) bool {
	switch rules.Mode {
	case sandboxpolicy.AccessModeOpen:
		return !networkRulesCoverDestination(rules.Deny, destination)
	case sandboxpolicy.AccessModeList:
		return networkRulesCoverDestination(rules.Allow, destination)
	default:
		return false
	}
}
