package session

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func TestRenderSeatbeltProfileGolden(t *testing.T) {
	plan := sandboxpolicy.MountPlan{
		NetworkPosture: sandboxpolicy.NetworkHostOpen,
		Entries: []sandboxpolicy.MountEntry{
			{Path: "/Users/dev", Mode: sandboxpolicy.MountHide},
			{Path: "/Users/dev/.tclaude", Mode: sandboxpolicy.MountRW},
			{Path: "/Users/dev/.tclaude/data/audit", Mode: sandboxpolicy.MountRO},
			{Path: "/Users/dev/work", Mode: sandboxpolicy.MountRW},
			{Path: "/Users/dev/work/readonly", Mode: sandboxpolicy.MountRO},
			{Path: "/Users/dev/work/readonly/hidden", Mode: sandboxpolicy.MountHide},
			{Path: "/Users/dev/work/readonly/hidden/reopen", Mode: sandboxpolicy.MountRW},
		},
	}
	got, params, err := renderSeatbeltProfile(
		[]string{"/Users/dev/.claude", "/Users/dev/work"},
		nil,
		plan,
		netip.AddrPort{},
		[]string{"/Users/dev/.tclaude/data", "/Users/dev/.claude/sessions"},
		"/private/tmp/tmux-501",
		"/private/var/folders/ab/runtime/T",
		nil,
		nil,
	)
	require.NoError(t, err)
	assertSeatbeltAllowDenyOrder(t, got)

	const want = `(version 1)
(allow default)

; Filesystem policy is deny-only. Positive descendants are carved out
; inside each deny predicate so plan precedence does not depend on
; Seatbelt allow/deny rule selection.

(deny file-write*
  (require-all
    (require-any (literal (param "WRITE_DENY_0")) (subpath (param "WRITE_DENY_0")))
    (require-not (literal "/dev/null"))
    (require-not (literal "/dev/tty"))
    (require-not (literal "/dev/ptmx"))
    (require-not (literal "/dev/fd"))
    (require-not (subpath "/dev/fd"))
    (require-not (regex #"^/dev/(tty|pty)[A-Za-z0-9]+$"))
    (require-not (literal (param "DARWIN_RUNTIME_TMPDIR")))
    (require-not (subpath (param "DARWIN_RUNTIME_TMPDIR")))
    (require-not (literal (param "WRITE_DENY_0_REOPEN_0")))
    (require-not (subpath (param "WRITE_DENY_0_REOPEN_0")))
    (require-not (literal (param "WRITE_DENY_0_REOPEN_1")))
    (require-not (subpath (param "WRITE_DENY_0_REOPEN_1")))
    (require-not (literal (param "WRITE_DENY_0_REOPEN_2")))
    (require-not (subpath (param "WRITE_DENY_0_REOPEN_2")))
  ))

(deny file-write*
  (require-all
    (require-any (literal (param "WRITE_DENY_1")) (subpath (param "WRITE_DENY_1")))
  ))

(deny file-write*
  (require-all
    (require-any (literal (param "WRITE_DENY_2")) (subpath (param "WRITE_DENY_2")))
  ))

(deny file-write*
  (require-all
    (require-any (literal (param "WRITE_DENY_3")) (subpath (param "WRITE_DENY_3")))
  ))

(deny file-write*
  (require-all
    (require-any (literal (param "WRITE_DENY_4")) (subpath (param "WRITE_DENY_4")))
    (require-not (literal (param "WRITE_DENY_4_REOPEN_0")))
    (require-not (subpath (param "WRITE_DENY_4_REOPEN_0")))
  ))

(deny file-read*
  (require-all
    (require-any (literal (param "READ_DENY_0")) (subpath (param "READ_DENY_0")))
  ))

(deny network-outbound
  (remote unix-socket
    (require-all
      (require-any (literal (param "READ_DENY_0")) (subpath (param "READ_DENY_0")))
    )))

(deny file-read*
  (require-all
    (require-any (literal (param "READ_DENY_1")) (subpath (param "READ_DENY_1")))
    (require-not (literal (param "READ_DENY_1_REOPEN_0")))
    (require-not (subpath (param "READ_DENY_1_REOPEN_0")))
    (require-not (literal (param "READ_DENY_1_REOPEN_1")))
    (require-not (subpath (param "READ_DENY_1_REOPEN_1")))
    (require-not (literal (param "READ_DENY_1_REOPEN_2")))
    (require-not (subpath (param "READ_DENY_1_REOPEN_2")))
  ))

(deny network-outbound
  (remote unix-socket
    (require-all
      (require-any (literal (param "READ_DENY_1")) (subpath (param "READ_DENY_1")))
      (require-not (literal (param "READ_DENY_1_REOPEN_0")))
      (require-not (subpath (param "READ_DENY_1_REOPEN_0")))
      (require-not (literal (param "READ_DENY_1_REOPEN_1")))
      (require-not (subpath (param "READ_DENY_1_REOPEN_1")))
      (require-not (literal (param "READ_DENY_1_REOPEN_2")))
      (require-not (subpath (param "READ_DENY_1_REOPEN_2")))
    )))

(deny file-read*
  (require-all
    (require-any (literal (param "READ_DENY_2")) (subpath (param "READ_DENY_2")))
  ))

(deny network-outbound
  (remote unix-socket
    (require-all
      (require-any (literal (param "READ_DENY_2")) (subpath (param "READ_DENY_2")))
    )))

(deny file-read*
  (require-all
    (require-any (literal (param "READ_DENY_3")) (subpath (param "READ_DENY_3")))
    (require-not (literal (param "READ_DENY_3_REOPEN_0")))
    (require-not (subpath (param "READ_DENY_3_REOPEN_0")))
  ))

(deny network-outbound
  (remote unix-socket
    (require-all
      (require-any (literal (param "READ_DENY_3")) (subpath (param "READ_DENY_3")))
      (require-not (literal (param "READ_DENY_3_REOPEN_0")))
      (require-not (subpath (param "READ_DENY_3_REOPEN_0")))
    )))

(deny file-read*
  (require-all
    (require-any (literal (param "READ_DENY_4")) (subpath (param "READ_DENY_4")))
    (require-not (literal (param "READ_DENY_4_REOPEN_0")))
    (require-not (subpath (param "READ_DENY_4_REOPEN_0")))
  ))

(deny network-outbound
  (remote unix-socket
    (require-all
      (require-any (literal (param "READ_DENY_4")) (subpath (param "READ_DENY_4")))
      (require-not (literal (param "READ_DENY_4_REOPEN_0")))
      (require-not (subpath (param "READ_DENY_4_REOPEN_0")))
    )))
`
	if got != want {
		t.Fatalf("Seatbelt profile golden mismatch\nparams: %#v\nprofile:\n%s", params, got)
	}
	assert.NotContains(t, got, "Isolated networking",
		"the host-open profile must remain byte-identical and gain no network denies")
}

func TestRenderSeatbeltProfileOptionallyAllowsMachRegister(t *testing.T) {
	profile, _, err := renderSeatbeltProfile(
		nil, nil,
		sandboxpolicy.MountPlan{
			NetworkPosture:          sandboxpolicy.NetworkHostOpen,
			DarwinAllowMachRegister: true,
		},
		netip.AddrPort{}, nil, "/private/tmp/tmux-501",
		"/private/var/folders/ab/runtime/T", nil, nil,
	)
	require.NoError(t, err)
	assert.Contains(t, profile, "(allow mach-register)")

	profile, _, err = renderSeatbeltProfile(
		nil, nil,
		sandboxpolicy.MountPlan{NetworkPosture: sandboxpolicy.NetworkHostOpen},
		netip.AddrPort{}, nil, "/private/tmp/tmux-501",
		"/private/var/folders/ab/runtime/T", nil, nil,
	)
	require.NoError(t, err)
	assert.NotContains(t, profile, "mach-register")
}

func TestRenderSeatbeltIsolatedNetworkProfileParameterizesAllowedSockets(t *testing.T) {
	const (
		agentd = "/Users/dev/.tclaude/api/agentd.sock"
		alias  = "/Users/dev/.tclaude-link/api/agentd.sock"
		policy = "/Users/dev/runtime/build.sock"
	)
	profile, params, err := renderSeatbeltProfile(
		nil,
		[]string{agentd, policy},
		sandboxpolicy.MountPlan{
			NetworkPosture: sandboxpolicy.NetworkIsolatedWithAgentd,
			Entries: []sandboxpolicy.MountEntry{{
				Path: agentd,
				Mode: sandboxpolicy.MountRO,
			}},
			Aliases: []sandboxpolicy.MountAlias{{
				Link:   "/Users/dev/.tclaude-link",
				Target: "/Users/dev/.tclaude",
			}},
		},
		netip.AddrPort{},
		[]string{"/Users/dev/.tclaude/data"},
		"/private/tmp/tmux-501",
		"/private/var/folders/ab/runtime/T",
		nil,
		nil,
	)
	require.NoError(t, err)
	assertSeatbeltAllowDenyOrder(t, profile)

	assert.Equal(t, 1, strings.Count(profile, "(deny network-bind)"))
	assert.NotContains(t, profile, "(deny network-inbound")
	assert.GreaterOrEqual(t, strings.Count(profile, "(deny network-outbound"), 1)
	assert.NotContains(t, profile, "(deny network*)")
	assert.NotContains(t, profile, "system-socket")

	exceptions := map[string]string{}
	for _, param := range params {
		if strings.HasPrefix(param.name, "AGENTD_SOCKET_") {
			exceptions[param.name] = param.path
		}
	}
	assert.ElementsMatch(t, []string{agentd, alias, policy}, []string{
		exceptions["AGENTD_SOCKET_0"],
		exceptions["AGENTD_SOCKET_1"],
		exceptions["AGENTD_SOCKET_2"],
	})
	for name := range exceptions {
		assert.Equal(t, 1, strings.Count(profile,
			`(remote unix-socket
        (literal (param "`+name+`")))`,
		),
			"each allowlisted socket path must be an exact outbound-connect exception",
		)
	}
}

func TestRenderSeatbeltLoopbackOnlyNetworkUsesProtocolSpecificPortPredicates(t *testing.T) {
	rules, err := sandboxpolicy.CompileFilteredNetworkRules(sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{
			{Loopback: true, Ports: []int{11434, 3000}},
			{Loopback: true, Ports: []int{3000}},
		},
	})
	require.NoError(t, err)
	profile, _, err := renderSeatbeltProfile(
		nil,
		nil,
		sandboxpolicy.MountPlan{
			NetworkPosture:  sandboxpolicy.NetworkFiltered,
			FilteredNetwork: &rules,
		},
		netip.AddrPort{},
		[]string{"/Users/dev/.tclaude/data"},
		"/private/tmp/tmux-501",
		"/private/var/folders/ab/runtime/T",
		nil,
		nil,
	)
	require.NoError(t, err)
	assert.Contains(t, profile, `(deny network-outbound (remote ip "*:*"))`)
	assert.Equal(t, 1, strings.Count(profile,
		`(allow network-outbound (remote tcp "localhost:3000"))`))
	assert.Equal(t, 1, strings.Count(profile,
		`(allow network-outbound (remote tcp "localhost:11434"))`))
	assert.Equal(t, 1, strings.Count(profile,
		`(allow network-outbound (remote udp "localhost:3000"))`))
	assert.Equal(t, 1, strings.Count(profile,
		`(allow network-outbound (remote udp "localhost:11434"))`))
	assert.NotContains(t, profile, `(remote ip "localhost:3000")`,
		"port-scoped exceptions need a narrower protocol predicate than the IP-wide deny")
	assert.NotContains(t, profile, `(local ip `)
	assert.NotContains(t, profile, `(deny network-bind)`,
		"the authored list is outbound-only and local services must still bind")
	assert.NotContains(t, profile, `(deny network-inbound)`)
}

func TestRenderSeatbeltLoopbackAllPortsCoalescesPortExceptions(t *testing.T) {
	rules, err := sandboxpolicy.CompileFilteredNetworkRules(sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{
			{Loopback: true, Ports: []int{11434}},
			{Loopback: true},
		},
	})
	require.NoError(t, err)
	profile, _, err := renderSeatbeltProfile(
		nil,
		nil,
		sandboxpolicy.MountPlan{
			NetworkPosture:  sandboxpolicy.NetworkFiltered,
			FilteredNetwork: &rules,
		},
		netip.AddrPort{},
		[]string{"/Users/dev/.tclaude/data"},
		"/private/tmp/tmux-501",
		"/private/var/folders/ab/runtime/T",
		nil,
		nil,
	)
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(profile,
		`(allow network-outbound (remote ip "localhost:*"))`))
	assert.NotContains(t, profile, `localhost:11434`)
}

func TestRenderSeatbeltIsolatedNetworkHiddenAgentdHasNoPostureException(t *testing.T) {
	const agentd = "/Users/dev/.tclaude/api/agentd.sock"
	profile, params, err := renderSeatbeltProfile(
		nil,
		[]string{agentd},
		sandboxpolicy.MountPlan{
			NetworkPosture: sandboxpolicy.NetworkIsolatedWithAgentd,
			Entries: []sandboxpolicy.MountEntry{{
				Path: agentd,
				Mode: sandboxpolicy.MountHide,
			}},
		},
		netip.AddrPort{},
		[]string{"/Users/dev/.tclaude/data"},
		"/private/tmp/tmux-501",
		"/private/var/folders/ab/runtime/T",
		nil,
		nil,
	)
	require.NoError(t, err)

	assert.Contains(t, profile, "(deny network-outbound)\n",
		"an explicitly hidden agentd socket must leave no posture exception")
	assert.GreaterOrEqual(t, strings.Count(profile, "(deny network-outbound"), 2,
		"the hide-region Unix-connect deny must stack above isolated posture denial")
	for _, param := range params {
		assert.NotContains(t, param.name, "AGENTD_SOCKET_")
	}
}

func TestSeatbeltRuntimeCarveoutsPierceOnlyBaselineWriteDeny(t *testing.T) {
	profile, _, err := renderSeatbeltProfile(
		nil,
		nil,
		sandboxpolicy.MountPlan{
			NetworkPosture: sandboxpolicy.NetworkHostOpen,
			Entries: []sandboxpolicy.MountEntry{{
				Path: "/dev",
				Mode: sandboxpolicy.MountRO,
			}},
		},
		netip.AddrPort{},
		[]string{"/Users/dev/.tclaude/data", "/Users/dev/.claude/sessions"},
		"/private/tmp/tmux-501",
		"/private/var/folders/ab/runtime/T",
		nil,
		nil,
	)
	require.NoError(t, err)
	assertSeatbeltAllowDenyOrder(t, profile)
	assert.Equal(t, 1, strings.Count(profile, `(require-not (literal "/dev/null"))`),
		"the runtime carveout belongs only to the root baseline deny")
	assert.Equal(t, 1, strings.Count(profile, `(require-not (literal "/dev/tty"))`))
	assert.Equal(t, 1, strings.Count(profile, `(require-not (literal "/dev/ptmx"))`))
	assert.Equal(t, 1, strings.Count(profile, `(require-not (subpath "/dev/fd"))`))
	assert.Equal(t, 1, strings.Count(profile, `(require-not (regex #"^/dev/(tty|pty)[A-Za-z0-9]+$"))`))
	assert.Contains(t, profile, `(param "WRITE_DENY_1")`,
		"the narrower /dev policy must retain a separate write deny with no runtime carveouts")
	baseline := strings.Index(profile, `(param "WRITE_DENY_0")`)
	strict := strings.Index(profile, `(param "WRITE_DENY_1")`)
	require.Greater(t, baseline, -1)
	require.Greater(t, strict, baseline,
		"the strict /dev re-deny must follow the baseline deny with runtime carveouts")
	t.Logf("actual rendered baseline deny:\n%s",
		seatbeltRuleContaining(profile, `(param "WRITE_DENY_0")`))
	t.Logf("actual rendered strict /dev re-deny:\n%s",
		seatbeltRuleContaining(profile, `(param "WRITE_DENY_1")`))
}

func TestSeatbeltDuplicateClaudeTempWriteGrantIsIdempotent(t *testing.T) {
	const (
		tempRoot  = "/private/tmp"
		claudeDir = "/private/tmp/claude-501"
	)
	_, params, err := renderSeatbeltProfile(
		[]string{tempRoot, claudeDir},
		nil,
		sandboxpolicy.MountPlan{
			NetworkPosture: sandboxpolicy.NetworkHostOpen,
			Entries: []sandboxpolicy.MountEntry{{
				Path: tempRoot,
				Mode: sandboxpolicy.MountRW,
			}},
		},
		netip.AddrPort{},
		[]string{"/Users/dev/.tclaude/data", "/Users/dev/.claude/sessions"},
		"/private/tmp/tmux-501",
		"/private/var/folders/ab/runtime/T",
		nil,
		nil,
	)
	require.NoError(t, err)

	count := 0
	for _, param := range params {
		if param.path == tempRoot {
			count++
		}
	}
	assert.Equal(t, 1, count,
		"an existing profile rw row for /tmp must merge with the automatic Claude grant")
}

func TestSeatbeltProfileReadOnlyTempStillNarrowsAutomaticClaudeGrant(t *testing.T) {
	const (
		tempRoot  = "/private/tmp"
		claudeDir = "/private/tmp/claude-501"
	)
	_, params, err := renderSeatbeltProfile(
		[]string{tempRoot, claudeDir},
		nil,
		sandboxpolicy.MountPlan{
			NetworkPosture: sandboxpolicy.NetworkHostOpen,
			Entries: []sandboxpolicy.MountEntry{{
				Path: tempRoot,
				Mode: sandboxpolicy.MountRO,
			}},
		},
		netip.AddrPort{},
		[]string{"/Users/dev/.tclaude/data", "/Users/dev/.claude/sessions"},
		"/private/tmp/tmux-501",
		"/private/var/folders/ab/runtime/T",
		nil,
		nil,
	)
	require.NoError(t, err)

	var tempRootParams, claudeDirParams int
	for _, param := range params {
		switch param.path {
		case tempRoot:
			tempRootParams++
		case claudeDir:
			claudeDirParams++
		}
	}
	assert.Zero(t, tempRootParams,
		"an explicit profile read-only row must remove the broad automatic reopen")
	assert.Equal(t, 1, claudeDirParams,
		"the secured per-user harness directory remains a narrower launch requirement")
}

func TestSeatbeltRuntimePolicyUsesIdentityAwareCarveoutIntersection(t *testing.T) {
	const policySpelling = "/DEV"
	sameIdentity := func(path string) (seatbeltFileIdentity, bool) {
		switch path {
		case "/dev", policySpelling:
			return seatbeltFileIdentity{dev: 1, ino: 7}, true
		default:
			return seatbeltFileIdentity{}, false
		}
	}
	profile, params, err := renderSeatbeltProfile(
		nil,
		nil,
		sandboxpolicy.MountPlan{
			NetworkPosture: sandboxpolicy.NetworkHostOpen,
			Entries: []sandboxpolicy.MountEntry{{
				Path: policySpelling,
				Mode: sandboxpolicy.MountRO,
			}},
		},
		netip.AddrPort{},
		[]string{"/Users/dev/.tclaude/data"},
		"/private/tmp/tmux-501",
		"/private/var/folders/ab/runtime/T",
		sameIdentity,
		nil,
	)
	require.NoError(t, err)

	strictParam := ""
	for _, param := range params {
		if param.path == policySpelling && strings.HasPrefix(param.name, "WRITE_DENY_") {
			strictParam = param.name
			break
		}
	}
	require.NotEmpty(t, strictParam,
		"identity-equivalent /DEV policy must emit a strict deny beyond the /dev carveout")
	assert.Contains(t, profile,
		`(require-any (literal (param "`+strictParam+`")) (subpath (param "`+strictParam+`")))`)
}

func TestSeatbeltOrdinaryAncestorHideRepairsRequiredAgentdSocket(t *testing.T) {
	const socket = "/Users/dev/.tclaude/api/agentd.sock"
	profile, params, err := renderSeatbeltProfile(
		nil,
		[]string{socket},
		sandboxpolicy.MountPlan{
			NetworkPosture: sandboxpolicy.NetworkHostOpen,
			Entries: []sandboxpolicy.MountEntry{{
				Path: "/Users/dev",
				Mode: sandboxpolicy.MountHide,
			}},
		},
		netip.AddrPort{},
		[]string{"/Users/dev/.tclaude/data", "/Users/dev/.claude/sessions"},
		"/private/tmp/tmux-501",
		"/private/var/folders/ab/runtime/T",
		nil,
		nil,
	)
	require.NoError(t, err)

	var socketParam string
	for _, param := range params {
		if param.path == socket {
			socketParam = param.name
			break
		}
	}
	require.NotEmpty(t, socketParam, "the required agentd socket must be a parameterized carveout")
	assert.Equal(t, 2, strings.Count(
		profile,
		`(require-not (literal (param "`+socketParam+`")))`,
	), "the same socket parameter must reopen file-read and Unix-connect denies")
	assert.Equal(t, 2, strings.Count(
		profile,
		`(require-not (subpath (param "`+socketParam+`")))`,
	))
	assert.Equal(t,
		strings.Count(profile, "\n(deny file-read*"),
		strings.Count(profile, "\n(deny network-outbound"),
		"every hide read deny must have one Unix-connect sibling",
	)
}

func TestSeatbeltPrivateAttachmentParentUsesUniformReadAndUnixConnectHide(t *testing.T) {
	const (
		parent  = "/Users/dev/.tclaude/data/spawn-attachments"
		current = parent + "/current-session"
		sibling = parent + "/sibling-session"
	)
	profile, params, err := renderSeatbeltProfile(
		nil,
		nil,
		sandboxpolicy.MountPlan{
			NetworkPosture: sandboxpolicy.NetworkHostOpen,
			Entries: []sandboxpolicy.MountEntry{
				{Path: parent, Mode: sandboxpolicy.MountRW},
				{Path: sibling, Mode: sandboxpolicy.MountRW},
			},
		},
		netip.AddrPort{},
		[]string{"/Users/dev/.tclaude/data"},
		"/private/tmp/tmux-501",
		"/private/var/folders/ab/runtime/T",
		nil,
		nil,
		TclaudeLayerPrivateWriteDir{Parent: parent, Current: current},
	)
	require.NoError(t, err)

	parentParam := ""
	currentParam := ""
	for _, param := range params {
		switch {
		case param.path == parent && strings.HasPrefix(param.name, "READ_DENY_"):
			parentParam = param.name
		case param.path == current && strings.Contains(param.name, "READ_DENY_"):
			currentParam = param.name
		}
	}
	require.NotEmpty(t, parentParam, "the shared parent must emit a read hide")
	require.Equal(t, parentParam+"_REOPEN_0", currentParam,
		"the current session child must be the hide's exact carveout")
	assert.Equal(t, 2, strings.Count(
		profile,
		`(require-any (literal (param "`+parentParam+`")) (subpath (param "`+parentParam+`")))`,
	), "file-read and remote-unix denies must use the identical parent parameter")
	assert.Equal(t, 2, strings.Count(
		profile,
		`(require-not (literal (param "`+currentParam+`")))`,
	), "file-read and remote-unix denies must use the identical child exception")
	assert.Equal(t, 2, strings.Count(
		profile,
		`(require-not (subpath (param "`+currentParam+`")))`,
	))
	for _, param := range params {
		if strings.HasPrefix(param.name, parentParam+"_REOPEN_") {
			assert.Equal(t, current, param.path,
				"ordinary rules and break-glass must not become exceptions to the daemon-only parent hide")
		}
	}
	assert.Equal(t, 4, strings.Count(profile, `(param "`+parentParam+`_REOPEN_0")`),
		"the paired file-read/remote-unix deny must share exactly one daemon reopen")
	assert.NotContains(t, profile, `(param "`+parentParam+`_REOPEN_1")`,
		"the break-glass sibling must not become a private-parent exception")
}

func TestSeatbeltDaemonFinalReadOnlyCannotBePiercedByPolicyWrite(t *testing.T) {
	const (
		shared     = "/Users/dev/.config/opencode"
		descendant = shared + "/plugins/operator"
	)
	profile, params, err := renderSeatbeltProfile(
		nil,
		nil,
		sandboxpolicy.MountPlan{
			NetworkPosture: sandboxpolicy.NetworkHostOpen,
			Entries: []sandboxpolicy.MountEntry{{
				Path: descendant,
				Mode: sandboxpolicy.MountRW,
			}},
		},
		netip.AddrPort{},
		[]string{"/Users/dev/.tclaude/data"},
		"/private/tmp/tmux-501",
		"/private/var/folders/ab/runtime/T",
		nil,
		[]string{shared},
	)
	require.NoError(t, err)

	var sharedParam string
	for _, param := range params {
		if param.path == shared && strings.HasPrefix(param.name, "WRITE_DENY_") {
			sharedParam = param.name
			break
		}
	}
	require.NotEmpty(t, sharedParam,
		"daemon-final read-only root must emit its own write deny")
	rule := seatbeltRuleContaining(profile, `(param "`+sharedParam+`")`)
	assert.NotContains(t, rule, "_REOPEN_",
		"an earlier, more-specific policy write must not pierce daemon-final read-only state")
}

// Class 4 (host tmux control) is strictly unreachable at the applier itself:
// an ordinary profile may legitimately grant a parent of the socket directory,
// so the renderer has to refuse the descendant reopen on its own.
func TestSeatbeltClass4TmuxHideHasNoDescendantReopens(t *testing.T) {
	const tmuxDir = "/private/tmp/tmux-501"
	profile, params, err := renderSeatbeltProfile(
		nil,
		nil,
		sandboxpolicy.MountPlan{
			NetworkPosture: sandboxpolicy.NetworkHostOpen,
			Entries: []sandboxpolicy.MountEntry{{
				Path: tmuxDir + "/operator-reopen",
				Mode: sandboxpolicy.MountRW,
			}},
		},
		netip.AddrPort{},
		[]string{"/Users/dev/.tclaude/data"},
		tmuxDir,
		"/private/var/folders/ab/runtime/T",
		nil,
		nil,
	)
	require.NoError(t, err)

	tmuxDenyParams := []string{}
	for _, param := range params {
		if param.path == tmuxDir &&
			(strings.HasPrefix(param.name, "WRITE_DENY_") ||
				strings.HasPrefix(param.name, "READ_DENY_")) {
			tmuxDenyParams = append(tmuxDenyParams, param.name)
		}
	}
	require.Len(t, tmuxDenyParams, 2, "class 4 must emit strict read and write denies")
	for _, denyParam := range tmuxDenyParams {
		assert.NotContains(t, profile, denyParam+"_REOPEN",
			"class-4 read, write, and connect denies cannot inherit descendant reopens")
	}
}

// Class 3 (protected roots) reaches the same end state by a different route:
// unlike class 4, the renderer does not need its own guard, because no plan
// entry can name a protected descendant in the first place. Break-glass was
// the only thing that ever produced one, and TCL-791 removed it.
//
// So this test drives the whole real path — Normalize, then RenderMountPlan,
// then the darwin renderer — with the broadest legal profile, and asserts the
// rendered Seatbelt profile carves out nothing at or beneath a protected root.
// If a reopen path came back upstream, the carve-out would appear here.
func TestSeatbeltClass3ProtectedRootsGetNoCarveOutFromAnyLegalProfile(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, "work")
	for _, dir := range []string{
		filepath.Join(home, ".tclaude", "data"),
		filepath.Join(home, ".claude", "sessions"),
		workspace,
	} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}
	protectedRoots, err := sandboxpolicy.ProtectedPaths()
	require.NoError(t, err)
	require.Len(t, protectedRoots, 2)

	normalized, err := sandboxpolicy.Normalize(sandboxpolicy.Profile{
		Name: "deny-home-reopen-work",
		Filesystem: []sandboxpolicy.FilesystemGrant{
			{Path: home, Access: sandboxpolicy.AccessDeny},
			{Path: workspace, Access: sandboxpolicy.AccessWrite},
		},
	})
	require.NoError(t, err)
	plan, err := sandboxpolicy.RenderMountPlan(sandboxpolicy.EffectiveProfile{
		Filesystem: normalized.Filesystem,
	})
	require.NoError(t, err)

	_, params, err := renderSeatbeltProfile(
		nil,
		nil,
		plan,
		netip.AddrPort{},
		protectedRoots,
		"/private/tmp/tmux-501",
		"/private/var/folders/ab/runtime/T",
		nil,
		nil,
	)
	require.NoError(t, err)

	sawWorkspaceCarveOut := false
	for _, param := range params {
		if !strings.Contains(param.name, "_REOPEN_") {
			continue
		}
		if param.path == workspace {
			sawWorkspaceCarveOut = true
		}
		for _, root := range protectedRoots {
			assert.False(t, sandboxpolicy.PathContainsOrEqual(root, param.path),
				"%s carves out %s, which is at or beneath protected root %s",
				param.name, param.path, root)
		}
	}
	assert.True(t, sawWorkspaceCarveOut,
		"the legal reopen must produce a carve-out, or the protected check above is vacuous")
}

func TestSeatbeltCaseAndNFCCandidatesRequireFileIdentity(t *testing.T) {
	sameIdentity := func(path string) (seatbeltFileIdentity, bool) {
		switch path {
		case "/Users/Café", "/users/cafe\u0301":
			return seatbeltFileIdentity{dev: 1, ino: 42}, true
		default:
			return seatbeltFileIdentity{}, false
		}
	}
	ordered := []seatbeltRegion{
		{path: "/Users/Café", mode: sandboxpolicy.MountRO},
		{path: "/users/cafe\u0301", mode: sandboxpolicy.MountRW},
	}
	merged := buildSeatbeltRegionTree(ordered, sameIdentity)
	require.Len(t, merged, 1)
	assert.Equal(t, "/users/cafe\u0301", merged[0].path)
	assert.Equal(t, sandboxpolicy.MountRW, merged[0].mode)

	distinctIdentity := func(path string) (seatbeltFileIdentity, bool) {
		if path == "/Users/Café" {
			return seatbeltFileIdentity{dev: 1, ino: 42}, true
		}
		return seatbeltFileIdentity{dev: 1, ino: 43}, true
	}
	assert.Len(t, buildSeatbeltRegionTree(ordered, distinctIdentity), 2,
		"case-sensitive APFS paths must stay independent")
	assert.Len(t, buildSeatbeltRegionTree(ordered, nil), 2,
		"unknown identity must stay independent")
}

func TestSeatbeltFoldedContainmentAlsoRequiresFileIdentity(t *testing.T) {
	sameIdentity := func(path string) (seatbeltFileIdentity, bool) {
		switch path {
		case "/Users/Case", "/users/case":
			return seatbeltFileIdentity{dev: 7, ino: 9}, true
		default:
			return seatbeltFileIdentity{}, false
		}
	}
	assert.True(t, seatbeltPathContains("/Users/Case", "/users/case/child", sameIdentity))
	assert.False(t, seatbeltPathContains("/Users/Case", "/users/case/child", nil))
	assert.False(t, seatbeltPathContains(
		"/Users/Case",
		"/users/case/child",
		func(path string) (seatbeltFileIdentity, bool) {
			if path == "/Users/Case" {
				return seatbeltFileIdentity{dev: 7, ino: 9}, true
			}
			return seatbeltFileIdentity{dev: 7, ino: 10}, true
		},
	))
}

func TestSeatbeltAliasesCoverLinkAndResolvedTargetWithoutInterpolation(t *testing.T) {
	operatorPath := "/private/tmp/project weird;$HOME"
	profile, params, err := renderSeatbeltProfile(
		nil,
		nil,
		sandboxpolicy.MountPlan{
			NetworkPosture: sandboxpolicy.NetworkHostOpen,
			Entries: []sandboxpolicy.MountEntry{
				{Path: operatorPath, Mode: sandboxpolicy.MountRW},
				{Path: operatorPath + "/hidden", Mode: sandboxpolicy.MountHide},
			},
			Aliases: []sandboxpolicy.MountAlias{{
				Link:   "/tmp",
				Target: "/private/tmp",
			}},
		},
		netip.AddrPort{},
		[]string{"/Users/dev/.tclaude/data", "/Users/dev/.claude/sessions"},
		"/private/tmp/tmux-501",
		"/private/var/folders/ab/runtime/T",
		nil,
		nil,
	)
	require.NoError(t, err)
	assert.NotContains(t, profile, operatorPath,
		"operator-controlled paths must travel only through -D parameters")

	gotPaths := make([]string, 0, len(params))
	for _, param := range params {
		gotPaths = append(gotPaths, param.path)
		assert.True(t, strings.HasPrefix(param.path, "/"), "all Seatbelt -D values stay absolute")
	}
	assert.Contains(t, gotPaths, operatorPath)
	assert.Contains(t, gotPaths, "/tmp/project weird;$HOME")
	assert.Contains(t, gotPaths, operatorPath+"/hidden")
	assert.Contains(t, gotPaths, "/tmp/project weird;$HOME/hidden")
}

func TestSeatbeltAliasesRequireAbsoluteParameterizedPaths(t *testing.T) {
	_, _, err := renderSeatbeltProfile(
		nil,
		nil,
		sandboxpolicy.MountPlan{
			NetworkPosture: sandboxpolicy.NetworkHostOpen,
			Aliases: []sandboxpolicy.MountAlias{{
				Link:   "relative-link",
				Target: "/private/tmp",
			}},
		},
		netip.AddrPort{},
		[]string{"/Users/dev/.tclaude/data"},
		"/private/tmp/tmux-501",
		"/private/var/folders/ab/runtime/T",
		nil,
		nil,
	)
	require.ErrorContains(t, err, "mount alias 0 link has non-absolute path")
}

func TestRenderSeatbeltProfileRefusesUnsupportedFilteredAndInvalidPostures(t *testing.T) {
	domainRules, err := sandboxpolicy.CompileFilteredNetworkRules(sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{{
			Domain: "api.example.com",
		}},
	})
	require.NoError(t, err)
	for _, plan := range []sandboxpolicy.MountPlan{
		{NetworkPosture: sandboxpolicy.NetworkFiltered},
		{NetworkPosture: sandboxpolicy.NetworkFiltered, FilteredNetwork: &domainRules},
		{NetworkPosture: sandboxpolicy.NetworkPosture(99)},
	} {
		_, _, err := renderSeatbeltProfile(
			nil,
			nil,
			plan,
			netip.AddrPort{},
			[]string{"/Users/dev/.tclaude/data", "/Users/dev/.claude/sessions"},
			"/private/tmp/tmux-501",
			"/private/var/folders/ab/runtime/T",
			nil,
			nil,
		)
		require.Error(t, err)
	}
}

func assertSeatbeltAllowDenyOrder(t *testing.T, profile string) {
	t.Helper()
	firstDeny := strings.Index(profile, "\n(deny ")
	require.Greater(t, firstDeny, -1, "rendered Seatbelt profile must contain a deny rule")
	require.Equal(t, 1, strings.Count(profile, "\n(allow "),
		"rendered Seatbelt profile must contain only its initial allow-default rule")
	assert.NotContains(t, profile[firstDeny:], "\n(allow ",
		"rendered Seatbelt profile must never place an allow after a deny")
	t.Logf("actual rendered Seatbelt rule ordering:\n%s", seatbeltRuleHeaders(profile))
}

func seatbeltRuleHeaders(profile string) string {
	headers := make([]string, 0)
	for _, line := range strings.Split(profile, "\n") {
		if strings.HasPrefix(line, "(allow ") || strings.HasPrefix(line, "(deny ") {
			headers = append(headers, line)
		}
	}
	return strings.Join(headers, "\n")
}

func seatbeltRuleContaining(profile, marker string) string {
	markerIndex := strings.Index(profile, marker)
	if markerIndex < 0 {
		return ""
	}
	start := strings.LastIndex(profile[:markerIndex], "\n(deny ")
	if start < 0 {
		return ""
	}
	start++
	next := strings.Index(profile[markerIndex:], "\n(deny ")
	if next < 0 {
		return strings.TrimSpace(profile[start:])
	}
	return strings.TrimSpace(profile[start : markerIndex+next])
}
