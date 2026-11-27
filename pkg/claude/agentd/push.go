package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/notify"
	"github.com/tofutools/tclaude/pkg/claude/common/webpush"
)

// Web Push to the operator's devices (tk_2c9055c6). notify queues a push in
// db push_outbox for each subscribed device when a notification's category
// is on for push; this file sends them, and serves the operator's controls:
// subscribe a device, list and remove devices, the categories, the
// content-free mode, a test push, and resetting the key.
//
// Every control is the operator's: the dashboard routes need the dashboard
// session and stay with this node in the peer view (peer_view_routes.go),
// and their /v1 twins, which the CLI uses, refuse any agent (requireHuman).
// Agents get no way to push: the only input to the outbox is the existing
// operator-facing notifications.

// pushSubject is the VAPID contact claim. A generic project URL: the
// operator's own address never goes to a push service.
const pushSubject = "https://github.com/tofutools/tclaude"

// pushTick is how often the sender looks for pushes other processes (hook
// callbacks) queued; pushes this process queues wake it at once.
const pushTick = 2 * time.Second

// pushBatch caps one pass over the outbox.
const pushBatch = 50

// pushRetryBase is the first retry delay after a busy or failing push
// service; it doubles per attempt.
const pushRetryBase = 30 * time.Second

// pushRetryMax caps a push service's Retry-After.
const pushRetryMax = 5 * time.Minute

// pushServiceHosts are the push services a subscription may point at
// (a host, or a domain its host ends in). A subscription endpoint comes from
// the operator's browser, but agentd posts to it on its own from then on,
// so it must be a push service and not an address of the operator's
// network. These are the services the browsers use: Mozilla (Firefox),
// Google FCM (Chrome, Edge on Android, Samsung, Brave, Opera), Apple
// (Safari, iOS home-screen apps) and Microsoft WNS (Edge on Windows).
var pushServiceHosts = []string{
	"push.services.mozilla.com",
	"fcm.googleapis.com",
	"android.googleapis.com",
	"push.apple.com",
	"notify.windows.com",
}

// pushHTTPClient sends pushes; tests swap it for one that trusts their fake
// push service. It never follows a redirect: the allowlist vouches for the
// endpoint's host only, so a 3xx is a refused push.
var pushHTTPClient = noRedirects(&http.Client{Timeout: 10 * time.Second})

// noRedirects makes c answer a redirect with the 3xx itself.
func noRedirects(c *http.Client) *http.Client {
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

// pushPruneRejections is how many refused pushes in a row (a 4xx that is
// not 404/410, such as a VAPID mismatch after a key reset) remove a device:
// it will never take a push again.
const pushPruneRejections = 5

// pushClock is the sender's clock; tests pin it.
var pushClock = time.Now

// pushEndpointAllowed reports whether endpoint is an https URL on a known
// push service.
func pushEndpointAllowed(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return errors.New("endpoint must be an https URL")
	}
	host := strings.ToLower(u.Hostname())
	for _, h := range pushServiceHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return nil
		}
	}
	return fmt.Errorf("%s is not a known push service (%s)", host, strings.Join(pushServiceHosts, ", "))
}

// pushVAPID is this node's VAPID key, made and stored on first use.
func pushVAPID() (*webpush.VAPID, error) {
	raw, err := db.PushVAPIDKey(func() ([]byte, error) {
		v, err := webpush.NewVAPID()
		if err != nil {
			return nil, err
		}
		return v.MarshalPrivate()
	}, pushClock())
	if err != nil {
		return nil, err
	}
	return webpush.ParseVAPID(raw)
}

// pushSender drains the outbox. One per daemon.
type pushSender struct {
	wake chan struct{}
	mu   sync.Mutex // one drain at a time
}

var thePushSender = &pushSender{wake: make(chan struct{}, 1)}

// poke wakes the sender without blocking.
func (s *pushSender) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// startPushSender runs the sender until stop closes.
func startPushSender(stop <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-stop
		cancel()
	}()
	notify.OnPushQueued = thePushSender.poke
	go func() {
		t := time.NewTicker(pushTick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			case <-thePushSender.wake:
			}
			thePushSender.drain(ctx)
		}
	}()
}

// drain sends every push due now and returns how many it handled.
func (s *pushSender) drain(ctx context.Context) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := pushClock()
	jobs, err := db.DuePushJobs(now, pushBatch)
	if err != nil {
		slog.Warn("push: reading the outbox", "err", err)
		return 0
	}
	if len(jobs) == 0 {
		return 0
	}
	raw, err := db.PushVAPIDKeyIfAny()
	if err != nil || raw == nil {
		slog.Warn("push: no VAPID key for queued pushes", "err", err)
		return 0
	}
	vapid, err := webpush.ParseVAPID(raw)
	if err != nil {
		slog.Warn("push: reading the VAPID key", "err", err)
		return 0
	}
	// Devices are sent to in parallel, each one's pushes in order, so a
	// device whose push service hangs delays only itself. A device that
	// failed is left for the next pass.
	byDevice := map[int64][]db.PushJob{}
	var order []int64
	for _, job := range jobs {
		if _, ok := byDevice[job.Sub.ID]; !ok {
			order = append(order, job.Sub.ID)
		}
		byDevice[job.Sub.ID] = append(byDevice[job.Sub.ID], job)
	}
	var wg sync.WaitGroup
	for _, id := range order {
		wg.Add(1)
		go func(device []db.PushJob) {
			defer wg.Done()
			for _, job := range device {
				if !sendPushJob(ctx, vapid, job, now) {
					return
				}
			}
		}(byDevice[id])
	}
	wg.Wait()
	return len(jobs)
}

// sendPushJob sends one push and records what came of it. It reports
// whether the device took it, so the rest of its pushes may follow.
func sendPushJob(ctx context.Context, vapid *webpush.VAPID, job db.PushJob, now time.Time) bool {
	sub := webpush.Subscription{Endpoint: job.Sub.Endpoint, P256DH: job.Sub.P256DH, Auth: job.Sub.Auth}
	if err := pushEndpointAllowed(sub.Endpoint); err != nil {
		_, _ = db.PushFailed(job, err.Error(), nil)
		return false
	}
	// What is left of the push's time, so a retried push does not outlive
	// the outbox's bound at the push service either.
	ttl := min(db.PushOutboxTTL-now.Sub(job.CreatedAt), db.PushOutboxTTL)
	if ttl < time.Second {
		ttl = time.Second
	}
	req, err := webpush.NewRequest(ctx, sub, job.Payload, vapid, webpush.Options{
		TTL: ttl, Urgency: job.Urgency, Topic: job.Tag, Subject: pushSubject,
	}, now)
	if err != nil {
		_, _ = db.PushFailed(job, err.Error(), nil)
		return false
	}
	resp, err := pushHTTPClient.Do(req)
	if err != nil {
		// The error names the endpoint, whose path is the subscription's
		// secret: keep only the cause, which the device list shows.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		retry := now.Add(pushRetryBase << job.Attempts)
		_, _ = db.PushFailed(job, "push service unreachable: "+err.Error(), &retry)
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	switch webpush.Classify(resp.StatusCode) {
	case webpush.Delivered:
		_ = db.PushDelivered(job, now)
		return true
	case webpush.Gone:
		// The browser dropped the subscription (unsubscribed, app removed,
		// permission revoked): forget the device.
		slog.Info("push: device unsubscribed at its push service, removing it", "device", job.Sub.ID, "label", job.Sub.Label, "status", resp.StatusCode)
		_ = db.DeletePushSubscription(job.Sub.ID)
	case webpush.Retry:
		retry := now.Add(retryAfter(resp.Header.Get("Retry-After"), now, pushRetryBase<<job.Attempts))
		_, _ = db.PushFailed(job, fmt.Sprintf("push service answered %d", resp.StatusCode), &retry)
	default:
		slog.Warn("push: push service refused a push", "device", job.Sub.ID, "status", resp.StatusCode)
		_, _ = db.PushFailed(job, fmt.Sprintf("push service refused the push (%d)", resp.StatusCode), nil)
		if job.Sub.Failures+1 >= pushPruneRejections {
			slog.Info("push: device refused every recent push, removing it", "device", job.Sub.ID, "label", job.Sub.Label)
			_ = db.DeletePushSubscription(job.Sub.ID)
		}
	}
	return false
}

// retryAfter reads a Retry-After header (seconds or an HTTP date), capped
// at pushRetryMax; fallback when absent or unreadable.
func retryAfter(h string, now time.Time, fallback time.Duration) time.Duration {
	d := fallback
	if secs, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && secs >= 0 {
		d = time.Duration(secs) * time.Second
	} else if t, err := http.ParseTime(h); err == nil {
		d = t.Sub(now)
	}
	if d < time.Second {
		d = time.Second
	}
	return min(d, pushRetryMax)
}

// pushState is what the controls show: the master switch, each category,
// the content-free mode, the devices, and the public key once there is one.
func pushState(cfg *config.Config) (map[string]any, error) {
	var n *config.NotificationConfig
	if cfg != nil {
		n = cfg.Notifications
	}
	cats := make(map[string]bool, len(config.PushCategories))
	for _, c := range config.PushCategories {
		cats[c] = n.PushCategoryOn(c)
	}
	devices, err := db.ListPushSubscriptions()
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"enabled":        n != nil && n.Enabled,
		"categories":     cats,
		"category_order": config.PushCategories,
		"content_free":   n.PushContentFree(),
		"devices":        devices,
	}
	raw, err := db.PushVAPIDKeyIfAny()
	if err != nil {
		return nil, err
	}
	if raw != nil {
		v, err := webpush.ParseVAPID(raw)
		if err != nil {
			return nil, err
		}
		out["public_key"] = v.PublicKey()
	}
	return out, nil
}

// pushRoute is one control, shared by the dashboard and the /v1 twin.
type pushRoute func(w http.ResponseWriter, r *http.Request)

// pushDashboard wraps a control for the dashboard session.
func pushDashboard(h pushRoute) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkDashboardAuth(w, r) {
			return
		}
		h(w, r)
	}
}

// pushSocket wraps a control for the agent socket: the operator's CLI only.
func pushSocket(h pushRoute) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireHuman(w, r, "manage push notifications") {
			return
		}
		h(w, r)
	}
}

// registerPushRoutes adds the dashboard controls (/api/push/…) to mux.
func registerPushRoutes(mux *http.ServeMux) {
	for pattern, h := range pushRoutes("/api/push") {
		mux.HandleFunc(pattern, pushDashboard(h))
	}
}

// registerPushSocketRoutes adds the /v1/push/… twins to the socket mux.
func registerPushSocketRoutes(mux *http.ServeMux) {
	for pattern, h := range pushRoutes("/v1/push") {
		mux.HandleFunc(pattern, pushSocket(h))
	}
}

// pushRoutes are the controls under prefix.
func pushRoutes(prefix string) map[string]pushRoute {
	return map[string]pushRoute{
		"GET " + prefix:                      handlePushState,
		"POST " + prefix + "/key":            handlePushKey,
		"POST " + prefix + "/devices":        handlePushSubscribe,
		"DELETE " + prefix + "/devices/{id}": handlePushRemove,
		"POST " + prefix + "/config":         handlePushConfig,
		"POST " + prefix + "/test":           handlePushTest,
		"POST " + prefix + "/reset-keys":     handlePushResetKeys,
	}
}

func writePushState(w http.ResponseWriter, status int, extra map[string]any) {
	cfg, err := config.Load()
	if err != nil {
		http.Error(w, "load config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	st, err := pushState(cfg)
	if err != nil {
		http.Error(w, "read push state: "+err.Error(), http.StatusInternalServerError)
		return
	}
	for k, v := range extra {
		st[k] = v
	}
	writeJSON(w, status, st)
}

// GET …/push → the state.
func handlePushState(w http.ResponseWriter, _ *http.Request) {
	writePushState(w, http.StatusOK, nil)
}

// POST …/push/key → {public_key}: the key a browser subscribes with, made
// on the first call.
func handlePushKey(w http.ResponseWriter, _ *http.Request) {
	v, err := pushVAPID()
	if err != nil {
		http.Error(w, "push key: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"public_key": v.PublicKey()})
}

// pushSubscribeBody is a browser's PushSubscription.toJSON(), plus an
// optional device name.
type pushSubscribeBody struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256DH string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
	Label string `json:"label"`
	// ApplicationServerKey is the key the browser subscribed with
	// (PushSubscription.options.applicationServerKey, base64url). When
	// given, it must be this node's current key: a subscription made for an
	// old one (before a reset-keys) can never be pushed to.
	ApplicationServerKey string `json:"application_server_key"`
	// Refresh updates a device agentd already knows and never adds one:
	// the app re-posts its subscription on every open, and a device the
	// operator removed (from another device, or the CLI) must stay removed.
	// An unknown endpoint is a 404, on which the app drops its subscription.
	Refresh bool `json:"refresh"`
	// OldEndpoint, with Refresh, is the subscription this one replaces: the
	// browser rotated it (pushsubscriptionchange) or the app re-subscribed
	// for a new key. A known old endpoint is swapped for the new one in
	// place, keeping the device's id and label.
	OldEndpoint string `json:"old_endpoint"`
}

// POST …/push/devices → {device, new, …state}: subscribe (or refresh) a
// device. A device re-posts on every app open, which keeps its keys
// current.
func handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	var body pushSubscribeBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&body); err != nil {
		http.Error(w, "expected a push subscription {endpoint, keys{p256dh, auth}, label?}", http.StatusBadRequest)
		return
	}
	sub := webpush.Subscription{Endpoint: body.Endpoint, P256DH: body.Keys.P256DH, Auth: body.Keys.Auth}
	if err := sub.Validate(); err != nil {
		http.Error(w, "push subscription: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := pushEndpointAllowed(sub.Endpoint); err != nil {
		http.Error(w, "push subscription: "+err.Error(), http.StatusBadRequest)
		return
	}
	// A refresh is checked against the known devices first, before the
	// key: a device removed (or dropped by a reset-keys) gets its 404 and
	// stays removed, rather than a 409 that would have it subscribe anew.
	replaces := ""
	if body.Refresh {
		known, err := pushEndpointKnown(sub.Endpoint)
		if err == nil && !known && body.OldEndpoint != "" && body.OldEndpoint != sub.Endpoint {
			if known, err = pushEndpointKnown(body.OldEndpoint); known {
				replaces = body.OldEndpoint
			}
		}
		if err != nil {
			http.Error(w, "read push subscription: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if !known {
			http.Error(w, "this device is not subscribed", http.StatusNotFound)
			return
		}
	}
	raw, err := db.PushVAPIDKeyIfAny()
	if err != nil || raw == nil {
		http.Error(w, "no push key yet: POST …/push/key first and subscribe with it", http.StatusConflict)
		return
	}
	if k := strings.TrimRight(body.ApplicationServerKey, "="); k != "" {
		if v, err := webpush.ParseVAPID(raw); err != nil || v.PublicKey() != k {
			http.Error(w, "the subscription was made with another push key: unsubscribe and subscribe again with the current one", http.StatusConflict)
			return
		}
	}
	label := strings.TrimSpace(body.Label)
	if label == "" {
		label = deviceLabel(r.UserAgent())
	}
	if len(label) > 200 {
		label = label[:200]
	}
	if replaces != "" {
		dev, err := db.ReplacePushEndpoint(replaces, sub.Endpoint, sub.P256DH, sub.Auth, pushClock())
		if err != nil {
			http.Error(w, "store push subscription: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writePushState(w, http.StatusOK, map[string]any{"device": dev, "new": false})
		return
	}
	dev, isNew, err := db.UpsertPushSubscription(sub.Endpoint, sub.P256DH, sub.Auth, label, pushClock())
	if err != nil {
		http.Error(w, "store push subscription: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if isNew {
		slog.Info("push: device subscribed", "device", dev.ID, "label", dev.Label)
	}
	writePushState(w, http.StatusOK, map[string]any{"device": dev, "new": isNew})
}

// pushEndpointKnown reports whether a device with that endpoint is
// subscribed.
func pushEndpointKnown(endpoint string) (bool, error) {
	_, err := db.PushSubscriptionByEndpoint(endpoint)
	if errors.Is(err, db.ErrPushSubscriptionNotFound) {
		return false, nil
	}
	return err == nil, err
}

// deviceLabel names a device from its browser's User-Agent: "iPhone ·
// Safari", "Android · Chrome", "Linux · Firefox".
func deviceLabel(ua string) string {
	osName := "Device"
	for _, c := range []struct{ match, name string }{
		{"iPhone", "iPhone"}, {"iPad", "iPad"}, {"Android", "Android"},
		{"Mac OS X", "Mac"}, {"Windows", "Windows"}, {"CrOS", "ChromeOS"}, {"Linux", "Linux"},
	} {
		if strings.Contains(ua, c.match) {
			osName = c.name
			break
		}
	}
	browser := ""
	for _, c := range []struct{ match, name string }{
		{"Edg/", "Edge"}, {"Firefox/", "Firefox"}, {"FxiOS", "Firefox"}, {"CriOS", "Chrome"},
		{"Chrome/", "Chrome"}, {"Safari/", "Safari"},
	} {
		if strings.Contains(ua, c.match) {
			browser = c.name
			break
		}
	}
	if browser == "" {
		return osName
	}
	return osName + " · " + browser
}

// DELETE …/push/devices/{id} → the state: forget a device.
func handlePushRemove(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "device id must be a positive integer", http.StatusBadRequest)
		return
	}
	if err := db.DeletePushSubscription(id); errors.Is(err, db.ErrPushSubscriptionNotFound) {
		http.Error(w, "no such device", http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, "remove device: "+err.Error(), http.StatusInternalServerError)
		return
	}
	slog.Info("push: device removed", "device", id)
	writePushState(w, http.StatusOK, nil)
}

// POST …/push/config {categories?{cat:bool}, content_free?} → the state.
func handlePushConfig(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Categories  map[string]bool `json:"categories"`
		ContentFree *bool           `json:"content_free"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&body); err != nil || (body.Categories == nil && body.ContentFree == nil) {
		http.Error(w, "expected {categories?: {category: bool}, content_free?: bool}", http.StatusBadRequest)
		return
	}
	for k := range body.Categories {
		if !config.IsPushCategory(k) {
			http.Error(w, "unknown push category: "+k+" (one of "+strings.Join(config.PushCategories, ", ")+")", http.StatusBadRequest)
			return
		}
	}
	_, err := config.Update(func(cfg *config.Config, loadErr error) error {
		if loadErr != nil {
			return errNotifConfigMalformed
		}
		if cfg.Notifications == nil {
			cfg.Notifications = config.DefaultConfig().Notifications
		}
		for c, on := range body.Categories {
			cfg.Notifications.SetPushCategory(c, on)
		}
		if body.ContentFree != nil {
			cfg.Notifications.SetPushContentFree(*body.ContentFree)
		}
		return nil
	})
	if errors.Is(err, errNotifConfigMalformed) {
		http.Error(w, "config.json on disk is corrupt — fix or replace it via the Config tab before changing notifications", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "save config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writePushState(w, http.StatusOK, nil)
}

// POST …/push/test {device?} → {queued}: a fixed test message to one device
// or all, whatever the categories say. It goes out at once.
func handlePushTest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Device int64 `json:"device"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "expected {device?: id}", http.StatusBadRequest)
			return
		}
	}
	if body.Device != 0 {
		if _, err := db.GetPushSubscription(body.Device); errors.Is(err, db.ErrPushSubscriptionNotFound) {
			http.Error(w, "no such device", http.StatusNotFound)
			return
		}
	}
	contentFree := false
	if cfg, err := config.Load(); err == nil {
		contentFree = cfg.Notifications.PushContentFree()
	}
	queued, err := notify.QueueTestPush(body.Device, contentFree)
	if err != nil {
		http.Error(w, "queue test push: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if queued == 0 {
		http.Error(w, "no device is subscribed", http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"queued": queued})
}

// POST …/push/reset-keys → {removed, …state}: a new VAPID key on the next
// subscribe. Every subscription was made for the old key, so every device
// is removed and must subscribe again.
func handlePushResetKeys(w http.ResponseWriter, _ *http.Request) {
	n, err := db.ResetPushVAPID()
	if err != nil {
		http.Error(w, "reset push keys: "+err.Error(), http.StatusInternalServerError)
		return
	}
	slog.Info("push: key reset, devices removed", "removed", n)
	writePushState(w, http.StatusOK, map[string]any{"removed": n})
}
