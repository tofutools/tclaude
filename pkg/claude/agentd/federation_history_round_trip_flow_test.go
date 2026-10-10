package agentd_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	mux.HandleFunc("/test/turn", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Conv string
			Text string
			Peer string
			Seed bool
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		if in.Seed {
			if name == "codex" {
				f.HaveAliveCodexSession(in.Conv, "traveller", "traveller-pane", cwd)
			} else {
				f.HaveAliveSession(in.Conv, "traveller", "traveller-pane", cwd)
			}
			f.HaveMember("project", in.Conv)
		}
		// Append through the same native writer as a harness completing a turn.
		if name == "codex" {
			require.NoError(t, f.World.Codexes.GetByConvID(in.Conv).WriteExchange(in.Text, "Recorded "+in.Text))
		} else {
			cc := f.World.CCs.GetByConvID(in.Conv)
			require.NoError(t, cc.WriteUserTurn(in.Text))
			require.NoError(t, cc.AppendTurn(map[string]any{"type": "assistant", "cwd": cwd, "message": map[string]any{"role": "assistant", "content": []map[string]string{{"type": "text", "text": "Recorded " + in.Text}}}}))
		}
		require.NoError(t, db.GrantAgentPermissionWithScope(in.Conv, agentd.PermSelfTeleport, string(mustJSON(t, map[string]any{"peer": []string{in.Peer}})), "test operator"))
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
	case <-time.After(90 * time.Second):
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
	cmd := exec.Command(os.Args[0], "-test.run=^TestFederation_HistoryRoundTripNode$", "-test.v", "-test.timeout=100s")
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
				raw, _ := os.ReadFile(log.Name())
				t.Errorf("%s node: %v\n%s", label, err, raw)
			}
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-finished
			_ = log.Close()
			t.Errorf("%s node failed to shut down", label)
		}
	})
	fedEventually(t, label+" node starts", func() bool { raw, e := os.ReadFile(infoPath); return e == nil && json.Unmarshal(raw, &node) == nil })
	return node
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
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, raw
}

func TestFederation_TeleportHistoryRoundTrip(t *testing.T) {
	for _, name := range []string{"claude", "codex"} {
		t.Run(name, func(t *testing.T) {
			st, err := hub.OpenStore(filepath.Join(testutil.CanonicalTempDir(t), "hub.sqlite"))
			require.NoError(t, err)
			h, err := hub.New(st, hub.Config{})
			require.NoError(t, err)
			srv := httptest.NewServer(h.Handler())
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
			code, raw := historyNodeRequest(t, a, "POST", "/test/turn", "", map[string]any{"Conv": moveSourceConv, "Text": original, "Peer": b.Instance, "Seed": true})
			require.Equal(t, 204, code, string(raw))
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
				return result.Offer.Offer
			}
			land := func(node historyNode, d bundletransfer.Descriptor) string {
				path := "/v1/federation/bundle-offers/" + d.ID + "/import"
				fedEventually(t, "offer received", func() bool {
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
			outward := teleport(a, b, moveSourceConv, false)
			remoteConv := land(b, outward)
			require.NotEqual(t, moveSourceConv, remoteConv)
			fedEventually(t, "A retires only after B is running", func() bool {
				code, raw := historyNodeRequest(t, a, "GET", "/v1/federation/moves/"+outward.ID, "", nil)
				var move struct {
					State string `json:"state"`
				}
				return code == 200 && json.Unmarshal(raw, &move) == nil && move.State == "moved"
			})
			const addition = "New result from node B: repaired the index and verified build 123."
			code, raw = historyNodeRequest(t, b, "POST", "/test/turn", "", map[string]any{"Conv": remoteConv, "Text": addition, "Peer": a.Instance})
			require.Equal(t, 204, code, string(raw))
			home := teleport(b, a, remoteConv, true)
			require.True(t, home.Teleport.Home)
			require.Equal(t, a.Instance, home.Teleport.OriginInstance)
			returned := land(a, home)
			require.NotEqual(t, moveSourceConv, returned)
			require.NotEqual(t, remoteConv, returned)
			// Export through the production read surface after native SpawnResume has
			// reopened the reminted transcript; both the original and new turns survive.
			code, raw = historyNodeRequest(t, a, "GET", "/v1/agent-bundle/export?agent="+returned+"&history=true", "", nil)
			require.Equal(t, 200, code, string(raw))
			bundle, err := agentbundle.Decode(raw)
			require.NoError(t, err)
			require.Contains(t, string(bundle.Transcript), original)
			require.Contains(t, string(bundle.Transcript), addition)
			require.Contains(t, string(bundle.Transcript), "Recorded "+original)
			require.Contains(t, string(bundle.Transcript), "Recorded "+addition)
			require.Equal(t, returned, bundle.Manifest.History.SourceConvID)
			require.Equal(t, a.Cwd, bundle.Manifest.Agent.Paths.Cwd)
		})
	}
}
