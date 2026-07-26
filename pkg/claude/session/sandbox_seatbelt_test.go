package session

import (
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
		[]string{"/Users/dev/.tclaude/data/audit"},
		plan,
		[]string{"/Users/dev/.tclaude/data", "/Users/dev/.claude/sessions"},
		"/private/tmp/tmux-501",
		"/private/var/folders/ab/runtime/T",
		nil,
	)
	require.NoError(t, err)

	const want = `(version 1)
(allow default)

; Filesystem policy is deny-only. Positive descendants are carved out
; inside each deny predicate because a Seatbelt deny cannot be reopened
; by a later allow rule.

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

func TestRenderSeatbeltIsolatedNetworkProfileParameterizesAgentdAliases(t *testing.T) {
	const (
		agentd = "/Users/dev/.tclaude/api/agentd.sock"
		alias  = "/Users/dev/.tclaude-link/api/agentd.sock"
	)
	profile, params, err := renderSeatbeltProfile(
		nil,
		[]string{agentd},
		nil,
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
		[]string{"/Users/dev/.tclaude/data"},
		"/private/tmp/tmux-501",
		"/private/var/folders/ab/runtime/T",
		nil,
	)
	require.NoError(t, err)

	assert.Equal(t, 1, strings.Count(profile, "(deny network-bind)"))
	assert.Equal(t, 1, strings.Count(profile, "(deny network-inbound)"))
	assert.NotContains(t, profile, "(deny network*)")
	assert.NotContains(t, profile, "system-socket")

	exceptions := map[string]string{}
	for _, param := range params {
		if strings.HasPrefix(param.name, "AGENTD_SOCKET_") {
			exceptions[param.name] = param.path
		}
	}
	assert.ElementsMatch(t, []string{agentd, alias}, []string{
		exceptions["AGENTD_SOCKET_0"],
		exceptions["AGENTD_SOCKET_1"],
	})
	for name := range exceptions {
		assert.Contains(t, profile,
			`(remote unix-socket
        (literal (param "`+name+`")))`,
		)
	}
}

func TestRenderSeatbeltIsolatedNetworkHiddenAgentdHasNoPostureException(t *testing.T) {
	const agentd = "/Users/dev/.tclaude/api/agentd.sock"
	profile, params, err := renderSeatbeltProfile(
		nil,
		[]string{agentd},
		nil,
		sandboxpolicy.MountPlan{
			NetworkPosture: sandboxpolicy.NetworkIsolatedWithAgentd,
			Entries: []sandboxpolicy.MountEntry{{
				Path: agentd,
				Mode: sandboxpolicy.MountHide,
			}},
		},
		[]string{"/Users/dev/.tclaude/data"},
		"/private/tmp/tmux-501",
		"/private/var/folders/ab/runtime/T",
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
		nil,
		sandboxpolicy.MountPlan{
			NetworkPosture: sandboxpolicy.NetworkHostOpen,
			Entries: []sandboxpolicy.MountEntry{{
				Path: "/dev",
				Mode: sandboxpolicy.MountRO,
			}},
		},
		[]string{"/Users/dev/.tclaude/data", "/Users/dev/.claude/sessions"},
		"/private/tmp/tmux-501",
		"/private/var/folders/ab/runtime/T",
		nil,
	)
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(profile, `(require-not (literal "/dev/null"))`),
		"the runtime carveout belongs only to the root baseline deny")
	assert.Equal(t, 1, strings.Count(profile, `(require-not (literal "/dev/tty"))`))
	assert.Equal(t, 1, strings.Count(profile, `(require-not (literal "/dev/ptmx"))`))
	assert.Equal(t, 1, strings.Count(profile, `(require-not (subpath "/dev/fd"))`))
	assert.Equal(t, 1, strings.Count(profile, `(require-not (regex #"^/dev/(tty|pty)[A-Za-z0-9]+$"))`))
	assert.Contains(t, profile, `(param "WRITE_DENY_1")`,
		"the narrower /dev policy must retain a separate write deny with no runtime carveouts")
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
		nil,
		sandboxpolicy.MountPlan{
			NetworkPosture: sandboxpolicy.NetworkHostOpen,
			Entries: []sandboxpolicy.MountEntry{{
				Path: policySpelling,
				Mode: sandboxpolicy.MountRO,
			}},
		},
		[]string{"/Users/dev/.tclaude/data"},
		"/private/tmp/tmux-501",
		"/private/var/folders/ab/runtime/T",
		sameIdentity,
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
		nil,
		sandboxpolicy.MountPlan{
			NetworkPosture: sandboxpolicy.NetworkHostOpen,
			Entries: []sandboxpolicy.MountEntry{{
				Path: "/Users/dev",
				Mode: sandboxpolicy.MountHide,
			}},
		},
		[]string{"/Users/dev/.tclaude/data", "/Users/dev/.claude/sessions"},
		"/private/tmp/tmux-501",
		"/private/var/folders/ab/runtime/T",
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

func TestSeatbeltClass4TmuxHideHasNoDescendantReopens(t *testing.T) {
	const tmuxDir = "/private/tmp/tmux-501"
	profile, params, err := renderSeatbeltProfile(
		nil,
		nil,
		nil,
		sandboxpolicy.MountPlan{
			NetworkPosture: sandboxpolicy.NetworkHostOpen,
			Entries: []sandboxpolicy.MountEntry{{
				Path: tmuxDir + "/operator-reopen",
				Mode: sandboxpolicy.MountRW,
			}},
		},
		[]string{"/Users/dev/.tclaude/data"},
		tmuxDir,
		"/private/var/folders/ab/runtime/T",
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
		[]string{"/Users/dev/.tclaude/data", "/Users/dev/.claude/sessions"},
		"/private/tmp/tmux-501",
		"/private/var/folders/ab/runtime/T",
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
		nil,
		sandboxpolicy.MountPlan{
			NetworkPosture: sandboxpolicy.NetworkHostOpen,
			Aliases: []sandboxpolicy.MountAlias{{
				Link:   "relative-link",
				Target: "/private/tmp",
			}},
		},
		[]string{"/Users/dev/.tclaude/data"},
		"/private/tmp/tmux-501",
		"/private/var/folders/ab/runtime/T",
		nil,
	)
	require.ErrorContains(t, err, "mount alias 0 link has non-absolute path")
}

func TestRenderSeatbeltProfileRefusesFilteredAndInvalidPostures(t *testing.T) {
	for _, posture := range []sandboxpolicy.NetworkPosture{
		sandboxpolicy.NetworkFiltered,
		sandboxpolicy.NetworkPosture(99),
	} {
		_, _, err := renderSeatbeltProfile(
			nil,
			nil,
			nil,
			sandboxpolicy.MountPlan{NetworkPosture: posture},
			[]string{"/Users/dev/.tclaude/data", "/Users/dev/.claude/sessions"},
			"/private/tmp/tmux-501",
			"/private/var/folders/ab/runtime/T",
			nil,
		)
		require.Error(t, err)
	}
}
