package agentd_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Process ids for the pi drive's caller check: pi itself under a shell, and
// a tool pi started.
const (
	piDriveShellPID = 4241
	piDrivePiPID    = 4242
	piDriveToolPID  = 4243
)

// piDriveExtension stands in for the drive loop in tclaude's pi extension: it
// long-polls POST /v1/whoami/pi-drive through the daemon mux as the pi
// process, runs each request on the pane simulator through pi's API (never
// the pane's input), and acknowledges it on the next poll.
type piDriveExtension struct {
	t      *testing.T
	f      *testharness.Flow
	sim    *testharness.PiSim
	conv   string
	cancel context.CancelFunc
	done   chan struct{}

	mu  sync.Mutex
	ops []string
}

func startPiDriveExtension(t *testing.T, f *testharness.Flow, sim *testharness.PiSim, conv string) *piDriveExtension {
	ctx, cancel := context.WithCancel(context.Background())
	e := &piDriveExtension{t: t, f: f, sim: sim, conv: conv, cancel: cancel, done: make(chan struct{})}
	go e.loop(ctx)
	t.Cleanup(e.stop)
	return e
}

func (e *piDriveExtension) loop(ctx context.Context) {
	defer close(e.done)
	var acks []map[string]any
	for ctx.Err() == nil {
		code, resp := piDrivePoll(e.t, e.f, e.conv, piDrivePiPID, acks, ctx)
		acks = nil
		if ctx.Err() != nil {
			return
		}
		if code != http.StatusOK || !resp.Drive {
			return
		}
		for _, req := range resp.Requests {
			err := e.sim.Drive(req.Op, req.Text, req.Name)
			e.mu.Lock()
			e.ops = append(e.ops, req.Op)
			e.mu.Unlock()
			ack := map[string]any{"id": req.ID, "ok": err == nil}
			if err != nil {
				ack["error"] = err.Error()
			}
			acks = append(acks, ack)
		}
	}
}

func (e *piDriveExtension) stop() {
	e.cancel()
	<-e.done
}

func (e *piDriveExtension) ran() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.ops...)
}

type piDriveResponse struct {
	Drive    bool `json:"drive"`
	Requests []struct {
		ID   int64  `json:"id"`
		Op   string `json:"op"`
		Text string `json:"text"`
		Name string `json:"name"`
	} `json:"requests"`
}

func piDrivePoll(t *testing.T, f *testharness.Flow, conv string, pid int, acks []map[string]any, ctx context.Context) (int, piDriveResponse) {
	req := testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/pi-drive", map[string]any{"acks": acks})
	req = agentd.AsAgentPeerWithPID(req.WithContext(ctx), conv, pid)
	rec := testharness.Serve(f.Mux, req)
	var resp piDriveResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, resp
}

func piDriveFlow(t *testing.T) *testharness.Flow {
	t.Helper()
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	t.Cleanup(agentd.SetProcTreeForTest(
		map[int]string{piDriveShellPID: "bash", piDrivePiPID: harness.PiName, piDriveToolPID: "node"},
		map[int]int{piDriveShellPID: 1, piDrivePiPID: piDriveShellPID, piDriveToolPID: piDrivePiPID}))
	t.Cleanup(agentd.SetPiDriveTimingsForTest(50*time.Millisecond, 200*time.Millisecond, 2*time.Second, 2*time.Second))
	t.Cleanup(agentd.SetNudgeRetryTimingForTest(10*time.Millisecond, 50*time.Millisecond))
	return newFlow(t)
}

// spawnSender spawns a Claude Code agent into the group to message from.
func spawnSender(t *testing.T, f *testharness.Flow) string {
	t.Helper()
	resp := f.AsHuman().SpawnWith("crew", map[string]any{"name": "sender"})
	require.Equalf(t, http.StatusOK, resp.Code, "sender spawn body=%s", resp.Raw)
	return resp.ConvID
}

func sendToAgent(t *testing.T, f *testharness.Flow, from, conv, body string) {
	t.Helper()
	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/messages",
		map[string]any{"to": conv, "subject": "drive", "body": body}), from))
	require.Equalf(t, http.StatusOK, rec.Code, "POST /v1/messages body=%s", rec.Body.String())
}

func sentToSession(f *testharness.Flow, tmuxSession string) []testharness.SentKey {
	var out []testharness.SentKey
	for _, k := range f.World.Tmux.Sent() {
		if strings.HasPrefix(k.Target, tmuxSession) {
			out = append(out, k)
		}
	}
	return out
}

// A pi agent spawned with the drive takes every delivery through tclaude's
// extension: an inbox message, a compaction with its follow-up, a rename,
// and the soft stop. Not one
// keystroke reaches its pane.
func TestPiDrive_DeliversEverythingWithoutSendKeys(t *testing.T) {
	f := piDriveFlow(t)
	f.HaveGroup("crew")
	sender := spawnSender(t, f)
	resp, sim := spawnPi(t, f, "crew", map[string]any{"name": "pi-driven", "pi_drive": true})
	ext := startPiDriveExtension(t, f, sim, resp.ConvID)

	// pi enrolls at launch: its name and welcome are launch arguments.
	require.Equal(t, "pi-driven", sim.Name())
	require.NoError(t, sim.Reply("ready"))

	sendToAgent(t, f, sender, resp.ConvID, "please look at the drive")
	require.Eventually(t, func() bool { return transcriptHas(sim, "please look at the drive") },
		5*time.Second, 10*time.Millisecond, "the inbox message arrives over the drive")
	require.NoError(t, sim.Reply("looked"))

	compacted := f.CompactWithFollowUp(resp.ConvID, "carry on after compaction")
	require.Equalf(t, http.StatusOK, compacted.Code, "compact body=%s", compacted.Raw)
	assert.Equal(t, 1, sim.Compactions())
	assert.True(t, transcriptHas(sim, "carry on after compaction"), "the follow-up follows the compaction")
	require.NoError(t, sim.Reply("continued"))

	renamed := f.Rename(resp.ConvID, "pi-renamed")
	require.Equalf(t, http.StatusOK, renamed.Code, "rename body=%s", renamed.Raw)
	assert.Equal(t, "pi-renamed", sim.Name())

	f.AssertSoftStopped(f.Stop(resp.ConvID, false))
	assert.False(t, sim.IsAlive(), "the soft stop shuts pi down over the drive")

	assert.Empty(t, sentToSession(f, resp.TmuxSession), "a pi on the drive never receives keystrokes")
	assert.Subset(t, ext.ran(), []string{"rename", "message", "compact", "exit"})

	profile, err := db.RecordedLaunchPostureForConv(resp.ConvID)
	require.NoError(t, err)
	require.NotNil(t, profile)
	require.NotNil(t, profile.PiDrive)
	assert.True(t, *profile.PiDrive, "the drive is frozen into the launch posture")
}

// While the extension is not polling, a message is held in the inbox, not
// typed into the pane; once the channel is back the queue delivers it.
func TestPiDrive_HoldsDeliveriesWhileTheChannelIsDown(t *testing.T) {
	f := piDriveFlow(t)
	f.HaveGroup("crew")
	sender := spawnSender(t, f)
	resp, sim := spawnPi(t, f, "crew", map[string]any{"name": "pi-driven", "pi_drive": true})
	ext := startPiDriveExtension(t, f, sim, resp.ConvID)
	require.NoError(t, sim.Reply("ready"))
	ext.stop()
	// Past the connected grace: the channel is down.
	time.Sleep(300 * time.Millisecond)

	sendToAgent(t, f, sender, resp.ConvID, "held while the drive is down")
	time.Sleep(300 * time.Millisecond)
	assert.False(t, transcriptHas(sim, "held while the drive is down"), "nothing reaches pi while the drive is down")
	assert.Empty(t, sentToSession(f, resp.TmuxSession), "and nothing is typed instead")
	renamed := f.Rename(resp.ConvID, "not-now")
	assert.NotEqual(t, http.StatusOK, renamed.Code, "a rename fails rather than typing /name")

	startPiDriveExtension(t, f, sim, resp.ConvID)
	require.Eventually(t, func() bool { return transcriptHas(sim, "held while the drive is down") },
		5*time.Second, 10*time.Millisecond, "the held message is delivered once the drive is back")
	assert.Empty(t, sentToSession(f, resp.TmuxSession))
}

// Only pi itself holds its channel: a tool pi started (a node script under
// pi) is refused, and a pi launched without the drive is told to stop.
func TestPiDrive_ChannelBelongsToTheOptedInPiProcess(t *testing.T) {
	f := piDriveFlow(t)
	f.HaveGroup("crew")
	driven, drivenSim := spawnPi(t, f, "crew", map[string]any{"name": "pi-driven", "pi_drive": true})
	code, _ := piDrivePoll(t, f, driven.ConvID, piDriveToolPID, nil, context.Background())
	assert.Equal(t, http.StatusForbidden, code, "a process under pi cannot take the channel")

	// The choice is the agent's, not the launch's: a resume keeps the drive.
	ext := startPiDriveExtension(t, f, drivenSim, driven.ConvID)
	f.AssertSoftStopped(f.Stop(driven.ConvID, false))
	ext.stop()
	require.Equal(t, http.StatusOK, f.Resume(driven.ConvID).Code)
	code, poll := piDrivePoll(t, f, driven.ConvID, piDrivePiPID, nil, context.Background())
	require.Equal(t, http.StatusOK, code)
	assert.True(t, poll.Drive, "the resumed agent is still on the drive")

	plain, plainSim := spawnPi(t, f, "crew", map[string]any{"name": "pi-plain"})
	code, poll = piDrivePoll(t, f, plain.ConvID, piDrivePiPID, nil, context.Background())
	require.Equal(t, http.StatusOK, code)
	assert.False(t, poll.Drive, "a launch without the drive is told so")
	renamed := f.Rename(plain.ConvID, "pi-plain-renamed")
	require.Equalf(t, http.StatusOK, renamed.Code, "rename body=%s", renamed.Raw)
	f.AssertSentContains(plain.TmuxSession, "/name pi-plain-renamed", 5*time.Second)
	assert.Equal(t, "pi-plain-renamed", plainSim.Name(), "and keeps the send-keys path")

	rec := f.AsHuman().SpawnWith("crew", map[string]any{"name": "claude-driven", "pi_drive": true})
	assert.Equal(t, http.StatusBadRequest, rec.Code, "pi_drive is pi only: %s", rec.Raw)
}

func transcriptHas(sim *testharness.PiSim, text string) bool {
	raw, err := os.ReadFile(sim.Path())
	return err == nil && strings.Contains(string(raw), text)
}
