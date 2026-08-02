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
	capability, ok := requireRouteHelperCredential(w, r)
	if !ok {
		return
	}
	role := strings.TrimSpace(r.Header.Get("X-Tclaude-Route-Role"))
	routeID := strings.TrimSpace(r.Header.Get("X-Tclaude-Route-ID"))
	if strings.TrimSpace(r.Header.Get("X-Tclaude-Route-Agent-ID")) != capability.agentID || strings.TrimSpace(r.Header.Get("X-Tclaude-Route-Conv-ID")) != capability.convID {
		writeRouteError(w, http.StatusForbidden, "route_identity", "route helper identity headers do not match its capability")
		return
	}
	convID, agentID := capability.convID, capability.agentID
	var err error
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
	// The caller-supplied generation is descriptive only.  The bearer
	// capability is the authoritative launch binding; accepting a different
	// header here would let one launch present another launch's route identity
	// to the M1 broker.
	if launchGeneration != capability.launchGeneration {
		writeRouteError(w, http.StatusForbidden, "route_identity", "route helper launch generation does not match its capability")
		return
	}
	launchGeneration = capability.launchGeneration

	var attach func(context.Context, net.Conn) error
	var consumerAuth routebroker.ConsumerAuth
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
		consumerAuth = routebroker.ConsumerAuth{
			LeaseID: leaseID, RouteID: routeID, AgentID: agentID, ConvID: convID,
			LaunchGeneration: launchGeneration, GroupGeneration: groupGeneration,
		}
		if authErr := (databaseRouteAuthority{}).AuthorizeConsumer(r.Context(), consumerAuth); authErr != nil {
			writeRouteError(w, http.StatusForbidden, "route_authority", authErr.Error())
			return
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
	if role == routeadapter.RoleConsumer {
		leaseID := strings.TrimSpace(r.Header.Get("X-Tclaude-Route-Lease-ID"))
		err := GroupRouteBroker().AttachConsumerWithReady(r.Context(), consumerAuth, conn, func() error {
			lease, err := db.GetAgentRouteLease(leaseID)
			if err != nil || lease == nil || lease.State != db.RouteLeaseOpen {
				return errors.New("route lease reached a terminal state before readiness")
			}
			if _, err := rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: tclaude-route-v1\r\n\r\n"); err != nil {
				return err
			}
			if err := rw.Flush(); err != nil {
				return err
			}
			if !setRouteConsumerEndpointReady(leaseID, consumerEndpoint) {
				return errors.New("route endpoint reached a terminal state before readiness")
			}
			return nil
		})
		if err != nil {
			_ = db.CloseAgentRouteLease(leaseID, consumerAuth.AgentID, consumerAuth.ConvID)
			setRouteConsumerEndpointRefused(leaseID, routeEndpointRefusalDetail(err))
		}
		clearRouteConsumerEndpoint(leaseID)
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
	if err := attach(r.Context(), conn); err != nil {
		_ = conn.Close()
	}
}

// Keep compile-time evidence that the handler's upgrade response is buffered
// by the same type the daemon's status/audit wrappers preserve.
var _ interface {
	Hijack() (net.Conn, *bufio.ReadWriter, error)
} = (*statusRec)(nil)

func routeEndpointRefusalDetail(err error) string {
	switch {
	case errors.Is(err, routebroker.ErrConsumerLimit):
		return "route adapter capacity exhausted"
	case errors.Is(err, routebroker.ErrUnauthorized):
		return "route adapter authorization refused"
	default:
		return "route adapter attachment refused"
	}
}
