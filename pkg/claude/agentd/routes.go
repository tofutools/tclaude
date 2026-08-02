package agentd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

const routeAPIVersion = "v1"

var errRouteStaleLaunchGeneration = errors.New("launch generation is stale")

type routePublishRequest struct {
	Group            string `json:"group"`
	GroupID          int64  `json:"group_id,omitempty"`
	GroupGeneration  *int64 `json:"group_generation,omitempty"`
	LaunchGeneration string `json:"launch_generation"`
	Name             string `json:"name"`
	Transport        string `json:"transport"`
	Target           string `json:"target"`
}

type routeOpenRequest struct {
	RouteID          string `json:"route_id"`
	Group            string `json:"group,omitempty"`
	GroupID          int64  `json:"group_id,omitempty"`
	GroupGeneration  *int64 `json:"group_generation,omitempty"`
	LaunchGeneration string `json:"launch_generation"`
}

type routeView struct {
	APIVersion                string `json:"api_version"`
	ID                        string `json:"id"`
	GroupID                   int64  `json:"group_id"`
	Group                     string `json:"group"`
	PublisherAgentID          string `json:"publisher_agent_id"`
	PublisherConvID           string `json:"publisher_conv_id,omitempty"`
	PublisherLaunchGeneration string `json:"publisher_launch_generation"`
	GroupGeneration           int64  `json:"group_generation"`
	Name                      string `json:"name"`
	Transport                 string `json:"transport"`
	Target                    string `json:"target"`
	State                     string `json:"state"`
	CreatedAt                 string `json:"created_at"`
	WithdrawnAt               string `json:"withdrawn_at,omitempty"`
	WithdrawReason            string `json:"withdraw_reason,omitempty"`
}

type routeLeaseView struct {
	APIVersion               string `json:"api_version"`
	ID                       string `json:"id"`
	RouteID                  string `json:"route_id"`
	ConsumerAgentID          string `json:"consumer_agent_id"`
	ConsumerConvID           string `json:"consumer_conv_id,omitempty"`
	ConsumerLaunchGeneration string `json:"consumer_launch_generation"`
	GroupGeneration          int64  `json:"group_generation"`
	State                    string `json:"state"`
	OpenedAt                 string `json:"opened_at"`
	Endpoint                 string `json:"endpoint,omitempty"`
	ClosedAt                 string `json:"closed_at,omitempty"`
}

func routeViewFor(r *db.AgentRoute) routeView {
	v := routeView{APIVersion: routeAPIVersion, ID: r.ID, GroupID: r.GroupID, Group: r.GroupName, PublisherAgentID: r.PublisherAgentID, PublisherConvID: r.PublisherConvID, PublisherLaunchGeneration: r.PublisherLaunchGeneration, GroupGeneration: r.GroupGeneration, Name: r.Name, Transport: r.Transport, Target: r.Target, State: r.State, CreatedAt: r.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"), WithdrawReason: r.WithdrawReason}
	if !r.WithdrawnAt.IsZero() {
		v.WithdrawnAt = r.WithdrawnAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
	}
	return v
}

func routeLeaseViewFor(l *db.AgentRouteLease) routeLeaseView {
	v := routeLeaseView{APIVersion: routeAPIVersion, ID: l.ID, RouteID: l.RouteID, ConsumerAgentID: l.ConsumerAgentID, ConsumerConvID: l.ConsumerConvID, ConsumerLaunchGeneration: l.ConsumerLaunchGeneration, GroupGeneration: l.GroupGeneration, State: l.State, OpenedAt: l.OpenedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"), Endpoint: routeConsumerEndpointForLease(l.ID)}
	if !l.ClosedAt.IsZero() {
		v.ClosedAt = l.ClosedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
	}
	return v
}

func writeRouteError(w http.ResponseWriter, status int, code, detail string) {
	writeJSON(w, status, map[string]any{"api_version": routeAPIVersion, "error": detail, "code": code})
}

func routeGroup(group string, groupID int64) (*db.AgentGroup, error) {
	if groupID != 0 {
		g, err := db.GetAgentGroupByID(groupID)
		if err != nil {
			return nil, err
		}
		if g == nil {
			return nil, sql.ErrNoRows
		}
		if named := strings.TrimSpace(group); named != "" {
			selected, selectErr := routeGroup(named, 0)
			if selectErr != nil || selected == nil || selected.ID != g.ID {
				return nil, errors.New("group and group_id select different groups")
			}
		}
		return g, nil
	}
	group = strings.TrimSpace(group)
	if group == "" {
		return nil, errors.New("explicit group selection is required")
	}
	if id, err := strconv.ParseInt(group, 10, 64); err == nil && id > 0 {
		return routeGroup("", id)
	}
	g, err := db.GetAgentGroupByName(group)
	if err != nil {
		return nil, err
	}
	if g == nil {
		return nil, sql.ErrNoRows
	}
	return g, nil
}

func routeGeneration(g *db.AgentGroup, requested *int64) int64 {
	if requested != nil {
		return *requested
	}
	return g.RouteGeneration
}

// routeLaunchGeneration derives the current launch identity when the caller
// does not send one. Existing session launch generations are preferred; test
// and legacy sessions without one fall back to the current conversation
// generation, which still prevents a rotated agent from inheriting a route.
func routeLaunchGeneration(convID, supplied string) (string, error) {
	supplied = strings.TrimSpace(supplied)
	if supplied != "" {
		if current, known := knownRouteLaunchGeneration(convID); known && current != supplied {
			return "", fmt.Errorf("%w: current launch generation is %q", errRouteStaleLaunchGeneration, current)
		}
		return supplied, nil
	}
	rows, err := db.FindSessionsByConvID(convID)
	if err != nil {
		return "", err
	}
	for _, row := range rows {
		identity, err := db.GetSessionExitLaunchIdentity(row.ID)
		if err == nil && strings.TrimSpace(identity.Generation) != "" {
			return identity.Generation, nil
		}
	}
	return convID, nil
}

func knownRouteLaunchGeneration(convID string) (string, bool) {
	rows, err := db.FindSessionsByConvID(convID)
	if err != nil {
		return "", false
	}
	for _, row := range rows {
		identity, err := db.GetSessionExitLaunchIdentity(row.ID)
		if err == nil && identity.Generation != "" {
			return identity.Generation, true
		}
	}
	return "", false
}

func routeCallerAgent(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	// The namespace helper is a sibling process, not a harness descendant.
	// Its opaque capability is accepted only on the read-only route discovery
	// endpoints; route mutation remains requireAgent-authenticated.
	if r.Method == http.MethodGet {
		if capability, present, valid := routeHelperCredentialForRequest(r); present {
			if !valid {
				writeRouteError(w, http.StatusUnauthorized, "route_helper_auth", "route helper credential is missing, stale, or invalid")
				return "", "", false
			}
			return capability.convID, capability.agentID, true
		}
	}
	convID, ok := requireAgent(w, r)
	if !ok {
		return "", "", false
	}
	agentID, err := db.AgentIDForConv(convID)
	if err != nil || agentID == "" {
		writeRouteError(w, http.StatusForbidden, "route_identity", "caller has no stable agent identity")
		return "", "", false
	}
	return convID, agentID, true
}

func requireRouteMembership(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) (string, string, bool) {
	convID, agentID, ok := routeCallerAgent(w, r)
	if !ok {
		return "", "", false
	}
	member, err := db.FindAgentMemberInGroup(g.ID, agentID)
	if err != nil {
		writeRouteError(w, http.StatusInternalServerError, "route_authority", "could not verify group membership")
		return "", "", false
	}
	if member == nil {
		writeRouteError(w, http.StatusForbidden, "route_not_member", fmt.Sprintf("caller is not a member of target group %q", g.Name))
		return "", "", false
	}
	return convID, agentID, true
}

// requireRouteCapability is deliberately separate from requirePermission:
// requirePermission sees group grants as a union. This check accepts a group
// grant only when it belongs to this exact target group, while preserving the
// established explicit per-agent/default/sudo and deny precedence.
func requireRouteCapability(w http.ResponseWriter, r *http.Request, g *db.AgentGroup, slug string) (string, string, bool) {
	convID, agentID, ok := requireRouteMembership(w, r, g)
	if !ok {
		return "", "", false
	}

	// Match the central permission resolver: a live, matching sudo grant is
	// authoritative over a permanent deny. Ordinary group grants remain
	// target-group scoped below rather than using the union resolver.
	if grantID, err := db.LookupActiveSudoGrantID(convID, slug); err == nil && grantID != 0 {
		return convID, agentID, true
	}
	if effect, exists, err := db.AgentPermissionOverride(convID, slug); err != nil {
		writeRouteError(w, http.StatusInternalServerError, "route_authority", "could not resolve permission")
		return "", "", false
	} else if exists {
		if effect == db.PermEffectDeny {
			writeRoutePermissionRefusal(w, g, slug)
			return "", "", false
		}
		return convID, agentID, true
	}
	if granted, err := db.HasAgentGroupPermissionForAgent(agentID, g.ID, slug); err != nil {
		writeRouteError(w, http.StatusInternalServerError, "route_authority", "could not resolve target-group permission")
		return "", "", false
	} else if granted {
		return convID, agentID, true
	}
	defaultAllowed := false
	if defaults, ok := r.Context().Value(permissionDefaultsKey{}).(map[string]bool); ok {
		defaultAllowed = defaults[slug]
	} else if cfg, _ := config.Load(); cfg != nil {
		defaultAllowed = cfg.HasDefaultPermission(slug)
	}
	if defaultAllowed {
		return convID, agentID, true
	}
	writeRoutePermissionRefusal(w, g, slug)
	return "", "", false
}

func writeRoutePermissionRefusal(w http.ResponseWriter, g *db.AgentGroup, slug string) {
	writeRouteError(w, http.StatusForbidden, "route_permission", fmt.Sprintf("caller is not granted %q for target group %q; grant the capability on that group, to this agent, globally, or via sudo", slug, g.Name))
}

// routeMembershipMutationAllowed freezes a route-enabled group's online
// roster. Offline changes remain available and advance route_generation via
// the DB membership primitive, invalidating old route credentials at next use.
func routeMembershipMutationAllowed(w http.ResponseWriter, g *db.AgentGroup) bool {
	enabled, err := db.IsAgentGroupRouteEnabled(g.ID, PermRoutesPublish, PermRoutesConsume)
	if err != nil {
		writeRouteError(w, http.StatusInternalServerError, "route_authority", "could not inspect route-enabled group policy")
		return false
	}
	if !enabled {
		return true
	}
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		writeRouteError(w, http.StatusInternalServerError, "route_authority", "could not inspect route-enabled group members")
		return false
	}
	for _, member := range members {
		if isConvOnline(member.ConvID) {
			writeRouteError(w, http.StatusConflict, "route_membership_locked", fmt.Sprintf("group %q has live route participants; membership changes require all members to be offline", g.Name))
			return false
		}
	}
	return true
}

func routePublisherLive(route *db.AgentRoute) bool {
	a, err := db.GetAgent(route.PublisherAgentID)
	if err != nil || a == nil || !a.Active() || a.CurrentConvID != route.PublisherConvID {
		return false
	}
	if generation, known := knownRouteLaunchGeneration(route.PublisherConvID); known && generation != route.PublisherLaunchGeneration {
		return false
	}
	return true
}

func refreshRoutePublisher(route *db.AgentRoute) *db.AgentRoute {
	if route == nil || route.State != db.RouteStateReady || routePublisherLive(route) {
		return route
	}
	if err := db.MarkAgentRoutePublisherLost(route.ID, "publisher generation is no longer current"); err == nil {
		routeAdapterCloseRoute(route.ID)
	}
	updated, err := db.GetAgentRoute(route.ID)
	if err == nil && updated != nil {
		return updated
	}
	return route
}

func handleRoutes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		handleRoutesList(w, r)
	case http.MethodPost:
		handleRoutePublish(w, r)
	default:
		writeRouteError(w, http.StatusMethodNotAllowed, "method", "GET or POST required")
	}
}

func handleRoutesList(w http.ResponseWriter, r *http.Request) {
	g, err := routeGroup(r.URL.Query().Get("group"), parseRouteGroupID(r.URL.Query().Get("group_id")))
	if err != nil {
		writeRouteError(w, http.StatusBadRequest, "route_group", err.Error())
		return
	}
	if classify(peerFromContext(r.Context())) == classAgent {
		if _, _, ok := requireRouteMembership(w, r, g); !ok {
			return
		}
	} else if classify(peerFromContext(r.Context())) != classHuman {
		_, _, _ = routeCallerAgent(w, r)
		return
	}
	routes, err := db.ListAgentRoutes(g.ID)
	if err != nil {
		writeRouteError(w, http.StatusInternalServerError, "route_io", "route registry unavailable")
		return
	}
	out := make([]routeView, 0, len(routes))
	for _, route := range routes {
		out = append(out, routeViewFor(refreshRoutePublisher(route)))
	}
	writeJSON(w, http.StatusOK, map[string]any{"api_version": routeAPIVersion, "group_id": g.ID, "group": g.Name, "group_generation": g.RouteGeneration, "routes": out})
}

// handleRouteLeasesList is the launch-helper read surface. It is intentionally
// consumer-scoped: a helper can discover only its own leases, and group
// membership is checked before the durable rows are read.
func handleRouteLeasesList(w http.ResponseWriter, r *http.Request) {
	g, err := routeGroup(r.URL.Query().Get("group"), parseRouteGroupID(r.URL.Query().Get("group_id")))
	if err != nil {
		writeRouteError(w, http.StatusBadRequest, "route_group", err.Error())
		return
	}
	convID, agentID, ok := requireRouteMembership(w, r, g)
	if !ok {
		return
	}
	leases, err := db.ListAgentRouteLeases(g.ID, agentID, convID)
	if err != nil {
		writeRouteError(w, http.StatusInternalServerError, "route_io", "route lease registry unavailable")
		return
	}
	out := make([]routeLeaseView, 0, len(leases))
	for _, lease := range leases {
		out = append(out, routeLeaseViewFor(lease))
	}
	writeJSON(w, http.StatusOK, map[string]any{"api_version": routeAPIVersion, "group_id": g.ID, "group": g.Name, "group_generation": g.RouteGeneration, "leases": out})
}

func parseRouteGroupID(raw string) int64 {
	id, _ := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	return id
}

func handleRoutePublish(w http.ResponseWriter, r *http.Request) {
	var body routePublishRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeRouteError(w, http.StatusBadRequest, "route_invalid_argument", err.Error())
		return
	}
	g, err := routeGroup(body.Group, body.GroupID)
	if err != nil {
		writeRouteError(w, http.StatusBadRequest, "route_group", err.Error())
		return
	}
	convID, agentID, ok := requireRouteCapability(w, r, g, PermRoutesPublish)
	if !ok {
		return
	}
	launchGeneration, err := routeLaunchGeneration(convID, body.LaunchGeneration)
	if err != nil {
		if errors.Is(err, errRouteStaleLaunchGeneration) {
			writeRouteError(w, http.StatusConflict, "route_generation_stale", err.Error())
			return
		}
		writeRouteError(w, http.StatusInternalServerError, "route_authority", "could not resolve launch generation")
		return
	}
	transport := strings.TrimSpace(body.Transport)
	if transport == "" {
		transport = "tcp"
	}
	if transport != "tcp" {
		writeRouteError(w, http.StatusBadRequest, "route_invalid_transport", "only tcp routes are in the M1 contract")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || len(name) > 128 || strings.ContainsAny(name, "/\r\n") {
		writeRouteError(w, http.StatusBadRequest, "route_invalid_name", "route name must be 1–128 characters without slash or newline")
		return
	}
	if strings.TrimSpace(body.Target) == "" {
		writeRouteError(w, http.StatusBadRequest, "route_invalid_target", "target is required")
		return
	}
	if err := validateLinuxRoutePublishTarget(strings.TrimSpace(body.Target)); err != nil {
		writeRouteError(w, http.StatusBadRequest, "route_target_not_local", err.Error())
		return
	}
	route, err := db.CreateAgentRoute(g.ID, agentID, convID, launchGeneration, routeGeneration(g, body.GroupGeneration), name, transport, strings.TrimSpace(body.Target))
	if err != nil {
		writeRouteError(w, http.StatusConflict, "route_conflict", err.Error())
		return
	}
	// The adapter lifetime is the durable route/lease lifecycle, not the
	// short-lived HTTP request context that created it.
	if _, err := routeAdapterPublish(context.Background(), route); err != nil {
		_ = db.WithdrawAgentRoute(route.ID, agentID, convID, "Darwin route adapter activation failed")
		writeRouteError(w, http.StatusConflict, "route_adapter", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, routeViewFor(route))
}

func handleRouteByID(w http.ResponseWriter, r *http.Request) {
	routeID := r.PathValue("route")
	route, err := db.GetAgentRoute(routeID)
	if errors.Is(err, sql.ErrNoRows) || route == nil {
		writeRouteError(w, http.StatusNotFound, "route_not_found", "no such route")
		return
	}
	route = refreshRoutePublisher(route)
	g, err := db.GetAgentGroupByID(route.GroupID)
	if err != nil || g == nil {
		writeRouteError(w, http.StatusNotFound, "route_group", "route target group no longer exists")
		return
	}
	switch r.Method {
	case http.MethodGet:
		if classify(peerFromContext(r.Context())) == classAgent {
			if _, _, ok := requireRouteMembership(w, r, g); !ok {
				return
			}
		} else if classify(peerFromContext(r.Context())) != classHuman {
			_, _, _ = routeCallerAgent(w, r)
			return
		}
		writeJSON(w, http.StatusOK, routeViewFor(route))
	case http.MethodDelete:
		convID, agentID, ok := requireRouteCapability(w, r, g, PermRoutesPublish)
		if !ok {
			return
		}
		if route.PublisherAgentID != agentID {
			writeRouteError(w, http.StatusForbidden, "route_not_owner", "only the publisher may withdraw this route")
			return
		}
		if err := db.WithdrawAgentRoute(route.ID, agentID, convID, "publisher requested withdrawal"); err != nil {
			writeRouteError(w, http.StatusConflict, "route_transition", err.Error())
			return
		}
		routeAdapterCloseRoute(route.ID)
		writeJSON(w, http.StatusOK, routeViewFor(refreshRoutePublisher(mustRoute(route.ID))))
	case http.MethodPost:
		handleRouteAction(w, r, route, g)
	default:
		writeRouteError(w, http.StatusMethodNotAllowed, "method", "GET, POST, or DELETE required")
	}
}

func mustRoute(routeID string) *db.AgentRoute {
	route, _ := db.GetAgentRoute(routeID)
	return route
}

func handleRouteAction(w http.ResponseWriter, r *http.Request, route *db.AgentRoute, g *db.AgentGroup) {
	action := strings.Trim(strings.TrimSpace(r.PathValue("action")), "/")
	switch action {
	case "open", "consume":
		var body routeOpenRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeRouteError(w, http.StatusBadRequest, "route_invalid_argument", err.Error())
			return
		}
		if body.RouteID != "" && body.RouteID != route.ID {
			writeRouteError(w, http.StatusBadRequest, "route_invalid_argument", "route_id does not match path")
			return
		}
		if body.Group == "" && body.GroupID == 0 {
			writeRouteError(w, http.StatusBadRequest, "route_group", "explicit group selection is required")
			return
		}
		if body.Group != "" || body.GroupID != 0 {
			selected, err := routeGroup(body.Group, body.GroupID)
			if err != nil || selected.ID != g.ID {
				writeRouteError(w, http.StatusForbidden, "route_group", "explicit group selection does not match route group")
				return
			}
		}
		convID, agentID, ok := requireRouteCapability(w, r, g, PermRoutesConsume)
		if !ok {
			return
		}
		if body.GroupGeneration == nil {
			body.GroupGeneration = &g.RouteGeneration
		}
		launchGeneration, err := routeLaunchGeneration(convID, body.LaunchGeneration)
		if err != nil {
			if errors.Is(err, errRouteStaleLaunchGeneration) {
				writeRouteError(w, http.StatusConflict, "route_generation_stale", err.Error())
				return
			}
			writeRouteError(w, http.StatusInternalServerError, "route_authority", "could not resolve launch generation")
			return
		}
		lease, err := db.OpenAgentRouteLease(route.ID, agentID, convID, launchGeneration, *body.GroupGeneration)
		if err != nil {
			writeRouteError(w, http.StatusConflict, "route_open_refused", err.Error())
			return
		}
		endpoint, enabled, err := routeAdapterOpen(context.Background(), route, lease)
		if err != nil {
			_ = db.CloseAgentRouteLease(lease.ID, agentID, convID)
			writeRouteError(w, http.StatusConflict, "route_adapter", err.Error())
			return
		}
		view := routeLeaseViewFor(lease)
		if enabled {
			view.Endpoint = endpoint
		}
		writeJSON(w, http.StatusCreated, view)
	case "withdraw":
		convID, agentID, ok := requireRouteCapability(w, r, g, PermRoutesPublish)
		if !ok {
			return
		}
		if route.PublisherAgentID != agentID {
			writeRouteError(w, http.StatusForbidden, "route_not_owner", "only the publisher may withdraw this route")
			return
		}
		if err := db.WithdrawAgentRoute(route.ID, agentID, convID, "publisher requested withdrawal"); err != nil {
			writeRouteError(w, http.StatusConflict, "route_transition", err.Error())
			return
		}
		routeAdapterCloseRoute(route.ID)
		writeJSON(w, http.StatusOK, routeViewFor(mustRoute(route.ID)))
	default:
		writeRouteError(w, http.StatusNotFound, "route_action", "unknown route action")
	}
}

func handleRouteOpenCollection(w http.ResponseWriter, r *http.Request) {
	var body routeOpenRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.RouteID) == "" {
		writeRouteError(w, http.StatusBadRequest, "route_invalid_argument", "route_id and group are required")
		return
	}
	if strings.TrimSpace(body.Group) == "" && body.GroupID == 0 {
		writeRouteError(w, http.StatusBadRequest, "route_group", "explicit group selection is required")
		return
	}
	route, err := db.GetAgentRoute(body.RouteID)
	if err != nil || route == nil {
		writeRouteError(w, http.StatusNotFound, "route_not_found", "no such route")
		return
	}
	// Reuse the path-shaped handler so both API spellings have identical
	// authorization and generation behavior.
	cloned := r.Clone(r.Context())
	cloned.SetPathValue("route", route.ID)
	cloned.SetPathValue("action", "open")
	payload, _ := json.Marshal(body)
	cloned.Body = io.NopCloser(bytes.NewReader(payload))
	handleRouteByID(w, cloned)
}

func handleRouteLeaseClose(w http.ResponseWriter, r *http.Request) {
	leaseID := r.PathValue("lease")
	lease, err := db.GetAgentRouteLease(leaseID)
	if errors.Is(err, sql.ErrNoRows) || lease == nil {
		writeRouteError(w, http.StatusNotFound, "route_lease_not_found", "no such route lease")
		return
	}
	route, err := db.GetAgentRoute(lease.RouteID)
	if err != nil || route == nil {
		writeRouteError(w, http.StatusNotFound, "route_not_found", "lease route no longer exists")
		return
	}
	g, err := db.GetAgentGroupByID(route.GroupID)
	if err != nil || g == nil {
		writeRouteError(w, http.StatusNotFound, "route_group", "route target group no longer exists")
		return
	}
	convID, agentID, ok := requireRouteCapability(w, r, g, PermRoutesConsume)
	if !ok {
		return
	}
	if lease.ConsumerAgentID != agentID {
		writeRouteError(w, http.StatusForbidden, "route_not_owner", "only the lease owner may close this lease")
		return
	}
	if err := db.CloseAgentRouteLease(lease.ID, agentID, convID); err != nil {
		writeRouteError(w, http.StatusConflict, "route_transition", err.Error())
		return
	}
	routeAdapterCloseLease(lease.ID)
	updated, _ := db.GetAgentRouteLease(lease.ID)
	writeJSON(w, http.StatusOK, routeLeaseViewFor(updated))
}
