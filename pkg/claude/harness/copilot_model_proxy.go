package harness

// CopilotModelProxyEnvironment pins HTTP Responses BYOK. Offline mode disables
// GitHub authentication and model routing even when a saved account exists.
// All competing COPILOT_PROVIDER_* inputs must be stripped before applying it.
func CopilotModelProxyEnvironment(base, bearer string) []string {
	return []string{
		"COPILOT_PROVIDER_BASE_URL=" + base,
		"COPILOT_PROVIDER_TYPE=openai",
		"COPILOT_PROVIDER_WIRE_API=responses",
		"COPILOT_PROVIDER_TRANSPORT=http",
		"COPILOT_PROVIDER_BEARER_TOKEN=" + bearer,
		"COPILOT_OFFLINE=true",
	}
}
