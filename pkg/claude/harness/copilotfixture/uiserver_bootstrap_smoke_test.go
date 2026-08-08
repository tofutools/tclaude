package copilotfixture_test

import (
	"context"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/copilotapi"
	"github.com/tofutools/tclaude/pkg/claude/harness/copilotfixture"
)

// TCL-1056's acceptance evidence: what `copilot --ui-server` actually does
// about folder trust, measured on a real pty against the pinned CLI.
//
// A note on the flag's availability, because it was nearly gated away on a
// guess: TCL-1051 FOUND `--ui-server` on 1.0.78, and a reasonable reading of
// that is "1.0.78 introduced it". It did not. These scenarios pass against the
// lab's pinned 1.0.77, so the flag predates the release it was discovered in
// and needs no version gate here.
//
// The ticket carried this as its main open risk — whether the folder-trust
// prompt could be satisfied for an unattended agent — and the answer turned out
// to be less about satisfying it than about what it does and does not block.
// Both halves are pinned here because tclaude's refusal gate
// (session.ValidateCopilotAPIFolderTrust) is built on them, and a gate whose
// premise silently stops holding is worse than no gate: it would keep refusing
// launches for a reason that had ceased to exist.
//
// What these scenarios deliberately do NOT do is complete a turn. A session
// created over RPC is not handed the fixture's BYOK provider configuration —
// the CLI answers "Session was not created with authentication info or custom
// provider" — so a turn is unobservable from a credential-free lab. It is
// observable with real credentials, and was: an untrusted, modal-blocked pane
// completed a full turn over RPC. That measurement is recorded on the ticket
// rather than here, because a scenario that needs the operator's GitHub account
// is not a scenario CI can own.

// uiServerDeadline bounds one bootstrap run. The listener appears in about a
// second and the four RPCs answer in milliseconds; the budget is generous
// because the arm that matters is the positive one, and each run ends as soon
// as its evidence has landed.
const uiServerDeadline = 90 * time.Second

// uiServerProbe drives the RPC side of a pty run from its own goroutine.
//
// The two sides have to run at once: the pty run does not return until the CLI
// exits or the deadline passes, and an interactive CLI never exits. So the
// probe runs alongside it and reports completion through SettledWhen, which is
// what lets a scenario end the moment its measurement is done.
type uiServerProbe struct {
	mu   sync.Mutex
	done bool

	// Observed facts, read after the run.
	listenerUp      bool
	dialErr         error
	createErr       error
	foregroundErr   error
	sessionID       string
	trustedBefore   bool
	trustedAfter    bool
	addTrustedOK    bool
	trustCallErrors []error
}

func (p *uiServerProbe) finish() {
	p.mu.Lock()
	p.done = true
	p.mu.Unlock()
}

func (p *uiServerProbe) settled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done
}

// waitForListener reports whether anything accepts a connection on port within
// d. It is deliberately a plain dial rather than tclaude's ownership proof:
// this scenario is measuring the CLI's behaviour, and the lab process owns
// every process in sight.
func waitForListener(port int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	address := "127.0.0.1:" + strconv.Itoa(port)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// run performs the bootstrap sequence tclaude's own bootstrap performs, plus
// the folder-trust reads the refusal gate rests on.
func (p *uiServerProbe) run(port int, workDir string) {
	defer p.finish()

	p.listenerUp = waitForListener(port, 60*time.Second)
	if !p.listenerUp {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := copilotapi.DialRetry(ctx, "127.0.0.1:"+strconv.Itoa(port), nil)
	if err != nil {
		p.dialErr = err
		return
	}
	defer func() { _ = client.Close() }()

	info, err := client.CreateSession(ctx, copilotapi.CreateSessionParams{
		WorkingDirectory: workDir, ClientName: "tclaude", Streaming: true,
	})
	if err != nil {
		p.createErr = err
		return
	}
	p.sessionID = info.SessionID

	isTrusted := func() bool {
		var result struct {
			Trusted bool `json:"trusted"`
		}
		if err := client.Call(ctx, "session.permissions.folderTrust.isTrusted",
			map[string]string{"sessionId": info.SessionID, "path": workDir}, &result); err != nil {
			p.trustCallErrors = append(p.trustCallErrors, err)
			return false
		}
		return result.Trusted
	}
	p.trustedBefore = isTrusted()

	var added struct {
		Success bool `json:"success"`
	}
	if err := client.Call(ctx, "session.permissions.folderTrust.addTrusted",
		map[string]string{"sessionId": info.SessionID, "path": workDir}, &added); err != nil {
		p.trustCallErrors = append(p.trustCallErrors, err)
	}
	p.addTrustedOK = added.Success
	p.trustedAfter = isTrusted()

	p.foregroundErr = client.SetForegroundSession(ctx, info.SessionID)
}

// uiServerRun launches the CLI in TUI+server mode and drives the probe against
// it. seedTrust chooses which of the two arms is being measured.
func uiServerRun(t *testing.T, dirs copilotfixture.Dirs, seedTrust bool) (*uiServerProbe, copilotfixture.PTYResult) {
	t.Helper()

	mock := copilotfixture.NewMockProvider(t, []copilotfixture.Turn{{Text: "MOCK STREAMED ANSWER"}})
	if seedTrust {
		seedTrustLikeProduction(t, dirs, dirs.WorkDir)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())

	probe := &uiServerProbe{}
	go probe.run(port, dirs.WorkDir)

	res := copilotfixture.RunPTY(t, copilotfixture.PTYOptions{
		RunOptions: copilotfixture.RunOptions{
			Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, WorkDir: dirs.WorkDir,
			BaseURL: mock.BaseURL(),
			Prompt:  "Reply with the text the provider gives you.",
			// --host is pinned for the same reason the spawner pins it: the
			// ownership proof, the reservation and the CLI's bind all have to
			// be talking about one address.
			ExtraArgs: []string{
				"--ui-server", "--host", "127.0.0.1", "--port", strconv.Itoa(port),
			},
		},
		Deadline:    uiServerDeadline,
		SettledWhen: probe.settled,
	})
	return probe, res
}

// TestCopilotUIServerIsDrivableWhileTheTrustModalBlocksTheTUI is the finding
// tclaude's refusal gate is built on, and the one that makes the gate necessary
// rather than merely tidy.
//
// The folder-trust modal blocks the TUI. It does NOT block the embedded server:
// the port is listening, a session can be created and foregrounded, and — with
// real credentials, off-lab — a full turn completes. So an unattended launch
// into an untrusted directory does not fail. It succeeds into a state where the
// agent is running and the human's pane shows a blocking dialog about it, which
// is a worse outcome than a refusal because nothing about it looks wrong.
func TestCopilotUIServerIsDrivableWhileTheTrustModalBlocksTheTUI(t *testing.T) {
	requireLabParallel(t)

	dirs := copilotfixture.NewSandboxDirs(t)
	probe, res := uiServerRun(t, dirs, false)

	require.True(t, res.Settled,
		"the probe must complete; transcript:\n%s", res.TranscriptText())
	assert.True(t, probe.listenerUp,
		"the embedded server must be listening even though the TUI is parked on the modal")
	require.NoError(t, probe.dialErr)
	require.NoError(t, probe.createErr,
		"session.create must succeed against a pane blocked on folder trust")
	require.NoError(t, probe.foregroundErr,
		"session.setForeground must succeed against a pane blocked on folder trust")
	assert.NotEmpty(t, probe.sessionID)

	// The other half, and the reason RPC-side trust is not an answer to the
	// startup prompt: addTrusted works and takes effect, and the modal is still
	// on screen anyway. It trusts the folder for the NEXT launch.
	assert.False(t, probe.trustedBefore, "the lab's work dir starts untrusted")
	assert.True(t, probe.addTrustedOK,
		"session.permissions.folderTrust.addTrusted must succeed from a session we created")
	assert.True(t, probe.trustedAfter, "addTrusted must actually take effect")
	assert.Empty(t, probe.trustCallErrors)

	assert.True(t, res.Contains(copilotfixture.TrustPromptMarker),
		"the modal must still be on screen: an already-drawn prompt is not retracted by "+
			"trusting the folder over RPC, which is why tclaude pre-seeds instead")
}

// TestCopilotUIServerBootstrapsCleanlyOnASeededDir is the positive arm: with
// tclaude's OWN seeder run first, exactly as session.New runs it, the modal
// never appears and the bootstrap sequence lands on a pane the human can see.
//
// It is what makes the refusal actionable rather than merely obstructive — the
// remedy the refusal names is measured here to work.
func TestCopilotUIServerBootstrapsCleanlyOnASeededDir(t *testing.T) {
	requireLabParallel(t)

	dirs := copilotfixture.NewSandboxDirs(t)
	probe, res := uiServerRun(t, dirs, true)

	require.True(t, res.Settled,
		"the probe must complete; transcript:\n%s", res.TranscriptText())
	require.NoError(t, probe.dialErr)
	require.NoError(t, probe.createErr)
	require.NoError(t, probe.foregroundErr)
	assert.NotEmpty(t, probe.sessionID)
	assert.True(t, probe.trustedBefore,
		"the production seeder must have trusted the dir before the CLI started")
	assert.False(t, res.Contains(copilotfixture.TrustPromptMarker),
		"a seeded launch must not show the modal at all")
}
