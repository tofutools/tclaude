package session

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/sandboxproxy"
)

func TestProxyRouteAuthorityOpensAndReleasesGenerationBoundLease(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(fmt.Sprint("late=", late), func(t *testing.T) { testProxyRouteAuthorityLease(t, late) })
	}
}

func testProxyRouteAuthorityLease(t *testing.T, late bool) {
	socketPath := "/tmp/tclaude-route-authority-" + strconv.Itoa(os.Getpid()) + ".sock"
	_ = os.Remove(socketPath)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var seenCredential atomic.Int32
	var closed atomic.Int32
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(proxyRouteCapabilityHeader) != "capability-only-in-memory" {
			http.Error(w, "missing capability", http.StatusUnauthorized)
			return
		}
		seenCredential.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/routes/helper-identity":
			_, _ = fmt.Fprint(w, `{"agent_id":"agt","conv_id":"conv","launch_generation":"launch"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/routes":
			_, _ = fmt.Fprint(w, `{"group_id":42,"group_generation":7,"routes":[]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/routes/route-a":
			_, _ = fmt.Fprint(w, `{"id":"route-a","group_id":42,"publisher_agent_id":"pub","publisher_conv_id":"pub-conv","publisher_launch_generation":"pub-launch","group_generation":7,"target":"tcp://127.0.0.1:43127","state":"ready"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/open"):
			_, _ = fmt.Fprint(w, `{"id":"lease-a","route_id":"route-a","consumer_agent_id":"agt","consumer_conv_id":"conv","consumer_launch_generation":"launch","group_generation":7,"state":"open","endpoint_state":"pending"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/routes/leases":
			_, _ = fmt.Fprint(w, `{"group_id":42,"group_generation":7,"leases":[{"id":"lease-a","route_id":"route-a","consumer_agent_id":"agt","consumer_conv_id":"conv","consumer_launch_generation":"launch","group_generation":7,"state":"open","endpoint":"tcp://127.0.0.1:43128","endpoint_state":"ready"}]}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/routes/leases/lease-a":
			closed.Add(1)
			_, _ = fmt.Fprint(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	})}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(listener) }()
	defer func() {
		_ = server.Close()
		select {
		case <-serverDone:
		case <-time.After(time.Second):
			t.Fatal("route authority test server did not stop")
		}
	}()

	config := proxyRouteAuthorityConfig{SocketPath: socketPath, AgentID: "agt", ConvID: "conv", LaunchGeneration: "launch", GroupIDs: []int64{42}, HandoffSocketPath: "/tmp/private-one-shot.sock"}
	if late {
		config.ConvID = ""
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "capability-only-in-memory") {
		t.Fatalf("route authority metadata serialized a capability: %s", encoded)
	}
	authority := newProxyRouteAuthority(config, "capability-only-in-memory")
	ctx := context.Background()
	identity, err := authority.Identity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if late {
		if identity.ConvID != "" {
			t.Fatal("pending launch already has a conversation")
		}
		identity, err = authority.IdentityForRoute(ctx, sandboxproxy.RouteRequest{RouteID: "route-a"})
		if err != nil {
			t.Fatal(err)
		}
		if identity.ConvID != "conv" {
			t.Fatal("late identity was not supplied by daemon")
		}
	}
	resolution, err := authority.ResolveRoute(ctx, sandboxproxy.RouteRequest{Identity: identity, RouteID: "route-a", Port: 43127})
	if err != nil {
		t.Fatal(err)
	}
	if resolution.LeaseID != "lease-a" || resolution.Endpoint.Port() != 43128 {
		t.Fatalf("resolution = %#v", resolution)
	}
	if err := authority.ReleaseRoute(ctx, resolution); err != nil {
		t.Fatal(err)
	}
	if seenCredential.Load() < 5 || closed.Load() != 1 {
		t.Fatalf("credential requests=%d closed=%d", seenCredential.Load(), closed.Load())
	}
	old := identity
	old.LaunchGeneration = "predecessor"
	if _, err := authority.ResolveRoute(ctx, sandboxproxy.RouteRequest{Identity: old, RouteID: "route-a", Port: 43127}); err == nil {
		t.Fatal("predecessor launch unexpectedly resolved a route")
	}
	authority.Close()
	if _, err := authority.Identity(ctx); err == nil {
		t.Fatal("closed route authority retained its capability")
	}
	_ = os.Remove(socketPath)
}

func TestHostDiscoveryDoesNotAuthorizeProxyConsumption(t *testing.T) {
	authority := newProxyRouteAuthority(proxyRouteAuthorityConfig{GroupIDs: []int64{0}}, "credential")
	if authority.allowedGroup(0) {
		t.Fatal("host discovery selector must not authorize consumer routes")
	}
	if _, err := authority.Identity(context.Background()); err == nil {
		t.Fatal("host-only discovery has no group consumer identity")
	}
}

func TestFilteringProxySkipsHostOnlyConsumerAuthority(t *testing.T) {
	if config := proxyConsumerAuthorityConfig(proxyRouteAuthorityConfig{GroupIDs: []int64{0}}); config != nil {
		t.Fatal("host-only selector must skip filtering proxy consumer identity setup")
	}
	config := proxyConsumerAuthorityConfig(proxyRouteAuthorityConfig{GroupIDs: []int64{0, 42}})
	if config == nil || len(config.GroupIDs) != 1 || config.GroupIDs[0] != 42 {
		t.Fatalf("mixed discovery must initialize proxy with real groups only: %#v", config)
	}
}
