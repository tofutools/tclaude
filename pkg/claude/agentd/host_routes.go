package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/routeadapter"
	"github.com/tofutools/tclaude/pkg/claude/routebroker"
)

const hostRouteLeasePrefix = "hlease_"

type hostRouteLease struct {
	ID           string    `json:"id"`
	RouteID      string    `json:"route_id"`
	ConsumerKind string    `json:"consumer_kind"`
	Endpoint     string    `json:"endpoint"`
	OpenedAt     time.Time `json:"opened_at"`
	groupID      int64
	publisher    routebroker.PublisherAuth
	consumer     routebroker.ConsumerAuth
	adapter      *routeadapter.Adapter
	cancel       context.CancelFunc
	audited      bool
}

var hostRouteLeases = struct {
	sync.Mutex
	items   map[string]*hostRouteLease
	closing bool
}{items: make(map[string]*hostRouteLease)}

// Only mounted on the cookie-authenticated dashboard, never on buildMux's
// agent-reachable Unix socket. requireHuman is a second, identity-based gate.
func registerDashboardHostRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/routes/{route}/host", dashboardRolesRoute(handleHostRouteOpen))
	mux.HandleFunc("DELETE /api/routes/host/{lease}", dashboardRolesRoute(handleHostRouteClose))
}

func handleHostRouteOpen(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "open a route on the host") {
		return
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		writeRouteError(w, http.StatusNotImplemented, "route_platform", "host routes require Linux or macOS")
		return
	}
	var body struct {
		Port int `json:"port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Port < 0 || body.Port > 65535 {
		writeRouteError(w, http.StatusBadRequest, "route_port", "JSON body with optional port (0–65535) required")
		return
	}
	route, err := db.GetAgentRoute(r.PathValue("route"))
	if err != nil || route == nil {
		writeRouteError(w, http.StatusNotFound, "route_not_found", "no such route")
		return
	}
	lease, err := openHostRoute(route, body.Port)
	if err != nil {
		writeRouteError(w, http.StatusConflict, "route_host", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, lease)
}

func openHostRoute(route *db.AgentRoute, port int) (*hostRouteLease, error) {
	// Remote routes have separate peer authority; this surface exposes local
	// publishers only, and must never borrow a federation proxy's identity.
	if mirror, err := db.GetFederationRouteMirror(route.ID); err != nil || mirror != nil {
		return nil, errors.New("host consumption requires a local publisher")
	}
	publisher := routebroker.PublisherAuth{RouteID: route.ID, AgentID: route.PublisherAgentID,
		ConvID: route.PublisherConvID, LaunchGeneration: route.PublisherLaunchGeneration, GroupGeneration: route.GroupGeneration}
	if err := (databaseRouteAuthority{}).AuthorizePublisher(context.Background(), publisher); err != nil {
		return nil, err
	}
	if !GroupRouteBroker().PublisherAttached(route.ID) {
		if runtime.GOOS == "linux" {
			enabled, err := db.IsAgentGroupRouteEnabled(route.GroupID, PermRoutesPublish, PermRoutesConsume)
			if err != nil {
				return nil, err
			}
			if !enabled {
				return nil, fmt.Errorf("publisher data channel is not attached: enable routes.publish in group %q's group permissions, then relaunch the publisher and republish the route", route.GroupName)
			}
			if !routeHelperCapabilityActive(route.PublisherAgentID, route.PublisherConvID, route.PublisherLaunchGeneration) {
				return nil, errors.New("publisher data channel is not attached: relaunch the publisher to install a current Linux route helper, then republish the route")
			}
		}
		return nil, errors.New("publisher data channel is not attached: wait for the publisher helper to connect and retry")
	}
	adapter, err := routeadapter.New(GroupRouteBroker(), nil)
	if err != nil {
		return nil, err
	}
	adapter.SetFlowWindow(routeFlowWindow)
	ctx, cancel := context.WithCancel(context.Background())
	id := hostRouteLeasePrefix + strings.ReplaceAll(uuid.NewString(), "-", "")
	lease := &hostRouteLease{ID: id, RouteID: route.ID, ConsumerKind: "host", OpenedAt: time.Now(),
		groupID: route.GroupID, publisher: publisher, adapter: adapter, cancel: cancel,
		consumer: routebroker.ConsumerAuth{LeaseID: id, RouteID: route.ID, AgentID: "operator-host", ConvID: id, LaunchGeneration: id, GroupGeneration: route.GroupGeneration}}
	hostRouteLeases.Lock()
	if hostRouteLeases.closing {
		hostRouteLeases.Unlock()
		cancel()
		adapter.Close()
		return nil, errors.New("host route runtime is shutting down")
	}
	if len(hostRouteLeases.items) >= 128 {
		hostRouteLeases.Unlock()
		cancel()
		adapter.Close()
		return nil, errors.New("host route lease limit reached")
	}
	// Publish the immutable authority before accepting connections. Keep the
	// lock until Endpoint is initialized so snapshots cannot race this write.
	hostRouteLeases.items[id] = lease
	listener, err := listenHostRoute(port, runtime.GOOS)
	if err == nil {
		lease.Endpoint, err = adapter.OpenHost(ctx, routeadapter.Consumer{LeaseID: id, RouteID: route.ID,
			AgentID: lease.consumer.AgentID, ConvID: id, LaunchGeneration: id, GroupGeneration: route.GroupGeneration}, listener)
	}
	if err == nil {
		err = db.RecordHostRouteAudit("host-open", route.GroupID, route.ID, id, lease.Endpoint)
		lease.audited = err == nil
	}
	hostRouteLeases.Unlock()
	if err != nil {
		closeHostRoute(id, "open failed")
		return nil, err
	}
	// Recheck after allocating the endpoint: withdrawal may race the bind.
	if err := authorizeHostRouteConsumer(lease.consumer); err != nil {
		closeHostRoute(id, err.Error())
		return nil, err
	}
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := authorizeHostRouteConsumer(lease.consumer); err != nil {
					closeHostRoute(id, err.Error())
					return
				}
			}
		}
	}()
	return lease, nil
}

func authorizeHostRouteConsumer(auth routebroker.ConsumerAuth) error {
	hostRouteLeases.Lock()
	lease := hostRouteLeases.items[auth.LeaseID]
	hostRouteLeases.Unlock()
	if lease == nil {
		return errors.New("host lease is closed")
	}
	expected := lease.consumer
	if auth.RouteID != expected.RouteID || auth.AgentID != expected.AgentID || auth.ConvID != expected.ConvID || auth.LaunchGeneration != expected.LaunchGeneration || auth.GroupGeneration != expected.GroupGeneration {
		return errors.New("host lease identity does not match")
	}
	return (databaseRouteAuthority{}).AuthorizePublisher(context.Background(), lease.publisher)
}

func closeHostRoute(id, reason string) {
	hostRouteLeases.Lock()
	lease := hostRouteLeases.items[id]
	delete(hostRouteLeases.items, id)
	hostRouteLeases.Unlock()
	if lease == nil {
		return
	}
	lease.cancel()
	lease.adapter.Close()
	if lease.audited {
		if err := db.RecordHostRouteAudit("host-close", lease.groupID, lease.RouteID, id, reason+": "+lease.Endpoint); err != nil {
			slog.Warn("host route close audit failed", "lease", id, "error", err)
		}
	}
}

// shutdownHostRoutes stops admission under the same lock that registers a
// listener, then drains every lease that was admitted before the gate closed.
func shutdownHostRoutes() {
	hostRouteLeases.Lock()
	hostRouteLeases.closing = true
	hostRouteLeases.Unlock()
	closeAllHostRoutes()
}

func closeAllHostRoutes() {
	hostRouteLeases.Lock()
	ids := make([]string, 0, len(hostRouteLeases.items))
	for id := range hostRouteLeases.items {
		ids = append(ids, id)
	}
	hostRouteLeases.Unlock()
	for _, id := range ids {
		closeHostRoute(id, "daemon shutdown")
	}
}

func hostRoutesForRoute(routeID string) []hostRouteLease {
	hostRouteLeases.Lock()
	defer hostRouteLeases.Unlock()
	out := []hostRouteLease{}
	for _, lease := range hostRouteLeases.items {
		if lease.RouteID == routeID {
			out = append(out, *lease)
		}
	}
	return out
}

func handleHostRouteClose(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "close a host route") {
		return
	}
	closeHostRoute(r.PathValue("lease"), "operator requested close")
	w.WriteHeader(http.StatusNoContent)
}

// On Darwin, idle sandbox slots remain allowed in Seatbelt even though their
// reservation sockets have been released. Never bind a host endpoint there.
func listenHostRoute(port int, platform string) (net.Listener, error) {
	bind := func() (net.Listener, error) {
		return net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	}
	if platform != "darwin" {
		return bind()
	}
	var listener net.Listener
	var rejected []net.Listener
	defer func() {
		for _, l := range rejected {
			_ = l.Close()
		}
	}()
	err := db.WithDarwinRouteSlotClaims(func(claimed map[int]bool) error {
		for range 16 {
			candidate, err := bind()
			if err != nil {
				return err
			}
			if !claimed[candidate.Addr().(*net.TCPAddr).Port] {
				listener = candidate
				return nil
			}
			rejected = append(rejected, candidate)
			if port != 0 {
				return errors.New("host port is reserved by a sandbox route launch")
			}
		}
		return errors.New("could not allocate a host port outside sandbox route slots")
	})
	if err != nil && listener != nil {
		_ = listener.Close()
		listener = nil
	}
	return listener, err
}
