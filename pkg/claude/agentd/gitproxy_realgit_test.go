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
	root := t.TempDir()
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
func TestRealGit_ProtocolPinsRefuseCommandTransports(t *testing.T) {
	gitPath := gitAvailable(t)
	work, _ := realGitRepo(t, gitPath)
	home := filepath.Dir(work)
	hooksDir := filepath.Join(home, "no-hooks")
	require.NoError(t, os.MkdirAll(hooksDir, 0o700))

	evidence := filepath.Join(home, "ext-evidence")
	pins := gitProxyConfigPins(hooksDir, "ssh -o BatchMode=yes", nil)
	for _, url := range []string{
		"ext::sh -c 'echo RAN >> " + evidence + "'",
		"file://" + work,
	} {
		args := append(append([]string{}, pins...), "ls-remote", "--", url)
		c := exec.Command(gitPath, args...)
		c.Dir = work
		c.Env = realGitEnv(home)
		out, err := c.CombinedOutput()
		assert.Errorf(t, err, "git must refuse %q; output=%s", url, out)
		assert.Contains(t, strings.ToLower(string(out)), "not allowed",
			"the refusal should come from the protocol policy for %q", url)
	}
	_, err := os.Stat(evidence)
	assert.True(t, os.IsNotExist(err), "no ext:: command may have run")
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
