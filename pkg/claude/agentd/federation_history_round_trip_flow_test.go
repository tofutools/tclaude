package agentd_test

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
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
	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/hub"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// Each node runs the production daemon mux in a separate test process: HOME,
// SQLite, native transcripts and federation globals must not be shared. Only
// the usual tmux/harness subprocess boundaries use the flow simulators.
func TestFederation_HistoryRoundTripNode(t *testing.T) {
	infoPath := os.Getenv("TCLAUDE_TEST_HISTORY_NODE")
	if infoPath == "" {
		t.Skip("round-trip subprocess helper")
	}
	name := os.Getenv("TCLAUDE_TEST_HISTORY_HARNESS")
	t.Setenv("CODEX_HOME", "")
	t.Setenv("GEMINI_CLI_HOME", "")
	f := newFlow(t)
	agentd.ResetFederationForTest()
	t.Cleanup(agentd.ResetFederationForTest)
	cwd := testutil.CanonicalTempDir(t)
	f.HaveGroup("project")
	_, err := db.SetAgentGroupDefaultCwd("project", cwd)
	require.NoError(t, err)
	// The operator explicitly permits return-home within the revisit window.
	_, err = config.Update(func(c *config.Config, e error) error {
		if e != nil {
			return e
		}
		if c.Federation == nil {
			c.Federation = &config.FederationConfig{}
		}
		if c.Federation.Teleport == nil {
			c.Federation.Teleport = &config.FederationTeleportConfig{}
		}
		c.Federation.Teleport.Limits.AllowReturn = true
		return nil
	})
	require.NoError(t, err)
	done := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/test/stop", func(w http.ResponseWriter, r *http.Request) { close(done) })
	mux.HandleFunc("/test/large", func(w http.ResponseWriter, r *http.Request) {
		if os.Getenv("TCLAUDE_LARGE_AGENT_TRANSFER") != "1" {
			w.WriteHeader(404)
			return
		}
		cc := f.World.CCs.GetByConvID(moveSourceConv)
		buf := make([]byte, 1<<20)
		for i := 0; i < 320; i++ {
			_, e := rand.Read(buf)
			require.NoError(t, e)
			require.NoError(t, cc.WriteUserTurn(base64.StdEncoding.EncodeToString(buf)))
		}
		require.NoError(t, cc.WriteUserTurn("LARGE-TRANSFER-END-MARKER"))
		w.WriteHeader(204)
	})
	mux.HandleFunc("/test/turn", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Conv      string
			Text      string
			Assistant string
			Peer      string
			Seed      bool
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		if in.Seed {
			switch name {
			case "gemini":
				resp, _ := spawnGemini(t, f, "project", map[string]any{"name": "traveller", "cwd": cwd, "initial_message": in.Text, "sandbox_implementation": "off"})
				in.Conv = resp.ConvID
			case "codex":
				f.HaveAliveCodexSession(in.Conv, "traveller", "traveller-pane", cwd)
			default:
				f.HaveAliveSession(in.Conv, "traveller", "traveller-pane", cwd)
			}
			f.HaveMember("project", in.Conv)
		}
		// Append through the same native writer as a harness completing a turn.
		switch name {
		case "gemini":
			sim := f.World.Geminis.GetByConvID(in.Conv)
			require.NotNil(t, sim)
			if !in.Seed {
				sim.Receive(in.Text)
				sim.Receive("Enter")
			}
			sim.WriteGeminiReply(in.Assistant, "gemini-2.5-flash")
		case "codex":
			require.NoError(t, f.World.Codexes.GetByConvID(in.Conv).WriteExchange(in.Text, in.Assistant))
		default:
			cc := f.World.CCs.GetByConvID(in.Conv)
			require.NoError(t, cc.WriteUserTurn(in.Text))
			require.NoError(t, cc.AppendTurn(map[string]any{"type": "assistant", "cwd": cwd, "message": map[string]any{"role": "assistant", "content": []map[string]string{{"type": "text", "text": in.Assistant}}}}))
		}
		require.NoError(t, db.GrantAgentPermissionWithScope(in.Conv, agentd.PermSelfTeleport, string(mustJSON(t, map[string]any{"peer": []string{in.Peer}})), "test operator"))
		if name == "gemini" && in.Seed {
			require.NoError(t, json.NewEncoder(w).Encode(map[string]string{"conv": in.Conv}))
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if conv := r.Header.Get("X-Test-Agent-Conv"); conv != "" {
			r = agentd.AsAgentPeer(r, conv)
		} else {
			r = agentd.AsHumanPeer(r)
		}
		f.Mux.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	instance := agentd.FederationInstanceIDForTest()
	identity, err := proto.LoadIdentity(agentd.FederationKeyPath())
	require.NoError(t, err)
	info := historyNode{URL: srv.URL, Instance: instance, Cwd: cwd, Fingerprint: proto.Fingerprint(identity.Pub)}
	raw, err := json.Marshal(info)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(infoPath, raw, 0600))
	select {
	case <-done:
	case <-time.After(historyTestLifetime()):
		t.Fatal("parent did not stop history node")
	}
}

type historyNode struct{ URL, Instance, Cwd, Fingerprint string }

func startHistoryNode(t *testing.T, name, label string) historyNode {
	t.Helper()
	root := testutil.CanonicalTempDir(t)
	infoPath := filepath.Join(root, "node.json")
	log, err := os.Create(filepath.Join(root, "node.log"))
	require.NoError(t, err)
	cmd := exec.Command(os.Args[0], "-test.run=^TestFederation_HistoryRoundTripNode$", "-test.v", "-test.timeout="+(historyTestLifetime()+10*time.Second).String())
	cmd.Env = append(os.Environ(), "TCLAUDE_TEST_HISTORY_NODE="+infoPath, "TCLAUDE_TEST_HISTORY_HARNESS="+name)
	cmd.Stdout, cmd.Stderr = log, log
	require.NoError(t, cmd.Start())
	var node historyNode
	t.Cleanup(func() {
		if node.URL != "" {
			resp, e := http.Post(node.URL+"/test/stop", "application/json", nil)
			if e == nil {
				_ = resp.Body.Close()
			}
		}
		finished := make(chan error, 1)
		go func() { finished <- cmd.Wait() }()
		select {
		case err := <-finished:
			_ = log.Close()
			if err != nil {
				t.Errorf("%s node: %v", label, err)
			}
		// Federation shutdown drains in-flight sends (each may take 15s), as
		// well as background reconciliation. Leave room under runner load.
		case <-time.After(30 * time.Second):
			_ = cmd.Process.Kill()
			<-finished
			_ = log.Close()
			t.Errorf("%s node failed to shut down", label)
		}
		if t.Failed() {
			raw, _ := os.ReadFile(log.Name())
			t.Logf("%s node log:\n%s", label, raw)
		}
	})
	// Each fresh process migrates its own SQLite database. Race instrumentation
	// plus concurrent CPU load can exceed the ordinary 30s startup budget.
	fedEventuallyWithin(t, label+" node starts", 90*time.Second, func() bool { raw, e := os.ReadFile(infoPath); return e == nil && json.Unmarshal(raw, &node) == nil })
	return node
}

// Capture both sides before node cleanup, including confirmation delivery and
// the last retirement error. Diagnostic requests must not fail the test again
// or hang cleanup when a node is unresponsive.
func historyMoveDiagnostics(t *testing.T, from, to historyNode, id string) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	for _, node := range []historyNode{from, to} {
		for _, path := range []string{"/v1/federation/moves/" + id, "/v1/federation/outbox", "/v1/federation/status"} {
			resp, err := client.Get(node.URL + path)
			if err != nil {
				t.Logf("%s %s: %v", node.Instance, path, err)
				continue
			}
			raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			_ = resp.Body.Close()
			t.Logf("%s %s: HTTP %d, read error %v\n%s", node.Instance, path, resp.StatusCode, err, raw)
		}
	}
}

func historyNodeRequest(t *testing.T, node historyNode, method, path, conv string, body any) (int, []byte) {
	t.Helper()
	var raw []byte
	if body != nil {
		raw = mustJSON(t, body)
	}
	req, err := http.NewRequest(method, node.URL+path, bytes.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Agent-Conv", conv)
	resp, err := (&http.Client{Timeout: historyTestRequestTimeout()}).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, raw
}

func TestFederation_TeleportHistoryRoundTrip(t *testing.T) {
	for _, name := range []string{"claude", "codex", "gemini"} {
		t.Run(name, func(t *testing.T) { runHistoryRoundTrip(t, name, false) })
	}
}
func TestFederation_LargeTeleportHistoryRoundTrip(t *testing.T) {
	if os.Getenv("TCLAUDE_LARGE_AGENT_TRANSFER") != "1" {
		t.Skip("set TCLAUDE_LARGE_AGENT_TRANSFER=1 for the 300 MiB flow")
	}
	runHistoryRoundTrip(t, "claude", true)
}
func historyTestLifetime() time.Duration {
	if os.Getenv("TCLAUDE_LARGE_AGENT_TRANSFER") == "1" {
		return 30 * time.Minute
	}
	return 5 * time.Minute
}
func historyTestRequestTimeout() time.Duration {
	if os.Getenv("TCLAUDE_LARGE_AGENT_TRANSFER") == "1" {
		return 5 * time.Minute
	}
	return 15 * time.Second
}
func runHistoryRoundTrip(t *testing.T, name string, large bool) {
	st, err := hub.OpenStore(filepath.Join(testutil.CanonicalTempDir(t), "hub.sqlite"))
	require.NoError(t, err)
	h, err := hub.New(st, hub.Config{BytesPerMinute: 2 << 30, StreamBytesPerSecond: 100 << 20, FramesPerMinute: 6000})
	require.NoError(t, err)
	var streamJoins atomic.Int32
	var interrupted atomic.Bool
	handler := h.Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if large && r.URL.Path == proto.StreamPath && streamJoins.Add(1) == 5 {
			handler.ServeHTTP(historyInterruptWriter{ResponseWriter: w, interrupted: &interrupted}, r)
		} else {
			handler.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(func() { h.Close(); srv.Close(); _ = st.Close() })
	a, b := startHistoryNode(t, name, "A"), startHistoryNode(t, name, "B")
	require.NotEqual(t, a.Instance, b.Instance)
	require.NotEqual(t, a.Cwd, b.Cwd)
	require.NoError(t, st.Admit(a.Instance))
	require.NoError(t, st.Admit(b.Instance))
	for _, pair := range [][2]historyNode{{a, b}, {b, a}} {
		code, raw := historyNodeRequest(t, pair[0], "POST", "/v1/federation/config", "", map[string]any{"enabled": true, "hub_url": "ws" + strings.TrimPrefix(srv.URL, "http"), "name": "history-" + name})
		require.Equal(t, 200, code, string(raw))
	}
	for _, pair := range [][2]historyNode{{a, b}, {b, a}} {
		fedEventually(t, "peer online", func() bool {
			_, raw := historyNodeRequest(t, pair[0], "GET", "/v1/federation/status", "", nil)
			var status fedStatusView
			require.NoError(t, json.Unmarshal(raw, &status))
			for _, p := range status.Peers {
				if p.InstanceID == pair[1].Instance && p.Online {
					return true
				}
			}
			return false
		})
		code, raw := historyNodeRequest(t, pair[0], "POST", "/v1/federation/peers/trust", "", map[string]any{"instance": pair[1].Instance, "level": "unrestricted", "confirm_fingerprint": pair[1].Fingerprint, "label": "other"})
		require.Equal(t, 200, code, string(raw))
	}
	const original = "Original plan from node A: repair the index."
	const originalReply = "I will inspect the source repository before travelling."
	code, raw := historyNodeRequest(t, a, "POST", "/test/turn", "", map[string]any{"Conv": moveSourceConv, "Text": original, "Assistant": originalReply, "Peer": b.Instance, "Seed": true})
	sourceConv := moveSourceConv
	if name == "gemini" {
		require.Equal(t, 200, code, string(raw))
		var seed struct{ Conv string }
		require.NoError(t, json.Unmarshal(raw, &seed))
		sourceConv = seed.Conv
		require.NotEmpty(t, sourceConv)
	} else {
		require.Equal(t, 204, code, string(raw))
	}
	if large {
		code, raw = historyNodeRequest(t, a, "POST", "/test/large", "", nil)
		require.Equal(t, 204, code, string(raw))
	}
	// Wait for the receiving group and teleport support to be advertised.
	for _, node := range []historyNode{a, b} {
		fedEventually(t, "receiving catalog", func() bool {
			_, raw := historyNodeRequest(t, node, "GET", "/v1/federation/status", "", nil)
			var status fedStatusView
			require.NoError(t, json.Unmarshal(raw, &status))
			for _, remote := range status.Remote {
				for _, group := range remote.Groups {
					if group.Name == "project" {
						return true
					}
				}
			}
			return false
		})
	}
	teleport := func(from, to historyNode, conv string, home bool) bundletransfer.Descriptor {
		input := map[string]any{"peer": to.Instance, "group": "project"}
		if home {
			input = map[string]any{"home": true, "group": "project"}
		}
		code, raw := historyNodeRequest(t, from, "POST", "/v1/whoami/teleport", conv, input)
		require.Equal(t, 200, code, string(raw))
		var result struct {
			Offer struct {
				Offer bundletransfer.Descriptor `json:"offer"`
			} `json:"offer"`
		}
		require.NoError(t, json.Unmarshal(raw, &result))
		require.NotEmpty(t, result.Offer.Offer.ID)
		if large {
			require.Greater(t, result.Offer.Offer.Bytes, int64(300<<20))
		}
		return result.Offer.Offer
	}
	land := func(node historyNode, d bundletransfer.Descriptor) string {
		path := "/v1/federation/bundle-offers/" + d.ID + "/import"
		fedEventuallyWithin(t, "offer received", historyTestRequestTimeout(), func() bool {
			code, _ := historyNodeRequest(t, node, "POST", path, "", map[string]any{"cwd": node.Cwd})
			return code == 200
		})
		code, raw := historyNodeRequest(t, node, "POST", path, "", map[string]any{"apply": true, "cwd": node.Cwd})
		require.Equal(t, 200, code, string(raw))
		var result struct {
			History bool `json:"history"`
			Spawn   struct {
				Conv string `json:"conv_id"`
			} `json:"spawn"`
		}
		require.NoError(t, json.Unmarshal(raw, &result))
		require.True(t, result.History)
		require.NotEmpty(t, result.Spawn.Conv)
		return result.Spawn.Conv
	}
	outward := teleport(a, b, sourceConv, false)
	remoteConv := land(b, outward)
	if large {
		require.True(t, interrupted.Load(), "the third stream chunk must be interrupted before successful resume")
		chunks := (outward.Bytes + bundletransfer.ChunkBytes - 1) / bundletransfer.ChunkBytes
		require.Equal(t, int32(2*(chunks+1)), streamJoins.Load(), "completed chunks must not be fetched again after interruption")
	}
	require.NotEqual(t, sourceConv, remoteConv)
	t.Cleanup(func() {
		if t.Failed() {
			historyMoveDiagnostics(t, a, b, outward.ID)
		}
	})
	// This crosses two real daemon processes: running observation, durable
	// outbox delivery/retry, then source teardown. The shared 10s in-process
	// test budget is shorter than even one production send timeout (15s).
	// Keep polling the committed state rather than sleeping for that budget.
	fedEventuallyWithin(t, "A retires only after B is running", 60*time.Second, func() bool {
		code, raw := historyNodeRequest(t, a, "GET", "/v1/federation/moves/"+outward.ID, "", nil)
		var move struct {
			State string `json:"state"`
		}
		return code == 200 && json.Unmarshal(raw, &move) == nil && move.State == "moved"
	})
	if large {
		code, raw = historyNodeRequest(t, a, "GET", "/v1/federation/moves/"+outward.ID, "", nil)
		require.Equal(t, 200, code, string(raw))
		var moved db.FederationAgentMove
		require.NoError(t, json.Unmarshal(raw, &moved))
		require.NotNil(t, moved.Transfer)
		require.Equal(t, outward.Bytes, moved.Transfer.BytesTotal)
		require.Equal(t, outward.Bytes, moved.Transfer.BytesDone)
	}
	const addition = "New result from node B: repaired the index and verified build 123."
	const addedReply = "The remote tests passed; return home with the patch."
	code, raw = historyNodeRequest(t, b, "POST", "/test/turn", "", map[string]any{"Conv": remoteConv, "Text": addition, "Assistant": addedReply, "Peer": a.Instance})
	require.Equal(t, 204, code, string(raw))
	home := teleport(b, a, remoteConv, true)
	require.True(t, home.Teleport.Home)
	require.Equal(t, a.Instance, home.Teleport.OriginInstance)
	returned := land(a, home)
	require.NotEqual(t, sourceConv, returned)
	require.NotEqual(t, remoteConv, returned)
	// Export through the production read surface after native SpawnResume has
	// reopened the reminted transcript; both the original and new turns survive.
	if large {
		req, e := http.NewRequest("GET", a.URL+"/v1/agent-bundle/export?agent="+returned+"&history=true", nil)
		require.NoError(t, e)
		resp, e := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
		require.NoError(t, e)
		defer resp.Body.Close()
		require.Equal(t, 200, resp.StatusCode)
		archive, e := os.CreateTemp(testutil.CanonicalTempDir(t), "returned.zip")
		require.NoError(t, e)
		defer archive.Close()
		_, e = io.Copy(archive, resp.Body)
		require.NoError(t, e)
		bundle, e := agentbundle.DecodeFile(archive, agentbundle.MaxBytes, testutil.CanonicalTempDir(t))
		require.NoError(t, e)
		defer bundle.Close()
		history, e := bundle.OpenHistory()
		require.NoError(t, e)
		defer history.Close()
		scan := bufio.NewScanner(history)
		scan.Buffer(make([]byte, 64<<10), 4<<20)
		found := map[string]bool{}
		for scan.Scan() {
			for _, want := range []string{original, originalReply, addition, addedReply, "LARGE-TRANSFER-END-MARKER"} {
				if bytes.Contains(scan.Bytes(), []byte(want)) {
					found[want] = true
				}
			}
		}
		require.NoError(t, scan.Err())
		require.Len(t, found, 5)
		require.Equal(t, returned, bundle.Manifest.History.SourceConvID)
		return
	}
	code, raw = historyNodeRequest(t, a, "GET", "/v1/agent-bundle/export?agent="+returned+"&history=true", "", nil)
	require.Equal(t, 200, code, string(raw))
	bundle, err := agentbundle.Decode(raw)
	require.NoError(t, err)
	require.Contains(t, string(bundle.Transcript), original)
	require.Contains(t, string(bundle.Transcript), addition)
	require.Contains(t, string(bundle.Transcript), originalReply)
	require.Contains(t, string(bundle.Transcript), addedReply)
	require.Equal(t, returned, bundle.Manifest.History.SourceConvID)
	require.Equal(t, a.Cwd, bundle.Manifest.Agent.Paths.Cwd)
}

// Interrupt one real hub-relayed stream after two whole chunks have already
// arrived. The production fetch retries through the same receiving API, keeping
// the verified prefix and validating the complete archive before import.
type historyInterruptWriter struct {
	http.ResponseWriter
	interrupted *atomic.Bool
}

func (w historyInterruptWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := w.ResponseWriter.(http.Hijacker).Hijack()
	if err != nil {
		return nil, nil, err
	}
	wrapped := &historyInterruptConn{Conn: conn, interrupted: w.interrupted}
	pending, err := rw.Peek(rw.Reader.Buffered())
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	prefix := append([]byte(nil), pending...)
	rw.Reader = bufio.NewReader(io.MultiReader(bytes.NewReader(prefix), wrapped))
	rw.Writer = bufio.NewWriter(wrapped)
	return wrapped, rw, nil
}

type historyInterruptConn struct {
	net.Conn
	bytes       atomic.Int64
	interrupted *atomic.Bool
}

func (c *historyInterruptConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if c.bytes.Add(int64(n)) > 1<<20 && c.interrupted.CompareAndSwap(false, true) {
		_ = c.Close()
	}
	return n, err
}

func (c *historyInterruptConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if c.bytes.Add(int64(n)) > 1<<20 && c.interrupted.CompareAndSwap(false, true) {
		_ = c.Close()
	}
	return n, err
}
