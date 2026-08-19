//go:build linux

package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// tclaudeLayerProbeCommand is the capability probe as a subcommand, so tmux can
// be asked to run it. Nothing else invokes it: it is the same predicate
// `probeBwrapInProcess` answers, moved to a process the tmux server forks.
const tclaudeLayerProbeCommand = "tclaude-layer-capability-probe"

const (
	tclaudeLayerProbeResultOK     = "ok"
	tclaudeLayerProbeResultPrefix = "err "
)

// probeBwrapInLaunchContext answers the capability question from the process
// ancestry the LAUNCH will have, not the one preparing it.
//
// TCL-1204. `tclaude session new` reaches this as a child of agentd or of the
// operator's shell, while the process that really execs bubblewrap is a
// descendant of the tmux server — which inherited its confinement from
// whichever process first auto-started it. On a host that confines those
// differently (an AppArmor profile per binary, an SELinux domain, a seccomp
// filter, a differing no_new_privs) the probe exercises a confinement the
// launch never runs under, so it passes where the launch cannot: the operator
// gets a dead pane at exit 125 instead of the refusal `docs/sandboxing.md`
// promises.
//
// Routing the probe through `tmux run-shell` puts it under the server, one
// `sh` and one `tclaude` exec away from bubblewrap — the same hops the relay
// takes — so a profile transition keyed on either binary applies to both.
//
// When the round trip cannot be made at all it degrades to the in-process
// probe, and never to a passed capability: only a verdict the job actually
// wrote can refuse a launch.
//
// With no tmux server running, that degradation is usually exact rather than
// merely safe — THIS process is the one that will auto-start the server, so its
// confinement is the one the pane inherits. Usually, not always: a profile that
// transitions on the tmux binary itself (AppArmor `/usr/bin/tmux Px -> tmux`,
// an SELinux type_transition) puts the server in a domain this process is not
// in, which is TCL-1204's own bug at the server-start edge. The relay refusal
// is what catches that, and every other way the round trip can fail — each of
// which is logged, because a silently disabled probe looks exactly like the
// original bug.
func probeBwrapInLaunchContext(
	binary string,
	posture sandboxpolicy.NetworkPosture,
	root sandboxpolicy.RootPosture,
) error {
	serverPID, err := tmuxServerPID()
	if err != nil {
		// The ordinary case on a host with nothing running yet, so Debug: this
		// is the branch whose in-process answer is faithful, not the one that
		// loses the fix.
		slog.Debug("tclaude-layer: no tmux server to probe from; "+
			"the preparing process's confinement is the one its pane will inherit", "error", err)
		return probeBwrapInProcess(binary, posture, root)
	}
	key := bwrapProbeKey{binary: binary, posture: posture, root: root}
	if bwrapProbeCache.healthy(serverPID, key) {
		return nil
	}
	ran, verdict := probeBwrapViaTmuxServer(binary, posture, root)
	if !ran {
		return probeBwrapInProcess(binary, posture, root)
	}
	if verdict == nil {
		bwrapProbeCache.record(serverPID, key)
	}
	return verdict
}

// tmuxServerPID identifies the confinement context a pane will inherit, and
// doubles as the cache key for it. A non-nil error means no server is running
// (or tmux could not be asked), which is itself the answer callers need: there
// is no foreign context to probe from.
//
// It must not be a command that STARTS a server. `display-message` fails
// against a dead server rather than spawning one, so a pure capability question
// — a dashboard disclosure, a dry run — never leaves a tmux server behind.
var tmuxServerPID = func() (int, error) {
	out, err := clcommon.Default.Command("display-message", "-p", "#{pid}").Output()
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("tmux reported unusable server pid %q", strings.TrimSpace(string(out)))
	}
	return pid, nil
}

type bwrapProbeKey struct {
	binary  string
	posture sandboxpolicy.NetworkPosture
	root    sandboxpolicy.RootPosture
}

// bwrapProbeMemoTTL bounds how long a passing posture may be reused.
//
// The tmux server pid alone would not bound it at all: a server can outlive any
// number of host changes. Callers on this predicate include
// ProbeFilteredNetworkPrerequisite, whose answer is DISCLOSED — an operator who
// turns a prerequisite off without restarting tmux must not keep being told the
// filtered gateway is available. Short enough that such a change self-corrects
// within one interaction; long enough that a burst of spawns pays for one round
// trip rather than one each.
const bwrapProbeMemoTTL = 30 * time.Second

// bwrapProbeCache remembers only that a posture PASSED, and only briefly,
// within the life of one tmux server.
//
// Positives only, deliberately. TCL-769 established that a caller which
// REFUSES on this predicate must never be answered from cache, because an
// operator who has just installed bubblewrap would be refused by a stale no.
// A stale yes is bounded in both directions instead: by the TTL, and by the
// relay, where TCL-1204's other half names a denial as a refusal rather than
// an opaque exit 125.
//
// Keying on the server pid as well as the clock ties the answer to the identity
// of the confinement it describes: a restarted server — the event that can
// change that confinement wholesale — gets a fresh answer immediately rather
// than after the TTL.
var bwrapProbeCache = &bwrapProbeMemo{}

type bwrapProbeMemo struct {
	mu        sync.Mutex
	now       func() time.Time
	serverPID int
	healthyOn map[bwrapProbeKey]time.Time
}

func (m *bwrapProbeMemo) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

func (m *bwrapProbeMemo) healthy(serverPID int, key bwrapProbeKey) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.serverPID != serverPID {
		return false
	}
	recorded, ok := m.healthyOn[key]
	return ok && m.clock().Sub(recorded) < bwrapProbeMemoTTL
}

func (m *bwrapProbeMemo) record(serverPID int, key bwrapProbeKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.serverPID != serverPID || m.healthyOn == nil {
		// Dropping the whole map on a pid change is what bounds it: entries
		// only ever accumulate within one server's lifetime, and there are a
		// handful of postures.
		m.serverPID = serverPID
		m.healthyOn = map[bwrapProbeKey]time.Time{}
	}
	m.healthyOn[key] = m.clock()
}

func (m *bwrapProbeMemo) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = nil
	m.serverPID = 0
	m.healthyOn = nil
}

// probeBwrapViaTmuxServer runs the capability probe as a tmux job and reads its
// verdict back through a private file.
//
// ran distinguishes "the probe did not happen" from "the probe said no", and
// the two must not collapse: the first is infrastructure trouble the caller
// recovers from by probing in-process, while the second is a refusal that must
// stand. A file the job never wrote is the first, never the second.
var probeBwrapViaTmuxServer = func(
	binary string,
	posture sandboxpolicy.NetworkPosture,
	root sandboxpolicy.RootPosture,
) (ran bool, verdict error) {
	if _, err := tclaudeLayerProbeArgs(posture, root); err != nil {
		// An invalid posture is a programming error, not a host capability
		// question; report it without a round trip.
		return true, err
	}
	dir, err := os.MkdirTemp("", "tclaude-bwrap-probe-")
	if err != nil {
		noLaunchContextVerdict("probe staging directory unavailable", err)
		return false, nil
	}
	defer func() { _ = os.RemoveAll(dir) }()
	resultPath := filepath.Join(dir, "result")

	command := tclaudeLayerProbeShellCommand(binary, posture, root, resultPath)

	// The round trip gets STRICTLY MORE than the probe it carries. The job runs
	// probeBwrapInProcess, which spends bwrapProbeTimeout on the bwrap exec
	// alone; giving the outer trip the same budget would make the job's own
	// timeout verdict — a real refusal, on a host with a wedged LSM — forever
	// unreachable, and would answer instead from the confinement TCL-1204 says
	// is the wrong one. The overhead is for tmux job scheduling and one tclaude
	// process start, not for the probe.
	deadline := time.Now().Add(bwrapProbeTimeout + tclaudeLayerProbeRoundTripOverhead)
	runErr := runBoundedTmuxCommand(clcommon.Default.Command("run-shell", command), deadline)

	// Read the verdict BEFORE deciding what a failed client means. run-shell
	// hands the job's exit status to its client, so a job that published a
	// refusal and then exited non-zero for any reason at all would otherwise
	// have that refusal discarded — a genuine capability refusal silently
	// downgraded to a fallback. Whatever is on disk is the answer; the client's
	// fate is not.
	wait := earlierDeadline(time.Now().Add(tclaudeLayerProbeResultGrace), deadline)
	if runErr != nil {
		// The client failed or was killed. Anything the job was going to
		// publish either already landed or is not coming within any budget
		// worth spending, so read once and move on.
		wait = time.Now()
	}
	// Without -b, run-shell waits for the job, so the verdict is normally on
	// disk already and this returns on its first read. The grace window is here
	// so the fix does not quietly evaporate into the in-process fallback on a
	// tmux whose run-shell returns early: an unwaited job would look exactly
	// like a host with no tmux server.
	//
	// It is a short window rather than the rest of the budget because the two
	// ways of reaching it are not equally likely. A run-shell that did NOT wait
	// is the rare one; a job that ran, was waited on, and wrote nothing — a
	// mis-resolved tclaude path, a probe that died — is the ordinary one, and
	// on that path every further millisecond is spent waiting for a file that
	// will never appear.
	raw, err := awaitTclaudeLayerProbeResult(resultPath, wait)
	if err != nil {
		// Absent, unreadable, or written after we gave up — all of them mean
		// the round trip produced no verdict, never that the capability is
		// missing.
		noLaunchContextVerdict("the tmux capability probe job published no verdict", errors.Join(runErr, err))
		return false, nil
	}
	ran, verdict = parseTclaudeLayerProbeResult(raw)
	if !ran {
		noLaunchContextVerdict("the tmux capability probe job published an unreadable verdict", nil)
	}
	return ran, verdict
}

// noLaunchContextVerdict records that this launch fell back to probing in the
// PREPARING process's confinement instead of the pane's.
//
// TCL-1204 is only fixed while the round trip works, and every way it can stop
// working is survivable by design — which is exactly why it needs to be
// audible. A host with agentd under systemd `PrivateTmp=yes`, or one whose
// confined tmux server cannot write the staging path, disables this silently
// and permanently; without a line naming it, the only symptom is the original
// bug coming back.
//
// Warn rather than Debug: a tmux server exists, so this is the abnormal branch.
// The ordinary "no server yet" case never reaches here.
func noLaunchContextVerdict(reason string, err error) {
	slog.Warn("tclaude-layer: capability probe fell back to the preparing process's confinement; "+
		"a launch this probe passes may still be denied in the pane",
		"reason", reason, "error", err)
}

const (
	// tclaudeLayerProbePollInterval is short enough that the overwhelmingly
	// common case — run-shell already waited, the file is there — costs one
	// read.
	tclaudeLayerProbePollInterval = 25 * time.Millisecond
	// tclaudeLayerProbeResultGrace is how long a verdict may still arrive after
	// run-shell has returned. See the call site for why it is small.
	tclaudeLayerProbeResultGrace = time.Second
	// tclaudeLayerProbeRoundTripOverhead is the round trip's allowance ON TOP
	// OF the probe budget it carries: tmux scheduling the job plus one tclaude
	// process start. See the call site for why it must not be zero.
	tclaudeLayerProbeRoundTripOverhead = 5 * time.Second
)

func earlierDeadline(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func awaitTclaudeLayerProbeResult(path string, deadline time.Time) (string, error) {
	for {
		raw, err := os.ReadFile(path)
		if err == nil {
			return string(raw), nil
		}
		if !errors.Is(err, os.ErrNotExist) || !time.Now().Before(deadline) {
			return "", err
		}
		time.Sleep(tclaudeLayerProbePollInterval)
	}
}

// tclaudeLayerProbeShellCommand renders what tmux will hand to `/bin/sh -c`.
//
// It invokes the tclaude BINARY rather than bubblewrap directly, and that is
// the point rather than convenience: the launch reaches bubblewrap through a
// `tclaude` exec too, so a confinement transition keyed on either executable
// applies to the probe exactly as it will to the relay. Executing bwrap
// straight from the shell would re-open a narrower version of the same gap
// TCL-1204 is about.
//
// The hops still are not identical, and the difference is worth naming: the
// real launch also passes through the dir-proof guard and exit-gate shells and,
// whenever the launch has a cgroup at all, `session resource-limit-exec`, which
// puts the process in that per-session cgroup. A confinement expressed as a
// cgroup policy is therefore NOT reproduced here. What this probe reproduces is
// the per-process confinement inherited from the tmux server, which is what
// TCL-1204 observed; the relay refusal remains the backstop for the rest.
//
// Every word is a compile-time constant, a path this process just created, or
// a value the caller resolved from PATH — never operator text. Each is
// shell-quoted regardless, because the string does reach a shell.
//
// TWO layers read this string, and shell quoting only answers to one of them:
// `run-shell` puts the command through tmux's format expansion before any
// shell sees it, and `#` is significant there even inside single quotes. See
// escapeTmuxFormat.
func tclaudeLayerProbeShellCommand(
	binary string,
	posture sandboxpolicy.NetworkPosture,
	root sandboxpolicy.RootPosture,
	resultPath string,
) string {
	return escapeTmuxFormat(
		clcommon.ShellQuoteArg(clcommon.SelfTclaudePath()) +
			" session " + tclaudeLayerProbeCommand +
			" --bwrap " + clcommon.ShellQuoteArg(binary) +
			" --network-posture " + clcommon.ShellQuoteArg(posture.String()) +
			" --root-posture " + clcommon.ShellQuoteArg(root.String()) +
			" --result " + clcommon.ShellQuoteArg(resultPath))
}

// escapeTmuxFormat neutralises tmux's format layer, which expands `#{…}`, the
// single-character aliases (`#H`, `#S`, `#W`, …) and `#,`/`#}` before the
// command reaches a shell. Doubling `#` is tmux's own escape for a literal one.
//
// Nothing operator-authored reaches this string, so this is not a security
// boundary — it is correctness. A checkout under a path containing `#`
// (`/home/u/work#2/bin/tclaude`) would otherwise exec a mangled path, and the
// probe would silently degrade to the wrong-confinement answer for a reason
// nobody would think to look for.
func escapeTmuxFormat(command string) string {
	return strings.ReplaceAll(command, "#", "##")
}

// runBoundedTmuxCommand runs a tmux command and gives up on it at deadline,
// killing the client so a hung server cannot hold the caller.
//
// The tmux JOB keeps running on the server after that; nothing here can stop
// it, and nothing needs to. Its only output is a file under a directory the
// caller removes, so a late writer writes into a path that no longer exists.
func runBoundedTmuxCommand(cmd *exec.Cmd, deadline time.Time) error {
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	if err := cmd.Start(); err != nil {
		return err
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	select {
	case err := <-waitCh:
		return err
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-waitCh
		return ctx.Err()
	}
}

// parseTclaudeLayerProbeResult turns the job's published line back into a
// verdict.
//
// A refusal crosses the process boundary as TEXT, so it comes back as a plain
// error where the in-process path would have returned the *exec.ExitError
// bubblewrap produced. Every caller of this predicate reports the message and
// none inspects the type, so the two are interchangeable today; a caller that
// starts matching on the type has to stop.
func parseTclaudeLayerProbeResult(raw string) (ran bool, verdict error) {
	result := strings.TrimSpace(raw)
	switch {
	case result == tclaudeLayerProbeResultOK:
		return true, nil
	case strings.HasPrefix(result, tclaudeLayerProbeResultPrefix):
		return true, errors.New(strings.TrimPrefix(result, tclaudeLayerProbeResultPrefix))
	default:
		// A result file we cannot interpret is evidence about nothing, exactly
		// like a file that was never written.
		return false, nil
	}
}

// tclaudeLayerProbeCmd is the far side of the round trip: it runs where tmux
// put it and records what it found. The result file is the channel, not the
// exit status — the caller reads the file whatever the job's status was.
//
// It carries its OWN PersistentPreRunE, which replaces the root command's
// (cobra runs the closest one only, and EnableTraverseRunHooks is off). The
// root's relocates legacy state and rewrites config on the way in. Neither is
// wanted here: a probe must not perform a config migration as a side effect of
// a question nobody asked it to persist, and on a confined tmux server that
// cannot write under the tclaude state root, a failing pre-run would abort
// before the verdict was ever written — turning "this host denies the exec"
// into "the round trip produced nothing", which falls back to the answer this
// whole file exists to stop trusting.
func tclaudeLayerProbeCmd() *cobra.Command {
	var binary, networkPosture, rootPosture, resultPath string
	cmd := &cobra.Command{
		Use:               tclaudeLayerProbeCommand,
		Short:             "Probe tclaude-layer host capability from the pane's confinement (internal)",
		Hidden:            true,
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		RunE: func(_ *cobra.Command, _ []string) error {
			posture, err := parseNetworkPostureToken(networkPosture)
			if err != nil {
				return err
			}
			root, err := parseRootPostureToken(rootPosture)
			if err != nil {
				return err
			}
			if strings.TrimSpace(resultPath) == "" {
				return errors.New("--result is required")
			}
			// A malformed invocation must produce NO VERDICT, not a refusal.
			// Without this, an empty --bwrap reaches exec as an empty program
			// name, fails, and gets published as `err exec: no command` — which
			// resolveBwrapServerBinary would then turn into a refused launch,
			// blaming the host for the caller's bug.
			if strings.TrimSpace(binary) == "" {
				return errors.New("--bwrap is required")
			}
			result := tclaudeLayerProbeResultOK
			if err := probeBwrapInProcess(binary, posture, root); err != nil {
				result = tclaudeLayerProbeResultPrefix + err.Error()
			}
			return writeTclaudeLayerProbeResult(resultPath, result)
		},
	}
	cmd.Flags().StringVar(&binary, "bwrap", "", "resolved bubblewrap binary to probe (internal)")
	cmd.Flags().StringVar(&networkPosture, "network-posture", "", "network posture token to probe (internal)")
	cmd.Flags().StringVar(&rootPosture, "root-posture", "", "root posture token to probe (internal)")
	cmd.Flags().StringVar(&resultPath, "result", "", "file the verdict is written to (internal)")
	return cmd
}

// writeTclaudeLayerProbeResult publishes the verdict under its final name only
// once it is whole, so the waiting side can never read half a message and treat
// a truncated "err …" as an unparseable file.
func writeTclaudeLayerProbeResult(path, result string) error {
	staging := path + ".partial"
	if err := os.WriteFile(staging, []byte(result), 0o600); err != nil {
		return err
	}
	if err := os.Rename(staging, path); err != nil {
		_ = os.Remove(staging)
		return err
	}
	return nil
}

func parseNetworkPostureToken(token string) (sandboxpolicy.NetworkPosture, error) {
	for _, posture := range []sandboxpolicy.NetworkPosture{
		sandboxpolicy.NetworkHostOpen,
		sandboxpolicy.NetworkIsolatedWithAgentd,
		sandboxpolicy.NetworkFiltered,
	} {
		if posture.String() == token {
			return posture, nil
		}
	}
	return 0, fmt.Errorf("unknown network posture %q", token)
}

func parseRootPostureToken(token string) (sandboxpolicy.RootPosture, error) {
	for _, root := range []sandboxpolicy.RootPosture{
		sandboxpolicy.RootHostInherited,
		sandboxpolicy.RootConstructed,
	} {
		if root.String() == token {
			return root, nil
		}
	}
	return 0, fmt.Errorf("unknown root posture %q", token)
}
