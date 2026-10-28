package agentd

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
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/routeadapter"
	"github.com/tofutools/tclaude/pkg/claude/routebroker"
)

func TestHostRouteHTTPPreview(t *testing.T) {
	setupTestDB(t)
	t.Cleanup(closeAllHostRoutes)
	publisher, _, err := db.EnsureAgentForConv("http-publisher", "publisher")
	require.NoError(t, err)
	groupID, err := db.CreateAgentGroup("http-routes", "")
	require.NoError(t, err)
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: groupID, ConvID: "http-publisher"}))
	require.NoError(t, db.ReplaceAgentGroupPermissions(groupID, []string{PermRoutesPublish}, "test"))
	group, err := db.GetAgentGroupByID(groupID)
	require.NoError(t, err)
	var targetHost string
	target := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != targetHost {
			http.Error(w, "invalid host", http.StatusMisdirectedRequest)
			return
		}
		if r.URL.Path == "/events" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: ready\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		if r.Header.Get("Upgrade") == "websocket" {
			c, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer c.Close()
			_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
			_ = rw.Flush()
			_, _ = io.Copy(c, rw)
			return
		}
		w.Header().Set("X-Seen-Forwarded-Host", r.Header.Get("X-Forwarded-Host"))
		body, _ := io.ReadAll(r.Body)
		_, _ = fmt.Fprintf(w, "%s %s %s", r.Host, r.RequestURI, body)
	}))
	targetHost = target.Listener.Addr().String()
	target.Start()
	defer target.Close()
	route, err := db.CreateAgentRoute(groupID, publisher, "http-publisher", "http-generation", group.RouteGeneration, "ui", "tcp", "tcp://"+targetHost)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ours, theirs := net.Pipe()
	defer ours.Close()
	defer theirs.Close()
	ready := make(chan error, 1)
	go func() {
		_ = GroupRouteBroker().AttachPublisherReady(ctx, routebroker.PublisherAuth{
			RouteID: route.ID, AgentID: publisher, ConvID: "http-publisher", LaunchGeneration: "http-generation", GroupGeneration: group.RouteGeneration,
		}, theirs, func(err error) { ready <- err })
	}()
	select {
	case err := <-ready:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("attach timed out")
	}
	go func() { _ = routeadapter.RunPublisher(ctx, ours, route.Target) }()
	cookie, base := withDashboardAuthForTest(t)
	mux := http.NewServeMux()
	registerDashboardHostRoutes(mux)
	open := func(mode string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", base+"/api/routes/"+route.ID+"/host", bytes.NewBufferString(`{"mode":"`+mode+`"}`))
		req.Header.Set("Origin", base)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	require.Equal(t, http.StatusBadRequest, open("https").Code)
	raw := open("tcp")
	require.Equal(t, http.StatusCreated, raw.Code, raw.Body.String())
	var rawLease hostRouteLease
	require.NoError(t, json.Unmarshal(raw.Body.Bytes(), &rawLease))
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Get("http://" + rawLease.Endpoint + "/")
	require.NoError(t, err)
	require.Equal(t, http.StatusMisdirectedRequest, resp.StatusCode)
	_ = resp.Body.Close()
	closeHostRoute(rawLease.ID, "test raw done")
	rec := open("http")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var lease hostRouteLease
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &lease))
	require.Equal(t, "http", lease.Mode)
	_, err = openHostRouteMode(route, connPort(t, lease.Endpoint), "http")
	require.Error(t, err)
	require.Len(t, hostRoutesForRoute(route.ID), 1)
	require.Equal(t, "http", buildDashboardRouteMap([]*db.AgentGroup{group}, nil, map[int64][]*db.AgentRoute{groupID: {route}}, nil, nil).Routes[0].Consumers[0].Mode)
	req, err := http.NewRequest("POST", "http://"+lease.Endpoint+"/assets/app.js?q=a%2Fb", bytes.NewBufferString("payload"))
	require.NoError(t, err)
	req.Host = "route-" + lease.ID[len(hostRouteLeasePrefix):] + ".localhost:" + fmt.Sprint(connPort(t, lease.Endpoint))
	badReq, err := http.NewRequest("GET", "http://"+lease.Endpoint+"/", nil)
	require.NoError(t, err)
	badReq.Host = "attacker.example:" + fmt.Sprint(connPort(t, lease.Endpoint))
	refused, err := client.Do(badReq)
	require.NoError(t, err)
	require.Equal(t, http.StatusMisdirectedRequest, refused.StatusCode)
	_ = refused.Body.Close()
	req.Header.Set("X-Forwarded-Host", "attacker.example")
	resp, err = client.Do(req)
	require.NoError(t, err)
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, targetHost+" /assets/app.js?q=a%2Fb payload", string(data))
	require.Empty(t, resp.Header.Get("X-Seen-Forwarded-Host"))
	stream, err := client.Get("http://" + lease.Endpoint + "/events")
	require.NoError(t, err)
	defer stream.Body.Close()
	event := make([]byte, len("data: ready\n\n"))
	_, err = io.ReadFull(stream.Body, event)
	require.NoError(t, err)
	require.Equal(t, "data: ready\n\n", string(event))
	conn, err := net.DialTimeout("tcp4", lease.Endpoint, time.Second)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(3*time.Second)))
	_, err = fmt.Fprintf(conn, "GET /live HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n", req.Host)
	require.NoError(t, err)
	reader := bufio.NewReader(conn)
	upgrade, err := http.ReadResponse(reader, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusSwitchingProtocols, upgrade.StatusCode)
	_, err = conn.Write([]byte("live-reload"))
	require.NoError(t, err)
	data = make([]byte, len("live-reload"))
	_, err = io.ReadFull(reader, data)
	require.NoError(t, err)
	require.Equal(t, "live-reload", string(data))
	// One SSE stream plus seven upgrades exhaust all eight broker slots.
	var extraUpgrades []net.Conn
	for range 6 {
		extra, err := net.DialTimeout("tcp4", lease.Endpoint, time.Second)
		require.NoError(t, err)
		defer extra.Close()
		require.NoError(t, extra.SetDeadline(time.Now().Add(3*time.Second)))
		_, err = fmt.Fprintf(extra, "GET /live HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n", req.Host)
		require.NoError(t, err)
		response, err := http.ReadResponse(bufio.NewReader(extra), nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusSwitchingProtocols, response.StatusCode)
		extraUpgrades = append(extraUpgrades, extra)
	}
	overflow, err := client.Get("http://" + lease.Endpoint + "/overflow")
	require.NoError(t, err)
	require.Equal(t, http.StatusServiceUnavailable, overflow.StatusCode)
	require.Equal(t, "1", overflow.Header.Get("Retry-After"))
	_ = overflow.Body.Close()
	require.NoError(t, db.MarkAgentRoutePublisherLost(route.ID, "test revoke"))
	_, err = reader.ReadByte()
	require.Error(t, err)
	var timeout net.Error
	require.False(t, errors.As(err, &timeout) && timeout.Timeout(), "revocation must close upgraded streams")
	for _, extra := range extraUpgrades {
		_, err = extra.Read(make([]byte, 1))
		require.Error(t, err)
		require.False(t, errors.As(err, &timeout) && timeout.Timeout(), "revocation must close every upgraded stream")
	}
	_, err = stream.Body.Read(make([]byte, 1))
	require.Error(t, err)
	require.False(t, errors.As(err, &timeout) && timeout.Timeout(), "revocation must close streaming responses")
	require.Eventually(t, func() bool {
		c, err := net.DialTimeout("tcp4", lease.Endpoint, 50*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return false
		}
		return true
	}, time.Second, 10*time.Millisecond)
}
