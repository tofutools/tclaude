package agentd

import (
	"context"
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

	address, workdir := startLiveCopilotServer(t, binary, realHome)

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

	startCopilotAPIStateConsumer(&copilotAPISession{
		ConvID: convID, SessionID: info.SessionID, Client: client,
	})

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
// returns once it is listening. The PTY is not optional: without a terminal the
// CLI takes a different startup branch and never starts the embedded server.
func startLiveCopilotServer(t *testing.T, binary, realHome string) (string, string) {
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

	command := exec.Command(binary, "--ui-server", "--port", strconv.Itoa(port),
		"--allow-all-tools", "--log-dir", logs)
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
			return address, workdir
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("copilot never listened on %s; logs in %s", address, logs)
	return "", ""
}
