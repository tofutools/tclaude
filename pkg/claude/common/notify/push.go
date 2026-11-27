package notify

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Web Push: a notification also queued for the operator's subscribed
// devices (db push_outbox), which agentd sends through their push services.
// Queued here, in whichever process decided the notification, the same way
// the browser queue is; agentd's sender drains it.
//
// What a push says is written for a third party to carry: the push service
// relays it encrypted, but the text is kept to what a lock screen should
// show anyway. An access request names who asks and for which permission,
// never the request's body or path; a notify-human message gives its
// sender and subject, never its body. With content_free on there is no text
// at all.

// PushPayload is the plaintext a device's service worker reads.
type PushPayload struct {
	V     int    `json:"v"`
	Tag   string `json:"tag"`
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`
	URL   string `json:"url"`
}

// pushTitleMax and pushBodyMax keep a push to what a lock screen shows.
const (
	pushTitleMax = 80
	pushBodyMax  = 240
)

// ContentFreePushTitle is what a content-free push shows.
const ContentFreePushTitle = "tclaude needs you"

// pushNote is a notification as a push: its category, its text and where
// tapping it opens.
type pushNote struct {
	category string
	title    string
	body     string
	url      string
	urgency  string
}

// OnPushQueued is called after pushes are queued in this process; agentd
// sets it to wake its sender. Pushes queued by other processes (hook
// callbacks) are found by the sender's own tick.
var OnPushQueued func()

// pushNow is the clock push timestamps use; tests pin it.
var pushNow = time.Now

// newPushTag is a random tag naming one notification across its devices.
// It is also the push's Topic, so it is base64url and at most 32 chars.
func newPushTag() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// pushText trims s to max runes on one line.
func pushText(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-1]) + "…"
}

// queuePush queues note for every subscribed device when the config wants
// its category. Best-effort, like the browser queue: a failure is logged
// and never fails the notification's other channels.
func queuePush(n *config.NotificationConfig, note pushNote) {
	if !n.PushWanted(note.category) {
		return
	}
	payload := PushPayload{V: 1, Tag: newPushTag(), URL: "/"}
	if !n.PushContentFree() {
		payload.Title = pushText(note.title, pushTitleMax)
		payload.Body = pushText(note.body, pushBodyMax)
		if note.url != "" {
			payload.URL = note.url
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	queued, err := db.EnqueuePush(payload.Tag, raw, note.urgency, 0, pushNow())
	if err != nil {
		slog.Warn("queueing push notification failed", "err", err, "category", note.category)
		return
	}
	if queued > 0 && OnPushQueued != nil {
		OnPushQueued()
	}
}

// QueueTestPush queues a fixed test message for one device (or every
// device when deviceID is 0), whatever the categories say: the operator
// asked for it. It returns how many were queued.
func QueueTestPush(deviceID int64, contentFree bool) (int64, error) {
	payload := PushPayload{V: 1, Tag: newPushTag(), URL: "/"}
	if !contentFree {
		payload.Title = "tclaude: test notification"
		payload.Body = "Push notifications reach this device."
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	queued, err := db.EnqueuePush(payload.Tag, raw, "high", deviceID, pushNow())
	if err == nil && queued > 0 && OnPushQueued != nil {
		OnPushQueued()
	}
	return queued, err
}

// statePushNote is a state change as a push: "Codex: Awaiting input" and
// the agent, like the desktop banner.
func statePushNote(category, harness, displayStatus, convTitle, cwd string) pushNote {
	body := strings.TrimSpace(convTitle)
	if body == "" {
		body = projectName(cwd)
	}
	urgency := "normal"
	if category == "awaiting_permission" || category == "awaiting_input" {
		urgency = "high"
	}
	return pushNote{
		category: category,
		title:    notificationTitle(harness, formatStatus(displayStatus)),
		body:     body,
		url:      "/",
		urgency:  urgency,
	}
}
