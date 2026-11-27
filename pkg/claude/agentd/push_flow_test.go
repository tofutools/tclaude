package agentd_test

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/notify"
	"github.com/tofutools/tclaude/pkg/claude/common/webpush"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// fakePush is one push as the fake push service received it, decrypted
// with the device's private key.
type fakePush struct {
	Device  string
	Header  http.Header
	Claims  map[string]any
	Payload notify.PushPayload
}

// fakePushService stands in for a browser vendor's push service. It checks
// each push's VAPID signature against the key it carries, decrypts the body
// with the subscribed device's private key, and answers with the status set
// for that device (201 by default).
type fakePushService struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	devices map[string]*fakeDevice
	status  map[string]int
	retry   map[string]string
	got     []fakePush
	now     func() time.Time
}

// fakeDevice is a browser's half of a subscription.
type fakeDevice struct {
	name string
	key  *ecdh.PrivateKey
	auth []byte
}

func newFakePushService(t *testing.T, now func() time.Time) *fakePushService {
	t.Helper()
	s := &fakePushService{t: t, devices: map[string]*fakeDevice{}, status: map[string]int{}, retry: map[string]string{}, now: now}
	s.srv = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	host, _, _ := strings.Cut(strings.TrimPrefix(s.srv.URL, "https://"), ":")
	t.Cleanup(agentd.SetPushTransportForTest(s.srv.Client(), host))
	return s
}

func (s *fakePushService) serve(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/push/")
	s.mu.Lock()
	dev := s.devices[name]
	status, retry := s.status[name], s.retry[name]
	s.mu.Unlock()
	if dev == nil {
		http.Error(w, "unknown subscription", http.StatusNotFound)
		return
	}
	claims, err := webpush.VerifyAuthorization(r.Header.Get("Authorization"), s.srv.URL, s.now())
	if err != nil {
		s.t.Errorf("push to %s: VAPID: %v", name, err)
		http.Error(w, "bad vapid", http.StatusUnauthorized)
		return
	}
	if r.Header.Get("Content-Encoding") != "aes128gcm" {
		s.t.Errorf("push to %s: Content-Encoding %q", name, r.Header.Get("Content-Encoding"))
	}
	body, _ := io.ReadAll(r.Body)
	plain, err := webpush.Decrypt(body, dev.key, dev.auth)
	if err != nil {
		s.t.Errorf("push to %s does not decrypt with the device's key: %v", name, err)
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	var payload notify.PushPayload
	require.NoError(s.t, json.Unmarshal(plain, &payload), "plaintext: %s", plain)
	s.mu.Lock()
	s.got = append(s.got, fakePush{Device: name, Header: r.Header.Clone(), Claims: claims, Payload: payload})
	s.mu.Unlock()
	if retry != "" {
		w.Header().Set("Retry-After", retry)
	}
	if status == 0 {
		status = http.StatusCreated
	}
	w.WriteHeader(status)
}

// pushes returns what arrived so far and forgets it.
func (s *fakePushService) pushes() []fakePush {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.got
	s.got = nil
	return out
}

func (s *fakePushService) answer(device string, status int, retryAfter string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status[device], s.retry[device] = status, retryAfter
}

// subscribe makes a device and subscribes it through the dashboard the way
// the service worker will: fetch the key, then post the subscription. It
// returns the device id.
func (s *fakePushService) subscribe(t *testing.T, dashboard http.Handler, name string) int64 {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	require.NoError(t, err)
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	s.mu.Lock()
	s.devices[name] = &fakeDevice{name: name, key: key, auth: auth}
	s.mu.Unlock()

	rec := testharness.Serve(dashboard, testharness.JSONRequest(t, http.MethodPost, "/api/push/key", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = testharness.Serve(dashboard, testharness.JSONRequest(t, http.MethodPost, "/api/push/devices", map[string]any{
		"endpoint": s.srv.URL + "/push/" + name,
		"keys": map[string]string{
			"p256dh": base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
			"auth":   base64.RawURLEncoding.EncodeToString(auth),
		},
		"label": name,
	}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		Device struct {
			ID int64 `json:"id"`
		} `json:"device"`
		New bool `json:"new"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.True(t, out.New)
	return out.Device.ID
}

// pushFixture is a flow with notifications on, a pinned push clock, and a
// fake push service.
type pushFixture struct {
	f         *testharness.Flow
	dashboard http.Handler
	svc       *fakePushService
	mu        sync.Mutex
	now       time.Time
}

func newPushFixture(t *testing.T) *pushFixture {
	t.Helper()
	return newPushFixtureOn(t, newFlow(t))
}

// newPushFixtureOn is newPushFixture on a flow some other fixture made.
func newPushFixtureOn(t *testing.T, f *testharness.Flow) *pushFixture {
	t.Helper()
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	p := &pushFixture{f: f, now: time.Now()}
	clock := func() time.Time {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.now
	}
	t.Cleanup(agentd.SetPushClockForTest(clock))
	t.Cleanup(notify.SetPushClockForTest(clock))
	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.Notifications.Enabled = true
	require.NoError(t, config.Save(cfg))
	p.dashboard = agentd.BuildDashboardHandlerForTest()
	p.svc = newFakePushService(t, clock)
	return p
}

func (p *pushFixture) advance(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.now = p.now.Add(d)
}

// pushState reads GET /api/push.
func (p *pushFixture) pushState(t *testing.T) map[string]any {
	t.Helper()
	rec := testharness.Serve(p.dashboard, testharness.JSONRequest(t, http.MethodGet, "/api/push", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var st map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &st))
	return st
}

// Scenario: the operator subscribed their phone, and an agent asks for sudo.
// The phone gets an encrypted, signed push that names who asks and for
// which permission, and nothing of the request itself.
func TestPush_AccessRequestReachesThePhoneWithoutTheRequest(t *testing.T) {
	p := newPushFixture(t)
	phone := p.svc.subscribe(t, p.dashboard, "phone")
	require.Positive(t, phone)

	const conv = "push-sudo-aaaa-bbbb-1111"
	p.f.HaveConvWithTitle(conv, "worker")
	ctx, hangUp := context.WithCancel(context.Background())
	t.Cleanup(hangUp)
	done := make(chan struct{})
	go func() {
		defer close(done)
		r := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/sudo", map[string]any{
			"slugs": []string{"groups.create"}, "duration": "15m", "reason": "make the sudo-mobile group",
		}).WithContext(ctx), conv)
		testharness.Serve(p.f.Mux, r)
	}()
	pendingAccessRequest(t, p.dashboard, "sudo.groups.create")

	var got []fakePush
	require.Eventually(t, func() bool {
		agentd.DrainPushForTest()
		got = append(got, p.svc.pushes()...)
		return len(got) > 0
	}, 10*time.Second, 5*time.Millisecond, "the access request must be pushed")
	require.Len(t, got, 1)
	push := got[0]
	assert.Equal(t, "phone", push.Device)
	assert.Equal(t, "tclaude: access request", push.Payload.Title)
	assert.Equal(t, "worker asks for sudo.groups.create", push.Payload.Body)
	assert.Equal(t, "/messages", push.Payload.URL)
	assert.NotContains(t, push.Payload.Body, "/v1/sudo", "the request's path stays off the push")
	assert.NotContains(t, push.Payload.Body, "sudo-mobile group", "the agent's reason stays off the push")
	assert.Equal(t, "high", push.Header.Get("Urgency"))
	assert.Equal(t, push.Payload.Tag, push.Header.Get("Topic"), "a newer push replaces an unseen one with the same tag")
	ttl, err := strconv.Atoi(push.Header.Get("TTL"))
	require.NoError(t, err)
	assert.InDelta(t, 600, ttl, 2)
	assert.Equal(t, "https://github.com/tofutools/tclaude", push.Claims["sub"], "the contact claim is the project, never the operator")

	hangUp()
	<-done

	devices := p.pushState(t)["devices"].([]any)
	require.Len(t, devices, 1)
	assert.NotEmpty(t, devices[0].(map[string]any)["last_ok_at"], "the device shows when it last got a push")
}

// Scenario: the categories are push's own. With idle off for push (its
// default) but on for the desktop, an idle agent pushes nothing; turning
// it on for push sends it, and content-free mode sends no text at all.
func TestPush_CategoriesAndContentFree(t *testing.T) {
	p := newPushFixture(t)
	p.svc.subscribe(t, p.dashboard, "phone")
	const conv = "push-cat-aaaa-bbbb-2222"
	p.f.HaveConvWithTitle(conv, "worker")

	st := p.pushState(t)
	cats := st["categories"].(map[string]any)
	// On by default: an agent waiting on the operator. Off: the rest.
	for cat, want := range map[string]bool{
		"access_requests": true, "operator_questions": true, "awaiting_permission": true, "awaiting_input": true,
		"human_messages": false, "present_pr": false, "idle": false, "error": false, "exited": false,
	} {
		assert.Equal(t, want, cats[cat], cat)
	}
	assert.Equal(t, false, st["content_free"])

	transition := func(session, to string) {
		notify.OnStateTransition(session, conv, "working", to, p.f.TestCwd("proj"), "worker", "codex")
		agentd.DrainPushForTest()
	}
	transition("sess-push-1", "idle")
	assert.Empty(t, p.svc.pushes(), "idle is off for push, though the desktop's rule is on")

	setPush := func(body map[string]any) {
		rec := testharness.Serve(p.dashboard, testharness.JSONRequest(t, http.MethodPost, "/api/push/config", body))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	setPush(map[string]any{"categories": map[string]bool{"idle": true}})
	transition("sess-push-2", "idle")
	got := p.svc.pushes()
	require.Len(t, got, 1)
	assert.Equal(t, "Codex: Idle", got[0].Payload.Title)
	assert.Equal(t, "worker", got[0].Payload.Body)
	assert.Equal(t, "normal", got[0].Header.Get("Urgency"))

	setPush(map[string]any{"content_free": true})
	transition("sess-push-3", "awaiting_input")
	got = p.svc.pushes()
	require.Len(t, got, 1)
	assert.Empty(t, got[0].Payload.Title, "content-free: the push service carries no text")
	assert.Empty(t, got[0].Payload.Body)
	assert.Equal(t, "/", got[0].Payload.URL)

	cfg, err := config.Load()
	require.NoError(t, err)
	require.NotNil(t, cfg.Notifications.Push)
	assert.Equal(t, map[string]bool{"idle": true}, cfg.Notifications.Push.Categories, "only choices are stored")
	assert.True(t, cfg.Notifications.Push.ContentFree)

	// The master switch is shared: off, nothing is pushed.
	cfg.Notifications.Enabled = false
	require.NoError(t, config.Save(cfg))
	transition("sess-push-4", "awaiting_input")
	assert.Empty(t, p.svc.pushes(), "the master switch silences push too")

	rec := testharness.Serve(p.dashboard, testharness.JSONRequest(t, http.MethodPost, "/api/push/config", map[string]any{"categories": map[string]bool{"bogus": true}}))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// Scenario: a device the browser dropped answers 410 at its push service;
// agentd forgets it. A busy push service (503, Retry-After) gets the same
// push again once the wait is over, not before.
func TestPush_GoneDeviceIsRemovedAndBusyServiceRetried(t *testing.T) {
	p := newPushFixture(t)
	gone := p.svc.subscribe(t, p.dashboard, "old-phone")
	busy := p.svc.subscribe(t, p.dashboard, "tablet")
	p.svc.answer("old-phone", http.StatusGone, "")
	p.svc.answer("tablet", http.StatusServiceUnavailable, "30")

	rec := testharness.Serve(p.dashboard, testharness.JSONRequest(t, http.MethodPost, "/api/push/test", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 2, agentd.DrainPushForTest())
	first := p.svc.pushes()
	require.Len(t, first, 2)
	assert.Equal(t, "tclaude: test notification", first[0].Payload.Title)

	_, err := db.GetPushSubscription(gone)
	assert.ErrorIs(t, err, db.ErrPushSubscriptionNotFound, "a 410 removes the device")
	dev, err := db.GetPushSubscription(busy)
	require.NoError(t, err)
	assert.Contains(t, dev.LastError, "503")

	p.advance(29 * time.Second)
	assert.Zero(t, agentd.DrainPushForTest(), "not before Retry-After")
	p.svc.answer("tablet", http.StatusCreated, "")
	p.advance(2 * time.Second)
	assert.Equal(t, 1, agentd.DrainPushForTest())
	again := p.svc.pushes()
	require.Len(t, again, 1)
	assert.Equal(t, first[1].Payload.Tag, again[0].Payload.Tag, "the same push, retried")
	dev, err = db.GetPushSubscription(busy)
	require.NoError(t, err)
	assert.Empty(t, dev.LastError, "a delivery clears the error")

	// A test push to one device reaches only it; removing it leaves none.
	rec = testharness.Serve(p.dashboard, testharness.JSONRequest(t, http.MethodPost, "/api/push/test", map[string]any{"device": busy}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	agentd.DrainPushForTest()
	require.Len(t, p.svc.pushes(), 1)
	rec = testharness.Serve(p.dashboard, testharness.JSONRequest(t, http.MethodDelete, "/api/push/devices/"+strconv.FormatInt(busy, 10), nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Empty(t, p.pushState(t)["devices"])
	rec = testharness.Serve(p.dashboard, testharness.JSONRequest(t, http.MethodPost, "/api/push/test", nil))
	assert.Equal(t, http.StatusConflict, rec.Code, "no device left to test")
}

// Scenario: a subscription must point at a push service. agentd posts to
// the endpoint on its own later, so an address of the operator's network
// is refused up front.
func TestPush_SubscribeRefusesEndpointsThatAreNotPushServices(t *testing.T) {
	p := newPushFixture(t)
	rec := testharness.Serve(p.dashboard, testharness.JSONRequest(t, http.MethodPost, "/api/push/key", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	key, _ := ecdh.P256().GenerateKey(rand.Reader)
	for _, endpoint := range []string{"https://192.168.1.10/admin", "http://fcm.googleapis.com/x", "https://fcm.googleapis.com.attacker.example/x"} {
		rec := testharness.Serve(p.dashboard, testharness.JSONRequest(t, http.MethodPost, "/api/push/devices", map[string]any{
			"endpoint": endpoint,
			"keys":     map[string]string{"p256dh": base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), "auth": base64.RawURLEncoding.EncodeToString(make([]byte, 16))},
		}))
		assert.Equal(t, http.StatusBadRequest, rec.Code, endpoint)
	}
	assert.Empty(t, p.pushState(t)["devices"])
}

// Scenario: the controls are the operator's. An agent on the socket is
// refused every one of them (the operator's CLI is not), and a federation
// peer reaches none of them through the peer view, nor changes the push
// block through the remote Config tab, unrestricted trust included.
func TestPush_ControlsAreTheOperatorsOnly(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	const conv = "push-agent-aaaa-bbbb-3333"
	fh.f.HaveConvWithTitle(conv, "worker")

	routes := []struct{ method, path string }{
		{http.MethodGet, "/v1/push"},
		{http.MethodPost, "/v1/push/key"},
		{http.MethodPost, "/v1/push/devices"},
		{http.MethodDelete, "/v1/push/devices/1"},
		{http.MethodPost, "/v1/push/config"},
		{http.MethodPost, "/v1/push/test"},
		{http.MethodPost, "/v1/push/reset-keys"},
	}
	for _, rt := range routes {
		rec := testharness.Serve(fh.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, rt.method, rt.path, map[string]any{}), conv))
		assert.Equal(t, http.StatusForbidden, rec.Code, "an agent is refused %s %s: %s", rt.method, rt.path, rec.Body.String())
	}
	rec := testharness.Serve(fh.f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/push", nil)))
	assert.Equal(t, http.StatusOK, rec.Code, "the operator's CLI reads the state: %s", rec.Body.String())
	rec = testharness.Serve(fh.f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/push/config", map[string]any{"categories": map[string]bool{"idle": true}})))
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	setFedTrustLevel(t, fh, db.FederationTrustUnrestricted)
	peer := agentd.PeerViewHandler(fh.peer.id.ID())
	for _, rt := range routes {
		path := "/api" + strings.TrimPrefix(rt.path, "/v1")
		rec := testharness.Serve(peer, testharness.JSONRequest(t, rt.method, path, map[string]any{}))
		assert.Equal(t, http.StatusForbidden, rec.Code, "a peer never reaches %s %s: %s", rt.method, path, rec.Body.String())
	}

	rec = testharness.Serve(peer, testharness.JSONRequest(t, http.MethodGet, "/api/config", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var view struct {
		Version string `json:"version"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &view))
	for path, value := range map[string]any{
		"notifications.push.content_free": true,
		"notifications.push.categories":   map[string]any{"idle": false},
	} {
		rec := testharness.Serve(peer, testharness.JSONRequest(t, http.MethodPatch, "/api/config", map[string]any{"version": view.Version, "set": map[string]any{path: value}}))
		assert.Equal(t, http.StatusForbidden, rec.Code, "no peer changes %s: %s", path, rec.Body.String())
	}
	cfg, err := config.Load()
	require.NoError(t, err)
	assert.False(t, cfg.Notifications.PushContentFree())
	assert.True(t, cfg.Notifications.PushCategoryOn("idle"), "the operator's choice stands")
}

// drainPushes runs the push sender until n pushes arrived (the question and
// message paths queue off the request goroutine), and returns them.
func drainPushes(t *testing.T, svc *fakePushService, n int) []fakePush {
	t.Helper()
	var got []fakePush
	require.Eventually(t, func() bool {
		agentd.WaitForBackgroundForTest()
		agentd.DrainPushForTest()
		got = append(got, svc.pushes()...)
		return len(got) >= n
	}, 10*time.Second, 5*time.Millisecond, "want %d push(es)", n)
	return got
}

// Scenario: what waits on the operator's answer reaches the phone by
// default; a plain notify-human message, often a status update, does not.
// A taskboard ask-operator question and a notify-human decision card are
// questions; the same notify-human without options is a message.
func TestPush_QuestionsForTheOperatorArePushedMessagesAreNot(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	p := newPushFixtureOn(t, h.f)
	p.svc.subscribe(t, p.dashboard, "phone")
	require.NoError(t, db.GrantAgentPermission(h.worker, agentd.PermHumanNotify, "test"))

	rec := h.call(h.worker, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "Lane tint"})
	task := h.task(rec, http.StatusCreated)
	code, body := h.askOn(h.worker, task.ID, map[string]any{"title": "Lane colour?", "body": "secret context",
		"options": []map[string]any{{"label": "Amber"}, {"label": "Teal"}}})
	require.Equal(t, http.StatusCreated, code, body)
	got := drainPushes(t, p.svc, 1)
	require.Len(t, got, 1)
	assert.Equal(t, "tclaude: Lane colour?", got[0].Payload.Title)
	assert.Contains(t, got[0].Payload.Body, "crew")
	assert.NotContains(t, got[0].Payload.Body, "secret context", "the question's context stays off the push")
	assert.Equal(t, "high", got[0].Header.Get("Urgency"))

	// A task made for a question asks it too, in a new window.
	p.advance(2 * time.Minute)
	rec = h.call(h.worker, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{
		"title": "Ship on Friday?", "question": map[string]any{"options": []map[string]any{{"label": "Yes"}, {"label": "No"}}},
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	got = drainPushes(t, p.svc, 1)
	assert.Equal(t, "tclaude: Ship on Friday?", got[0].Payload.Title, "a new window: the question itself")

	notifyHuman := func(body map[string]any) {
		rec := testharness.Serve(h.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/notify-human", body), h.worker))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	notifyHuman(map[string]any{"subject": "Status", "body": "CI is green."})
	agentd.WaitForBackgroundForTest()
	agentd.DrainPushForTest()
	assert.Empty(t, p.svc.pushes(), "a plain message is off for push by default")

	p.advance(2 * time.Minute)
	notifyHuman(map[string]any{"subject": "Layout?", "body": "Pick one.", "options": []string{"Tabs", "Sidebar"}})
	got = drainPushes(t, p.svc, 1)
	assert.Equal(t, "tclaude: Layout?", got[0].Payload.Title, "a decision card waits on the operator")
	assert.NotContains(t, got[0].Payload.Body, "Pick one.")
}

// Scenario: after `reset-keys`, a device still holding a subscription made
// with the old key is refused when it re-posts it; and a device whose push
// service keeps refusing every push (a VAPID mismatch) is removed after a
// few, instead of failing silently forever.
func TestPush_StaleKeyRefusedAndRejectingDevicePruned(t *testing.T) {
	p := newPushFixture(t)
	rec := testharness.Serve(p.dashboard, testharness.JSONRequest(t, http.MethodPost, "/api/push/key", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	other, err := webpush.NewVAPID()
	require.NoError(t, err)
	key, _ := ecdh.P256().GenerateKey(rand.Reader)
	rec = testharness.Serve(p.dashboard, testharness.JSONRequest(t, http.MethodPost, "/api/push/devices", map[string]any{
		"endpoint":               p.svc.srv.URL + "/push/stale",
		"keys":                   map[string]string{"p256dh": base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), "auth": base64.RawURLEncoding.EncodeToString(make([]byte, 16))},
		"application_server_key": other.PublicKey(),
	}))
	assert.Equal(t, http.StatusConflict, rec.Code, "a subscription for another key is refused: %s", rec.Body.String())

	id := p.svc.subscribe(t, p.dashboard, "phone")
	p.svc.answer("phone", http.StatusForbidden, "")
	for i := range 5 {
		_, err := db.GetPushSubscription(id)
		require.NoError(t, err, "still subscribed before refusal %d", i+1)
		rec := testharness.Serve(p.dashboard, testharness.JSONRequest(t, http.MethodPost, "/api/push/test", nil))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, 1, agentd.DrainPushForTest())
	}
	_, err = db.GetPushSubscription(id)
	assert.ErrorIs(t, err, db.ErrPushSubscriptionNotFound, "five refusals in a row remove the device")
}

// Scenario: an agent asks three questions in a row. The first alerts; the
// next two, within the minute, replace it in place (same tag and topic, so
// the phone does not buzz again) with how many wait. After a quiet minute
// the next question alerts afresh.
func TestPush_QuestionsCollapsePerAsker(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	p := newPushFixtureOn(t, h.f)
	p.svc.subscribe(t, p.dashboard, "phone")
	rec := h.call(h.worker, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "Lane tint"})
	task := h.task(rec, http.StatusCreated)
	ask := func(title string) fakePush {
		code, body := h.askOn(h.worker, task.ID, map[string]any{"title": title, "options": []map[string]any{{"label": "Yes"}, {"label": "No"}}})
		require.Equal(t, http.StatusCreated, code, body)
		return drainPushes(t, p.svc, 1)[0]
	}
	first := ask("Amber?")
	assert.Equal(t, "tclaude: Amber?", first.Payload.Title)
	p.advance(20 * time.Second)
	second := ask("Teal?")
	assert.Equal(t, first.Payload.Tag, second.Payload.Tag, "a repeat replaces the first notification")
	assert.Equal(t, first.Header.Get("Topic"), second.Header.Get("Topic"))
	assert.Equal(t, "tclaude: 2 questions from tb-worker-2222 · crew", second.Payload.Title)
	assert.Equal(t, "Latest: Teal?", second.Payload.Body)
	p.advance(50 * time.Second)
	third := ask("Mauve?")
	assert.Equal(t, first.Payload.Tag, third.Payload.Tag, "the window runs on from the latest question")
	assert.Contains(t, third.Payload.Title, "3 questions")

	p.advance(61 * time.Second)
	fresh := ask("Grey?")
	assert.NotEqual(t, first.Payload.Tag, fresh.Payload.Tag, "after a quiet minute a question alerts afresh")
	assert.Equal(t, "tclaude: Grey?", fresh.Payload.Title)
}

// Scenario: with browser delivery on, a notification goes to the open-tab
// queue and is pushed; the push carries the queue's id, so a device that
// gets both (a backgrounded tab still polling) shows one banner.
func TestPush_CarriesTheBrowserQueueID(t *testing.T) {
	p := newPushFixture(t)
	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.Notifications.Delivery = config.NotifyDeliveryBrowser
	require.NoError(t, config.Save(cfg))
	p.svc.subscribe(t, p.dashboard, "phone")
	const conv = "push-bn-aaaa-bbbb-4444"
	p.f.HaveConvWithTitle(conv, "worker")

	notify.OnStateTransition("sess-bn-1", conv, "working", "awaiting_permission", p.f.TestCwd("proj"), "worker", "claude")
	agentd.DrainPushForTest()
	got := p.svc.pushes()
	require.Len(t, got, 1)
	banners, _, err := db.ListBrowserNotificationsSince(0)
	require.NoError(t, err)
	require.Len(t, banners, 1)
	assert.Equal(t, banners[0].ID, got[0].Payload.BN, "the push names the open tab's banner")
}

// Scenario: the browser rotates a device's subscription
// (pushsubscriptionchange). The refresh naming the old endpoint moves the
// device onto the new one: same device, pushes reach the new endpoint. A
// refresh is answered by "is this device known" before any key check, so a
// device the operator removed gets 404, even when its subscription was made
// for an old key (it would otherwise get a 409 and subscribe anew).
func TestPush_RefreshMovesARotatedDeviceAndNeverRevivesARemovedOne(t *testing.T) {
	p := newPushFixture(t)
	id := p.svc.subscribe(t, p.dashboard, "phone")

	key, err := ecdh.P256().GenerateKey(rand.Reader)
	require.NoError(t, err)
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	p.svc.mu.Lock()
	p.svc.devices["phone-rotated"] = &fakeDevice{name: "phone-rotated", key: key, auth: auth}
	p.svc.mu.Unlock()
	refresh := func(endpoint, old, appKey string) *httptest.ResponseRecorder {
		return testharness.Serve(p.dashboard, testharness.JSONRequest(t, http.MethodPost, "/api/push/devices", map[string]any{
			"endpoint":               p.svc.srv.URL + "/push/" + endpoint,
			"keys":                   map[string]string{"p256dh": base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), "auth": base64.RawURLEncoding.EncodeToString(auth)},
			"refresh":                true,
			"old_endpoint":           old,
			"application_server_key": appKey,
		}))
	}

	rec := refresh("phone-rotated", p.svc.srv.URL+"/push/phone", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		Device struct {
			ID int64 `json:"id"`
		} `json:"device"`
		New bool `json:"new"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, id, out.Device.ID, "the same device, moved")
	assert.False(t, out.New)
	rec = testharness.Serve(p.dashboard, testharness.JSONRequest(t, http.MethodPost, "/api/push/test", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := drainPushes(t, p.svc, 1)
	assert.Equal(t, "phone-rotated", got[0].Device)

	rec = testharness.Serve(p.dashboard, testharness.JSONRequest(t, http.MethodDelete, "/api/push/devices/"+strconv.FormatInt(id, 10), nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	other, err := webpush.NewVAPID()
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, refresh("phone-rotated", "", "").Code, "a removed device stays removed")
	assert.Equal(t, http.StatusNotFound, refresh("phone-rotated", "", other.PublicKey()).Code,
		"an old-key subscription of a removed device gets 404, not the 409 that re-subscribes")
	assert.Equal(t, http.StatusNotFound, refresh("phone-rotated", p.svc.srv.URL+"/push/phone", "").Code,
		"naming a removed old endpoint does not revive it")
}
