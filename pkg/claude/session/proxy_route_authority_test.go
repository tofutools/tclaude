package session

import (
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
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "capability-only-in-memory") {
		t.Fatalf("route authority metadata serialized a capability: %s", encoded)
	}
	authority := newProxyRouteAuthority(config, "capability-only-in-memory")
	identity, err := authority.Identity(nil)
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := authority.ResolveRoute(nil, sandboxproxy.RouteRequest{Identity: identity, RouteID: "route-a", Port: 43127})
	if err != nil {
		t.Fatal(err)
	}
	if resolution.LeaseID != "lease-a" || resolution.Endpoint.Port() != 43128 {
		t.Fatalf("resolution = %#v", resolution)
	}
	if err := authority.ReleaseRoute(nil, resolution); err != nil {
		t.Fatal(err)
	}
	if seenCredential.Load() < 5 || closed.Load() != 1 {
		t.Fatalf("credential requests=%d closed=%d", seenCredential.Load(), closed.Load())
	}
	old := identity
	old.LaunchGeneration = "predecessor"
	if _, err := authority.ResolveRoute(nil, sandboxproxy.RouteRequest{Identity: old, RouteID: "route-a", Port: 43127}); err == nil {
		t.Fatal("predecessor launch unexpectedly resolved a route")
	}
	authority.Close()
	if _, err := authority.Identity(nil); err == nil {
		t.Fatal("closed route authority retained its capability")
	}
	_ = os.Remove(socketPath)
}
