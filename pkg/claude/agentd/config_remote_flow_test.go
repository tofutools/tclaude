package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type remoteConfigView struct {
	Raw      string `json:"raw"`
	Version  string `json:"version"`
	Path     string `json:"path"`
	Redacted []struct {
		Path string `json:"path"`
		Set  bool   `json:"set"`
	} `json:"redacted"`
	Access struct {
		Level   string            `json:"level"`
		Classes map[string]string `json:"classes"`
		Keys    []struct {
			Path   string `json:"path"`
			Class  string `json:"class"`
			Secret bool   `json:"secret"`
		} `json:"keys"`
	} `json:"access"`
	Changed []string `json:"changed"`
	Code    string   `json:"code"`
	Denied  []struct {
		Path     string `json:"path"`
		Requires string `json:"requires"`
	} `json:"denied"`
}

var remoteConfigSecrets = []string{"inv-s3cret", "hv-s3cret", "hook-s3cret", "deploy-s3cret"}

// seedRemoteConfigSecrets gives the node one value of every kind a peer must
// never read.
func seedRemoteConfigSecrets(t *testing.T) {
	t.Helper()
	_, err := config.Update(func(cfg *config.Config, _ error) error {
		if cfg.Federation == nil {
			cfg.Federation = &config.FederationConfig{}
		}
		cfg.Federation.Invite = "inv-s3cret"
		if cfg.Agent == nil {
			cfg.Agent = &config.AgentConfig{}
		}
		cfg.Agent.HTTPProxies = map[string]config.HTTPProxyConfig{
			"search": {URL: "https://api.example.com", Header: "Authorization", HeaderValue: "hv-s3cret"},
		}
		cfg.Notifications.NotificationCommand = []string{"curl", "https://hooks.example.com/hook-s3cret"}
		cfg.Updates = &config.UpdatesConfig{Self: &config.SelfUpdateConfig{Method: "custom", Custom: &config.CustomUpdateConfig{Command: "deploy-s3cret"}}}
		return nil
	})
	require.NoError(t, err)
}

func TestRemoteConfigGrantsSecretsAndDrift(t *testing.T) {
	fh := newFedHarness(t)
	seedRemoteConfigSecrets(t)
	peer := agentd.PeerViewHandler(fh.peer.id.ID())
	grant := func(slug string) {
		require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: fh.peer.id.ID(), Slug: slug}))
	}
	call := func(method, path string, body any) (int, remoteConfigView, string) {
		rec := testharness.Serve(peer, testharness.JSONRequest(t, method, path, body))
		var v remoteConfigView
		_ = json.Unmarshal(rec.Body.Bytes(), &v)
		return rec.Code, v, rec.Body.String()
	}
	get := func() remoteConfigView {
		code, v, body := call("GET", "/api/config", nil)
		require.Equal(t, 200, code, body)
		for _, secret := range remoteConfigSecrets {
			require.NotContains(t, body, secret, "a secret value never leaves the node")
		}
		return v
	}
	// edit returns the tab's save body: the view with mutate applied.
	edit := func(v remoteConfigView, mutate func(m map[string]any)) map[string]any {
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(v.Raw), &m))
		mutate(m)
		return map[string]any{"config": m, "base": v.Raw}
	}
	setLogLevel := func(m map[string]any) { m["log_level"] = "debug" }

	// Without node.config.read, the tab is not shared.
	code, _, _ := call("GET", "/api/config", nil)
	require.Equal(t, http.StatusForbidden, code)

	// Given node.config.read: every value but the secrets, all read-only.
	grant(agentd.PermNodeConfigRead)
	v := get()
	require.Equal(t, "restricted", v.Access.Level)
	require.Equal(t, "read", v.Access.Classes["general"])
	require.Empty(t, v.Path, "the config path names the node's home")
	redacted := map[string]bool{}
	for _, r := range v.Redacted {
		redacted[r.Path] = r.Set
	}
	require.Equal(t, map[string]bool{
		"federation.invite":                      true,
		"agent.http_proxies.search.header_value": true,
		"notifications.notification_command":     true,
		"updates.self.custom.command":            true,
	}, redacted)
	require.Contains(t, v.Raw, "api.example.com", "the rest of a secret's entry is shown")
	code, _, _ = call("POST", "/api/config", edit(v, setLogLevel))
	require.Equal(t, http.StatusForbidden, code, "reading does not change")

	// node.config.write changes the general keys, and only them; the
	// withheld secrets it sent back empty are kept.
	grant(agentd.PermNodeConfigWrite)
	v = get()
	require.Equal(t, "write", v.Access.Classes["general"])
	require.Equal(t, "read", v.Access.Classes["agents"])
	code, preview, body := call("POST", "/api/config?dry_run=1", edit(v, setLogLevel))
	require.Equal(t, 200, code, body)
	require.Equal(t, []string{"log_level"}, preview.Changed)
	cfg, err := config.Load()
	require.NoError(t, err)
	require.Equal(t, "info", cfg.LogLevel, "a dry run writes nothing")
	code, v, body = call("POST", "/api/config", edit(v, setLogLevel))
	require.Equal(t, 200, code, body)
	require.Equal(t, []string{"log_level"}, v.Changed)
	cfg, err = config.Load()
	require.NoError(t, err)
	require.Equal(t, "debug", cfg.LogLevel)
	require.Equal(t, "inv-s3cret", cfg.Federation.Invite)
	require.Equal(t, "hv-s3cret", cfg.Agent.HTTPProxies["search"].HeaderValue)
	require.Equal(t, []string{"curl", "https://hooks.example.com/hook-s3cret"}, cfg.Notifications.NotificationCommand)
	require.Equal(t, "deploy-s3cret", cfg.SelfUpdate().Custom.Command)

	// A key of another class refuses the whole change, naming the grant.
	code, refused, _ := call("POST", "/api/config", edit(v, func(m map[string]any) {
		m["log_level"] = "warn"
		m["agent"].(map[string]any)["default_permissions"] = []string{"node.exec"}
	}))
	require.Equal(t, http.StatusForbidden, code)
	require.Equal(t, "permission_denied", refused.Code)
	require.Len(t, refused.Denied, 1)
	require.Equal(t, "agent.default_permissions", refused.Denied[0].Path)
	require.Equal(t, agentd.PermNodeConfigAgents, refused.Denied[0].Requires)
	cfg, err = config.Load()
	require.NoError(t, err)
	require.Equal(t, "debug", cfg.LogLevel, "nothing of a refused change is written")

	// A secret sent back counts as a change, so a peer cannot confirm a
	// guess: the right and the wrong value are refused alike.
	for _, guess := range []string{"https://hooks.example.com/hook-s3cret", "https://hooks.example.com/wrong"} {
		code, refused, _ = call("POST", "/api/config", edit(v, func(m map[string]any) {
			m["notifications"].(map[string]any)["notification_command"] = []string{"curl", guess}
		}))
		require.Equal(t, http.StatusForbidden, code, guess)
		require.Equal(t, "notifications.notification_command", refused.Denied[0].Path)
		require.Equal(t, "the node itself", refused.Denied[0].Requires)
	}

	// A change made against an older view is refused.
	stale := edit(v, setLogLevel)
	code, _, _ = call("PATCH", "/api/config", map[string]any{"version": v.Version, "set": map[string]any{"log_level": "warn"}})
	require.Equal(t, 200, code)
	code, refused, _ = call("POST", "/api/config", stale)
	require.Equal(t, http.StatusConflict, code)
	require.Equal(t, "config_drift", refused.Code)
	code, _, _ = call("PATCH", "/api/config", map[string]any{"set": map[string]any{"log_level": "info"}})
	require.Equal(t, http.StatusBadRequest, code, "a peer names the version it changed")
	code, _, _ = call("PATCH", "/api/config", map[string]any{"version": get().Version, "set": map[string]any{"no_such": 1}})
	require.Equal(t, http.StatusBadRequest, code)

	// node.config.credentials: a secret is replaced or cleared, never read.
	grant(agentd.PermNodeConfigCredentials)
	v = get()
	require.Equal(t, "write", v.Access.Classes["credentials"])
	code, v, body = call("PATCH", "/api/config", map[string]any{"version": v.Version, "set": map[string]any{
		"agent.http_proxies": map[string]any{"search": map[string]any{"url": "https://api.example.com", "header": "Authorization", "environment_variable": "SEARCH"}},
	}})
	require.Equal(t, http.StatusForbidden, code, "a withheld header is bound to where it goes: "+body)
	code, v, body = call("PATCH", "/api/config", map[string]any{"version": get().Version, "set": map[string]any{
		"agent.http_proxies": map[string]any{"search": map[string]any{"url": "https://api.example.com", "header": "Authorization", "model_policy": nil}},
	}})
	require.Equal(t, 200, code, body)
	cfg, err = config.Load()
	require.NoError(t, err)
	require.Equal(t, "hv-s3cret", cfg.Agent.HTTPProxies["search"].HeaderValue, "the withheld value is kept")
	code, v, body = call("PATCH", "/api/config", map[string]any{"version": v.Version, "set": map[string]any{
		"agent.http_proxies": map[string]any{"search": map[string]any{"url": "https://api2.example.com", "header": "Authorization", "header_value": "new-value"}},
	}})
	require.Equal(t, 200, code, body)
	require.NotContains(t, body, "new-value")
	cfg, err = config.Load()
	require.NoError(t, err)
	require.Equal(t, "new-value", cfg.Agent.HTTPProxies["search"].HeaderValue)
	code, _, body = call("PATCH", "/api/config", map[string]any{"version": v.Version, "clear": []string{"agent.http_proxies.search.header_value"}})
	require.Equal(t, 200, code, body)
	cfg, err = config.Load()
	require.NoError(t, err)
	require.Empty(t, cfg.Agent.HTTPProxies["search"].HeaderValue)
	code, _, _ = call("PATCH", "/api/config", map[string]any{"version": get().Version, "clear": []string{"federation.invite"}})
	require.Equal(t, http.StatusForbidden, code, "the hub invite is federation trust: local")

	// The credential bindings: a kept secret goes nowhere new, and no peer
	// points a credential at a file.
	proxy := func(fields map[string]any) map[string]any {
		entry := map[string]any{"url": "https://api2.example.com", "header": "Authorization"}
		for k, val := range fields {
			entry[k] = val
		}
		return map[string]any{"version": get().Version, "set": map[string]any{"agent.http_proxies": map[string]any{"search": entry}}}
	}
	code, _, body = call("PATCH", "/api/config", proxy(map[string]any{"header_value": "kept-again"}))
	require.Equal(t, 200, code, body)
	code, refused, _ = call("PATCH", "/api/config", proxy(map[string]any{"url": "https://attacker.example"}))
	require.Equal(t, http.StatusForbidden, code, "a kept header does not follow a new URL")
	require.Equal(t, "agent.http_proxies.search.url", refused.Denied[0].Path)
	code, _, body = call("PATCH", "/api/config", proxy(map[string]any{"url": "https://attacker.example", "header_value": "the-peers-own"}))
	require.Equal(t, 200, code, "a secret sent again may go anywhere: "+body)
	code, refused, _ = call("PATCH", "/api/config", proxy(map[string]any{"header_value_file": "/home/op/.tclaude/data/operator-token"}))
	require.Equal(t, http.StatusForbidden, code, "no peer sets a credential file")
	require.Equal(t, "agent.http_proxies.search.header_value_file", refused.Denied[0].Path)
	// Renaming an entry would drop its withheld secret: refused until sent.
	code, refused, _ = call("PATCH", "/api/config", map[string]any{"version": get().Version, "set": map[string]any{
		"agent.http_proxies": map[string]any{"search2": map[string]any{"url": "https://attacker.example", "header": "Authorization"}},
	}})
	require.Equal(t, http.StatusBadRequest, code)
	require.Equal(t, "secret_not_sent", refused.Code)
	// A risky slug is never a peer's to hand every agent.
	grant(agentd.PermNodeConfigAgents)
	code, refused, _ = call("PATCH", "/api/config", map[string]any{"version": get().Version, "set": map[string]any{"agent.default_permissions": []string{"permissions.grant"}}})
	require.Equal(t, http.StatusForbidden, code)
	require.Equal(t, "permission_denied", refused.Code)
	// Adding is an allowlist: the setup defaults and read-only slugs. A
	// manager verb or a library manage could let a new agent grant itself
	// more.
	for _, slug := range []string{"roles.manage", "agent.spawn", "groups.create", "templates.manage", "human.inbox.read", "no.such.read"} {
		code, refused, _ = call("PATCH", "/api/config", map[string]any{"version": get().Version, "set": map[string]any{"agent.default_permissions": []string{slug}}})
		require.Equal(t, http.StatusForbidden, code, slug)
		require.Equal(t, "the node itself", refused.Denied[0].Requires, slug)
	}
	code, _, body = call("PATCH", "/api/config", map[string]any{"version": get().Version, "set": map[string]any{"agent.default_permissions": []string{"self.rename", agentd.PermAgentsStatusRead}}})
	require.Equal(t, 200, code, body)
	code, _, body = call("PATCH", "/api/config", map[string]any{"version": get().Version, "set": map[string]any{"agent.default_permissions": []string{}}})
	require.Equal(t, 200, code, "a peer may always remove: "+body)

	// The git proxy reaches only hosts the node already knows.
	code, refused, _ = call("PATCH", "/api/config", map[string]any{"version": get().Version, "set": map[string]any{"agent.git_proxy.allowed_remotes": []string{"evil.example/x"}}})
	require.Equal(t, http.StatusForbidden, code)
	require.Contains(t, refused.Denied[0].Path, "evil.example/x")
	code, _, body = call("PATCH", "/api/config", map[string]any{"version": get().Version, "set": map[string]any{"agent.git_proxy.allowed_remotes": []string{"github.com/tofutools"}}})
	require.Equal(t, 200, code, body)

	// An unrestricted peer changes every class but the node's own, and
	// still reads no secret.
	setFedTrustLevel(t, fh, db.FederationTrustUnrestricted)
	v = get()
	require.Equal(t, "unrestricted", v.Access.Level)
	for class, access := range v.Access.Classes {
		want := "write"
		if class == "local" {
			want = "read"
		}
		require.Equal(t, want, access, class)
	}
	require.NotEmpty(t, v.Path)
	code, _, body = call("PATCH", "/api/config", map[string]any{"version": v.Version, "set": map[string]any{"agent.dashboard_port": 9123}})
	require.Equal(t, 200, code, body)
	cfg, err = config.Load()
	require.NoError(t, err)
	require.Equal(t, 9123, cfg.Agent.DashboardPort)
	// Shell commands, remote scripts, the hub link and the audit trail are
	// the node's own: no peer changes them, unrestricted included.
	for path, value := range map[string]any{
		"federation.scripts.accept_remote_scripts": true,
		"notifications.notification_command":       []string{"sh", "-c", "curl attacker"},
		"federation.hub_url":                       "wss://attacker.example",
		"audit.retention_days":                     1,
	} {
		code, refused, _ = call("PATCH", "/api/config", map[string]any{"version": get().Version, "set": map[string]any{path: value}})
		require.Equal(t, http.StatusForbidden, code, path)
		require.Equal(t, "the node itself", refused.Denied[0].Requires, path)
	}
	cfg, err = config.Load()
	require.NoError(t, err)
	require.False(t, cfg.Federation.Scripts != nil && cfg.Federation.Scripts.AcceptRemoteScripts)
	require.Equal(t, "deploy-s3cret", cfg.SelfUpdate().Custom.Command)

	// Every change and refusal is audited under the peer, without values.
	rec := fedHuman(t, fh.f, "GET", "/v1/federation/audit", nil)
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), "federation.node.config")
	require.Contains(t, rec.Body.String(), "operator@"+fh.peer.id.ID())
	require.NotContains(t, rec.Body.String(), "new-value")
}
