//go:build linux

package agentd

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/routeadapter"
	"github.com/tofutools/tclaude/pkg/claude/routebroker"
)

const routeChannelPath = "/v1/routes/channel"

func registerV1RouteAdapter(mux *http.ServeMux) {
	mux.HandleFunc("POST "+routeChannelPath, handleRouteChannel)
}

// handleRouteChannel is the only production entry point from a namespace
// helper into the M2 broker. The HTTP request is authenticated and generation
// checked before hijacking; after the 101 response the connection carries only
// routebroker frames.
func handleRouteChannel(w http.ResponseWriter, r *http.Request) {
	convID, ok := requireAgent(w, r)
	if !ok {
		return
	}
	role := strings.TrimSpace(r.Header.Get("X-Tclaude-Route-Role"))
	routeID := strings.TrimSpace(r.Header.Get("X-Tclaude-Route-ID"))
	agentID, err := db.AgentIDForConv(convID)
	if err != nil || agentID == "" {
		writeRouteError(w, http.StatusForbidden, "route_identity", "caller has no stable agent identity")
		return
	}
	if routeID == "" {
		writeRouteError(w, http.StatusBadRequest, "route_channel", "route id is required")
		return
	}
	groupGeneration, err := strconv.ParseInt(strings.TrimSpace(r.Header.Get("X-Tclaude-Route-Group-Generation")), 10, 64)
	if err != nil || groupGeneration <= 0 {
		writeRouteError(w, http.StatusBadRequest, "route_channel", "group generation is required")
		return
	}
	launchGeneration := strings.TrimSpace(r.Header.Get("X-Tclaude-Route-Launch-Generation"))
	if launchGeneration == "" {
		writeRouteError(w, http.StatusBadRequest, "route_channel", "launch generation is required")
		return
	}

	var attach func(context.Context, net.Conn) error
	consumerEndpoint := ""
	switch role {
	case routeadapter.RolePublisher:
		route, loadErr := db.GetAgentRoute(routeID)
		if errors.Is(loadErr, sql.ErrNoRows) || route == nil {
			writeRouteError(w, http.StatusNotFound, "route_not_found", "no such route")
			return
		}
		if loadErr != nil {
			writeRouteError(w, http.StatusInternalServerError, "route_io", "route registry unavailable")
			return
		}
		if _, targetErr := routeadapter.ValidatePublisherTarget(route.Target); targetErr != nil {
			writeRouteError(w, http.StatusForbidden, "route_target_not_local", targetErr.Error())
			return
		}
		auth := routebroker.PublisherAuth{
			RouteID: routeID, AgentID: agentID, ConvID: convID,
			LaunchGeneration: launchGeneration, GroupGeneration: groupGeneration,
		}
		if authErr := (databaseRouteAuthority{}).AuthorizePublisher(r.Context(), auth); authErr != nil {
			writeRouteError(w, http.StatusForbidden, "route_authority", authErr.Error())
			return
		}
		attach = func(ctx context.Context, conn net.Conn) error {
			return GroupRouteBroker().AttachPublisher(ctx, auth, conn)
		}
	case routeadapter.RoleConsumer:
		leaseID := strings.TrimSpace(r.Header.Get("X-Tclaude-Route-Lease-ID"))
		if leaseID == "" {
			writeRouteError(w, http.StatusBadRequest, "route_channel", "consumer lease id is required")
			return
		}
		consumerEndpoint = strings.TrimSpace(r.Header.Get("X-Tclaude-Route-Consumer-Endpoint"))
		if consumerEndpoint == "" {
			writeRouteError(w, http.StatusBadRequest, "route_channel", "consumer endpoint is required")
			return
		}
		if _, endpointErr := routeadapter.ValidateConsumerEndpoint(consumerEndpoint); endpointErr != nil {
			writeRouteError(w, http.StatusForbidden, "route_endpoint_not_local", endpointErr.Error())
			return
		}
		auth := routebroker.ConsumerAuth{
			LeaseID: leaseID, RouteID: routeID, AgentID: agentID, ConvID: convID,
			LaunchGeneration: launchGeneration, GroupGeneration: groupGeneration,
		}
		if authErr := (databaseRouteAuthority{}).AuthorizeConsumer(r.Context(), auth); authErr != nil {
			writeRouteError(w, http.StatusForbidden, "route_authority", authErr.Error())
			return
		}
		attach = func(ctx context.Context, conn net.Conn) error {
			return GroupRouteBroker().AttachConsumer(ctx, auth, conn)
		}
	default:
		writeRouteError(w, http.StatusBadRequest, "route_channel", "role must be publisher or consumer")
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		writeRouteError(w, http.StatusInternalServerError, "route_channel", "route channel upgrade is unavailable")
		return
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		writeRouteError(w, http.StatusInternalServerError, "route_channel", fmt.Sprintf("hijack route channel: %v", err))
		return
	}
	if _, err := rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: tclaude-route-v1\r\n\r\n"); err != nil {
		_ = conn.Close()
		return
	}
	if err := rw.Flush(); err != nil {
		_ = conn.Close()
		return
	}
	if role == routeadapter.RoleConsumer {
		setRouteConsumerEndpoint(strings.TrimSpace(r.Header.Get("X-Tclaude-Route-Lease-ID")), consumerEndpoint)
	}
	if err := attach(r.Context(), conn); err != nil {
		_ = conn.Close()
	}
	if role == routeadapter.RoleConsumer {
		clearRouteConsumerEndpoint(strings.TrimSpace(r.Header.Get("X-Tclaude-Route-Lease-ID")))
	}
}

// Keep compile-time evidence that the handler's upgrade response is buffered
// by the same type the daemon's status/audit wrappers preserve.
var _ interface {
	Hijack() (net.Conn, *bufio.ReadWriter, error)
} = (*statusRec)(nil)
