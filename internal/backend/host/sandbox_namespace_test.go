//go:build linux || darwin

package host

import "strings"

// Optional native journeys distinguish unavailable user namespaces from policy
// failures. Callers never skip when TCLAUDE_REQUIRE_SANDBOX_NATIVE=1.
func sandboxNamespaceUnavailable(output string) bool {
	for _, message := range []string{
		"No permissions to create a new namespace",
		"Creating new namespace failed: Operation not permitted",
		"loopback: Failed RTM_NEWADDR: Operation not permitted",
		"bwrap: setting up uid map: Permission denied",
	} {
		if strings.Contains(output, message) {
			return true
		}
	}
	return false
}
