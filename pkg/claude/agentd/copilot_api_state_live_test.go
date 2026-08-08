package agentd

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/copilotapi"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// The end-to-end check: a real copilot, a real turn, and the numbers that reach
// the session row compared against what the server says a moment later.
//
//	TCLAUDE_COPILOT_LIVE=1 go test ./pkg/claude/agentd/ -run TestLiveCopilotAPIState -v
//
// It DOES run from inside an agent sandbox. An earlier version of this comment
// claimed the opposite — that Copilot resolves its GitHub credentials through
// the OS keyring, which a sandbox cannot reach — on the strength of a single
// `session.error → {"errorType":"authentication"}`. That was wrong: the token
// is read from `~/.config/gh/hosts.yml`, and the claim did not survive contact
// with ~30 in-sandbox launches under TCL-1078, none of which saw that error.
// The one run that did has never been explained. An agent that hits it should
// report it rather than conclude the sandbox is at fault.
//
// It is opt-in because it needs Copilot installed and authenticated and spends
// the operator's quota, and it exists because the unit tests above cannot catch
// the class of bug this whole series keeps producing: a number that decodes
// cleanly, renders plausibly, and is not the quantity it claims to be. Two were
// caught by exactly this test rather than by any of the others — a session
// whose model reads "auto", and a mid-turn refresh landing before any model
// call had completed.

func TestLiveCopilotAPIStateWritesWhatTheServerReports(t *testing.T) {
	if os.Getenv("TCLAUDE_COPILOT_LIVE") != "1" {
		t.Skip("set TCLAUDE_COPILOT_LIVE=1 to run against a real copilot --ui-server")
	}
	binary, err := exec.LookPath("copilot")
	if err != nil {
		t.Skipf("copilot not on PATH: %v", err)
	}

	// Captured BEFORE setupTestDB, which repoints HOME at a temp directory.
	// That is right for tclaude's own database and fatal for copilot, which
	// resolves its GitHub credentials from the real home: without this the CLI
	// comes up, serves RPC, accepts the prompt, and never runs a turn — which
	// looks exactly like a broken consumer.
	realHome := os.Getenv("HOME")

	setupTestDB(t)
	resetCopilotAPIStateForTest()
	t.Cleanup(resetCopilotAPIStateForTest)

	address, workdir, serverPID := startLiveCopilotServer(t, binary, realHome)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	client, err := copilotapi.DialRetry(ctx, address, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	convID := copilotapi.NewSessionID()
	info, err := client.CreateSession(ctx, copilotapi.CreateSessionParams{
		SessionID: convID, WorkingDirectory: workdir,
		ClientName: "tclaude", Streaming: true,
	})
	require.NoError(t, err)
	require.NoError(t, client.SetForegroundSession(ctx, info.SessionID))

	row := &db.SessionRow{
		ID: "s-live", ConvID: convID, TmuxSession: "pane-live",
		Status: session.StatusIdle, Harness: harness.CopilotName, CreatedAt: time.Now(),
	}
	require.NoError(t, db.SaveSession(row))

	// Adopted with the real port and the real copilot pid, because every
	// refresh re-proves ownership before it reads. Here that proof runs against
	// an actual copilot process holding an actual listener — the only place in
	// the suite where the gate is exercised on exactly the shape production has.
	handle := &copilotAPISession{
		ConvID: convID, SessionID: info.SessionID, Client: client,
		Port: portFromAddress(t, address), PanePID: serverPID,
	}
	copilotAPISessions.Adopt(handle)
	t.Cleanup(func() { copilotAPISessions.Drop(convID) })
	startCopilotAPIStateConsumer(handle)

	// Before the first turn the server answers contextInfo with null, and
	// nothing must be published or written. A zeroed reading here would render
	// "not measured yet" as a measured 0% AND make the other Copilot sources
	// stand down in favour of a source with nothing to say.
	time.Sleep(2 * time.Second)
	_, published := lookupCopilotAPIState(convID)
	assert.False(t, published, "a reading was published before the first turn")

	_, err = client.Send(ctx, copilotapi.SendParams{
		SessionID: info.SessionID,
		Prompt:    "Run the shell command `echo live-check` and report its exact output.",
	})
	require.NoError(t, err)

	// Waits for a COMPLETED call rather than for the first write. The consumer
	// legitimately publishes mid-turn, when usage has no model and no output
	// yet, so asserting on the first reading would be asserting on a snapshot
	// of an unfinished turn.
	require.Eventually(t, func() bool {
		reading, ok := lookupCopilotAPIState(convID)
		return ok && reading.Model != "" && reading.OutputTokens > 0
	}, 3*time.Minute, 200*time.Millisecond,
		"the consumer never published a reading from a completed model call — if "+
			"usage also reports zero user requests, check the pane for a Copilot "+
			"authentication error rather than suspecting the consumer")

	snapshot, err := db.GetContextSnapshot(row.ID)
	require.NoError(t, err)
	reading, ok := lookupCopilotAPIState(convID)
	require.True(t, ok)

	direct, err := client.ContextInfo(ctx, copilotapi.ContextInfoParams{SessionID: info.SessionID})
	require.NoError(t, err)
	require.NotNil(t, direct)
	metrics, err := client.UsageMetrics(ctx, info.SessionID)
	require.NoError(t, err)

	t.Logf("row: pct=%.3f in=%d out=%d window=%d | server: total=%d limit=%d model=%s",
		snapshot.ContextPct, snapshot.TokensInput, snapshot.TokensOutput,
		snapshot.ContextWindowSize, direct.TotalTokens, direct.PromptTokenLimit,
		metrics.CurrentModel)

	// The window is the one assertion that can be compared exactly: it is a
	// property of the model rather than of the moment, so it does not move
	// between the consumer's read and this one.
	assert.Equal(t, int64(direct.PromptTokenLimit), snapshot.ContextWindowSize,
		"the row's window must be the limit Copilot reported, not a static assumption")
	assert.Positive(t, snapshot.TokensInput)
	assert.Positive(t, snapshot.TokensOutput,
		"output must be read from the NESTED usage shape; flattened it decodes as 0")

	// Occupancy moves as the turn advances, so the row is checked for INTERNAL
	// consistency — its own percentage against its own numerator and window —
	// rather than against a total read at a different instant.
	assert.InDelta(t,
		100*float64(snapshot.TokensInput)/float64(direct.PromptTokenLimit),
		snapshot.ContextPct, 0.001)
	assert.InDelta(t, float64(direct.TotalTokens), float64(snapshot.TokensInput),
		float64(direct.TotalTokens)*0.1,
		"the row's occupancy must be within a turn's growth of the server's")

	// "auto" is a mode, not a model, and it is what usage reports until a call
	// resolves one.
	assert.NotEqual(t, copilotAPIAutoModel, reading.Model,
		"the automatic-selection sentinel must never be published as a model")
	assert.NotEqual(t, direct.ModelName, reading.Model,
		"the model must come from usage; contextInfo.modelName names a different "+
			"model than the turn ran on under auto mode")
}

// startLiveCopilotServer launches copilot in TUI+server mode on a free port and
// returns once it is listening, with the address, the working directory, and the
// pid holding the listener. The PTY is not optional: without a terminal the CLI
// takes a different startup branch and never starts the embedded server.
//
// extra is appended to the argv, for the launch options a caller needs to state
// rather than inherit — `--model` in particular, which production always passes.
func startLiveCopilotServer(t *testing.T, binary, realHome string, extra ...string) (string, string, int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())

	root := t.TempDir()
	home := filepath.Join(root, "home")
	logs := filepath.Join(root, "logs")
	workdir := filepath.Join(root, "work")
	for _, dir := range []string{home, logs, workdir} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}

	argv := append([]string{"--ui-server", "--port", strconv.Itoa(port),
		"--allow-all-tools", "--log-dir", logs}, extra...)
	command := exec.Command(binary, argv...)
	command.Dir = workdir
	// A throwaway COPILOT_HOME keeps the run out of the operator's real
	// profile; the real HOME is what makes it authenticated.
	command.Env = append(os.Environ(), "COPILOT_HOME="+home, "HOME="+realHome)

	terminal, err := pty.Start(command)
	require.NoError(t, err)
	// Drained, or a full pipe wedges the process.
	go func() {
		buffer := make([]byte, 4096)
		for {
			if _, err := terminal.Read(buffer); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		_ = command.Wait()
		_ = terminal.Close()
	})

	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, time.Second)
		if err == nil {
			_ = conn.Close()
			return address, workdir, command.Process.Pid
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("copilot never listened on %s; logs in %s", address, logs)
	return "", "", 0
}

// portFromAddress splits the port back out of a host:port the helper built.
func portFromAddress(t *testing.T, address string) int {
	t.Helper()
	_, portText, err := net.SplitHostPort(address)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	return port
}

// TestLiveCopilotAPINeverPublishesTheAutoSentinel is the live check for the
// auto-model fix SPECIFICALLY, and it exists because the broader live test
// above does not reliably cover it.
//
// That test does assert the sentinel is never published — but only after
// waiting for a reading with a model and a non-zero output count, by which
// point a completed call has usually resolved a real model and the assertion
// passes without the fix having done anything. It caught the bug originally by
// timing accident, not by design. A green from it is therefore not evidence
// about this fix, which is worse than no evidence.
//
// This one reads usage BEFORE any prompt is sent, when no call has resolved a
// model, so the sentinel is what usage reports rather than an accident of when
// the read landed. It spends no quota: no turn is run, in any attempt.
//
// It FAILS rather than skips when the session is not in automatic selection,
// because the whole point is that it must never report success without having
// verified the thing it is named after.
//
// # Why it relaunches
//
// Measured against Copilot CLI 1.0.78 (TCL-1078), usage.currentModel is not the
// model the session will run on. It carries whatever model was EXPLICITLY
// SELECTED, and a fresh session's startup selection of "auto" is a race that
// Copilot loses a substantial fraction of the time. Whether it happened is visible on
// the event stream as a bare `session.model_change {"newModel":"auto"}` — no
// contextTier, no reasoningEffort, unlike the startup event of the same name —
// and when it fires, usage reports the sentinel about 50ms later. When it does
// not, currentModel stays "" for the life of the session and goes straight to
// the resolved model once a call completes.
//
// Waiting longer does not help: across twelve launches the sentinel was there
// within one second of session.create or was still absent after 45 seconds of
// polling, with nothing in between. So the wait per attempt is short and the
// retry is a fresh server — each launch is an independent trial costing a few
// seconds and no quota.
//
// # The residual false-failure rate, as a number
//
// A retry-until-reproduced test has a false-failure rate by construction, so
// here is the measurement it rests on rather than an assurance. Per-attempt hit
// rate for the launch below, on 1.0.78 from an agent sandbox: 16 of 17 in one
// batch, and 5 of 9 across four later runs of this test as written. The rate is
// not stable — it is a property of a Copilot build racing a network fetch, not
// of this code — so the loop is sized on the WORSE figure. At p=0.5, ten
// attempts miss about once in a thousand runs; at p=0.94 the number stops
// mattering. If it ever does fail, the likely reading is not bad luck but that
// the precondition stopped being reproducible, which is what the message says.
//
// Nothing budgets for this in CI. The whole file is gated behind
// TCLAUDE_COPILOT_LIVE=1, which CI does not set, so these runs are an
// operator's or an agent's and never a pipeline's.
//
// Note what this rules out. The first repair anyone reaches for — wait for
// `session.model_change`, then re-read — does NOT work, and a run that took a
// turn shows why: model_change to "auto" arrived at 4.74s with usage still
// answering "" a second later, and currentModel went "" → "gpt-5-mini" without
// ever passing through the sentinel. The event says a selection happened; only
// the usage read says what usage will report.
func TestLiveCopilotAPINeverPublishesTheAutoSentinel(t *testing.T) {
	if os.Getenv("TCLAUDE_COPILOT_LIVE") != "1" {
		t.Skip("set TCLAUDE_COPILOT_LIVE=1 to run against a real copilot --ui-server")
	}
	binary, err := exec.LookPath("copilot")
	if err != nil {
		t.Skipf("copilot not on PATH: %v", err)
	}
	realHome := os.Getenv("HOME")

	const attempts = 10
	var metrics copilotapi.UsageMetrics
	reproduced := false
	for attempt := 1; attempt <= attempts && !reproduced; attempt++ {
		// A subtest per attempt, so the server, its PTY and its temporary
		// COPILOT_HOME are torn down as the attempt ends rather than piling up
		// ten live copilots until the parent finishes.
		t.Run(fmt.Sprintf("launch-%d", attempt), func(t *testing.T) {
			metrics, reproduced = liveCopilotAutoSentinelAttempt(t, binary, realHome)
		})
	}

	if !reproduced {
		t.Fatalf("none of %d launches reproduced automatic model selection: usage kept "+
			"reporting currentModel=%q before any call. The auto-model fix is therefore "+
			"NOT verified by this run — do not read this failure as a defect in the fix. "+
			"Report it: at the rate measured on 1.0.78 this should be far rarer than a "+
			"single run, so a host that never reproduces it has changed something about "+
			"how Copilot records the startup model selection", attempts, metrics.CurrentModel)
	}

	// Part of the precondition rather than an aside. copilotAPIReadingModel also
	// returns "" when modelMetrics holds anything other than exactly one key, so
	// without this the assertion below could come back green off the wrong
	// branch — passing for a reason that has nothing to do with the sentinel.
	require.Empty(t, metrics.ModelMetrics,
		"the sentinel is present but a call has already been billed, so an empty reading "+
			"model would no longer be evidence about the sentinel branch")

	// The fix, applied to the real payload the server just produced.
	assert.Empty(t, copilotAPIReadingModel(metrics),
		"with the sentinel present and no resolved model in modelMetrics, the reading "+
			"must carry NO model rather than the sentinel")

	// And what publishing it would have cost, stated as a number rather than as
	// a worry: the static table answers the sentinel with a generic window, so a
	// 128000-token session would have been metered against 200000.
	assert.Equal(t, int64(200000), harness.CopilotContextWindowDefault(copilotAPIAutoModel),
		"if this stops being a plausible-looking wrong answer, the trap this test "+
			"guards has changed shape and the reasoning above needs rereading")
}

// copilotUnavailableModelName is a model Copilot will not have. Naming one is
// how this test reaches automatic selection; see liveCopilotAutoSentinelAttempt
// for why that is the production condition rather than a trick.
const copilotUnavailableModelName = "tclaude-no-such-model-9x"

// liveCopilotAutoSentinelAttempt runs one trial: a fresh server, a fresh
// session, and a short poll for the sentinel. It reports the metrics it last
// read and whether they carry the sentinel. No prompt is sent.
//
// # Why the launch names a model Copilot does not have
//
// It looks like a trick and is the opposite of one. Copilot validates `--model`
// against a model list it is still fetching — `session.model.list` answers
// `{"list":[]}` at this point in startup — so a name it cannot find produces
// `Model "X" from --model flag is not available. Using "auto" instead.` and the
// session lands in automatic selection with the sentinel recorded. That is not
// a synthetic path: real tclaude spawns hit exactly this message in the field,
// which is how the condition arises in production at all.
//
// It is also the highest-yield way in. Measured across launches on 1.0.78:
// an unavailable name reproduced the sentinel 16 times in 17 in one batch,
// `--model=auto` 8 times in 13, and a bare launch 2 in 4. The unavailable name
// is the best of the three on every batch measured, which is what it is chosen
// for; the loop above is nonetheless sized against a worse rate than any batch
// showed.
//
// The two paths produce the SAME observable, which is the part worth checking
// rather than assuming, since the whole test rests on it: the raw
// `session.usage.getMetrics` payload is identical under both — `"modelMetrics":
// {}` with `"currentModel":"auto"` — and the code under test reads nothing but
// that field.
func liveCopilotAutoSentinelAttempt(
	t *testing.T, binary, realHome string,
) (copilotapi.UsageMetrics, bool) {
	t.Helper()
	address, workdir, _ := startLiveCopilotServer(t, binary, realHome,
		"--model="+copilotUnavailableModelName)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	client, err := copilotapi.DialRetry(ctx, address, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	info, err := client.CreateSession(ctx, copilotapi.CreateSessionParams{
		SessionID: copilotapi.NewSessionID(), WorkingDirectory: workdir,
		ClientName: "tclaude", Streaming: true,
	})
	require.NoError(t, err)
	require.NoError(t, client.SetForegroundSession(ctx, info.SessionID))

	// Five seconds against a selection that lands inside one, so a slow host is
	// covered several times over while a launch that lost the race is abandoned
	// promptly enough that ten of them stay cheap.
	var metrics copilotapi.UsageMetrics
	deadline := time.Now().Add(5 * time.Second)
	for {
		metrics, err = client.UsageMetrics(ctx, info.SessionID)
		require.NoError(t, err)
		if metrics.CurrentModel == copilotAPIAutoModel || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("pre-turn usage: currentModel=%q modelMetrics=%d",
		metrics.CurrentModel, len(metrics.ModelMetrics))
	return metrics, metrics.CurrentModel == copilotAPIAutoModel
}
