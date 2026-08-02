package session

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/sandboxproxy"
	"golang.org/x/sys/unix"
)

const (
	proxyRouteCapabilityHeader = "X-Tclaude-Route-Helper-Credential"
	proxyRouteEndpointWait     = 5 * time.Second
	proxyRoutePollInterval     = 50 * time.Millisecond
)

// proxyRouteAuthorityConfig is launch metadata, never a bearer. It may cross
// an argv or JSON boundary; the capability itself only crosses by descriptor.
type proxyRouteAuthorityConfig struct {
	SocketPath        string  `json:"socket_path"`
	AgentID           string  `json:"agent_id"`
	ConvID            string  `json:"conv_id"`
	LaunchGeneration  string  `json:"launch_generation"`
	GroupIDs          []int64 `json:"group_ids"`
	HandoffSocketPath string  `json:"handoff_socket_path"`
}

type proxyRouteAuthority struct {
	config     proxyRouteAuthorityConfig
	credential string
	mu         sync.Mutex
}

func proxyRouteAuthorityConfigFromHelper(helper *TclaudeLayerRouteHelper) *proxyRouteAuthorityConfig {
	if helper == nil {
		return nil
	}
	return &proxyRouteAuthorityConfig{
		SocketPath: helper.SocketPath, AgentID: helper.AgentID,
		ConvID: helper.ConvID, LaunchGeneration: helper.LaunchGeneration,
		GroupIDs:          append([]int64(nil), helper.GroupIDs...),
		HandoffSocketPath: helper.HandoffSocketPath,
	}
}

type proxyRouteView struct {
	ID                        string `json:"id"`
	GroupID                   int64  `json:"group_id"`
	PublisherAgentID          string `json:"publisher_agent_id"`
	PublisherConvID           string `json:"publisher_conv_id"`
	PublisherLaunchGeneration string `json:"publisher_launch_generation"`
	GroupGeneration           int64  `json:"group_generation"`
	Target                    string `json:"target"`
	State                     string `json:"state"`
}

type proxyRouteLeaseView struct {
	ID                       string `json:"id"`
	RouteID                  string `json:"route_id"`
	ConsumerAgentID          string `json:"consumer_agent_id"`
	ConsumerConvID           string `json:"consumer_conv_id"`
	ConsumerLaunchGeneration string `json:"consumer_launch_generation"`
	GroupGeneration          int64  `json:"group_generation"`
	State                    string `json:"state"`
	Endpoint                 string `json:"endpoint"`
	EndpointState            string `json:"endpoint_state"`
	EndpointError            string `json:"endpoint_error"`
}

func newProxyRouteAuthority(config proxyRouteAuthorityConfig, credential string) *proxyRouteAuthority {
	return &proxyRouteAuthority{config: config, credential: strings.TrimSpace(credential)}
}

func (a *proxyRouteAuthority) Close() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.credential = ""
	a.mu.Unlock()
}

func (a *proxyRouteAuthority) credentialValue() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.credential
}

func (a *proxyRouteAuthority) Identity(ctx context.Context) (sandboxproxy.RouteIdentity, error) {
	if a == nil || len(a.config.GroupIDs) == 0 {
		return sandboxproxy.RouteIdentity{}, errors.New("proxy route authority requires an explicit launch group")
	}
	groupID := a.config.GroupIDs[0]
	if groupID <= 0 {
		return sandboxproxy.RouteIdentity{}, errors.New("proxy route authority group is invalid")
	}
	var payload struct {
		GroupID         int64 `json:"group_id"`
		GroupGeneration int64 `json:"group_generation"`
	}
	if err := a.request(ctx, http.MethodGet,
		"/v1/routes?group_id="+strconv.FormatInt(groupID, 10), nil, &payload); err != nil {
		return sandboxproxy.RouteIdentity{}, fmt.Errorf("resolve route group authority: %w", err)
	}
	if payload.GroupID != groupID || payload.GroupGeneration <= 0 {
		return sandboxproxy.RouteIdentity{}, errors.New("route group authority is missing or stale")
	}
	return sandboxproxy.RouteIdentity{
		GroupID:          groupID,
		GroupGeneration:  payload.GroupGeneration,
		AgentID:          a.config.AgentID,
		ConvID:           a.config.ConvID,
		LaunchGeneration: a.config.LaunchGeneration,
	}, nil
}

func (a *proxyRouteAuthority) IdentityForRoute(ctx context.Context, request sandboxproxy.RouteRequest) (sandboxproxy.RouteIdentity, error) {
	if a == nil || len(a.config.GroupIDs) == 0 {
		return sandboxproxy.RouteIdentity{}, errors.New("proxy route authority has no launch groups")
	}
	var route proxyRouteView
	if err := a.request(ctx, http.MethodGet, "/v1/routes/"+url.PathEscape(request.RouteID), nil, &route); err != nil {
		return sandboxproxy.RouteIdentity{}, fmt.Errorf("read named route group: %w", err)
	}
	if !a.allowedGroup(route.GroupID) || route.GroupGeneration <= 0 || route.State != "ready" {
		return sandboxproxy.RouteIdentity{}, errors.New("named route group is stale or outside the launch")
	}
	return sandboxproxy.RouteIdentity{
		GroupID: route.GroupID, GroupGeneration: route.GroupGeneration,
		AgentID: a.config.AgentID, ConvID: a.config.ConvID,
		LaunchGeneration: a.config.LaunchGeneration,
	}, nil
}

func (a *proxyRouteAuthority) ResolveRoute(ctx context.Context, request sandboxproxy.RouteRequest) (sandboxproxy.RouteResolution, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if a == nil || strings.TrimSpace(a.credentialValue()) == "" {
		return sandboxproxy.RouteResolution{}, errors.New("route authority capability is unavailable")
	}
	if request.Identity.AgentID != a.config.AgentID ||
		request.Identity.ConvID != a.config.ConvID ||
		request.Identity.LaunchGeneration != a.config.LaunchGeneration ||
		!a.allowedGroup(request.Identity.GroupID) {
		return sandboxproxy.RouteResolution{}, errors.New("route request identity is not the live launch")
	}
	var route proxyRouteView
	if err := a.request(ctx, http.MethodGet, "/v1/routes/"+url.PathEscape(request.RouteID), nil, &route); err != nil {
		return sandboxproxy.RouteResolution{}, fmt.Errorf("read named route: %w", err)
	}
	if route.ID != request.RouteID || route.GroupID != request.Identity.GroupID ||
		route.GroupGeneration != request.Identity.GroupGeneration || route.State != "ready" {
		return sandboxproxy.RouteResolution{}, errors.New("named route is stale, withdrawn, or outside the launch group")
	}
	targetPort, err := proxyRouteTargetPort(route.Target)
	if err != nil || targetPort != request.Port {
		return sandboxproxy.RouteResolution{}, errors.New("named route target port does not match the request")
	}

	body := map[string]any{
		"route_id":          request.RouteID,
		"group_id":          request.Identity.GroupID,
		"group_generation":  request.Identity.GroupGeneration,
		"launch_generation": request.Identity.LaunchGeneration,
	}
	var opened proxyRouteLeaseView
	if err := a.request(ctx, http.MethodPost,
		"/v1/routes/"+url.PathEscape(request.RouteID)+"/open", body, &opened); err != nil {
		return sandboxproxy.RouteResolution{}, fmt.Errorf("open named route: %w", err)
	}
	if opened.ID == "" || opened.RouteID != request.RouteID ||
		opened.ConsumerAgentID != a.config.AgentID || opened.ConsumerConvID != a.config.ConvID ||
		opened.ConsumerLaunchGeneration != a.config.LaunchGeneration ||
		opened.GroupGeneration != request.Identity.GroupGeneration || opened.State != "open" {
		if opened.ID != "" {
			_ = a.ReleaseRoute(context.Background(), sandboxproxy.RouteResolution{LeaseID: opened.ID, RouteID: request.RouteID})
		}
		return sandboxproxy.RouteResolution{}, errors.New("route lease authority returned an invalid lease")
	}

	deadline := time.Now().Add(proxyRouteEndpointWait)
	for {
		lease, pollErr := a.readLease(ctx, request.Identity.GroupID, opened.ID)
		if pollErr == nil {
			if lease.State != "open" || lease.RouteID != request.RouteID ||
				lease.ConsumerAgentID != a.config.AgentID || lease.ConsumerConvID != a.config.ConvID ||
				lease.ConsumerLaunchGeneration != a.config.LaunchGeneration ||
				lease.GroupGeneration != request.Identity.GroupGeneration {
				_ = a.ReleaseRoute(context.Background(), sandboxproxy.RouteResolution{LeaseID: opened.ID, RouteID: request.RouteID})
				return sandboxproxy.RouteResolution{}, errors.New("route lease became stale")
			}
			if lease.EndpointState == "refused" || lease.EndpointState == "closed" {
				_ = a.ReleaseRoute(context.Background(), sandboxproxy.RouteResolution{LeaseID: opened.ID, RouteID: request.RouteID})
				return sandboxproxy.RouteResolution{}, errors.New("route lease endpoint was refused or closed")
			}
			if lease.EndpointState == "ready" {
				endpoint, endpointErr := proxyRouteEndpoint(lease.Endpoint)
				if endpointErr != nil {
					_ = a.ReleaseRoute(context.Background(), sandboxproxy.RouteResolution{LeaseID: opened.ID, RouteID: request.RouteID})
					return sandboxproxy.RouteResolution{}, endpointErr
				}
				return sandboxproxy.RouteResolution{
					RouteID: request.RouteID, LeaseID: opened.ID,
					GroupID: route.GroupID, GroupGeneration: route.GroupGeneration,
					PublisherAgentID: route.PublisherAgentID, PublisherConvID: route.PublisherConvID,
					PublisherLaunchGeneration: route.PublisherLaunchGeneration,
					Endpoint:                  endpoint, TargetPort: targetPort,
				}, nil
			}
		}
		if time.Now().After(deadline) {
			_ = a.ReleaseRoute(context.Background(), sandboxproxy.RouteResolution{LeaseID: opened.ID, RouteID: request.RouteID})
			if pollErr != nil {
				return sandboxproxy.RouteResolution{}, fmt.Errorf("wait for named route endpoint: %w", pollErr)
			}
			return sandboxproxy.RouteResolution{}, errors.New("named route endpoint did not become ready")
		}
		select {
		case <-ctx.Done():
			_ = a.ReleaseRoute(context.Background(), sandboxproxy.RouteResolution{LeaseID: opened.ID, RouteID: request.RouteID})
			return sandboxproxy.RouteResolution{}, ctx.Err()
		case <-time.After(proxyRoutePollInterval):
		}
	}
}

func (a *proxyRouteAuthority) readLease(ctx context.Context, groupID int64, leaseID string) (proxyRouteLeaseView, error) {
	var payload struct {
		Leases []proxyRouteLeaseView `json:"leases"`
	}
	if err := a.request(ctx, http.MethodGet,
		"/v1/routes/leases?group_id="+strconv.FormatInt(groupID, 10), nil, &payload); err != nil {
		return proxyRouteLeaseView{}, err
	}
	for _, lease := range payload.Leases {
		if lease.ID == leaseID {
			return lease, nil
		}
	}
	return proxyRouteLeaseView{}, errors.New("route lease is not visible to the live launch")
}

func (a *proxyRouteAuthority) ReleaseRoute(ctx context.Context, resolution sandboxproxy.RouteResolution) error {
	if a == nil || strings.TrimSpace(resolution.LeaseID) == "" {
		return nil
	}
	return a.request(ctx, http.MethodDelete,
		"/v1/routes/leases/"+url.PathEscape(resolution.LeaseID), nil, nil)
}

func (a *proxyRouteAuthority) allowedGroup(groupID int64) bool {
	for _, allowed := range a.config.GroupIDs {
		if allowed == groupID {
			return true
		}
	}
	return false
}

func (a *proxyRouteAuthority) request(ctx context.Context, method, path string, body any, out any) error {
	if a == nil || strings.TrimSpace(a.config.SocketPath) == "" {
		return errors.New("route authority socket is unavailable")
	}
	credential := a.credentialValue()
	if credential == "" {
		return errors.New("route authority capability is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(data)
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", a.config.SocketPath)
	if err != nil {
		return err
	}
	defer conn.Close()
	req, err := http.NewRequestWithContext(ctx, method, "http://tclaude.invalid"+path, payload)
	if err != nil {
		return err
	}
	req.Header.Set("Connection", "close")
	req.Header.Set(proxyRouteCapabilityHeader, credential)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if err := req.Write(conn); err != nil {
		return err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("agentd route authority refused request with status %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

func proxyRouteTargetPort(raw string) (int, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "tcp" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return 0, errors.New("route target is not a TCP endpoint")
	}
	host, portText, err := net.SplitHostPort(u.Host)
	if err != nil {
		return 0, errors.New("route target has no TCP port")
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || !addr.IsLoopback() || addr.IsUnspecified() {
		return 0, errors.New("route target is not a loopback endpoint")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return 0, errors.New("route target has an invalid TCP port")
	}
	return port, nil
}

func proxyRouteEndpoint(raw string) (netip.AddrPort, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "tcp" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return netip.AddrPort{}, errors.New("route lease endpoint is not a TCP endpoint")
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		return netip.AddrPort{}, errors.New("route lease endpoint has no TCP port")
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || !addr.IsLoopback() || addr.IsUnspecified() {
		return netip.AddrPort{}, errors.New("route lease endpoint is not a loopback endpoint")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return netip.AddrPort{}, errors.New("route lease endpoint has an invalid TCP port")
	}
	return netip.AddrPortFrom(addr, uint16(portNumber)), nil
}

// receiveProxyRouteCredential consumes the established one-shot descriptor
// handoff. The returned bearer exists only in process memory and is never
// encoded in a command, environment, hostname, or readable file.
func receiveProxyRouteCredential(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("route capability handoff path is empty")
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		return "", fmt.Errorf("connect route capability handoff: %w", err)
	}
	defer conn.Close()
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return "", errors.New("route capability handoff is not a Unix socket")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return "", err
	}
	fd := -1
	var recvErr error
	if err := raw.Read(func(socketFD uintptr) bool {
		payload := make([]byte, 1)
		oob := make([]byte, unix.CmsgSpace(4))
		_, _, _, _, err := unix.Recvmsg(int(socketFD), payload, oob, 0)
		if err != nil {
			recvErr = err
			return true
		}
		messages, err := unix.ParseSocketControlMessage(oob)
		if err != nil {
			recvErr = err
			return true
		}
		for _, message := range messages {
			rights, err := unix.ParseUnixRights(&message)
			if err != nil || len(rights) != 1 || fd != -1 {
				recvErr = errors.New("route capability handoff must contain exactly one descriptor")
				return true
			}
			fd = rights[0]
		}
		return true
	}); err != nil {
		return "", err
	}
	if recvErr != nil || fd <= 0 {
		if fd > 0 {
			_ = unix.Close(fd)
		}
		if recvErr == nil {
			recvErr = errors.New("route capability handoff did not contain a descriptor")
		}
		return "", recvErr
	}
	f := os.NewFile(uintptr(fd), "route-capability")
	if f == nil {
		_ = unix.Close(fd)
		return "", errors.New("route capability descriptor is invalid")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return "", fmt.Errorf("read route capability descriptor: %w", err)
	}
	credential := strings.TrimSpace(string(data))
	if credential == "" {
		return "", errors.New("route capability descriptor was empty")
	}
	return credential, nil
}
