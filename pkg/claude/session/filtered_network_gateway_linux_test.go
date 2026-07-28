//go:build linux

package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"golang.org/x/sys/unix"
)

func TestFilteredNetworkHelperEnvExcludesAmbientInjectionVariables(t *testing.T) {
	assert.Equal(t, []string{
		"PATH=/usr/sbin:/usr/bin:/sbin:/bin",
		"LANG=C",
		"LC_ALL=C",
	}, filteredNetworkHelperEnv())
}

func TestFilteredNetworkNFTCommandCarriesOnlyBootstrapCapability(t *testing.T) {
	cmd := filteredNetworkNFTCommand("/usr/sbin/nft")
	assert.Equal(t, []string{
		"/usr/sbin/nft",
		"-f",
		sandboxpolicy.FilteredNetworkNFTPolicyPath,
	}, cmd.Args)
	assert.Equal(t, []uintptr{unix.CAP_NET_ADMIN}, cmd.SysProcAttr.AmbientCaps)
	assert.Equal(t, filteredNetworkHelperEnv(), cmd.Env)
}
