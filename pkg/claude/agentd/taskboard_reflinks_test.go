package agentd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestProbeAWBCLIBaseReadsTheRemoteServer(t *testing.T) {
	for _, tc := range []struct {
		name, out, base, note string
	}{
		{"remote", `{"connection":{"mode":"remote","server":"https://www.example.org/awb/"}}`, "https://www.example.org/awb", ""},
		{"local", `{"connection":{"mode":"local","database":"/x.db"}}`, "", "local database"},
		{"credentials", `{"connection":{"mode":"remote","server":"https://u:p@awb.example/"}}`, "", "not a usable base URL"},
		{"garbage", `nope`, "", "no JSON"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restore := SetAWBCLIStatusForTest(func(context.Context) ([]byte, error) { return []byte(tc.out), nil })
			defer restore()
			base, note := probeAWBCLIBase()
			assert.Equal(t, tc.base, base)
			assert.Contains(t, note, tc.note)
		})
	}
}

func TestLinearWorkspaceOf(t *testing.T) {
	base, team, ok := linearWorkspaceOf("https://linear.app/acme/issue/LIN-7/cart-total")
	assert.True(t, ok)
	assert.Equal(t, "https://linear.app/acme", base)
	assert.Equal(t, "LIN", team)
	for _, bad := range []string{"https://linear.app/issue/LIN-7", "http://linear.app/acme/issue/LIN-7", "https://evil.example/acme/issue/LIN-7", "https://linear.app/acme/issue/nope"} {
		_, _, ok := linearWorkspaceOf(bad)
		assert.False(t, ok, bad)
	}
}

func TestTaskRefLinkerLinksAGitHubLabel(t *testing.T) {
	ref := &taskboardRef{Kind: "github", Label: "owner/repo#7"}
	var l taskRefLinker
	l.add(ref)
	l.finish()
	assert.Equal(t, "https://github.com/owner/repo/issues/7", ref.URL)
}
