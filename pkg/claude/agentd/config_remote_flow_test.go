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
		require.Equal(t, "unrestricted trust", refused.Denied[0].Requires)
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
		"agent.http_proxies": map[string]any{"search": map[string]any{"url": "https://api2.example.com", "header": "Authorization"}},
	}})
	require.Equal(t, 200, code, body)
	cfg, err = config.Load()
	require.NoError(t, err)
	require.Equal(t, "https://api2.example.com", cfg.Agent.HTTPProxies["search"].URL)
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

	// An unrestricted peer changes every class, and still reads no secret.
	setFedTrustLevel(t, fh, db.FederationTrustUnrestricted)
	v = get()
	require.Equal(t, "unrestricted", v.Access.Level)
	for class, access := range v.Access.Classes {
		require.Equal(t, "write", access, class)
	}
	require.NotEmpty(t, v.Path)
	code, _, body = call("PATCH", "/api/config", map[string]any{"version": v.Version, "set": map[string]any{"audit.retention_days": 400}})
	require.Equal(t, 200, code, body)
	cfg, err = config.Load()
	require.NoError(t, err)
	require.Equal(t, 400, cfg.Audit.RetentionDays)
	require.Equal(t, "deploy-s3cret", cfg.SelfUpdate().Custom.Command)

	// Every change and refusal is audited under the peer, without values.
	rec := fedHuman(t, fh.f, "GET", "/v1/federation/audit", nil)
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), "federation.node.config")
	require.Contains(t, rec.Body.String(), "operator@"+fh.peer.id.ID())
	require.NotContains(t, rec.Body.String(), "new-value")
}
