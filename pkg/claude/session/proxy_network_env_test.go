package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/sandboxproxy"
)

// TestProxyNetworkCarriageCoversEveryOwnedVariable pins the two properties the
// OpenCode carriage-cooperation arm reads out of this definition, because that
// arm selects the variables for ONE carriage and would silently measure the
// wrong thing if either drifted.
//
// 1. Coverage: every variable the launcher strips is also set. The launcher's
//    own seam test pins the values; this pins the correspondence, which is what
//    keeps a name added to proxyNetworkProxyVariables from being stripped and
//    never written.
// 2. Tagging: HTTP_PROXY/HTTPS_PROXY are the HTTP carriage and ALL_PROXY is the
//    SOCKS5 carriage, matching the scheme each value actually names. An arm
//    told that ALL_PROXY was the HTTP carriage would report "OpenCode carries
//    HTTP" from a launch that in fact only offered SOCKS.
func TestProxyNetworkCarriageCoversEveryOwnedVariable(t *testing.T) {
	entries := ProxyNetworkCarriage("127.0.0.1:41234")
	byName := make(map[string]ProxyNetworkCarriageEntry, len(entries))
	for _, entry := range entries {
		_, duplicate := byName[entry.Name]
		require.Falsef(t, duplicate, "%s is assigned twice", entry.Name)
		byName[entry.Name] = entry
	}
	require.Len(t, byName, len(proxyNetworkProxyVariables))
	for _, name := range proxyNetworkProxyVariables {
		_, ok := byName[name]
		assert.Truef(t, ok, "%s is stripped by the launcher but never set", name)
	}

	for _, name := range []string{
		"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy",
	} {
		assert.Equal(t, sandboxproxy.CarriageHTTP, byName[name].Carriage, name)
		assert.Equal(t, "http://127.0.0.1:41234", byName[name].Value, name)
	}
	for _, name := range []string{"ALL_PROXY", "all_proxy"} {
		assert.Equal(t, sandboxproxy.CarriageSOCKS5, byName[name].Carriage, name)
		// socks5h, not socks5: name resolution stays at the proxy.
		assert.Equal(t, "socks5h://127.0.0.1:41234", byName[name].Value, name)
	}
	for _, name := range proxyNetworkExemptionVariables {
		assert.Emptyf(t, byName[name].Carriage,
			"%s routes nothing and must carry no carriage tag", name)
		assert.Emptyf(t, byName[name].Value,
			"%s exempts nothing and must be set empty", name)
	}
}
