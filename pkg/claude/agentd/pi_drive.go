package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// The pi extension drive (`tclaude agent spawn --pi-drive`).
//
// A pi agent spawned with the drive takes tclaude's deliveries — inbox
// messages, the welcome, rename, compaction and soft exit — from tclaude's pi
// extension instead of as tmux keystrokes. The extension, running inside pi,
// long-polls POST /v1/whoami/pi-drive on the agent socket; the daemon answers
// with the requests queued for that conversation, and the extension runs each
// one through pi's extension API (sendUserMessage, compact, setSessionName,
// shutdown) and acknowledges it on its next poll.
//
// The rule is the other API drives' rule: an opted-in agent is channel-only.
// When the channel is down a delivery is refused, not typed: the durable inbox
// holds the message and retries, and a lifecycle command fails as a control
// failure. Nothing here falls back to send-keys.
//
// Requests carry fixed operation names, never commands: the text of a message
// travels as data to sendUserMessage, which pi treats as one user turn.

const (
	piDriveOpMessage = "message"
	piDriveOpCompact = "compact"
	piDriveOpRename  = "rename"
	piDriveOpExit    = "exit"
)

// Timings, package vars so flow tests can shrink them.
var (
	// piDrivePollWait is how long a poll is held open with nothing to send.
	piDrivePollWait = 20 * time.Second
	// piDriveConnectedGrace is how long after its last poll the channel still
	// counts as up: the gap between one poll's answer and the next.
	piDriveConnectedGrace = 10 * time.Second
	// piDriveAckTimeout bounds the wait for the extension's acknowledgement.
	piDriveAckTimeout = 30 * time.Second
	// piDriveConnectTimeout bounds post-init's wait for a new pi's channel.
	piDriveConnectTimeout = 60 * time.Second
)

// errPiDriveDown is the hold/retry answer: the extension is not polling.
var errPiDriveDown = errors.New("pi drive channel is not connected")

type piDriveRequest struct {
	ID      int64  `json:"id"`
	Op      string `json:"op"`
	Text    string `json:"text,omitempty"`
	Deliver string `json:"deliver,omitempty"`
	Name    string `json:"name,omitempty"`
}

type piDriveAck struct {
	ID    int64  `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

type piDrivePollBody struct {
	Acks []piDriveAck `json:"acks,omitempty"`
}

type piDrivePollResponse struct {
	Drive    bool             `json:"drive"`
	Requests []piDriveRequest `json:"requests,omitempty"`
}

type piDriveChannel struct {
	queue    []piDriveRequest
	waiters  map[int64]chan piDriveAck
	polls    int
	lastPoll time.Time
	wake     chan struct{}
}

type piDriveRegistry struct {
	mu     sync.Mutex
	nextID int64
	convs  map[string]*piDriveChannel
}

var piDrives = &piDriveRegistry{convs: map[string]*piDriveChannel{}}

func (r *piDriveRegistry) channel(convID string) *piDriveChannel {
	c := r.convs[convID]
	if c == nil {
		c = &piDriveChannel{waiters: map[int64]chan piDriveAck{}, wake: make(chan struct{})}
		r.convs[convID] = c
	}
	return c
}

// connected reports whether the extension is polling now or polled recently.
func (r *piDriveRegistry) connected(convID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.convs[convID]
	return c != nil && (c.polls > 0 || time.Since(c.lastPoll) < piDriveConnectedGrace)
}

// call queues req for the conversation's extension and waits for its ack.
// errPiDriveDown when no extension is polling. An exit is not acknowledged —
// pi shuts down running it — so it is done once a poll has taken it.
func (r *piDriveRegistry) call(convID string, req piDriveRequest) error {
	r.mu.Lock()
	c := r.convs[convID]
	if c == nil || (c.polls == 0 && time.Since(c.lastPoll) >= piDriveConnectedGrace) {
		r.mu.Unlock()
		return errPiDriveDown
	}
	r.nextID++
	req.ID = r.nextID
	ack := make(chan piDriveAck, 1)
	c.waiters[req.ID] = ack
	c.queue = append(c.queue, req)
	close(c.wake)
	c.wake = make(chan struct{})
	r.mu.Unlock()

	result := func(a piDriveAck) error {
		if !a.OK {
			return fmt.Errorf("pi extension: %s %s", req.Op, a.Error)
		}
		return nil
	}
	timer := time.NewTimer(piDriveAckTimeout)
	defer timer.Stop()
	// While the request still waits in the queue, a channel that goes quiet
	// (pi died) withdraws it at once instead of after the whole ack timeout.
	check := time.NewTicker(piDriveConnectedGrace / 4)
	defer check.Stop()
wait:
	for {
		select {
		case a := <-ack:
			return result(a)
		case <-timer.C:
			break wait
		case <-check.C:
			r.mu.Lock()
			quiet := c.polls == 0 && time.Since(c.lastPoll) >= piDriveConnectedGrace
			r.mu.Unlock()
			if quiet {
				break wait
			}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	select {
	case a := <-ack:
		// Acknowledged as the wait ended.
		return result(a)
	default:
	}
	delete(c.waiters, req.ID)
	for i, q := range c.queue {
		if q.ID == req.ID {
			// Never handed over: nothing ran, so a retry cannot duplicate it.
			c.queue = append(c.queue[:i], c.queue[i+1:]...)
			return errPiDriveDown
		}
	}
	return fmt.Errorf("pi extension did not acknowledge %s within %s", req.Op, piDriveAckTimeout)
}

// poll records acks, then waits up to piDrivePollWait for queued requests.
// onConnect runs when this poll brings the channel up, so deliveries held
// while it was down go out now instead of at their next retry.
func (r *piDriveRegistry) poll(stop <-chan struct{}, convID string, acks []piDriveAck, onConnect func()) []piDriveRequest {
	r.mu.Lock()
	c := r.channel(convID)
	for _, a := range acks {
		if w := c.waiters[a.ID]; w != nil {
			w <- a
			delete(c.waiters, a.ID)
		}
	}
	reconnected := c.polls == 0 && time.Since(c.lastPoll) >= piDriveConnectedGrace
	c.polls++
	c.lastPoll = time.Now()
	if reconnected && onConnect != nil {
		r.mu.Unlock()
		onConnect()
		r.mu.Lock()
	}
	defer func() {
		r.mu.Lock()
		c.polls--
		c.lastPoll = time.Now()
		r.mu.Unlock()
	}()
	timer := time.NewTimer(piDrivePollWait)
	defer timer.Stop()
	for len(c.queue) == 0 {
		wake := c.wake
		r.mu.Unlock()
		select {
		case <-wake:
		case <-timer.C:
			return nil
		case <-stop:
			return nil
		}
		r.mu.Lock()
	}
	out := c.queue
	c.queue = nil
	for _, req := range out {
		if req.Op == piDriveOpExit {
			// pi shuts down running it, so it is never acknowledged.
			if w := c.waiters[req.ID]; w != nil {
				w <- piDriveAck{ID: req.ID, OK: true}
				delete(c.waiters, req.ID)
			}
		}
	}
	r.mu.Unlock()
	return out
}

// piDriveSelected reports whether convID is a pi agent spawned with the drive.
func piDriveSelected(convID string) (bool, error) {
	if harnessForConv(convID).Name != harness.PiName {
		return false, nil
	}
	profile, err := db.RecordedLaunchPostureForConv(convID)
	if err != nil {
		return false, err
	}
	return profile != nil && profile.PiDrive != nil && *profile.PiDrive, nil
}

func piDriveMessage(convID, text, deliver string) error {
	return piDrives.call(convID, piDriveRequest{Op: piDriveOpMessage, Text: text, Deliver: deliver})
}

func piDriveRename(convID, title string) error {
	return piDrives.call(convID, piDriveRequest{Op: piDriveOpRename, Name: title})
}

func piDriveExit(convID string) error {
	return piDrives.call(convID, piDriveRequest{Op: piDriveOpExit})
}

// piDriveSlashCommand maps a pi lifecycle command onto the drive. The mapping
// is by lifecycle TOKEN — compile-time constants, never caller input — so this
// is a fixed switch. An unmapped token fails closed; there is no keystroke
// fallback for an agent on the drive.
func piDriveSlashCommand(convID, line, followUp, reason string) bool {
	life := harnessForConv(convID).Life
	var err error
	switch {
	case life != nil && line == life.CompactCommand():
		err = piDrives.call(convID, piDriveRequest{Op: piDriveOpCompact, Text: followUp})
		followUp = ""
	case life != nil && line == life.SoftExitCommand():
		err = piDriveExit(convID)
	default:
		slog.Warn("pi drive: lifecycle command has no drive operation; refusing",
			"conv_id", convID, "line", line, "reason", reason)
		return false
	}
	if err == nil && followUp != "" {
		err = piDriveMessage(convID, followUp, "followUp")
	}
	if err != nil {
		slog.Warn("pi drive: lifecycle command failed", "conv_id", convID,
			"line", line, "reason", reason, "error", err)
		return false
	}
	slog.Info("pi drive: lifecycle command dispatched", "conv_id", convID,
		"line", line, "reason", reason)
	return true
}

// awaitPiDrive waits for a new pi's extension to start polling.
func awaitPiDrive(convID string) bool {
	deadline := time.Now().Add(piDriveConnectTimeout)
	for {
		if piDrives.connected(convID) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// handleWhoamiPiDrive is the extension's long poll. Only the pi process
// itself may hold its conversation's channel: the caller must resolve to the
// conversation as an agent, AND its socket peer must be a harness process
// with no harness ancestor — pi, not a tool pi started. A tool child (a bash
// command, a node script) has pi above it and is refused, so an agent's own
// tool calls cannot take over its deliveries.
func handleWhoamiPiDrive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	convID, ok := requireAgent(w, r)
	if !ok {
		return
	}
	if !piDrivePeerIsHarnessRoot(peerFromContext(r.Context()).PID) {
		writeError(w, http.StatusForbidden, "forbidden",
			"the pi drive channel belongs to the pi process itself")
		return
	}
	var body piDrivePollBody
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "bad pi drive poll body: "+err.Error())
			return
		}
	}
	selected, err := piDriveSelected(convID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "launch posture unreadable: "+err.Error())
		return
	}
	if !selected {
		writeJSON(w, http.StatusOK, piDrivePollResponse{Drive: false})
		return
	}
	requests := piDrives.poll(r.Context().Done(), convID, body.Acks, func() {
		enqueueDeliveryForConv(convID)
	})
	writeJSON(w, http.StatusOK, piDrivePollResponse{Drive: true, Requests: requests})
}

func piDrivePeerIsHarnessRoot(pid int) bool {
	if pid <= 0 {
		return false
	}
	table := newBrokerProcTable()
	if table.harnessName(pid) == "" {
		return false
	}
	for cur, hops := table.parent(pid), 0; cur > 1 && hops < 64; cur, hops = table.parent(cur), hops+1 {
		if table.harnessName(cur) != "" {
			return false
		}
	}
	return true
}
