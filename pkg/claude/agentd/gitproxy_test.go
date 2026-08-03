package agentd

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// gitproxy_test.go pins the REFUSALS. Every case here is a way a repository an
// agent can write could otherwise aim the daemon's credentials somewhere the
// operator never authorized, or turn a proxied git call into command
// execution. A regression in any of them is silent — nothing looks different
// until it is exploited — so each gets its own named case.

// TestParseRemoteURL_RefusesCommandExecutingAndUnscopedTransports guards the
// sharpest edge in the whole feature: `.git/config` is agent-writable, and
// several remote-URL forms are not "a server" at all.
func TestParseRemoteURL_RefusesCommandExecutingAndUnscopedTransports(t *testing.T) {
	refused := []struct {
		name string
		url  string
		want string // substring the refusal must explain
	}{
		{"ext transport executes a command", "ext::sh -c 'id >&2'", "execute"},
		{"ext transport uppercase", "EXT::sh -c id", "execute"},
		{"file url has no host to allow-list", "file:///tmp/evil.git", "local-path"},
		{"absolute path has no host", "/tmp/evil.git", "local-path"},
		{"relative path has no host", "./evil.git", "local-path"},
		{"parent-relative path has no host", "../evil.git", "local-path"},
		{"http would send the credential in clear", "http://github.com/o/r.git", "clear text"},
		{"git protocol is unauthenticated", "git://github.com/o/r.git", "authenticated"},
		{"leading dash parses as a flag", "-oProxyCommand=id", "'-'"},
		{"embedded newline", "https://github.com/o/r\nfoo", "control characters"},
		{"embedded space", "https://github.com/o/ r", "whitespace"},
		{"embedded password https", "https://user:hunter2@github.com/o/r", "password"},
		{"embedded password scp", "user:hunter2@github.com:o/r", "password"},
		{"no repository path", "https://github.com/", "no repository path"},
		{"owner without repo", "https://github.com/onlyowner", "owner and repository"},
		{"explicit port cannot be allow-listed", "ssh://github.com:2222/o/r", "explicit port"},
		{"host is not a plain dns name", "ssh://[fe80::1]/o/r", "plain DNS name"},
		{"empty", "", "no URL"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseRemoteURL(tc.url)
			require.Error(t, err, "must refuse %q", tc.url)
			assert.Contains(t, strings.ToLower(err.Error()), strings.ToLower(tc.want),
				"the refusal should explain WHY: %v", err)
		})
	}
}

// TestParseRemoteURL_AcceptsOrdinaryForms is the other half: the refusals must
// not be so broad that a normal remote stops working.
func TestParseRemoteURL_AcceptsOrdinaryForms(t *testing.T) {
	cases := []struct {
		url    string
		scheme string
		key    string
	}{
		{"https://github.com/tofutools/tclaude.git", "https", "github.com/tofutools/tclaude"},
		{"https://github.com/tofutools/tclaude", "https", "github.com/tofutools/tclaude"},
		{"ssh://git@github.com/tofutools/tclaude.git", "ssh", "github.com/tofutools/tclaude"},
		{"git@github.com:tofutools/tclaude.git", "ssh", "github.com/tofutools/tclaude"},
		{"git@GitHub.com:TofuTools/TClaude.git", "ssh", "github.com/tofutools/tclaude"},
		{"https://gitlab.com/group/subgroup/proj.git", "https", "gitlab.com/group/subgroup/proj"},
	}
	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			ref, err := parseRemoteURL(tc.url)
			require.NoError(t, err)
			assert.Equal(t, tc.scheme, ref.Scheme)
			assert.Equal(t, tc.key, ref.Key())
		})
	}
}

// TestRemoteRef_OwnerRepo checks the gh --repo derivation, including the
// nested-group case where first+last is the right answer for GitHub and
// harmless elsewhere.
func TestRemoteRef_OwnerRepo(t *testing.T) {
	ref, err := parseRemoteURL("https://github.com/tofutools/tclaude.git")
	require.NoError(t, err)
	assert.Equal(t, "tofutools/tclaude", ref.OwnerRepo())

	nested, err := parseRemoteURL("https://gitlab.com/group/sub/proj.git")
	require.NoError(t, err)
	assert.Equal(t, "group/proj", nested.OwnerRepo())
}

// TestRemoteAllowed pins the allow-list semantics an operator relies on: a
// shorter pattern is a prefix, "*" is exactly one segment, and — the
// load-bearing case — an empty list allows NOTHING.
func TestRemoteAllowed(t *testing.T) {
	ref, err := parseRemoteURL("https://github.com/tofutools/tclaude.git")
	require.NoError(t, err)
	evil, err := parseRemoteURL("https://github.com/attacker/exfil.git")
	require.NoError(t, err)
	otherHost, err := parseRemoteURL("https://gitlab.com/tofutools/tclaude.git")
	require.NoError(t, err)

	assert.False(t, remoteAllowed(ref, nil), "an empty allow-list must allow nothing")
	assert.False(t, remoteAllowed(ref, []string{}), "an empty allow-list must allow nothing")

	assert.True(t, remoteAllowed(ref, []string{"github.com"}), "host prefix covers the host")
	assert.True(t, remoteAllowed(evil, []string{"github.com"}), "host prefix covers every owner on it")
	assert.True(t, remoteAllowed(ref, []string{"github.com/tofutools"}), "owner prefix")
	assert.False(t, remoteAllowed(evil, []string{"github.com/tofutools"}), "owner prefix excludes other owners")
	assert.True(t, remoteAllowed(ref, []string{"github.com/tofutools/*"}), "explicit repo wildcard")
	assert.True(t, remoteAllowed(ref, []string{"github.com/tofutools/tclaude"}), "exact repo")
	assert.False(t, remoteAllowed(ref, []string{"github.com/tofutools/other"}), "exact repo excludes siblings")
	assert.False(t, remoteAllowed(otherHost, []string{"github.com/tofutools"}), "the host segment is matched too")
	assert.False(t, remoteAllowed(ref, []string{"github.com/tofutools/tclaude/extra"}),
		"a longer pattern than the target must not match")
}

// TestRemoteAllowed_PrefixIsSegmentwise catches the classic string-prefix bug:
// "github.com/tofu" must not authorize "github.com/tofutools-evil".
func TestRemoteAllowed_PrefixIsSegmentwise(t *testing.T) {
	lookalike, err := parseRemoteURL("https://github.com/tofutools-evil/x.git")
	require.NoError(t, err)
	assert.False(t, remoteAllowed(lookalike, []string{"github.com/tofutools"}),
		"a partial segment must not match")
	evilHost, err := parseRemoteURL("https://github.com.attacker.net/tofutools/x.git")
	require.NoError(t, err)
	assert.False(t, remoteAllowed(evilHost, []string{"github.com"}),
		"a lookalike host must not match")
}

// TestValidateBranchName pins git's ref rules locally. The leading-"-" case is
// the security-relevant one — it would reach argv as a flag.
func TestValidateBranchName(t *testing.T) {
	for _, bad := range []string{
		"", "-x", "--force", "feat/", "/feat", "a//b", "a..b", "a@{0}", "a.lock", "a.",
		"has space", "ti~lde", "ca^ret", "co:lon", "quest?ion", "sta*r", "brack[et",
		"back\\slash", "ctrl\x01char", "@", "HEAD",
	} {
		assert.NotNil(t, validateBranchName(bad), "must refuse branch %q", bad)
	}
	for _, good := range []string{
		"main", "feat/thing", "release/1.2", "a_b-c.d", "TCL-960-opus",
	} {
		assert.Nil(t, validateBranchName(good), "must accept branch %q", good)
	}
	assert.NotNil(t, validateBranchName(strings.Repeat("a", maxGitProxyRefLen+1)), "must bound length")
}

// TestValidateRemoteName keeps a remote name safe both as argv and as the
// `-c remote.<name>.uploadpack=…` key it is interpolated into.
func TestValidateRemoteName(t *testing.T) {
	for _, bad := range []string{"", "-o", ".hidden", "has space", "sla/sh", "quo\"te", "semi;colon"} {
		assert.NotNil(t, validateRemoteName(bad), "must refuse remote %q", bad)
	}
	for _, good := range []string{"origin", "up-stream", "fork_2", "a.b"} {
		assert.Nil(t, validateRemoteName(good), "must accept remote %q", good)
	}
	assert.NotNil(t, validateRemoteName(strings.Repeat("a", 101)), "must bound length")
}

// TestRefProtected pins protected-branch matching, including the namespace
// wildcard an operator would write for release branches.
func TestRefProtected(t *testing.T) {
	refs := []string{"main", "master", "release/*"}
	assert.True(t, refProtected("main", refs))
	assert.True(t, refProtected("MAIN", refs), "matching is case-insensitive")
	assert.True(t, refProtected("release/1.2", refs))
	assert.True(t, refProtected("release/", refs), "an empty tail still matches the namespace")
	assert.False(t, refProtected("feat/thing", refs))
	assert.False(t, refProtected("mainline", refs), "an exact pattern must not prefix-match")
	assert.False(t, refProtected("main", nil), "an empty protection list protects nothing")
}

// TestValidateRefPattern keeps the ls-remote positional from becoming a flag.
func TestValidateRefPattern(t *testing.T) {
	assert.NotNil(t, validateRefPattern("-x"))
	assert.NotNil(t, validateRefPattern("refs/heads/a b"))
	assert.NotNil(t, validateRefPattern("refs/heads/$(id)"))
	assert.Nil(t, validateRefPattern("refs/heads/feat-*"))
	assert.Nil(t, validateRefPattern("v1.2.3"))
}

// TestGitProxyConfigPins is the regression guard with the least visible
// failure mode: if a pin is dropped, everything keeps working — right up until
// a repo-local hook or transport is used to run code as the operator. So the
// exact set is asserted by key.
func TestGitProxyConfigPins(t *testing.T) {
	args := gitProxyConfigPins("/daemon/no-hooks", "ssh -o BatchMode=yes", "origin", nil)

	// Every pin must arrive as a separate `-c` flag, never folded into one.
	pins := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "-c" {
			continue
		}
		k, v, _ := strings.Cut(args[i+1], "=")
		pins[k] = v
	}

	assert.Equal(t, "/daemon/no-hooks", pins["core.hooksPath"],
		"hooks MUST be redirected: .git/hooks/pre-push is agent-writable")
	assert.Equal(t, "false", pins["core.fsmonitor"])
	assert.Equal(t, "", pins["core.alternateRefsCommand"])
	assert.Equal(t, "ssh -o BatchMode=yes", pins["core.sshCommand"])
	assert.Equal(t, "false", pins["core.editor"])
	assert.Equal(t, "cat", pins["core.pager"])
	assert.Equal(t, "false", pins["gpg.program"])
	assert.Equal(t, "", pins["diff.external"])
	assert.Equal(t, "", pins["http.proxy"])
	assert.Equal(t, "0", pins["gc.auto"])
	assert.Equal(t, "false", pins["maintenance.auto"])

	// Transport restriction: deny by default, re-allow exactly https+ssh.
	assert.Equal(t, "never", pins["protocol.allow"], "ext:: and file: must be denied by default")
	assert.Equal(t, "always", pins["protocol.https.allow"])
	assert.Equal(t, "always", pins["protocol.ssh.allow"])
	assert.Equal(t, "never", pins["protocol.file.allow"])
	assert.Equal(t, "never", pins["protocol.ext.allow"])

	// Server-side command execution through the named remote.
	assert.Equal(t, "git-upload-pack", pins["remote.origin.uploadpack"])
	assert.Equal(t, "git-receive-pack", pins["remote.origin.receivepack"])

	assert.True(t, slices.Contains(args, "--no-pager"), "git must not try to page into a daemon")
}

// TestGitProxyConfigPins_CredentialHelperResetThenReadd pins the ordering that
// makes the helper handling correct: the empty value FIRST (which clears any
// repo-local helper — an arbitrary command), then the operator's own global
// helpers.
func TestGitProxyConfigPins_CredentialHelperResetThenReadd(t *testing.T) {
	args := gitProxyConfigPins("/h", "ssh", "", []string{"osxkeychain", "cache --timeout=300"})
	var helpers []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-c" && strings.HasPrefix(args[i+1], "credential.helper=") {
			helpers = append(helpers, strings.TrimPrefix(args[i+1], "credential.helper="))
		}
	}
	require.Equal(t, []string{"", "osxkeychain", "cache --timeout=300"}, helpers,
		"the reset must come first, or a repo-local helper survives")
}

// TestGitProxyConfigPins_NoRemoteOmitsRemoteKeys — a route that names no
// remote must not emit a `remote..uploadpack` key.
func TestGitProxyConfigPins_NoRemoteOmitsRemoteKeys(t *testing.T) {
	args := gitProxyConfigPins("/h", "ssh", "", nil)
	for _, a := range args {
		assert.False(t, strings.HasPrefix(a, "remote."),
			"no remote named → no remote.* pin, got %q", a)
	}
}

// TestGitProxyEnv_ConstructsRatherThanFilters is why the environment is built
// from an allow-list: a deny-list would have to track git forever.
func TestGitProxyEnv_ConstructsRatherThanFilters(t *testing.T) {
	t.Setenv("GIT_SSH_COMMAND", "sh -c id")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
	t.Setenv("GIT_CONFIG_VALUE_0", "/tmp/evil")
	t.Setenv("GIT_ASKPASS", "/tmp/evil")
	t.Setenv("GH_TOKEN", "secret")
	t.Setenv("HOME", "/home/operator")
	t.Setenv("PATH", "/usr/bin")

	env := gitProxyEnv()
	joined := strings.Join(env, "\n")
	for _, leaked := range []string{"GIT_SSH_COMMAND", "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_ASKPASS", "GH_TOKEN"} {
		assert.NotContains(t, joined, leaked+"=", "%s must not reach the child", leaked)
	}
	assert.Contains(t, env, "HOME=/home/operator", "the operator's git/ssh config must stay reachable")
	assert.Contains(t, env, "PATH=/usr/bin", "git needs its transport helpers")
	assert.Contains(t, env, "GIT_TERMINAL_PROMPT=0", "the daemon has no terminal to prompt on")
	assert.Contains(t, env, "LC_ALL=C")
}

// TestGitProxySSHCommand pins BatchMode, which is what turns "the key needs a
// passphrase" into a fast failure instead of a hung daemon goroutine.
func TestGitProxySSHCommand(t *testing.T) {
	assert.Equal(t, "ssh -o BatchMode=yes", gitProxySSHCommand(config.GitProxyConfig{}))
	assert.Equal(t, "ssh -o BatchMode=yes -i /keys/id_ed25519 -o IdentitiesOnly=yes",
		gitProxySSHCommand(config.GitProxyConfig{SSHKey: "/keys/id_ed25519"}))
}

// TestResolvedGitProxy_Defaults pins the two fail-closed defaults an operator
// depends on without reading the code: no allow-list means OFF, and main/master
// are protected unless they explicitly say otherwise.
func TestResolvedGitProxy_Defaults(t *testing.T) {
	var nilCfg *config.Config
	assert.Empty(t, nilCfg.ResolvedGitProxy().AllowedRemotes, "absent config must allow nothing")
	assert.False(t, nilCfg.GitProxyEnabled())

	empty := &config.Config{}
	assert.False(t, empty.GitProxyEnabled(), "an agent block with no git_proxy must stay off")

	on := &config.Config{Agent: &config.AgentConfig{GitProxy: &config.GitProxyConfig{
		AllowedRemotes: []string{" GitHub.com/TofuTools/ ", "", "github.com/tofutools"},
	}}}
	resolved := on.ResolvedGitProxy()
	assert.Equal(t, []string{"github.com/tofutools"}, resolved.AllowedRemotes,
		"patterns are trimmed, lower-cased and de-duplicated")
	assert.True(t, on.GitProxyEnabled())
	require.NotNil(t, resolved.ProtectedRefs)
	assert.Equal(t, config.DefaultGitProxyProtectedRefs, *resolved.ProtectedRefs)
	assert.False(t, resolved.AllowForcePush, "force-push is opt-in")

	// An explicit empty list is a deliberate "protect nothing", distinct from
	// an absent one — which is why the field is a pointer.
	none := []string{}
	off := &config.Config{Agent: &config.AgentConfig{GitProxy: &config.GitProxyConfig{
		AllowedRemotes: []string{"github.com"},
		ProtectedRefs:  &none,
	}}}
	require.NotNil(t, off.ResolvedGitProxy().ProtectedRefs)
	assert.Empty(t, *off.ResolvedGitProxy().ProtectedRefs)
}
