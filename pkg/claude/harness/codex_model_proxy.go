package harness

import (
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"strings"
)

// CodexModelProxyOverrides is shared by both workload commands and Codex's own
// effective-config probe. The provider ID is launch-unique, preventing inherited
// provider tables from contributing helpers, headers or routing settings.
func CodexModelProxyOverrides(provider, base string) []string {
	return []string{
		"model_provider=" + codexTOMLString(provider),
		"model_providers." + provider + "={name=\"tclaude model gateway\",base_url=" + codexTOMLString(base) + ",env_key=\"TCLAUDE_MODEL_PROXY_TOKEN\",wire_api=\"responses\",requires_openai_auth=false,supports_websockets=false}",
		"web_search=\"disabled\"",
		// A provider 401 otherwise triggers refresh of a saved ChatGPT login,
		// even with requires_openai_auth=false. Ignore persistent credentials.
		"cli_auth_credentials_store=\"ephemeral\"",
	}
}
func CodexModelProxyArgs(provider, base string) string {
	var b strings.Builder
	for _, value := range CodexModelProxyOverrides(provider, base) {
		b.WriteString(" -c ")
		b.WriteString(clcommon.ShellQuoteArg(value))
	}
	return b.String()
}
