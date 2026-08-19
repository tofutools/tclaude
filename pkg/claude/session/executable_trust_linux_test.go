//go:build linux

package session

import (
	"io/fs"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// stubTrustedExecutableWalk neutralizes the trust walk for tests that fake an
// executable onto PATH. The faked path does not exist on the runner — that is
// the point of faking it — so without this the walk refuses it at lstat.
func stubTrustedExecutableWalk(t *testing.T) {
	t.Helper()
	oldValidate, oldEval := validateTrustedExecutablePath, trustWalkEvalSymlinks
	t.Cleanup(func() {
		validateTrustedExecutablePath, trustWalkEvalSymlinks = oldValidate, oldEval
	})
	validateTrustedExecutablePath = func(string) error { return nil }
	trustWalkEvalSymlinks = func(path string) (string, error) { return path, nil }
}

type fakeTrustWalkFileInfo struct {
	name string
	mode fs.FileMode
	uid  uint32
}

func (f fakeTrustWalkFileInfo) Name() string       { return f.name }
func (f fakeTrustWalkFileInfo) Size() int64        { return 0 }
func (f fakeTrustWalkFileInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeTrustWalkFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeTrustWalkFileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeTrustWalkFileInfo) Sys() any           { return &syscall.Stat_t{Uid: f.uid} }

func TestResolveBwrapServerBinaryTrustWalksBwrapBeforeProbingIt(t *testing.T) {
	tree := map[string]fakeTrustWalkFileInfo{
		"/":              {name: "/", mode: fs.ModeDir | 0o755},
		"/usr":           {name: "usr", mode: fs.ModeDir | 0o755},
		"/usr/bin":       {name: "bin", mode: fs.ModeDir | 0o755},
		"/usr/lib":       {name: "lib", mode: fs.ModeDir | 0o755},
		"/usr/lib/bwrap": {name: "bwrap", mode: 0o755, uid: 1000},
		"/opt":           {name: "opt", mode: fs.ModeDir | 0o777},
		"/opt/bin":       {name: "bin", mode: fs.ModeDir | 0o755, uid: 1000},
		"/opt/bin/bwrap": {name: "bwrap", mode: 0o755, uid: 1000},
	}
	oldLookPath, oldProbe := lookPathBwrap, probeBwrap
	oldLstat, oldEval := trustWalkLstat, trustWalkEvalSymlinks
	t.Cleanup(func() {
		lookPathBwrap, probeBwrap = oldLookPath, oldProbe
		trustWalkLstat, trustWalkEvalSymlinks = oldLstat, oldEval
	})
	trustWalkLstat = func(path string) (fs.FileInfo, error) {
		info, ok := tree[path]
		if !ok {
			return nil, fs.ErrNotExist
		}
		return info, nil
	}

	// A bwrap under a world-writable prefix is refused, and the probe never
	// execs it: the walk is what decides whether that exec is safe.
	trustWalkEvalSymlinks = func(path string) (string, error) { return path, nil }
	lookPathBwrap = func(string) (string, error) { return "/opt/bin/bwrap", nil }
	probeBwrap = func(string, sandboxpolicy.NetworkPosture, sandboxpolicy.RootPosture) error {
		t.Error("an untrusted bwrap must be refused before it is probed")
		return nil
	}
	_, err := resolveBwrapServerBinary(
		sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootHostInherited)
	require.ErrorContains(t, err, "could not resolve a trusted bubblewrap")
	assert.ErrorContains(t, err, `"/opt" is group/world writable`)

	// A trusted one resolves, and what gets probed — and returned for the
	// launch to exec — is the path the walk actually described: the symlink
	// target, not the PATH entry that named it.
	lookPathBwrap = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	trustWalkEvalSymlinks = func(path string) (string, error) {
		assert.Equal(t, "/usr/bin/bwrap", path,
			"the PATH lookup's result is what gets canonicalized")
		return "/usr/lib/bwrap", nil
	}
	var probed string
	probeBwrap = func(binary string, _ sandboxpolicy.NetworkPosture, _ sandboxpolicy.RootPosture) error {
		probed = binary
		return nil
	}
	binary, err := resolveBwrapServerBinary(
		sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootHostInherited)
	require.NoError(t, err)
	assert.Equal(t, "/usr/lib/bwrap", binary)
	assert.Equal(t, "/usr/lib/bwrap", probed)
}

func TestFilteredNetworkTrustWalkAcceptsNonRootOwnedExecutables(t *testing.T) {
	const userPasta = "/home/agent/.local/bin/pasta"
	tree := map[string]fakeTrustWalkFileInfo{
		"/":                        {name: "/", mode: fs.ModeDir | 0o755},
		"/home":                    {name: "home", mode: fs.ModeDir | 0o755},
		"/home/agent":              {name: "agent", mode: fs.ModeDir | 0o755, uid: 1000},
		"/home/agent/.local":       {name: ".local", mode: fs.ModeDir | 0o755, uid: 1000},
		"/home/agent/.local/bin":   {name: "bin", mode: fs.ModeDir | 0o755, uid: 1000},
		userPasta:                  {name: "pasta", mode: 0o755, uid: 1000},
		"/usr":                     {name: "usr", mode: fs.ModeDir | 0o755},
		"/usr/bin":                 {name: "bin", mode: fs.ModeDir | 0o755},
		"/usr/bin/pasta":           {name: "pasta", mode: 0o755, uid: 1000},
		"/usr/bin/nft":             {name: "nft", mode: 0o755, uid: 1000},
		"/usr/local":               {name: "local", mode: fs.ModeDir | 0o777},
		"/usr/local/bin":           {name: "bin", mode: fs.ModeDir | 0o755, uid: 1000},
		"/usr/local/bin/pasta":     {name: "pasta", mode: 0o755, uid: 1000},
		"/home/agent/.local/pasta": {name: "pasta", mode: 0o644, uid: 1000},
	}
	oldLstat := trustWalkLstat
	t.Cleanup(func() { trustWalkLstat = oldLstat })
	trustWalkLstat = func(path string) (fs.FileInfo, error) {
		info, ok := tree[path]
		if !ok {
			return nil, fs.ErrNotExist
		}
		return info, nil
	}

	// Ownership is not part of the trust walk for any helper: a
	// user-installed build is trusted wherever it lives.
	require.NoError(t, validateTrustedExecutable(userPasta))
	require.NoError(t, validateTrustedExecutable("/usr/bin/pasta"),
		"a non-root-owned /usr/bin/pasta must still be trusted")
	require.NoError(t, validateTrustedExecutable("/usr/bin/nft"),
		"nft and nsenter are treated exactly like pasta")

	// The rest of the trust walk still applies.
	assert.ErrorContains(t,
		validateTrustedExecutable("/usr/local/bin/pasta"),
		`"/usr/local" is group/world writable`)
	assert.ErrorContains(t,
		validateTrustedExecutable("/home/agent/.local/pasta"),
		"not a regular executable")
}
