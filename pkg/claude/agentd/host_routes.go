package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"runtime"
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
}

var hostRouteLeases = struct {
	sync.Mutex
	items map[string]*hostRouteLease
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
	if len(hostRouteLeases.items) >= 128 {
		hostRouteLeases.Unlock()
		cancel()
		adapter.Close()
		return nil, errors.New("host route lease limit reached")
	}
	// Publish the immutable authority before accepting connections. Keep the
	// lock until Endpoint is initialized so snapshots cannot race this write.
	hostRouteLeases.items[id] = lease
	endpoint, err := adapter.OpenHost(ctx, routeadapter.Consumer{LeaseID: id, RouteID: route.ID,
		AgentID: lease.consumer.AgentID, ConvID: id, LaunchGeneration: id, GroupGeneration: route.GroupGeneration}, port)
	lease.Endpoint = endpoint
	hostRouteLeases.Unlock()
	if err == nil {
		err = db.RecordHostRouteAudit("host-open", route.GroupID, route.ID, id, endpoint)
	}
	if err != nil {
		closeHostRoute(id)
		return nil, err
	}
	// Recheck after allocating the endpoint: withdrawal may race the bind.
	if err := authorizeHostRouteConsumer(lease.consumer); err != nil {
		closeHostRoute(id)
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
				if authorizeHostRouteConsumer(lease.consumer) != nil {
					closeHostRoute(id)
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

func closeHostRoute(id string) {
	hostRouteLeases.Lock()
	lease := hostRouteLeases.items[id]
	delete(hostRouteLeases.items, id)
	hostRouteLeases.Unlock()
	if lease == nil {
		return
	}
	lease.cancel()
	lease.adapter.Close()
	if err := db.RecordHostRouteAudit("host-close", lease.groupID, lease.RouteID, id, lease.Endpoint); err != nil {
		slog.Warn("host route close audit failed", "lease", id, "error", err)
	}
}

func closeAllHostRoutes() {
	hostRouteLeases.Lock()
	ids := make([]string, 0, len(hostRouteLeases.items))
	for id := range hostRouteLeases.items {
		ids = append(ids, id)
	}
	hostRouteLeases.Unlock()
	for _, id := range ids {
		closeHostRoute(id)
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
	closeHostRoute(r.PathValue("lease"))
	w.WriteHeader(http.StatusNoContent)
}
