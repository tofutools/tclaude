package agentd_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/tofutools/tclaude/pkg/federation/hub"
	"github.com/tofutools/tclaude/pkg/noderun"
	"github.com/tofutools/tclaude/pkg/selfupdate"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// Separate test processes isolate the daemon's singleton identity, database,
// harness and tmux simulators. Neither endpoint scripts federation replies:
// both run the real dashboard mux and federation dispatcher through the hub.
func TestSkynetJourneyInstance(t *testing.T) {
	if os.Getenv("TCLAUDE_SKYNET_JOURNEY_CHILD") != "1" {
		t.Skip("fixture process for TestSkynetJourney")
	}
	f := newFlow(t)
	agentd.ResetFederationForTest()
	t.Cleanup(agentd.ResetFederationForTest)
	for _, name := range []string{"shared", "hidden", "receiver"} {
		f.HaveGroup(name)
	}
	for i, name := range []string{"worker", "retiring", "traveler", "secret"} {
		conv := fmt.Sprintf("019fe740-43a4-7023-b8ae-1ee64459f2a%d", i+1)
		f.HaveConvWithTitle(conv, name)
		f.HaveAliveSession(conv, name, name+"-pane", testutil.CanonicalTempDir(t))
		group := "shared"
		if name == "secret" {
			group = "hidden"
		}
		f.HaveMember(group, conv)
	}
	cleanup, err := agentd.SetNodeRunExecutorForTest(func(_ context.Context, path, _ string, _ int64) noderun.Result {
		raw, err := os.ReadFile(path)
		if err != nil {
			return noderun.Result{ExitCode: 125, Error: err.Error()}
		}
		return noderun.Result{Stdout: string(raw)}
	})
	require.NoError(t, err)
	t.Cleanup(cleanup)
	update, err := selfupdate.New(filepath.Join(testutil.CanonicalTempDir(t), "updates"), nil, selfupdate.Hooks{})
	require.NoError(t, err)
	t.Cleanup(agentd.SetNodeUpdateServiceForTest(update))
	transport := http.DefaultTransport
	http.DefaultTransport = journeyTransport{underlying: transport}
	t.Cleanup(func() { http.DefaultTransport = transport })
	server := httptest.NewServer(agentd.BuildDashboardHandlerForTest())
	t.Cleanup(server.Close)
	t.Cleanup(agentd.SetPopupBaseURLForTest(server.URL))
	fmt.Println("SKYNET_JOURNEY_READY " + server.URL)
	// Parent closes stdin after completing the journey; normal return executes
	// all fixture cleanup and reports failures to the parent's Wait.
	_, err = io.Copy(io.Discard, os.Stdin)
	require.NoError(t, err)
}

type journeyTransport struct{ underlying http.RoundTripper }

func (tr journeyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host == "api.github.com" {
		if r.Method != "GET" || !strings.HasSuffix(r.URL.Path, "/releases/latest") {
			return nil, fmt.Errorf("unexpected update network request: %s %s", r.Method, r.URL)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"tag_name":"v99.0.0","assets":[]}`)), Request: r}, nil
	}
	return tr.underlying.RoundTrip(r)
}

type journeyNode struct {
	url    string
	client *http.Client
}

func startJourneyNode(t *testing.T) *journeyNode {
	t.Helper()
	binary, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.Command(binary, "-test.run=^TestSkynetJourneyInstance$", "-test.timeout=60s")
	cmd.Env = append(os.Environ(), "TCLAUDE_SKYNET_JOURNEY_CHILD=1")
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	logPath := filepath.Join(testutil.CanonicalTempDir(t), "instance.log")
	log, err := os.Create(logPath)
	require.NoError(t, err)
	cmd.Stderr = log
	require.NoError(t, cmd.Start())
	ready := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "SKYNET_JOURNEY_READY ") {
				ready <- strings.TrimPrefix(line, "SKYNET_JOURNEY_READY ")
			} else {
				_, _ = fmt.Fprintln(log, line)
			}
		}
		close(ready)
	}()
	t.Cleanup(func() {
		_ = stdin.Close()
		if err := cmd.Wait(); err != nil {
			raw, _ := os.ReadFile(logPath)
			t.Errorf("journey instance failed: %v\n%s", err, raw)
		}
		_ = log.Close()
	})
	select {
	case url := <-ready:
		require.NotEmpty(t, url, "instance did not start; see %s", logPath)
		return &journeyNode{url: url, client: &http.Client{Timeout: 10 * time.Second}}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("journey instance startup timed out")
		return nil
	}
}

func (n *journeyNode) call(t *testing.T, method, path string, body any, status int) map[string]any {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(method, n.url+path, bytes.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	res, err := n.client.Do(req)
	require.NoError(t, err, "%s %s", method, path)
	defer res.Body.Close()
	out, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	require.Equal(t, status, res.StatusCode, "%s %s: %s", method, path, out)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(out, &decoded), "%s", out)
	return decoded
}

// Requests themselves synchronize observable async transitions. No sleep,
// wall-clock expiry or fabricated catalog/decision replies are used.
func journeyAwait(t *testing.T, what string, ready func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		if ready() {
			return
		}
	}
	t.Fatalf("journey did not reach %s", what)
}

func journeyRows(t *testing.T, value any) []map[string]any {
	t.Helper()
	rows, ok := value.([]any)
	require.True(t, ok, "expected array, got %#v", value)
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		object, ok := row.(map[string]any)
		require.True(t, ok)
		out = append(out, object)
	}
	return out
}

func TestSkynetJourney(t *testing.T) {
	store, err := hub.OpenStore(filepath.Join(testutil.CanonicalTempDir(t), "hub.sqlite"))
	require.NoError(t, err)
	h, err := hub.New(store, hub.Config{})
	require.NoError(t, err)
	server := httptest.NewServer(h.Handler())
	t.Cleanup(func() { h.Close(); server.Close(); _ = store.Close() })
	alice, bob := startJourneyNode(t), startJourneyNode(t)
	hubURL := "ws" + strings.TrimPrefix(server.URL, "http")
	for _, node := range []*journeyNode{alice, bob} {
		invite, err := store.CreateInvite("", time.Minute)
		require.NoError(t, err)
		node.call(t, "POST", "/api/federation/config", map[string]any{"enabled": true, "hub_url": hubURL, "invite": invite}, 200)
	}
	aliceID := alice.call(t, "GET", "/api/federation/status", nil, 200)["instance_id"].(string)
	bobID := bob.call(t, "GET", "/api/federation/status", nil, 200)["instance_id"].(string)
	journeyAwait(t, "hub directory", func() bool {
		return len(journeyRows(t, alice.call(t, "GET", "/api/federation/status", nil, 200)["peers"])) == 1 &&
			len(journeyRows(t, bob.call(t, "GET", "/api/federation/status", nil, 200)["peers"])) == 1
	})
	// Signed enrollment is previewed before consent and establishes restricted
	// trust on both real instances.
	alice.call(t, "POST", "/api/federation/profiles", map[string]any{"name": "journey", "definition": map[string]any{"trust_level": "restricted"}}, 200)
	token := alice.call(t, "POST", "/api/federation/enroll-tokens", map[string]any{"profile": "journey", "uses": 1, "ttl_seconds": 600}, 200)["token"].(string)
	enroll := map[string]any{"master": aliceID, "token": token}
	enroll["preview_token"] = bob.call(t, "POST", "/api/federation/enroll/preview", enroll, 200)["preview_token"]
	bob.call(t, "POST", "/api/federation/enroll", enroll, 200)
	for _, pair := range []struct {
		node *journeyNode
		peer string
	}{{alice, bobID}, {bob, aliceID}} {
		pair.node.call(t, "POST", "/api/federation/peers/trust", map[string]any{"instance": pair.peer, "level": "restricted"}, 200)
		summary := pair.node.call(t, "GET", "/api/federation/status?summary=1", nil, 200)
		require.Contains(t, fmt.Sprint(summary), pair.peer)
		pair.node.call(t, "GET", "/api/peer/"+pair.peer+"/node-summary", nil, 200)
	}
	remote := "/api/peer/" + bobID
	grant := func(slug, scope string) map[string]any {
		body := map[string]any{"peer": aliceID, "slug": slug, "scope": scope}
		bob.call(t, "POST", "/api/federation/grants", body, 200)
		return body
	}
	grant(agentd.PermGroupsRosterRead, "group=shared")
	snapshot := alice.call(t, "GET", remote+"/snapshot", nil, 200)
	groups := journeyRows(t, snapshot["groups"])
	require.Len(t, groups, 1)
	require.Equal(t, "shared", groups[0]["name"])
	groupID := groups[0]["id"]
	require.NotNil(t, groupID)
	omitted := journeyRows(t, snapshot["peer_view"].(map[string]any)["omitted"])
	require.NotEmpty(t, omitted)
	for _, entry := range omitted {
		_, ok := entry["requestable"].(bool)
		require.True(t, ok, "omission has no requestability: %#v", entry)
	}
	agents := journeyRows(t, snapshot["agents"])
	ids := map[string]string{}
	for _, agent := range agents {
		ids[agent["title"].(string)] = agent["agent_id"].(string)
	}
	require.Len(t, ids, 3)
	alice.call(t, "GET", remote+"/groups/hidden", nil, 404)
	alice.call(t, "POST", remote+"/agents/"+ids["worker"]+"/stop", nil, 403)

	request := alice.call(t, "POST", remote+"/peer-access-requests", map[string]any{"permission": agentd.PermGroupsMembersStop, "group_id": groupID, "grant_ttl_seconds": 600, "reason": "journey stop"}, 202)
	requestID := request["id"].(string)
	pending := bob.call(t, "GET", "/api/federation/access-requests", nil, 200)
	require.Contains(t, fmt.Sprint(pending), requestID)
	bob.call(t, "POST", "/api/federation/access-requests/"+requestID+"/decision", map[string]any{"decision": "approve", "grant_ttl_seconds": 300}, 200)
	journeyAwait(t, "access approval", func() bool {
		return alice.call(t, "GET", remote+"/peer-access-requests/"+requestID, nil, 200)["status"] == "approved"
	})
	alice.call(t, "POST", remote+"/agents/"+ids["worker"]+"/stop", nil, 200)
	bob.call(t, "DELETE", "/api/federation/grants", map[string]any{"peer": aliceID, "slug": agentd.PermGroupsMembersStop, "scope": fmt.Sprintf("group_id=%.0f", groupID)}, 200)
	alice.call(t, "POST", remote+"/agents/"+ids["worker"]+"/stop", nil, 403)
	for _, slug := range []string{agentd.PermGroupsMembersClone, agentd.PermGroupsMembersRetire, agentd.PermAgentMove} {
		grant(slug, "group=shared")
	}
	cloned := alice.call(t, "POST", remote+"/agents/"+ids["worker"]+"/clone", map[string]any{"no_copy_conv": true}, 200)
	require.NotEmpty(t, cloned["new_conv"])
	alice.call(t, "POST", remote+"/agents/"+ids["retiring"]+"/retire", map[string]any{}, 200)
	alice.call(t, "POST", remote+"/agents/"+ids["traveler"]+"/teleport", map[string]any{"group": "receiver"}, 200)
	for _, action := range []string{"stop", "retire", "clone", "teleport"} {
		alice.call(t, "POST", remote+"/agents/secret/"+action, map[string]any{"group": "receiver"}, 404)
	}

	// Merged views use local plus peer snapshots. Links and grant rows are
	// strictly local administration, with stable typed identities for revokes.
	alice.call(t, "GET", "/api/snapshot", nil, 200)
	alice.call(t, "GET", remote+"/snapshot", nil, 200)
	links := bob.call(t, "GET", "/api/federation/links?group=shared", nil, 200)
	require.Contains(t, fmt.Sprint(links), aliceID)
	grants := bob.call(t, "GET", "/api/federation/grants?peer="+aliceID, nil, 200)
	require.Contains(t, fmt.Sprint(grants), "group_id=")
	for _, slug := range []string{agentd.PermNodeHarnessesRead, agentd.PermNodeUpdate, agentd.PermNodeExec} {
		grant(slug, "")
	}
	alice.call(t, "GET", remote+"/harnesses/availability", nil, 200)
	update := alice.call(t, "POST", remote+"/node/update", map[string]any{"action": "check"}, 202)
	journeyAwait(t, "check-only update", func() bool {
		return alice.call(t, "GET", remote+"/node/update/jobs/"+update["id"].(string), nil, 200)["state"] == "succeeded"
	})
	script := map[string]any{"script": "journey script", "timeout_seconds": 10}
	alice.call(t, "POST", remote+"/node/run", script, 403)
	bob.call(t, "PUT", "/api/node/run/settings", map[string]any{"accept_remote_scripts": true}, 200)
	run := alice.call(t, "POST", remote+"/node/run", script, 202)
	journeyAwait(t, "script completion", func() bool {
		return alice.call(t, "GET", remote+"/node/run/jobs/"+run["id"].(string), nil, 200)["state"] == "completed"
	})
	bob.call(t, "DELETE", "/api/groups/shared", nil, 200)
	remaining := bob.call(t, "GET", "/api/federation/grants?peer="+aliceID, nil, 200)
	require.NotContains(t, fmt.Sprint(remaining), "group_id=")
}
