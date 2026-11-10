//go:build linux

package session

// TrustedNsenterPath resolves util-linux nsenter the way the filtered-network
// supervisor does: through the trust-walked helper lookup, never a bare PATH
// search. The live-page stage uses it to dial into an agent's private network
// namespace (see netnsdial).
func TrustedNsenterPath() (string, error) {
	return resolveFilteredNetworkExecutable("nsenter")
}
