//go:build linux

package routeadapter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// HelperConfig is the immutable launch identity for one namespace-local
// helper. Group IDs are explicit because a multi-group agent must never
// discover routes through an ambiguous name or an all-groups query.
type HelperConfig struct {
	SocketPath       string
	AgentID          string
	ConvID           string
	LaunchGeneration string
	GroupIDs         []int64
	PollInterval     time.Duration
}

// RunHelper watches the M1 route/lease read surfaces and attaches endpoint
// channels as routes are published or consumed after launch. It retries
// through an agentd restart and returns only when its enclosing sandbox is
// being torn down.
func RunHelper(ctx context.Context, cfg HelperConfig) error {
	if strings.TrimSpace(cfg.SocketPath) == "" || strings.TrimSpace(cfg.AgentID) == "" || strings.TrimSpace(cfg.ConvID) == "" || strings.TrimSpace(cfg.LaunchGeneration) == "" {
		return errors.New("route helper launch identity is incomplete")
	}
	if len(cfg.GroupIDs) == 0 {
		return errors.New("route helper requires at least one explicit group")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 250 * time.Millisecond
	}

	pubs := make(map[string]*helperHandle)
	cons := make(map[string]*helperHandle)
	defer cancelHandles(pubs)
	defer cancelHandles(cons)

	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()
	for {
		if err := syncPublisherRoutes(ctx, cfg, pubs); err != nil {
			// A daemon restart or a transient socket failure is expected. Keep
			// the namespace helper alive so the next tick reattaches channels.
			if ctx.Err() != nil {
				return nil
			}
		}
		if err := syncConsumerLeases(ctx, cfg, cons); err != nil {
			if ctx.Err() != nil {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

type helperHandle struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func cancelHandles(handles map[string]*helperHandle) {
	for key, handle := range handles {
		handle.cancel()
		delete(handles, key)
	}
}

func syncPublisherRoutes(ctx context.Context, cfg HelperConfig, active map[string]*helperHandle) error {
	seen := make(map[string]bool)
	var firstErr error
	for _, groupID := range cfg.GroupIDs {
		var payload struct {
			Routes []routeRecord `json:"routes"`
		}
		path := "/v1/routes?group_id=" + strconv.FormatInt(groupID, 10)
		if err := GetUnixJSON(ctx, cfg.SocketPath, path, &payload); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, route := range payload.Routes {
			if route.State != "ready" || route.PublisherAgentID != cfg.AgentID || route.PublisherConvID != cfg.ConvID || route.PublisherLaunchGeneration != cfg.LaunchGeneration {
				continue
			}
			seen[route.ID] = true
			if _, exists := active[route.ID]; exists {
				continue
			}
			handleCtx, cancel := context.WithCancel(ctx)
			handle := &helperHandle{cancel: cancel, done: make(chan struct{})}
			active[route.ID] = handle
			go func(route routeRecord, id string, h *helperHandle) {
				defer close(h.done)
				auth := ChannelAuth{Role: RolePublisher, RouteID: route.ID, AgentID: cfg.AgentID, ConvID: cfg.ConvID, LaunchGeneration: cfg.LaunchGeneration, GroupGeneration: route.GroupGeneration}
				channel, err := DialUnixChannel(handleCtx, cfg.SocketPath, auth)
				if err == nil {
					_ = RunPublisher(handleCtx, channel, route.Target)
				}
			}(route, route.ID, handle)
		}
	}
	for id, handle := range active {
		if !seen[id] || handleFinished(handle) {
			handle.cancel()
			delete(active, id)
		}
	}
	return firstErr
}

func syncConsumerLeases(ctx context.Context, cfg HelperConfig, active map[string]*helperHandle) error {
	seen := make(map[string]bool)
	var firstErr error
	for _, groupID := range cfg.GroupIDs {
		var payload struct {
			Leases []leaseRecord `json:"leases"`
		}
		path := "/v1/routes/leases?group_id=" + strconv.FormatInt(groupID, 10)
		if err := GetUnixJSON(ctx, cfg.SocketPath, path, &payload); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, lease := range payload.Leases {
			if lease.State != "open" || lease.ConsumerAgentID != cfg.AgentID || lease.ConsumerConvID != cfg.ConvID || lease.ConsumerLaunchGeneration != cfg.LaunchGeneration {
				continue
			}
			seen[lease.ID] = true
			if _, exists := active[lease.ID]; exists {
				continue
			}
			route, err := loadRoute(ctx, cfg.SocketPath, lease.RouteID)
			if err != nil || route.State != "ready" {
				continue
			}
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				return err
			}
			endpoint := "tcp://" + listener.Addr().String()
			if _, err := ValidateConsumerEndpoint(endpoint); err != nil {
				_ = listener.Close()
				return err
			}
			handleCtx, cancel := context.WithCancel(ctx)
			handle := &helperHandle{cancel: cancel, done: make(chan struct{})}
			active[lease.ID] = handle
			go func(lease leaseRecord, h *helperHandle) {
				defer close(h.done)
				auth := ChannelAuth{Role: RoleConsumer, RouteID: lease.RouteID, LeaseID: lease.ID, AgentID: cfg.AgentID, ConvID: cfg.ConvID, LaunchGeneration: cfg.LaunchGeneration, GroupGeneration: lease.GroupGeneration, ConsumerEndpoint: endpoint}
				channel, err := DialUnixChannel(handleCtx, cfg.SocketPath, auth)
				if err == nil {
					_ = RunConsumer(handleCtx, channel, listener)
				} else {
					_ = listener.Close()
				}
			}(lease, handle)
		}
	}
	for id, handle := range active {
		if !seen[id] || handleFinished(handle) {
			handle.cancel()
			delete(active, id)
		}
	}
	return firstErr
}

func handleFinished(handle *helperHandle) bool {
	select {
	case <-handle.done:
		return true
	default:
		return false
	}
}

type routeRecord struct {
	ID                        string `json:"id"`
	GroupGeneration           int64  `json:"group_generation"`
	PublisherAgentID          string `json:"publisher_agent_id"`
	PublisherConvID           string `json:"publisher_conv_id"`
	PublisherLaunchGeneration string `json:"publisher_launch_generation"`
	Target                    string `json:"target"`
	State                     string `json:"state"`
}

type leaseRecord struct {
	ID                       string `json:"id"`
	RouteID                  string `json:"route_id"`
	ConsumerAgentID          string `json:"consumer_agent_id"`
	ConsumerConvID           string `json:"consumer_conv_id"`
	ConsumerLaunchGeneration string `json:"consumer_launch_generation"`
	GroupGeneration          int64  `json:"group_generation"`
	State                    string `json:"state"`
}

func loadRoute(ctx context.Context, socketPath, routeID string) (routeRecord, error) {
	var route routeRecord
	err := GetUnixJSON(ctx, socketPath, "/v1/routes/"+routeID, &route)
	return route, err
}

// GetUnixJSON is the read-only half of the launch helper's agentd client. It
// uses a real Unix socket and preserves response limits so a daemon error or
// proxy cannot make a helper allocate unbounded memory.
func GetUnixJSON(ctx context.Context, socketPath, path string, out any) error {
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\r\n") {
		return errors.New("invalid agentd route path")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return err
	}
	defer conn.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://tclaude.invalid"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Connection", "close")
	if err := req.Write(conn); err != nil {
		return err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("agentd route read refused: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}
