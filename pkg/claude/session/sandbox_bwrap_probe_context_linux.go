//go:build linux

package session

import (
	"context"
	"errors"
	"fmt"
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
// probe. That is not a silent fallback past a missing capability: it is the
// case where there is no tmux server yet, so THIS process is the one that will
// auto-start it and its confinement is the one the pane will inherit — the
// in-process probe is then the faithful answer, not a weaker one.
func probeBwrapInLaunchContext(
	binary string,
	posture sandboxpolicy.NetworkPosture,
	root sandboxpolicy.RootPosture,
) error {
	serverPID, err := tmuxServerPID()
	if err != nil {
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

// bwrapProbeCache remembers only that a posture PASSED, and only for the life
// of one tmux server.
//
// Positives only, deliberately. TCL-769 established that a caller which
// REFUSES on this predicate must never be answered from cache, because an
// operator who has just installed bubblewrap would be refused by a stale no.
// A stale yes has no such failure mode here: it can only let a launch proceed
// that then fails at the relay, where TCL-1204's other half now names the
// denial as a refusal rather than an opaque exit 125.
//
// Keying on the server pid — rather than a clock — ties the answer to the
// identity of the confinement it describes: a restarted server, which is the
// event that can change that confinement, gets a fresh answer.
var bwrapProbeCache = &bwrapProbeMemo{}

type bwrapProbeMemo struct {
	mu        sync.Mutex
	serverPID int
	healthyOn map[bwrapProbeKey]struct{}
}

func (m *bwrapProbeMemo) healthy(serverPID int, key bwrapProbeKey) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.serverPID != serverPID {
		return false
	}
	_, ok := m.healthyOn[key]
	return ok
}

func (m *bwrapProbeMemo) record(serverPID int, key bwrapProbeKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.serverPID != serverPID || m.healthyOn == nil {
		// Dropping the whole map on a pid change is what bounds it: entries
		// only ever accumulate within one server's lifetime, and there are a
		// handful of postures.
		m.serverPID = serverPID
		m.healthyOn = map[bwrapProbeKey]struct{}{}
	}
	m.healthyOn[key] = struct{}{}
}

func (m *bwrapProbeMemo) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
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
		return false, nil
	}
	defer func() { _ = os.RemoveAll(dir) }()
	resultPath := filepath.Join(dir, "result")

	command := tclaudeLayerProbeShellCommand(binary, posture, root, resultPath)

	// One deadline covers the whole round trip rather than one per step, so
	// this path can never outlast the in-process probe it replaces. That budget
	// exists for the reason bwrapProbeTimeout gives: a wedged namespace setup
	// must cost one launch, not a poll loop.
	deadline := time.Now().Add(bwrapProbeTimeout)
	if err := runBoundedTmuxCommand(
		clcommon.Default.Command("run-shell", command), deadline,
	); err != nil {
		// tmux refused the job outright — no server, an unknown command on an
		// old tmux, a killed client. Nothing was measured.
		return false, nil
	}

	// Without -b, run-shell waits for the job, so the verdict is normally on
	// disk already and this returns on its first read. The grace window is here
	// so the fix does not quietly evaporate into the in-process fallback on a
	// tmux whose run-shell returns early: an unwaited job would look exactly
	// like a host with no tmux server.
	//
	// It is a short window rather than the rest of the budget because the two
	// ways of reaching it are not equally likely. A run-shell that did NOT wait
	// is the rare one; a job that ran, waited on, and wrote nothing — a
	// mis-resolved tclaude path, a probe that died — is the ordinary one, and
	// on that path every further millisecond is spent waiting for a file that
	// will never appear. Clamped to the overall deadline so the round trip
	// still cannot outlast the in-process probe it replaces.
	raw, err := awaitTclaudeLayerProbeResult(
		resultPath, earlierDeadline(time.Now().Add(tclaudeLayerProbeResultGrace), deadline))
	if err != nil {
		// Absent, unreadable, or written after we gave up — all of them mean
		// the round trip produced no verdict, never that the capability is
		// missing.
		return false, nil
	}
	return parseTclaudeLayerProbeResult(raw)
}

const (
	// tclaudeLayerProbePollInterval is short enough that the overwhelmingly
	// common case — run-shell already waited, the file is there — costs one
	// read.
	tclaudeLayerProbePollInterval = 25 * time.Millisecond
	// tclaudeLayerProbeResultGrace is how long a verdict may still arrive after
	// run-shell has returned. See the call site for why it is small.
	tclaudeLayerProbeResultGrace = time.Second
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
// Every word is a compile-time constant, a path this process just created, or
// a value the caller resolved from PATH — never operator text. Each is
// shell-quoted regardless, because the string does reach a shell.
func tclaudeLayerProbeShellCommand(
	binary string,
	posture sandboxpolicy.NetworkPosture,
	root sandboxpolicy.RootPosture,
	resultPath string,
) string {
	return clcommon.ShellQuoteArg(clcommon.SelfTclaudePath()) +
		" session " + tclaudeLayerProbeCommand +
		" --bwrap " + clcommon.ShellQuoteArg(binary) +
		" --network-posture " + clcommon.ShellQuoteArg(posture.String()) +
		" --root-posture " + clcommon.ShellQuoteArg(root.String()) +
		" --result " + clcommon.ShellQuoteArg(resultPath)
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
// put it and records what it found. It exits 0 whether or not the capability is
// present, because its exit status is not the channel — tmux's run-shell does
// not hand a job's status back to the client. The result file is.
func tclaudeLayerProbeCmd() *cobra.Command {
	var binary, networkPosture, rootPosture, resultPath string
	cmd := &cobra.Command{
		Use:    tclaudeLayerProbeCommand,
		Short:  "Probe tclaude-layer host capability from the pane's confinement (internal)",
		Hidden: true,
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
