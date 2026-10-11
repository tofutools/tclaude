package agentd_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/client"
	"github.com/tofutools/tclaude/pkg/federation/hub"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/federation/stream"
	"github.com/tofutools/tclaude/pkg/testutil"
	"golang.org/x/sys/unix"
)

// Isolate receiver CPU from the hub and scripted peers in the parent process.
// No metrics endpoint or instrumentation is compiled into production binaries.
func TestSkynetPollingInstance(t *testing.T) {
	if os.Getenv("TCLAUDE_SKYNET_POLLING_CHILD") != "1" {
		t.Skip("polling fixture child")
	}
	f := newFlow(t)
	agentd.ResetFederationForTest()
	t.Cleanup(agentd.ResetFederationForTest)
	git, err := exec.LookPath("git")
	require.NoError(t, err)
	bin := testutil.CanonicalTempDir(t)
	require.NoError(t, os.Symlink(git, filepath.Join(bin, "git")))
	t.Setenv("PATH", bin) // never launch installed host harness probes
	f.HaveGroup("team")
	for i := 0; i < 20; i++ {
		conv := fmt.Sprintf("019fe740-43a4-7023-b8ae-1ee64459%04d", i)
		f.HaveConvWithTitle(conv, fmt.Sprintf("worker-%02d", i))
		f.HaveMember("team", conv)
		f.HaveAliveSession(conv, fmt.Sprintf("poll-session-%d", i), fmt.Sprintf("poll-pane-%d", i), f.TestCwd("work"))
	}
	var gathers, peerGathers, dispatchCPU, dispatchWall, requests atomic.Int64
	cpu := func() int64 {
		var usage unix.Rusage
		require.NoError(t, unix.Getrusage(unix.RUSAGE_SELF, &usage))
		return usage.Utime.Nano() + usage.Stime.Nano()
	}
	t.Cleanup(agentd.SetPeerViewObserverForTest(func() func() {
		before, start := cpu(), time.Now()
		return func() {
			dispatchCPU.Add(cpu() - before)
			dispatchWall.Add(time.Since(start).Nanoseconds())
			requests.Add(1)
		}
	}))
	t.Cleanup(agentd.SetStatusGatherHookForTest(func() { gathers.Add(1) }))
	t.Cleanup(agentd.SetPeerStatusGatherHookForTest(func() { peerGathers.Add(1) }))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /polling-metrics", func(w http.ResponseWriter, _ *http.Request) {
		var usage unix.Rusage
		if err := unix.Getrusage(unix.RUSAGE_SELF, &usage); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"cpu_ns": usage.Utime.Nano() + usage.Stime.Nano(), "gathers": gathers.Load(), "peer_gathers": peerGathers.Load(), "dispatch_cpu_ns": dispatchCPU.Load(), "dispatch_wall_ns": dispatchWall.Load(), "requests": requests.Load()})
	})
	mux.HandleFunc("POST /polling-reset", func(w http.ResponseWriter, _ *http.Request) { agentd.ResetStatusSnapshotForTest(); w.WriteHeader(204) })
	mux.HandleFunc("POST /polling-status-write", func(w http.ResponseWriter, _ *http.Request) {
		require.NoError(t, db.UpdateSessionModel("poll-session-0", "model-after-write"))
		w.WriteHeader(204)
	})
	mux.Handle("/", agentd.BuildDashboardHandlerForTest())
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	t.Cleanup(agentd.SetPopupBaseURLForTest(server.URL))
	fmt.Println("SKYNET_JOURNEY_READY " + server.URL)
	_, err = io.Copy(io.Discard, os.Stdin)
	require.NoError(t, err)
}

//	TCLAUDE_SKYNET_POLLING=1 scripts/test.sh ./pkg/claude/agentd/ \
//	  -run '^TestSkynetPollingEfficiency$' -v -count=1 -timeout=180s
//
// Real cadence takes roughly one minute; normal CI only sees a skipped test.
func TestSkynetPollingEfficiency(t *testing.T) {
	if os.Getenv("TCLAUDE_SKYNET_POLLING") != "1" {
		t.Skip("set TCLAUDE_SKYNET_POLLING=1 to measure the receiver")
	}
	receiver := startSkynetFixtureNode(t, "TestSkynetPollingInstance", "TCLAUDE_SKYNET_POLLING_CHILD=1")
	store, err := hub.OpenStore(filepath.Join(testutil.CanonicalTempDir(t), "hub.sqlite"))
	require.NoError(t, err)
	// Exclude startup catalog fan-out and hub control-plane quotas from the
	// receiver-work measurement (the bridge doubles normal control traffic).
	h, err := hub.New(store, hub.Config{FramesPerMinute: 100000})
	require.NoError(t, err)
	server := httptest.NewServer(h.Handler())
	t.Cleanup(func() { h.Close(); server.Close(); _ = store.Close() })
	hubURL := "ws" + strings.TrimPrefix(server.URL, "http")
	invite, err := store.CreateInvite("", time.Minute)
	require.NoError(t, err)
	receiver.call(t, "POST", "/api/federation/config", map[string]any{"enabled": true, "hub_url": hubURL, "invite": invite}, 200)
	id := receiver.call(t, "GET", "/api/federation/status", nil, 200)["instance_id"].(string)
	peers := make([]*fedPeer, 0, 10)
	for i := 0; i < 10; i++ {
		identity, err := proto.NewIdentity()
		require.NoError(t, err)
		require.NoError(t, store.Admit(identity.ID()))
		p := &fedPeer{t: t, id: identity, agentdID: id}
		opens := make(chan *proto.Envelope, 16)
		cl, err := client.New(client.Options{URL: hubURL, Identity: identity, Name: fmt.Sprintf("poll-peer-%d", i), OnDeliver: func(from string, s *proto.Sealed) {
			key, ok := p.cl.LookupKey(from)
			if !ok {
				return
			}
			env, err := proto.Open(s, ed25519.PublicKey(key), p.id, time.Now())
			if err != nil {
				return
			}
			p.mu.Lock()
			p.got = append(p.got, env)
			p.mu.Unlock()
			if env.Kind == proto.KindPeerViewOpen {
				opens <- env
			}
		}})
		require.NoError(t, err)
		p.cl = cl
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { cl.Run(ctx); close(done) }()
		t.Cleanup(func() { cancel(); <-done })
		journeyAwait(t, "peer directory", func() bool { _, ok := cl.LookupKey(id); return ok })
		journeyAwait(t, "receiver directory", func() bool {
			rows := journeyRows(t, receiver.call(t, "GET", "/api/federation/status", nil, 200)["peers"])
			for _, row := range rows {
				if row["instance_id"] == p.id.ID() {
					return true
				}
			}
			return false
		})
		receiver.call(t, "POST", "/api/federation/peers/trust", map[string]any{"instance": p.id.ID(), "level": "restricted"}, 200)
		for _, slug := range []string{agentd.PermAgentsStatusRead, agentd.PermGroupsRosterRead} {
			receiver.call(t, "POST", "/api/federation/grants", map[string]any{"peer": p.id.ID(), "slug": slug, "scope": "group=team"}, 200)
		}
		// A simulated node bridges the outbound proxy request back through a
		// separately authenticated inbound stream to the real receiving dispatcher.
		// It never fabricates snapshot or summary responses. Both encrypted hops
		// traverse the hub; all CPU measurements below come from the child daemon.
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case env := <-opens:
					var opening proto.PeerViewOpenPayload
					require.NoError(t, env.DecodePayload(&opening))
					kp, err := stream.NewKeyPair()
					require.NoError(t, err)
					answer := p.envelope(proto.KindPeerViewAnswer, proto.Endpoint{}, proto.PeerViewAnswerPayload{Stream: opening.Stream, OK: true, Key: kp.Pub})
					answer.InReplyTo = env.ID
					p.send(answer)
					outbound := fedPeerStream(t, p, opening.Stream, kp, opening.Key, false)
					request := readPeerViewWire(t, outbound)
					inbound := openInboundPeerView(t, p)
					writePeerViewWire(t, inbound, request)
					reply := readPeerViewWire(t, inbound)
					_ = inbound.Close()
					writePeerViewWire(t, outbound, reply)
					_ = outbound.Close()
				}
			}
		}()
		peers = append(peers, p)
	}
	type response struct {
		status int
		bytes  int
		etag   string
		body   []byte
	}
	request := func(peer *fedPeer, tail, tag string) response {
		t.Helper()
		req, err := http.NewRequest("GET", receiver.url+"/api/peer/"+peer.id.ID()+"/"+tail, nil)
		require.NoError(t, err)
		req.Header.Set("If-None-Match", tag)
		res, err := receiver.client.Do(req)
		require.NoError(t, err)
		defer res.Body.Close()
		raw, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		require.Contains(t, []int{200, 304}, res.StatusCode, string(raw))
		return response{res.StatusCode, len(raw), res.Header.Get("ETag"), raw}
	}
	first := request(peers[0], "node-summary", "")
	require.NotEmpty(t, first.etag)
	unchanged := request(peers[0], "node-summary", first.etag)
	require.Equal(t, 304, unchanged.status)
	require.Zero(t, unchanged.bytes)
	require.Equal(t, first.etag, unchanged.etag)
	metrics := func() (int64, int64, int64) {
		v := receiver.call(t, "GET", "/polling-metrics", nil, 200)
		return int64(v["dispatch_cpu_ns"].(float64)), int64(v["peer_gathers"].(float64)), int64(v["gathers"].(float64))
	}
	// A tight ten-peer fan-out must share one gather, rather than one per peer.
	receiver.call(t, "POST", "/polling-reset", nil, 204)
	cpu, gather, total := metrics()
	start := time.Now()
	bytes := 0
	for i, peer := range peers {
		res := request(peer, "snapshot", "")
		bytes += res.bytes
		if i == 0 {
			var snapshot struct {
				Agents []struct {
					Online bool `json:"online"`
				} `json:"agents"`
			}
			require.NoError(t, json.Unmarshal(res.body, &snapshot))
			require.Len(t, snapshot.Agents, 20, "measure the intended visible workload")
			for _, agent := range snapshot.Agents {
				require.True(t, agent.Online)
			}
		}
	}
	afterCPU, afterGather, afterTotal := metrics()
	t.Logf("warm fan-out: peers=10 agents=20 cpu/request=%.3fms wall/request=%.3fms peer-gathers=%d total-gathers=%d bytes/request=%d", float64(afterCPU-cpu)/1e7, float64(time.Since(start).Microseconds())/10000, afterGather-gather, afterTotal-total, bytes/10)
	require.Equal(t, int64(1), afterGather-gather, "ten peer polls must share one status gather")
	receiver.call(t, "POST", "/polling-status-write", nil, 204)
	local := receiver.call(t, "GET", "/api/snapshot", nil, 200)
	require.Contains(t, fmt.Sprint(local), "model-after-write", "local status remains immediately fresh")
	_, changedGather, _ := metrics()
	require.Equal(t, afterGather, changedGather, "the local fresh read does not refresh the peer cache")
	for _, scenario := range []struct {
		name, tail  string
		interval    time.Duration
		count       int
		conditional bool
	}{
		{"map", "node-summary", time.Second, 20, true}, // 10 nodes each at 10s, staggered
		{"per-node", "snapshot", 2 * time.Second, 10, false},
		{"merged", "snapshot", 500 * time.Millisecond, 40, false}, // 10 nodes each at 5s
	} {
		tags := map[string]string{}
		bytes, notModified := 0, 0
		cpu, gather, total := metrics()
		start := time.Now()
		for i := 0; i < scenario.count; i++ {
			target := start.Add(time.Duration(i) * scenario.interval)
			if wait := time.Until(target); wait > 0 {
				time.Sleep(wait)
			}
			peer := peers[i%len(peers)]
			if scenario.name == "per-node" {
				peer = peers[0]
			}
			tag := ""
			if scenario.conditional {
				tag = tags[peer.id.ID()]
			}
			res := request(peer, scenario.tail, tag)
			bytes += res.bytes
			tags[peer.id.ID()] = res.etag
			if res.status == 304 {
				notModified++
			}
		}
		afterCPU, afterGather, afterTotal := metrics()
		t.Logf("%s: cadence=%s requests=%d cpu/request=%.3fms peer-gathers=%d total-gathers=%d cache-reuses=%d mean-body=%dB 304=%d elapsed=%s", scenario.name, scenario.interval, scenario.count, float64(afterCPU-cpu)/float64(scenario.count)/1e6, afterGather-gather, afterTotal-total, scenario.count-int(afterGather-gather), bytes/scenario.count, notModified, time.Since(start).Round(time.Millisecond))
	}
}
