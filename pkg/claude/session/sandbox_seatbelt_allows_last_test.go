package session

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// seatbeltTopLevelForms splits a rendered profile into its top-level
// s-expressions, skipping comments and quoted strings.
func seatbeltTopLevelForms(profile string) []string {
	var forms []string
	depth, start := 0, -1
	for i := 0; i < len(profile); i++ {
		switch profile[i] {
		case ';':
			if depth == 0 {
				for i < len(profile) && profile[i] != '\n' {
					i++
				}
			}
		case '"':
			for i++; i < len(profile) && profile[i] != '"'; i++ {
				if profile[i] == '\\' {
					i++
				}
			}
		case '(':
			if depth == 0 {
				start = i
			}
			depth++
		case ')':
			depth--
			if depth == 0 {
				forms = append(forms, profile[start:i+1])
			}
		}
	}
	return forms
}

// assertSeatbeltAllowsFollowDenies fails when any allow other than
// (allow default) is rendered before a deny. A later deny intermittently
// overrides an earlier allow on macOS 26.7.2 and 27.2 (awb tcl-gejtfc), so
// the order is part of the profile's meaning, not cosmetics.
func assertSeatbeltAllowsFollowDenies(t *testing.T, profile string) {
	t.Helper()
	forms := seatbeltTopLevelForms(profile)
	lastDeny := -1
	for i, form := range forms {
		if strings.HasPrefix(form, "(deny") {
			lastDeny = i
		}
	}
	for i, form := range forms[:lastDeny+1] {
		if strings.HasPrefix(form, "(allow") && form != "(allow default)" {
			t.Errorf("form %d %q is an allow rendered before the deny at form %d", i, form, lastDeny)
		}
	}
	require.NotEmpty(t, forms)
	assert.Equal(t, "(version 1)", forms[0])
	assert.Equal(t, "(allow default)", forms[1])
}

func TestSeatbeltTopLevelFormsSkipsCommentsAndStrings(t *testing.T) {
	forms := seatbeltTopLevelForms("(version 1)\n; a (comment \"x\n(deny file-read* (literal \"a)b\"))\n(allow default)")
	assert.Equal(t, []string{"(version 1)", `(deny file-read* (literal "a)b"))`, "(allow default)"}, forms)
}

func TestRenderSeatbeltProfileRendersAllowsAfterEveryDeny(t *testing.T) {
	const agentd = "/Users/dev/.tclaude/api/agentd.sock"
	hidden := []sandboxpolicy.MountEntry{
		{Path: "/Users/dev/secrets", Mode: sandboxpolicy.MountHide},
		{Path: "/Users/dev/readonly", Mode: sandboxpolicy.MountRO},
	}
	ports, err := sandboxpolicy.CompileFilteredNetworkRules(sandboxpolicy.NetworkRules{
		Mode:  sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{{Loopback: true, Ports: []int{11434, 3000}}},
	})
	require.NoError(t, err)
	allPorts, err := sandboxpolicy.CompileFilteredNetworkRules(sandboxpolicy.NetworkRules{
		Mode:  sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{{Loopback: true}},
	})
	require.NoError(t, err)

	cases := []struct {
		name       string
		plan       sandboxpolicy.MountPlan
		sockets    []string
		wantAllows []string
	}{
		{
			name: "host network with mach-register",
			plan: sandboxpolicy.MountPlan{
				NetworkPosture: sandboxpolicy.NetworkHostOpen, DarwinAllowMachRegister: true, Entries: hidden,
			},
			wantAllows: []string{"(allow mach-register)"},
		},
		{
			name: "local access on a port list",
			plan: sandboxpolicy.MountPlan{
				NetworkPosture: sandboxpolicy.NetworkFiltered, FilteredNetwork: &ports, Entries: hidden,
				DarwinAllowMachRegister: true,
			},
			wantAllows: []string{
				`(allow network-outbound (remote tcp "localhost:3000"))`,
				`(allow network-outbound (remote udp "localhost:11434"))`,
				"(allow mach-register)",
			},
		},
		{
			name: "local access on every port",
			plan: sandboxpolicy.MountPlan{
				NetworkPosture: sandboxpolicy.NetworkFiltered, FilteredNetwork: &allPorts, Entries: hidden,
			},
			wantAllows: []string{`(allow network-outbound (remote ip "localhost:*"))`},
		},
		{
			name: "isolated with agentd",
			plan: sandboxpolicy.MountPlan{
				NetworkPosture: sandboxpolicy.NetworkIsolatedWithAgentd, Entries: hidden,
			},
			sockets: []string{agentd},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			profile, _, err := renderSeatbeltProfile(
				nil, tc.sockets, tc.plan, netip.AddrPort{},
				[]string{"/Users/dev/.tclaude/data"},
				"/private/tmp/tmux-501", "/private/var/folders/ab/runtime/T",
				nil, nil,
			)
			require.NoError(t, err)
			assert.Contains(t, profile, "(remote unix-socket",
				"the fixture must render a unix-socket outbound deny, the rule that broke an earlier allow")
			assertSeatbeltAllowsFollowDenies(t, profile)
			for _, allow := range tc.wantAllows {
				assert.Equal(t, 1, strings.Count(profile, allow), allow)
			}
		})
	}
}
