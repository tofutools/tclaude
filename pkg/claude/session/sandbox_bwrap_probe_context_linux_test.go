//go:build linux

package session

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// stubTmuxProbeContext replaces both halves of the round trip and restores them
// after the test, so nothing here forks tmux or bubblewrap.
func stubTmuxProbeContext(
	t *testing.T,
	serverPID func() (int, error),
	viaTmux func(string, sandboxpolicy.NetworkPosture, sandboxpolicy.RootPosture) (bool, error),
) {
	t.Helper()
	oldPID, oldVia := tmuxServerPID, probeBwrapViaTmuxServer
	t.Cleanup(func() {
		tmuxServerPID = oldPID
		probeBwrapViaTmuxServer = oldVia
		bwrapProbeCache.reset()
	})
	bwrapProbeCache.reset()
	tmuxServerPID = serverPID
	probeBwrapViaTmuxServer = viaTmux
}

// The refusal contract only holds if the probe's verdict is the one that
// decides. A tmux-server probe that says no must not be second-guessed by an
// in-process probe standing in a confinement the launch never uses.
func TestProbeBwrapInLaunchContextRefusesOnTheTmuxServerVerdict(t *testing.T) {
	denied := errors.New("bwrap: setting up uid map: Permission denied")
	calls := 0
	stubTmuxProbeContext(t,
		func() (int, error) { return 4242, nil },
		func(string, sandboxpolicy.NetworkPosture, sandboxpolicy.RootPosture) (bool, error) {
			calls++
			return true, denied
		},
	)

	err := probeBwrapInLaunchContext("/usr/bin/bwrap",
		sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootHostInherited)
	require.ErrorIs(t, err, denied)
	assert.Equal(t, 1, calls)
}

// No tmux server means THIS process is the one that will auto-start it, so its
// confinement is the one the pane inherits and the in-process probe is the
// faithful answer — not a weaker fallback.
func TestProbeBwrapInLaunchContextProbesInProcessWithoutATmuxServer(t *testing.T) {
	stubTmuxProbeContext(t,
		func() (int, error) { return 0, errors.New("no server running on /tmp/tmux-1000/tclaude") },
		func(string, sandboxpolicy.NetworkPosture, sandboxpolicy.RootPosture) (bool, error) {
			t.Fatal("no tmux server exists; the round trip must not be attempted")
			return false, nil
		},
	)

	// /bin/true stands in for bubblewrap: it ignores the probe argv and exits 0,
	// so a nil result proves the in-process path ran and nothing else did.
	require.NoError(t, probeBwrapInLaunchContext("/bin/true",
		sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootHostInherited))
}

// A round trip that produces no verdict is infrastructure trouble, not evidence
// that the capability is missing. Collapsing the two would turn a tmux hiccup
// into a refused launch on a host that can run the boundary perfectly well.
func TestProbeBwrapInLaunchContextFallsBackWhenTheRoundTripProducesNoVerdict(t *testing.T) {
	stubTmuxProbeContext(t,
		func() (int, error) { return 4242, nil },
		func(string, sandboxpolicy.NetworkPosture, sandboxpolicy.RootPosture) (bool, error) {
			return false, nil
		},
	)

	require.NoError(t, probeBwrapInLaunchContext("/bin/true",
		sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootHostInherited))
}

func TestProbeBwrapInLaunchContextCachesPositivesPerTmuxServer(t *testing.T) {
	pid := 4242
	calls := 0
	stubTmuxProbeContext(t,
		func() (int, error) { return pid, nil },
		func(string, sandboxpolicy.NetworkPosture, sandboxpolicy.RootPosture) (bool, error) {
			calls++
			return true, nil
		},
	)

	for range 3 {
		require.NoError(t, probeBwrapInLaunchContext("/usr/bin/bwrap",
			sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootHostInherited))
	}
	assert.Equal(t, 1, calls, "one server, one posture: the round trip is made once")

	// A different posture asks a different question of the same server.
	require.NoError(t, probeBwrapInLaunchContext("/usr/bin/bwrap",
		sandboxpolicy.NetworkFiltered, sandboxpolicy.RootConstructed))
	assert.Equal(t, 2, calls)

	// A restarted server is the event that can change the confinement being
	// described, so its answer must be taken fresh.
	pid = 5353
	require.NoError(t, probeBwrapInLaunchContext("/usr/bin/bwrap",
		sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootHostInherited))
	assert.Equal(t, 3, calls)
}

func TestBwrapProbeCacheSeparatesCallerIdentityMode(t *testing.T) {
	memo := &bwrapProbeMemo{}
	base := bwrapProbeKey{
		binary: "/usr/bin/bwrap", posture: sandboxpolicy.NetworkFiltered,
		root: sandboxpolicy.RootConstructed,
	}
	memo.record(4242, base)
	assert.True(t, memo.healthy(4242, base))
	base.preserveCallerIdentity = true
	assert.False(t, memo.healthy(4242, base),
		"a namespace-root pass must not answer the caller-identity probe")
}

// A tmux server can outlive any number of host changes, so the pid alone does
// not bound a remembered yes. ProbeFilteredNetworkPrerequisite DISCLOSES this
// answer: an operator who turns a prerequisite off must stop being told the
// boundary is available, without having to restart tmux.
func TestProbeBwrapInLaunchContextExpiresARememberedPass(t *testing.T) {
	calls := 0
	stubTmuxProbeContext(t,
		func() (int, error) { return 4242, nil },
		func(string, sandboxpolicy.NetworkPosture, sandboxpolicy.RootPosture) (bool, error) {
			calls++
			return true, nil
		},
	)
	now := time.Now()
	bwrapProbeCache.now = func() time.Time { return now }

	require.NoError(t, probeBwrapInLaunchContext("/usr/bin/bwrap",
		sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootHostInherited))
	now = now.Add(bwrapProbeMemoTTL - time.Second)
	require.NoError(t, probeBwrapInLaunchContext("/usr/bin/bwrap",
		sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootHostInherited))
	assert.Equal(t, 1, calls, "inside the window the remembered pass answers")

	now = now.Add(2 * time.Second)
	require.NoError(t, probeBwrapInLaunchContext("/usr/bin/bwrap",
		sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootHostInherited))
	assert.Equal(t, 2, calls, "past the window the host is asked again")
}

// TCL-769: a caller that REFUSES on this predicate must never be answered from
// cache, or an operator who has just fixed the host stays refused.
func TestProbeBwrapInLaunchContextNeverCachesARefusal(t *testing.T) {
	calls := 0
	stubTmuxProbeContext(t,
		func() (int, error) { return 4242, nil },
		func(string, sandboxpolicy.NetworkPosture, sandboxpolicy.RootPosture) (bool, error) {
			calls++
			if calls == 1 {
				return true, errors.New("operation not permitted")
			}
			return true, nil
		},
	)

	require.Error(t, probeBwrapInLaunchContext("/usr/bin/bwrap",
		sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootHostInherited))
	require.NoError(t, probeBwrapInLaunchContext("/usr/bin/bwrap",
		sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootHostInherited))
	assert.Equal(t, 2, calls)
}

func TestParseTclaudeLayerProbeResult(t *testing.T) {
	ran, verdict := parseTclaudeLayerProbeResult("ok\n")
	assert.True(t, ran)
	assert.NoError(t, verdict)

	ran, verdict = parseTclaudeLayerProbeResult("err bwrap: Operation not permitted")
	assert.True(t, ran)
	assert.ErrorContains(t, verdict, "bwrap: Operation not permitted")

	// A half-written or foreign file is evidence about nothing, exactly like a
	// file that was never written — never a refusal.
	for _, raw := range []string{"", "   ", "okay", "e", "Pane is dead"} {
		ran, verdict = parseTclaudeLayerProbeResult(raw)
		assert.False(t, ran, "raw %q", raw)
		assert.NoError(t, verdict)
	}
}

// run-shell format-expands its command before any shell sees it, so `#` is
// significant inside single quotes too. A checkout under a path containing one
// would otherwise exec a mangled path and degrade silently.
func TestTclaudeLayerProbeShellCommandEscapesTmuxFormatCharacters(t *testing.T) {
	command := tclaudeLayerProbeShellCommand("/usr/bin/bwrap",
		sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootHostInherited,
		"/tmp/work#{pid}/result")

	// `##` is tmux's escape for a literal `#`, so the expansion tmux performs on
	// this string yields the path back unchanged instead of substituting the
	// server pid into it.
	assert.Contains(t, command, "'/tmp/work##{pid}/result'")
	assert.Equal(t, "a##b####c", escapeTmuxFormat("a#b##c"))
	// No odd-length run of '#' may survive: one of those is a live format
	// introducer or alias.
	for _, run := range regexp.MustCompile(`#+`).FindAllString(command, -1) {
		assert.Zero(t, len(run)%2, "unescaped # run %q", run)
	}
}

// The probe subcommand re-parses posture tokens on the far side of the round
// trip. A posture added to tclaudeLayerProbeArgs but not here would make the
// job error out — and an errored job is indistinguishable from a host with no
// tmux server, so the fix would silently stop applying to that posture.
func TestPostureTokensRoundTripThroughTheProbeSubcommand(t *testing.T) {
	for _, posture := range []sandboxpolicy.NetworkPosture{
		sandboxpolicy.NetworkHostOpen,
		sandboxpolicy.NetworkIsolatedWithAgentd,
		sandboxpolicy.NetworkFiltered,
	} {
		parsed, err := parseNetworkPostureToken(posture.String())
		require.NoError(t, err, "network posture %s", posture)
		assert.Equal(t, posture, parsed)
	}
	for _, root := range []sandboxpolicy.RootPosture{
		sandboxpolicy.RootHostInherited,
		sandboxpolicy.RootConstructed,
	} {
		parsed, err := parseRootPostureToken(root.String())
		require.NoError(t, err, "root posture %s", root)
		assert.Equal(t, root, parsed)
	}

	// Every posture tclaudeLayerProbeArgs accepts must also be nameable, or the
	// round trip cannot ask for it.
	for posture := sandboxpolicy.NetworkPosture(0); ; posture++ {
		if _, err := tclaudeLayerProbeArgs(posture, sandboxpolicy.RootHostInherited); err != nil {
			break
		}
		_, err := parseNetworkPostureToken(posture.String())
		assert.NoError(t, err,
			"probe args accept network posture %d but the subcommand cannot parse its token", int(posture))
	}
}

func TestTclaudeLayerProbeShellCommandInvokesTheTclaudeBinary(t *testing.T) {
	command := tclaudeLayerProbeShellCommand("/usr/bin/bwrap",
		sandboxpolicy.NetworkFiltered, sandboxpolicy.RootConstructed, "/tmp/probe dir/result")

	assert.Contains(t, command, clcommon.ShellQuoteArg(clcommon.SelfTclaudePath()))
	assert.Contains(t, command, " session "+tclaudeLayerProbeCommand+" ")
	assert.Contains(t, command, "--network-posture filtered")
	assert.Contains(t, command, "--root-posture constructed")
	// The result path is ours, but it still reaches a shell, so it is quoted.
	assert.Contains(t, command, clcommon.ShellQuoteArg("/tmp/probe dir/result"))
	assert.NotContains(t, command, "--result /tmp/probe dir/result")
}

func TestTclaudeLayerProbeCmdWritesItsVerdictToTheResultFile(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		bwrap  string
		expect string
	}{
		// /bin/true ignores the probe argv and exits 0 — a stand-in for a host
		// whose confinement lets the exec through.
		{name: "capability present", bwrap: "/bin/true", expect: "ok"},
		{name: "capability missing", bwrap: filepath.Join(t.TempDir(), "absent-bwrap"), expect: "err "},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			resultPath := filepath.Join(t.TempDir(), "result")
			cmd := tclaudeLayerProbeCmd()
			cmd.SetArgs([]string{
				"--bwrap", testCase.bwrap,
				"--network-posture", "host-open",
				"--root-posture", "host-inherited",
				"--result", resultPath,
			})
			require.NoError(t, cmd.Execute())

			raw, err := os.ReadFile(resultPath)
			require.NoError(t, err)
			assert.True(t, strings.HasPrefix(string(raw), testCase.expect),
				"result %q does not start with %q", string(raw), testCase.expect)
		})
	}
}

func TestTclaudeLayerProbeCmdRejectsUnusableArguments(t *testing.T) {
	for _, testCase := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "unknown network posture",
			args: []string{"--network-posture", "wide-open", "--root-posture", "host-inherited", "--result", "/tmp/x"},
			want: "unknown network posture",
		},
		{
			name: "unknown root posture",
			args: []string{"--network-posture", "host-open", "--root-posture", "borrowed", "--result", "/tmp/x"},
			want: "unknown root posture",
		},
		{
			name: "missing result path",
			args: []string{"--network-posture", "host-open", "--root-posture", "host-inherited"},
			want: "--result is required",
		},
		{
			name: "missing bwrap binary",
			args: []string{"--network-posture", "host-open", "--root-posture", "host-inherited", "--result", "/tmp/x"},
			want: "--bwrap is required",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			cmd := tclaudeLayerProbeCmd()
			cmd.SetArgs(testCase.args)
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			assert.ErrorContains(t, cmd.Execute(), testCase.want)
		})
	}
}

// Every argument-validation refusal must leave NO verdict behind. Publishing
// one would make the caller's malformed invocation look like the host refusing
// the capability, and resolveBwrapServerBinary would refuse the launch for it.
func TestTclaudeLayerProbeCmdPublishesNoVerdictForAMalformedInvocation(t *testing.T) {
	resultPath := filepath.Join(t.TempDir(), "result")
	cmd := tclaudeLayerProbeCmd()
	cmd.SetArgs([]string{
		"--network-posture", "host-open",
		"--root-posture", "host-inherited",
		"--result", resultPath,
		// --bwrap deliberately absent: an empty program name execs, fails, and
		// would otherwise be published as a capability refusal.
	})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true

	require.Error(t, cmd.Execute())
	_, err := os.Stat(resultPath)
	assert.ErrorIs(t, err, fs.ErrNotExist)
}

// The waiting side polls a filename. A verdict that appears under its final
// name before it is whole would be read as an unparseable file, discarding a
// genuine refusal.
func TestWriteTclaudeLayerProbeResultPublishesAtomically(t *testing.T) {
	dir := t.TempDir()
	resultPath := filepath.Join(dir, "result")
	require.NoError(t, writeTclaudeLayerProbeResult(resultPath, "err denied"))

	raw, err := os.ReadFile(resultPath)
	require.NoError(t, err)
	assert.Equal(t, "err denied", string(raw))
	_, err = os.Stat(resultPath + ".partial")
	assert.ErrorIs(t, err, fs.ErrNotExist, "the staging name must not survive a successful publish")
}

// probeJobTmux stands in for the tmux server: it accepts the run-shell job,
// records the command it was handed, and plays the job's part by writing the
// verdict file — without executing anything.
type probeJobTmux struct {
	commands []string
	// write is the verdict the simulated job publishes; empty means the job
	// produced nothing, as a killed or never-scheduled job would.
	write string
	// refuse makes tmux reject the job itself — no server, or a run-shell an
	// old tmux does not know. The verdict, if any, is still published: a client
	// that failed says nothing about what the job managed to write.
	refuse bool
	// writeAfter delays the write past run-shell's return, standing in for a
	// tmux whose run-shell did not wait for its job.
	writeAfter time.Duration
	// argv records every tmux invocation, so the two tmux facts this design
	// rides on — no -b, and a pid lookup that cannot start a server — are
	// pinned rather than assumed.
	argv [][]string
}

func (p *probeJobTmux) Command(args ...string) *exec.Cmd {
	p.argv = append(p.argv, append([]string(nil), args...))
	if len(args) == 2 && args[0] == "run-shell" {
		p.commands = append(p.commands, args[1])
		if p.write != "" {
			resultPath := probeResultPathFromCommand(args[1])
			switch {
			case resultPath == "":
			case p.writeAfter > 0:
				go func() {
					time.Sleep(p.writeAfter)
					_ = os.WriteFile(resultPath, []byte(p.write), 0o600)
				}()
			default:
				_ = os.WriteFile(resultPath, []byte(p.write), 0o600)
			}
		}
		if p.refuse {
			return exec.Command("/bin/false")
		}
		return exec.Command("/bin/true")
	}
	if len(args) == 3 && args[0] == "display-message" {
		return exec.Command("/bin/echo", "4242")
	}
	return exec.Command("/bin/false")
}

func (p *probeJobTmux) ListSessions() (map[string]struct{}, error) {
	return map[string]struct{}{}, nil
}

func probeResultPathFromCommand(command string) string {
	_, after, found := strings.Cut(command, "--result ")
	if !found {
		return ""
	}
	return strings.Trim(strings.TrimSpace(after), "'")
}

func TestProbeBwrapViaTmuxServerReturnsTheJobsVerdict(t *testing.T) {
	fake := &probeJobTmux{write: "err bwrap: Operation not permitted"}
	swapTmux(t, fake)

	ran, verdict := probeBwrapViaTmuxServer("/usr/bin/bwrap",
		sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootHostInherited)
	assert.True(t, ran)
	assert.ErrorContains(t, verdict, "bwrap: Operation not permitted")
	require.Len(t, fake.commands, 1)
	assert.Contains(t, fake.commands[0], " session "+tclaudeLayerProbeCommand+" ")
}

// tmux refusing the job outright — no server, an unknown command on an old
// tmux — must be reported as "nothing was measured" and must not be waited out.
func TestProbeBwrapViaTmuxServerReportsNoVerdictWhenTmuxRefusesTheJob(t *testing.T) {
	swapTmux(t, &probeJobTmux{refuse: true})

	start := time.Now()
	ran, verdict := probeBwrapViaTmuxServer("/usr/bin/bwrap",
		sandboxpolicy.NetworkFiltered, sandboxpolicy.RootConstructed)
	assert.False(t, ran)
	assert.NoError(t, verdict)
	assert.Less(t, time.Since(start), tclaudeLayerProbeResultGrace,
		"a refused job must not be waited on for a verdict it will never write")
}

// run-shell hands the JOB's exit status to its client, so a job that published
// a refusal and then exited non-zero would have that refusal thrown away if the
// client's fate decided. A genuine capability refusal must not be downgraded to
// a fallback by anything except the absence of a verdict.
func TestProbeBwrapViaTmuxServerKeepsAVerdictTheFailedClientAlreadyPublished(t *testing.T) {
	swapTmux(t, &probeJobTmux{refuse: true, write: "err bwrap: Operation not permitted"})

	ran, verdict := probeBwrapViaTmuxServer("/usr/bin/bwrap",
		sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootHostInherited)
	assert.True(t, ran, "the job published a verdict; the client's exit status is not the channel")
	assert.ErrorContains(t, verdict, "Operation not permitted")
}

// The grace window's whole purpose: on a tmux whose run-shell does not wait,
// the verdict lands after the client returns. Without the window it would look
// identical to a host with no tmux server, and the fix would evaporate.
func TestProbeBwrapViaTmuxServerWaitsOutAVerdictThatArrivesLate(t *testing.T) {
	swapTmux(t, &probeJobTmux{write: "err denied late", writeAfter: 100 * time.Millisecond})

	ran, verdict := probeBwrapViaTmuxServer("/usr/bin/bwrap",
		sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootHostInherited)
	assert.True(t, ran)
	assert.ErrorContains(t, verdict, "denied late")
}

// The two tmux facts the design rides on, neither of which can be exercised
// live here: the pid lookup must be a command that cannot START a server, and
// run-shell must not carry -b, or it would not wait for its job.
func TestTclaudeLayerProbeUsesTmuxCommandsThatCannotStartAServerOrDetach(t *testing.T) {
	fake := &probeJobTmux{write: "ok"}
	swapTmux(t, fake)

	_, err := tmuxServerPID()
	require.NoError(t, err, "the fake answers display-message; only the argv matters here")
	_, _ = probeBwrapViaTmuxServer("/usr/bin/bwrap",
		sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootHostInherited)

	require.Len(t, fake.argv, 2)
	assert.Equal(t, []string{"display-message", "-p", "#{pid}"}, fake.argv[0],
		"new-session and start-server would auto-start a server; display-message must not")
	assert.Equal(t, "run-shell", fake.argv[1][0])
	assert.NotContains(t, fake.argv[1], "-b",
		"-b would background the job, so run-shell would not wait for the verdict")
}

// A job that was waited on and wrote nothing is the ordinary broken case — a
// mis-resolved tclaude path, a probe that died. It must cost the short grace
// window, not the whole probe budget, because the launch still has to fall back
// and probe in-process afterwards.
func TestProbeBwrapViaTmuxServerGivesUpQuicklyOnAJobThatWritesNothing(t *testing.T) {
	swapTmux(t, &probeJobTmux{})

	start := time.Now()
	ran, verdict := probeBwrapViaTmuxServer("/usr/bin/bwrap",
		sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootHostInherited)
	assert.False(t, ran)
	assert.NoError(t, verdict)
	assert.Less(t, time.Since(start), bwrapProbeTimeout,
		"the grace window must be well inside the overall probe budget")
}

// The probe leaves nothing behind on a host it merely asked a question of.
func TestProbeBwrapViaTmuxServerRemovesItsStagingDirectory(t *testing.T) {
	fake := &probeJobTmux{write: "ok"}
	swapTmux(t, fake)

	ran, verdict := probeBwrapViaTmuxServer("/usr/bin/bwrap",
		sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootHostInherited)
	require.True(t, ran)
	require.NoError(t, verdict)
	require.Len(t, fake.commands, 1)

	resultPath := probeResultPathFromCommand(fake.commands[0])
	require.NotEmpty(t, resultPath)
	assert.NoDirExists(t, filepath.Dir(resultPath))
}

func TestAwaitTclaudeLayerProbeResultGivesUpAtTheDeadline(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "result")

	start := time.Now()
	_, err := awaitTclaudeLayerProbeResult(missing, start.Add(50*time.Millisecond))
	assert.ErrorIs(t, err, fs.ErrNotExist)
	assert.Less(t, time.Since(start), bwrapProbeTimeout)
}

// A blocking run-shell is the normal case: the file is already there, and the
// wait must cost one read rather than a poll interval.
func TestAwaitTclaudeLayerProbeResultReturnsAnAlreadyWrittenVerdict(t *testing.T) {
	resultPath := filepath.Join(t.TempDir(), "result")
	require.NoError(t, os.WriteFile(resultPath, []byte("ok"), 0o600))

	raw, err := awaitTclaudeLayerProbeResult(resultPath, time.Now())
	require.NoError(t, err)
	assert.Equal(t, "ok", raw)
}

func TestRunBoundedTmuxCommandKillsACommandThatOutlivesItsDeadline(t *testing.T) {
	start := time.Now()
	err := runBoundedTmuxCommand(
		exec.Command("/bin/sh", "-c", "sleep 30"), start.Add(100*time.Millisecond))
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 5*time.Second)
}

func TestTclaudeLayerStartRefusalNamesADeniedExecAsARefusal(t *testing.T) {
	err := tclaudeLayerStartRefusal("/usr/bin/bwrap",
		&os.PathError{Op: "fork/exec", Path: "/usr/bin/bwrap", Err: syscall.EPERM})

	assert.ErrorContains(t, err, tclaudeLayerRefusalPrefix)
	assert.ErrorContains(t, err, "permission to execute bubblewrap (/usr/bin/bwrap)")
	assert.ErrorContains(t, err, "inherits its confinement from the tmux server")
	assert.ErrorIs(t, err, syscall.EPERM)
}

func TestTclaudeLayerStartRefusalClassifiesTheOtherExecFailures(t *testing.T) {
	access := tclaudeLayerStartRefusal("/usr/bin/bwrap",
		&os.PathError{Op: "fork/exec", Path: "/usr/bin/bwrap", Err: syscall.EACCES})
	assert.ErrorContains(t, access, tclaudeLayerRefusalPrefix)
	assert.ErrorIs(t, access, syscall.EACCES)

	// bwrap present at probe time and gone at launch time is the same broken
	// contract, and the operator needs the path named rather than a namespace
	// lecture.
	missing := tclaudeLayerStartRefusal("/usr/bin/bwrap",
		&os.PathError{Op: "fork/exec", Path: "/usr/bin/bwrap", Err: syscall.ENOENT})
	assert.ErrorContains(t, missing, tclaudeLayerRefusalPrefix)
	assert.ErrorContains(t, missing, "no longer present at /usr/bin/bwrap")
	assert.NotContains(t, missing.Error(), tclaudeLayerConfinementHint)

	// Anything else is not a capability question and must not borrow the
	// refusal vocabulary: calling an unrelated failure a refusal would send the
	// operator hunting for a confinement policy that is not there.
	other := tclaudeLayerStartRefusal("/usr/bin/bwrap", errors.New("too many open files"))
	assert.ErrorContains(t, other, "start bubblewrap: too many open files")
	assert.NotContains(t, other.Error(), tclaudeLayerRefusalPrefix)
}
