package agentd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/groupexport"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestHostBreakGlassOperatorGrantExecRevoke(t *testing.T) {
	f := newFlow(t)
	const conv = "glass-aaaa-bbbb-cccc-0001"
	f.HaveConvWithTitle(conv, "glass-agent")
	f.HaveEnrolledAgent(conv)
	grantBody := map[string]any{"target": conv, "duration": "30m", "reason": "test"}
	agentGrant := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/break-glass/grant", grantBody), conv))
	require.Equal(t, 403, agentGrant.Code, agentGrant.Body.String())
	sudo := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/sudo", map[string]any{"slugs": []string{"host.break-glass"}}), conv))
	require.Equal(t, 403, sudo.Code, sudo.Body.String())
	require.Contains(t, sudo.Body.String(), "not_requestable")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.Mux.ServeHTTP(w, agentd.AsAgentPeer(r, conv)) }))
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/break-glass/exec"
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.Error(t, err)
	require.Nil(t, conn)
	require.Equal(t, 403, response.StatusCode)
	require.NoError(t, response.Body.Close())
	grant := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/break-glass/grant", grantBody)))
	require.Equal(t, 200, grant.Code, grant.Body.String())
	conn, response, err = websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, response.Body.Close())
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	require.NoError(t, conn.WriteJSON(map[string]any{"shell": "printf hello; printf problem >&2; exit 7"}))
	var stdout, stderr string
	for {
		var frame struct {
			Stream   string `json:"stream"`
			Data     []byte `json:"data"`
			ExitCode *int   `json:"exit_code"`
			Error    string `json:"error"`
		}
		require.NoError(t, conn.ReadJSON(&frame))
		require.Empty(t, frame.Error)
		if frame.Stream == "stdout" {
			stdout += string(frame.Data)
		}
		if frame.Stream == "stderr" {
			stderr += string(frame.Data)
		}
		if frame.Stream == "exit" {
			require.NotNil(t, frame.ExitCode)
			require.Equal(t, 7, *frame.ExitCode)
			break
		}
	}
	require.Equal(t, "hello", stdout)
	require.Equal(t, "problem", stderr)
	revoked := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/break-glass/revoke", map[string]any{"target": conv})))
	require.Equal(t, 200, revoked.Code, revoked.Body.String())
	rows, err := db.ListActiveSudoGrants(conv)
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestHostBreakGlassRevokedDuringHandshakeCannotExecute(t *testing.T) {
	f := newFlow(t)
	const conv = "glass-race-aaaa-bbbb-0001"
	f.HaveConvWithTitle(conv, "glass-race")
	f.HaveEnrolledAgent(conv)
	grant := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/break-glass/grant", map[string]any{"target": conv})))
	require.Equal(t, 200, grant.Code, grant.Body.String())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.Mux.ServeHTTP(w, agentd.AsAgentPeer(r, conv)) }))
	defer server.Close()
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/break-glass/exec", nil)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, response.Body.Close())
	revoked := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/break-glass/revoke", map[string]any{"target": conv})))
	require.Equal(t, 200, revoked.Code, revoked.Body.String())
	marker := filepath.Join(testutil.CanonicalTempDir(t), "must-not-exist")
	require.NoError(t, conn.WriteJSON(map[string]any{"args": []string{"touch", marker}}))
	var frame struct {
		Stream string `json:"stream"`
		Error  string `json:"error"`
	}
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	require.NoError(t, conn.ReadJSON(&frame))
	require.Equal(t, "error", frame.Stream)
	require.Contains(t, frame.Error, "no live operator grant")
	_, err = os.Stat(marker)
	require.True(t, os.IsNotExist(err))
}

func TestHostBreakGlassIOAndProcessGroupCleanup(t *testing.T) {
	for _, mode := range []string{"stdin-env-cwd", "timeout", "disconnect", "background-child", "revoke", "expiry"} {
		t.Run(mode, func(t *testing.T) {
			f := newFlow(t)
			const conv = "glass-process-aaaa-bbbb-0001"
			f.HaveConvWithTitle(conv, "glass-process")
			f.HaveEnrolledAgent(conv)
			duration := "30m"

			granted := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/break-glass/grant", map[string]any{"target": conv, "duration": duration})))
			require.Equal(t, 200, granted.Code, granted.Body.String())
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.Mux.ServeHTTP(w, agentd.AsAgentPeer(r, conv)) }))
			defer server.Close()
			conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/break-glass/exec", nil)
			require.NoError(t, err)
			defer conn.Close()
			require.NoError(t, response.Body.Close())
			require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
			cwd := testutil.CanonicalTempDir(t)
			spec := map[string]any{"shell": "sleep 30 & child=$!; printf '%s\n' \"$child\"; wait \"$child\""}
			if mode == "background-child" {
				spec["shell"] = "sleep 30 & printf '%s\n' \"$!\""
			}
			if mode == "stdin-env-cwd" {
				spec = map[string]any{"shell": "cat; printf '\n%s\n' \"$GLASS_TEST\"; pwd", "stdin": true, "env": []string{"GLASS_TEST=override"}, "cwd": cwd}
			}
			if mode == "timeout" {
				spec["timeout"] = "500ms"
			}
			require.NoError(t, conn.WriteJSON(spec))
			if mode == "stdin-env-cwd" {
				require.NoError(t, conn.WriteJSON(map[string]any{"stream": "stdin", "data": []byte("hello input")}))
				require.NoError(t, conn.WriteJSON(map[string]any{"stream": "eof"}))
			}
			var output string
			var childPID int
			for {
				var frame struct {
					Stream   string `json:"stream"`
					Data     []byte `json:"data"`
					ExitCode *int   `json:"exit_code"`
				}
				require.NoError(t, conn.ReadJSON(&frame))
				if frame.Stream == "stdout" {
					output += string(frame.Data)
					if mode != "stdin-env-cwd" && childPID == 0 && strings.Contains(output, "\n") {
						childPID, err = strconv.Atoi(strings.TrimSpace(output))
						require.NoError(t, err)
						if mode == "disconnect" {
							require.NoError(t, conn.Close())
							break
						}
						if mode == "expiry" {
							handle, e := db.Open()
							require.NoError(t, e)
							_, e = handle.Exec("UPDATE agent_sudo_grants SET expires_at = ? WHERE slug = ?", time.Now().Add(-time.Second).UnixNano(), "host.break-glass")
							require.NoError(t, e)
						}
						if mode == "revoke" {
							revoked := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/break-glass/revoke", map[string]any{"target": conv})))
							require.Equal(t, 200, revoked.Code)
						}
					}
				}
				if frame.Stream == "exit" {
					require.NotNil(t, frame.ExitCode)
					if mode == "stdin-env-cwd" || mode == "background-child" {
						require.Equal(t, 0, *frame.ExitCode)
					} else {
						require.Equal(t, 124, *frame.ExitCode)
					}
					break
				}
			}
			if mode == "stdin-env-cwd" {
				require.Equal(t, "hello input\noverride\n"+cwd+"\n", output)
			} else {
				require.Positive(t, childPID)
				require.Eventually(t, func() bool {
					out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(childPID)).Output()
					return err != nil || strings.TrimSpace(string(out)) == "" || strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
				}, 5*time.Second, 20*time.Millisecond, "background child must be dead after "+mode)
			}
		})
	}
}

func TestHostBreakGlassOrdinaryPermissionsCannotEnableExecution(t *testing.T) {
	f := newFlow(t)
	const conv = "glass-denied-aaaa-bbbb-0001"
	f.HaveConvWithTitle(conv, "glass-denied")
	f.HaveEnrolledAgent(conv)
	// Ordinary grant rows and generic sudo grants are not dedicated operator grants.
	require.NoError(t, db.GrantAgentPermission(conv, "host.break-glass", "test"))
	_, err := db.InsertSudoGrant(&db.SudoGrant{ConvID: conv, Slug: "host.break-glass", ExpiresAt: time.Now().Add(time.Hour), GrantedBy: "human:popup-id=test"})
	require.NoError(t, err)
	req := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/break-glass/exec", nil), conv)
	result := testharness.Serve(f.Mux, req)
	require.Equal(t, 403, result.Code, result.Body.String())
	require.Contains(t, result.Body.String(), "operator_grant_required")
}

func TestHostBreakGlassPOSTStreamsOutputAndAuditLogs(t *testing.T) {
	f := newFlow(t)
	const conv = "glass-http-aaaa-bbbb-0001"
	f.HaveConvWithTitle(conv, "glass-http")
	f.HaveEnrolledAgent(conv)
	granted := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/break-glass/grant", map[string]any{"target": conv, "reason": "host diagnosis"})))
	require.Equal(t, 200, granted.Code, granted.Body.String())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.Mux.ServeHTTP(w, agentd.AsAgentPeer(r, conv)) }))
	defer server.Close()
	response, err := http.Post(server.URL+"/v1/break-glass/exec", "application/json", strings.NewReader(`{"shell":"printf hello; exit 3","reason":"check host"}`))
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, 200, response.StatusCode)
	require.Equal(t, "application/x-ndjson", response.Header.Get("Content-Type"))
	decoder := json.NewDecoder(response.Body)
	var output string
	for {
		var frame struct {
			Stream   string `json:"stream"`
			Data     []byte `json:"data"`
			ExitCode *int   `json:"exit_code"`
			Error    string `json:"error"`
		}
		require.NoError(t, decoder.Decode(&frame))
		require.Empty(t, frame.Error)
		if frame.Stream == "stdout" {
			output += string(frame.Data)
		}
		if frame.Stream == "exit" {
			require.NotNil(t, frame.ExitCode)
			require.Equal(t, 3, *frame.ExitCode)
			break
		}
	}
	require.Equal(t, "hello", output)
	require.Eventually(t, func() bool {
		rows, e := db.ListAuditLog(db.AuditLogFilter{Verb: "break-glass.exec"})
		return e == nil && len(rows) == 1
	}, time.Second, 10*time.Millisecond)
	rows, err := db.ListAuditLog(db.AuditLogFilter{Verb: "break-glass.exec"})
	require.NoError(t, err)
	require.Contains(t, rows[0].Detail, `"exit_code":3`)
	require.Contains(t, rows[0].Detail, "check host")
	logs := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/break-glass/runs", nil)))
	require.Equal(t, 200, logs.Code, logs.Body.String())
	var files []struct {
		Name string `json:"name"`
	}
	require.NoError(t, json.Unmarshal(logs.Body.Bytes(), &files))
	require.Len(t, files, 1)
	log := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/break-glass/runs/"+files[0].Name, nil)))
	require.Equal(t, 200, log.Code, log.Body.String())
	require.Contains(t, log.Body.String(), "hello")
	require.Contains(t, log.Body.String(), "Exit code: 3")
	denied := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/break-glass/runs", nil), conv))
	require.Equal(t, 403, denied.Code)
}

func TestHostBreakGlassCannotTravelThroughGroupArchives(t *testing.T) {
	f := newFlow(t)
	const conv = "glass-export-aaaa-bbbb-0001"
	f.HaveGroup("glass-source")
	f.HaveConvWithTitle(conv, "glass-export")
	f.HaveMember("glass-source", conv)
	granted := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/break-glass/grant", map[string]any{"target": conv})))
	require.Equal(t, 200, granted.Code, granted.Body.String())
	archive := exportGroupAs(t, f, "glass-source", "zip")
	exp, err := groupexport.Unmarshal(archive)
	require.NoError(t, err)
	for _, row := range exp.SudoGrants {
		require.NotEqual(t, "host.break-glass", row.Slug)
	}
	// Hand-crafted archive input must fail even though no exporter emits it.
	forged := &groupexport.Export{FormatVersion: groupexport.FormatVersion, SourceGroup: "glass-forged", SudoGrants: []groupexport.SudoGrant{{ConvID: "forged-conv", Slug: "host.break-glass", GrantedBy: "human:break-glass", GrantedAt: time.Now().Format(time.RFC3339Nano), ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339Nano)}}}
	body, err := groupexport.Marshal(forged)
	require.NoError(t, err)
	require.NoError(t, db.GrantAgentPermission(conv, "groups.import", "test"))
	request := agentd.AsAgentPeer(httptest.NewRequest(http.MethodPost, "/v1/groups/import?into="+url.QueryEscape(f.TestCwd("forged")), bytes.NewReader(body)), conv)
	result := testharness.Serve(f.Mux, request)
	require.Equal(t, 400, result.Code, result.Body.String())
	require.Contains(t, result.Body.String(), "dedicated operator grant")
	rows, err := db.ListHostBreakGlassGrants("")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, conv, rows[0].ConvID)
}
